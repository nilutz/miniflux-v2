// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

// configureUISearchTestOptions installs a parsed configuration with
// SEARCH_SIDECAR_URL forced to sidecarURL (empty string included),
// overriding whatever the ambient environment happens to have, so these
// tests are deterministic regardless of what else is running. Restored
// automatically at the end of the test.
func configureUISearchTestOptions(t *testing.T, sidecarURL string) {
	t.Helper()

	t.Setenv("SEARCH_SIDECAR_URL", sidecarURL)
	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("unable to configure test options: %v", err)
	}

	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() { config.Opts = previousOptions })
}

// makeSearchTestEntrySearchable gives a directly-inserted entry (see
// insertUIHiddenTestEntry, which writes with plain SQL and so skips it) a
// populated document_vectors tsvector, the same way a real feed refresh
// would, so EntryQueryBuilder.WithSearchQuery can find it by title.
func makeSearchTestEntrySearchable(t *testing.T, store *storage.Storage, userID, entryID int64, title string) {
	t.Helper()

	if err := store.UpdateEntryTitleAndContent(&model.Entry{
		ID:          entryID,
		UserID:      userID,
		Title:       title,
		Content:     "<p>content</p>",
		ReadingTime: 1,
	}); err != nil {
		t.Fatalf("unable to make entry %d searchable: %v", entryID, err)
	}
}

// newSearchTestEntry inserts one searchable entry, still unread and not
// hidden or starred, with the given title and published_at.
func newSearchTestEntry(t *testing.T, db *sql.DB, store *storage.Storage, userID, feedID int64, title, hash string, publishedAt time.Time) int64 {
	t.Helper()

	entryID := insertUIHiddenTestEntry(t, db, userID, feedID, title, hash, publishedAt)
	makeSearchTestEntrySearchable(t, store, userID, entryID, title)
	return entryID
}

// searchTestRequest builds an authenticated GET request against path with
// the given query values.
func searchTestRequest(userID int64, path string, query url.Values) *http.Request {
	target := path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, target, nil), userID)
}

func TestSearchPageDefaultIncludesHiddenEntries(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-default-includes-hidden"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	hiddenTitle := "SearchMarkerAlpha hidden result for " + username
	visibleTitle := "SearchMarkerAlpha visible result for " + username
	hiddenID := newSearchTestEntry(t, db, store, userID, feedID, hiddenTitle, "hash-hidden-"+username, time.Now())
	newSearchTestEntry(t, db, store, userID, feedID, visibleTitle, "hash-visible-"+username, time.Now())

	if err := store.SetEntriesHiddenState(userID, []int64{hiddenID}, true, ""); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	r := searchTestRequest(userID, "/search", url.Values{"q": {"SearchMarkerAlpha"}})
	w := httptest.NewRecorder()
	h.showSearchPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// This is the §13.3 guarantee the task brief calls "not negotiable":
	// asserted explicitly, not inferred from the absence of a filter.
	if !strings.Contains(body, hiddenTitle) {
		t.Fatalf("expected the hidden entry %q to be returned when the exclude-hidden box is unchecked; body:\n%s", hiddenTitle, body)
	}
	if !strings.Contains(body, visibleTitle) {
		t.Fatalf("expected the visible entry %q to be returned; body:\n%s", visibleTitle, body)
	}
}

func TestSearchPageExcludeHiddenChecked(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-exclude-hidden-checked"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	hiddenTitle := "SearchMarkerBravo hidden result for " + username
	visibleTitle := "SearchMarkerBravo visible result for " + username
	hiddenID := newSearchTestEntry(t, db, store, userID, feedID, hiddenTitle, "hash-hidden-"+username, time.Now())
	newSearchTestEntry(t, db, store, userID, feedID, visibleTitle, "hash-visible-"+username, time.Now())

	if err := store.SetEntriesHiddenState(userID, []int64{hiddenID}, true, ""); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	r := searchTestRequest(userID, "/search", url.Values{"q": {"SearchMarkerBravo"}, "excludeHidden": {"1"}})
	w := httptest.NewRecorder()
	h.showSearchPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if strings.Contains(body, hiddenTitle) {
		t.Fatalf("expected the hidden entry %q to be excluded once the checkbox is ticked; body:\n%s", hiddenTitle, body)
	}
	// The non-hidden entry matching the same query must still be present -
	// otherwise this test would pass just as well if the query matched
	// nothing at all.
	if !strings.Contains(body, visibleTitle) {
		t.Fatalf("expected the non-hidden entry %q matching the same query to still be returned; body:\n%s", visibleTitle, body)
	}
}

// TestSearchPageHiddenAndUnreadFiltersAreIndependent covers all four
// combinations of the two checkboxes, proving "unread only" and "exclude
// hidden" do not interact - conflating them (a third state on "unread")
// is exactly what the task brief says not to do.
func TestSearchPageHiddenAndUnreadFiltersAreIndependent(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-filters-independent"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	marker := "SearchMarkerCharlie"
	unreadVisible := marker + " unread visible for " + username
	unreadHidden := marker + " unread hidden for " + username
	readVisible := marker + " read visible for " + username
	readHidden := marker + " read hidden for " + username

	newSearchTestEntry(t, db, store, userID, feedID, unreadVisible, "hash-uv-"+username, time.Now())
	idUnreadHidden := newSearchTestEntry(t, db, store, userID, feedID, unreadHidden, "hash-uh-"+username, time.Now())
	idReadVisible := newSearchTestEntry(t, db, store, userID, feedID, readVisible, "hash-rv-"+username, time.Now())
	idReadHidden := newSearchTestEntry(t, db, store, userID, feedID, readHidden, "hash-rh-"+username, time.Now())

	if err := store.SetEntriesHiddenState(userID, []int64{idUnreadHidden, idReadHidden}, true, ""); err != nil {
		t.Fatalf("unable to hide entries: %v", err)
	}
	if err := store.SetEntriesStatus(userID, []int64{idReadVisible, idReadHidden}, model.EntryStatusRead); err != nil {
		t.Fatalf("unable to mark entries read: %v", err)
	}

	render := func(unreadOnly, excludeHidden bool) string {
		query := url.Values{"q": {marker}}
		if unreadOnly {
			query.Set("unread", "1")
		}
		if excludeHidden {
			query.Set("excludeHidden", "1")
		}
		r := searchTestRequest(userID, "/search", query)
		w := httptest.NewRecorder()
		h.showSearchPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	cases := []struct {
		name                                                                 string
		unreadOnly, excludeHidden                                            bool
		wantUnreadVisible, wantUnreadHidden, wantReadVisible, wantReadHidden bool
	}{
		{"neither filter", false, false, true, true, true, true},
		{"unread only", true, false, true, true, false, false},
		{"exclude hidden only", false, true, true, false, true, false},
		{"both filters", true, true, true, false, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := render(c.unreadOnly, c.excludeHidden)
			got := map[string]bool{
				unreadVisible: strings.Contains(body, unreadVisible),
				unreadHidden:  strings.Contains(body, unreadHidden),
				readVisible:   strings.Contains(body, readVisible),
				readHidden:    strings.Contains(body, readHidden),
			}
			want := map[string]bool{
				unreadVisible: c.wantUnreadVisible,
				unreadHidden:  c.wantUnreadHidden,
				readVisible:   c.wantReadVisible,
				readHidden:    c.wantReadHidden,
			}
			for title, wantPresent := range want {
				if got[title] != wantPresent {
					t.Errorf("unread=%v excludeHidden=%v: entry %q presence = %v, want %v", c.unreadOnly, c.excludeHidden, title, got[title], wantPresent)
				}
			}
		})
	}
}

// TestSearchPageInvalidOrderFallsBackToPreference proves a crafted
// ?order= that is not in ValidateEntryOrder's allowlist never reaches
// WithSorting: "zzz_not_a_real_column" is a syntactically valid SQL
// identifier (pq.QuoteIdentifier would happily quote it) but not a real
// column, so if parseEntryOrder's validation were skipped, Postgres would
// reject the query and the page would 500 instead of rendering.
func TestSearchPageInvalidOrderFallsBackToPreference(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-invalid-order"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	marker := "SearchMarkerDelta"
	titleOne := marker + " one for " + username
	titleTwo := marker + " two for " + username
	newSearchTestEntry(t, db, store, userID, feedID, titleOne, "hash-one-"+username, time.Now())
	newSearchTestEntry(t, db, store, userID, feedID, titleTwo, "hash-two-"+username, time.Now())

	r := searchTestRequest(userID, "/search", url.Values{"q": {marker}, "order": {"zzz_not_a_real_column"}})
	w := httptest.NewRecorder()
	h.showSearchPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected the page to render on the fallback order rather than fail, got status %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, titleOne) || !strings.Contains(body, titleTwo) {
		t.Fatalf("expected both entries to render via the fallback order; body:\n%s", body)
	}
}

// TestSearchPageFullTextDefaultsToRelevanceNotSavedPreference covers the
// reviewer's Minor: with no explicit "order" query parameter, a plain
// full-text search must stay relevance-ordered (WithSearchQuery's own
// ts_rank), not silently switch to the reader's saved
// entry_sorting_order the moment the sort picker exists. highRelevance's
// content repeats the query term five times (raising its ts_rank);
// lowRelevance's content contains it once. Their published_at values are
// deliberately the opposite of what relevance would produce: under the
// default preference (published_at, ascending - oldest first),
// lowRelevance (older) would sort first; only true relevance-first
// ordering puts highRelevance first despite being newer. A first version
// of this fixture had the ages backwards (matching, not contradicting,
// the date-order outcome) and stayed green under a mutation that always
// applied the saved-preference order - see the mutation log in the task
// report.
func TestSearchPageFullTextDefaultsToRelevanceNotSavedPreference(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-relevance-default"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	marker := "SearchMarkerKilo"
	titleHigh := marker + " high relevance for " + username
	titleLow := marker + " low relevance for " + username

	now := time.Now()
	// Newer, but the query term appears 5 times in the content: under
	// published_at ascending this would sort SECOND, so it only sorts
	// first if relevance actually won.
	highID := insertUIHiddenTestEntry(t, db, userID, feedID, titleHigh, "hash-high-"+username, now)
	if err := store.UpdateEntryTitleAndContent(&model.Entry{
		ID:      highID,
		UserID:  userID,
		Title:   titleHigh,
		Content: "<p>" + marker + " " + marker + " " + marker + " " + marker + " " + marker + "</p>",
	}); err != nil {
		t.Fatalf("unable to make high-relevance entry searchable: %v", err)
	}
	// Older, but the query term appears only once: under published_at
	// ascending this would sort FIRST.
	lowID := insertUIHiddenTestEntry(t, db, userID, feedID, titleLow, "hash-low-"+username, now.Add(-time.Hour))
	if err := store.UpdateEntryTitleAndContent(&model.Entry{
		ID:      lowID,
		UserID:  userID,
		Title:   titleLow,
		Content: "<p>" + marker + "</p>",
	}); err != nil {
		t.Fatalf("unable to make low-relevance entry searchable: %v", err)
	}

	// No "order" query parameter at all.
	r := searchTestRequest(userID, "/search", url.Values{"q": {marker}, "mode": {"fulltext"}})
	w := httptest.NewRecorder()
	h.showSearchPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	posHigh := strings.Index(body, titleHigh)
	posLow := strings.Index(body, titleLow)
	if posHigh < 0 || posLow < 0 {
		t.Fatalf("expected both entries present; body:\n%s", body)
	}
	if posHigh >= posLow {
		t.Fatalf("expected the higher-relevance entry first by default (no explicit order), got positions high=%d low=%d", posHigh, posLow)
	}
}

// TestWithStableEntrySortingGroupsStarredAndOrdersNewestFirstWithinGroup
// covers the stable secondary sort at the builder level, deliberately
// bypassing WithSearchQuery: WithSearchQuery appends its own ts_rank
// tiebreak (see resolveSearchResults' comments on relevance vs. picker
// order), and an earlier version of this test went through the full
// search handler with WithSearchQuery in the chain - removing the
// published_at/id tiebreak still passed there, because the two starred
// entries' titles had equal ts_rank and ties happened to fall the same
// way by coincidence of Postgres's query plan, not because the tiebreak
// was doing anything. Testing the builder directly removes that
// confound.
//
// D is the single most recently published entry overall but is NOT
// starred, so if grouping itself were broken (falling through to a plain
// published_at ordering) it would sort first - that is what makes this
// assertion discriminate rather than merely plausible.
func TestWithStableEntrySortingGroupsStarredAndOrdersNewestFirstWithinGroup(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)

	username := "stable-sorting-starred"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	idA := insertUIHiddenTestEntry(t, db, userID, feedID, "A-starred-older", "hash-a-"+username, now.Add(-3*time.Hour))
	idB := insertUIHiddenTestEntry(t, db, userID, feedID, "B-starred-newer", "hash-b-"+username, now.Add(-1*time.Hour))
	idC := insertUIHiddenTestEntry(t, db, userID, feedID, "C-plain-older", "hash-c-"+username, now.Add(-2*time.Hour))
	idD := insertUIHiddenTestEntry(t, db, userID, feedID, "D-plain-newest-overall", "hash-d-"+username, now)

	if err := store.SetEntriesStarredState(userID, []int64{idA, idB}, true); err != nil {
		t.Fatalf("unable to star entries: %v", err)
	}

	entries, err := withStableEntrySorting(store.NewEntryQueryBuilder(userID).WithFeedID(feedID), "starred", "desc").GetEntries()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	got := []int64{entries[0].ID, entries[1].ID, entries[2].ID, entries[3].ID}
	want := []int64{idB, idA, idD, idC}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected order [B A D C] = %v (starred group newest-first, then the rest newest-first), got %v", want, got)
		}
	}
}

// TestSearchPageOrderStarredIsWiredFromTheQueryString is a lighter,
// handler-level companion: it proves the "order"/"direction" query
// parameters actually reach withStableEntrySorting through the fallback
// closure (the wiring TestWithStableEntrySortingGroupsStarredAndOrdersNewestFirstWithinGroup
// does not exercise), using an order with no ties so it needs no
// separate tiebreak proof.
func TestSearchPageOrderStarredIsWiredFromTheQueryString(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-starred-wiring"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	marker := "SearchMarkerEcho"
	titleStarred := marker + " starred for " + username
	titlePlain := marker + " plain for " + username
	// Published_at is the reverse of the expected starred-first order,
	// so this only passes if "order=starred" actually took effect.
	starredID := newSearchTestEntry(t, db, store, userID, feedID, titleStarred, "hash-starred-"+username, time.Now().Add(-time.Hour))
	newSearchTestEntry(t, db, store, userID, feedID, titlePlain, "hash-plain-"+username, time.Now())

	if err := store.SetEntriesStarredState(userID, []int64{starredID}, true); err != nil {
		t.Fatalf("unable to star entry: %v", err)
	}

	r := searchTestRequest(userID, "/search", url.Values{"q": {marker}, "order": {"starred"}, "direction": {"desc"}})
	w := httptest.NewRecorder()
	h.showSearchPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	posStarred := strings.Index(body, titleStarred)
	posPlain := strings.Index(body, titlePlain)
	if posStarred < 0 || posPlain < 0 {
		t.Fatalf("expected both entries present; body:\n%s", body)
	}
	if posStarred >= posPlain {
		t.Fatalf("expected the starred entry first under order=starred&direction=desc, got positions starred=%d plain=%d", posStarred, posPlain)
	}
}

// TestSearchPagePickerOverridesPreferenceForThatViewOnly proves the sort
// picker only affects the current render: it must not persist back to
// the user's saved entry_sorting_order/entry_sorting_direction.
func TestSearchPagePickerOverridesPreferenceForThatViewOnly(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-picker-scoped"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	before, err := store.UserByID(userID)
	if err != nil {
		t.Fatalf("unable to load user: %v", err)
	}
	if before.EntryOrder != "published_at" || before.EntryDirection != "asc" {
		t.Fatalf("expected the fresh user's default sorting preference to be published_at/asc, got %s/%s", before.EntryOrder, before.EntryDirection)
	}

	marker := "SearchMarkerFoxtrot"
	titleAlpha := "Alpha " + marker + " for " + username
	titleZulu := "Zulu " + marker + " for " + username
	now := time.Now()
	// published_at is deliberately the reverse of title order, so a
	// title-ascending render only matches if the picker's order actually
	// took effect rather than the (published_at) default.
	newSearchTestEntry(t, db, store, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	newSearchTestEntry(t, db, store, userID, feedID, titleZulu, "hash-zulu-"+username, now.Add(-time.Hour))

	r := searchTestRequest(userID, "/search", url.Values{"q": {marker}, "order": {"title"}, "direction": {"asc"}})
	w := httptest.NewRecorder()
	h.showSearchPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	posAlpha := strings.Index(body, titleAlpha)
	posZulu := strings.Index(body, titleZulu)
	if posAlpha < 0 || posZulu < 0 {
		t.Fatalf("expected both entries present; body:\n%s", body)
	}
	if posAlpha >= posZulu {
		t.Fatalf("expected title-ascending order (Alpha before Zulu) when order=title is requested, got positions Alpha=%d Zulu=%d", posAlpha, posZulu)
	}

	after, err := store.UserByID(userID)
	if err != nil {
		t.Fatalf("unable to reload user: %v", err)
	}
	if after.EntryOrder != before.EntryOrder || after.EntryDirection != before.EntryDirection {
		t.Fatalf("expected the saved preference to be unchanged by the picker, got order=%s direction=%s (was %s/%s)", after.EntryOrder, after.EntryDirection, before.EntryOrder, before.EntryDirection)
	}
}

// TestSearchEntryPaginationWalksThePickerOrder proves prev/next links on
// a search result page walk the same order the list itself was sorted
// by, not the reader's global preference. published_at is deliberately
// the reverse of title order: if showSearchEntryPage's pagination used
// user.EntryOrder (published_at) instead of the picker's "title", the
// neighbours of the middle entry would swap.
func TestSearchEntryPaginationWalksThePickerOrder(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-entry-pagination-order"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	marker := "SearchMarkerGolf"
	titleAlpha := "Alpha " + marker + " for " + username     // newest published_at
	titleBravo := "Bravo " + marker + " for " + username     // middle published_at
	titleCharlie := "Charlie " + marker + " for " + username // oldest published_at

	// published_at is the exact reverse of title order (Alpha newest,
	// Charlie oldest): under the reader's default preference
	// (published_at, ascending) the order would be Charlie, Bravo, Alpha
	// - the opposite of the title-ascending order requested below. If
	// showSearchEntryPage's pagination used user.EntryOrder/
	// user.EntryDirection instead of the picker's "order"/"direction",
	// Bravo's neighbours would swap (Charlie as previous, Alpha as
	// next) - that swap is what this test is built to catch.
	now := time.Now()
	newSearchTestEntry(t, db, store, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	bravoID := newSearchTestEntry(t, db, store, userID, feedID, titleBravo, "hash-bravo-"+username, now.Add(-1*time.Hour))
	newSearchTestEntry(t, db, store, userID, feedID, titleCharlie, "hash-charlie-"+username, now.Add(-2*time.Hour))

	query := url.Values{"q": {marker}, "order": {"title"}, "direction": {"asc"}}
	r := searchTestRequest(userID, "/search/entry/"+strconv.FormatInt(bravoID, 10), query)
	r.SetPathValue("entryID", strconv.FormatInt(bravoID, 10))
	w := httptest.NewRecorder()
	h.showSearchEntryPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Title order is Alpha, Bravo, Charlie: Bravo's neighbours must be
	// Alpha as PREVIOUS and Charlie as NEXT specifically - not merely
	// present somewhere in the body, which would still pass if the two
	// were swapped (exactly what the published_at/asc default produces).
	prevTitle := extractAnchorAttrBefore(t, body, `rel="prev"`, "title")
	nextTitle := extractAnchorAttrBefore(t, body, `rel="next"`, "title")
	if prevTitle != titleAlpha {
		t.Fatalf("expected %q as the previous entry under order=title, got %q; body:\n%s", titleAlpha, prevTitle, body)
	}
	if nextTitle != titleCharlie {
		t.Fatalf("expected %q as the next entry under order=title, got %q; body:\n%s", titleCharlie, nextTitle, body)
	}
}

// TestSearchPageFullTextModeWorksWithoutSidecar covers part C: the
// explicit "fulltext" mode must return results with no sidecar
// configured at all - the same path selecting it always ran, now reached
// on purpose rather than only as a fallback.
func TestSearchPageFullTextModeWorksWithoutSidecar(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-fulltext-no-sidecar"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	marker := "SearchMarkerHotel"
	title := marker + " for " + username
	newSearchTestEntry(t, db, store, userID, feedID, title, "hash-"+username, time.Now())

	r := searchTestRequest(userID, "/search", url.Values{"q": {marker}, "mode": {"fulltext"}})
	w := httptest.NewRecorder()
	h.showSearchPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, title) {
		t.Fatalf("expected the fulltext mode to return the matching entry; body:\n%s", body)
	}
}

// selectTagHasAttr returns whether the <select ... id="id" ...> opening
// tag in body contains attr (e.g. "disabled").
func selectTagHasAttr(t *testing.T, body, id, attr string) bool {
	t.Helper()

	marker := `id="` + id + `"`
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("no element with id=%q found in body", id)
	}
	tagStart := strings.LastIndex(body[:idx], "<select ")
	if tagStart < 0 {
		t.Fatalf("no enclosing <select> tag found for id=%q", id)
	}
	tagEnd := strings.Index(body[tagStart:], ">")
	if tagEnd < 0 {
		t.Fatalf("unterminated <select> tag for id=%q", id)
	}
	tag := body[tagStart : tagStart+tagEnd]
	return strings.Contains(tag, attr)
}

// TestSearchPageSortControlsDisabledOutsideFullTextMode covers Major 2:
// the order/direction selects have no effect on the four sidecar-ranked
// modes (see resolveSearchResults' doc comment on relevance vs. the
// picker), so they must render disabled - communicating "not applicable
// here" - rather than sitting there inert with no explanation. A visible
// hint explains why. The same sidecar is used for both renders, so this
// isolates the effect to searchMode, not sidecar availability (which
// TestSearchPageFullTextModeWorksWithoutSidecar already covers).
func TestSearchPageSortControlsDisabledOutsideFullTextMode(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"hybrid","query":"coffee","entries":[]}`))
	}))
	defer server.Close()
	configureUISearchTestOptions(t, server.URL)

	username := "search-sort-disabled"
	userID, _, _ := createUIHiddenTestUserAndFeed(t, db, username)

	render := func(mode string) string {
		r := searchTestRequest(userID, "/search", url.Values{"q": {"coffee"}, "mode": {mode}})
		w := httptest.NewRecorder()
		h.showSearchPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	hybridBody := render("hybrid")
	if !selectTagHasAttr(t, hybridBody, "search-order", "disabled") {
		t.Fatalf("expected the order select to be disabled in hybrid mode; body:\n%s", hybridBody)
	}
	if !selectTagHasAttr(t, hybridBody, "search-direction", "disabled") {
		t.Fatalf("expected the direction select to be disabled in hybrid mode; body:\n%s", hybridBody)
	}
	if !strings.Contains(hybridBody, "search-filter-hint") {
		t.Fatalf("expected a hint explaining why the sort controls are disabled; body:\n%s", hybridBody)
	}

	fullTextBody := render("fulltext")
	if selectTagHasAttr(t, fullTextBody, "search-order", "disabled") {
		t.Fatalf("expected the order select to be enabled in fulltext mode; body:\n%s", fullTextBody)
	}
	if selectTagHasAttr(t, fullTextBody, "search-direction", "disabled") {
		t.Fatalf("expected the direction select to be enabled in fulltext mode; body:\n%s", fullTextBody)
	}
	if strings.Contains(fullTextBody, "search-filter-hint") {
		t.Fatalf("expected no disabled-hint in fulltext mode, where the controls are active; body:\n%s", fullTextBody)
	}
}

// TestSearchPageFullTextModeNeverCallsTheSidecar proves mode=fulltext
// bypasses the sidecar entirely even when one is configured: the test
// server fails the test if it receives any request at all.
func TestSearchPageFullTextModeNeverCallsTheSidecar(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the sidecar must not be called when mode=fulltext is explicitly requested")
	}))
	defer server.Close()
	configureUISearchTestOptions(t, server.URL)

	username := "search-fulltext-bypasses-sidecar"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	marker := "SearchMarkerIndia"
	title := marker + " for " + username
	newSearchTestEntry(t, db, store, userID, feedID, title, "hash-"+username, time.Now())

	r := searchTestRequest(userID, "/search", url.Values{"q": {marker}, "mode": {"fulltext"}})
	w := httptest.NewRecorder()
	h.showSearchPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, title) {
		t.Fatalf("expected the fulltext mode to still return results from the local database; body:\n%s", body)
	}

	// A deliberate choice of full-text search is not a degradation: the
	// notice shown when the sidecar failed must not appear here.
	if strings.Contains(body, "search.degraded_notice") {
		t.Fatalf("unexpected translation key leaked into the body (should never happen, but would mean the notice rendered): body:\n%s", body)
	}
	degradedNoticeText := "Advanced search is temporarily unavailable. Showing basic keyword results instead."
	if strings.Contains(body, degradedNoticeText) {
		t.Fatalf("expected no degraded notice for a deliberate mode=fulltext search; body:\n%s", body)
	}
}

// TestSearchResultLinkAndPaginationCarryAllThreeNewParameters is the
// round-trip check the task brief calls out explicitly: excludeHidden,
// order and direction (and the pre-existing mode) must all survive the
// result link built at search.html's queryString dict, and the
// pagination links built from the same pagination struct - not just on
// first render.
func TestSearchResultLinkAndPaginationCarryAllThreeNewParameters(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	configureUISearchTestOptions(t, "")
	h := newUIHiddenTestHandler(t, store)

	username := "search-roundtrip"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	// A page size of 1 makes two matching entries enough to force a
	// "next" pagination link, without creating 100+ rows just to cross
	// the default page size.
	if _, err := db.Exec(`UPDATE users SET entries_per_page = 1 WHERE id = $1`, userID); err != nil {
		t.Fatalf("unable to shrink the page size: %v", err)
	}

	marker := "SearchMarkerJuliet"
	title := marker + " for " + username
	entryID := newSearchTestEntry(t, db, store, userID, feedID, title, "hash-"+username, time.Now())
	// A second entry forces pagination-worthy output and keeps this test
	// from depending on exactly one result.
	newSearchTestEntry(t, db, store, userID, feedID, marker+" second for "+username, "hash-second-"+username, time.Now())

	query := url.Values{
		"q":             {marker},
		"unread":        {"1"},
		"excludeHidden": {"1"},
		"mode":          {"fulltext"},
		"order":         {"title"},
		"direction":     {"asc"},
	}
	// unread=1 would normally exclude nothing here (both entries are
	// unread), which keeps the fixture simple while still exercising the
	// parameter through the whole render.
	r := searchTestRequest(userID, "/search", query)
	w := httptest.NewRecorder()
	h.showSearchPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	resultHref := extractAnchorHrefBefore(t, body, "/search/entry/"+strconv.FormatInt(entryID, 10))
	for param, want := range map[string]string{
		"q":             marker,
		"unread":        "1",
		"excludeHidden": "1",
		"mode":          "fulltext",
		"order":         "title",
		"direction":     "asc",
	} {
		assertHrefHasParam(t, resultHref, param, want)
	}

	paginationHref := extractAnchorHrefBefore(t, body, `data-page="next"`)
	if paginationHref == "" {
		t.Fatalf("expected a \"next\" pagination link (entries_per_page=1, 2 matching entries); body:\n%s", body)
	}
	for param, want := range map[string]string{
		"excludeHidden": "1",
		"order":         "title",
		"direction":     "asc",
		"mode":          "fulltext",
	} {
		assertHrefHasParam(t, paginationHref, param, want)
	}
}

// extractAnchorHrefBefore finds the <a ...> tag whose (possibly
// multi-attribute) opening tag contains or is immediately followed by
// marker - covering both "the marker text sits inside the anchor" (the
// search result link, whose href precedes its own title text) and "the
// marker is itself another attribute on the same tag" (the pagination
// links' data-page="next") - and returns its href attribute's value.
func extractAnchorHrefBefore(t *testing.T, body, marker string) string {
	t.Helper()
	return extractAnchorAttrBefore(t, body, marker, "href")
}

// extractAnchorAttrBefore locates the <a ...> tag associated with marker
// (see extractAnchorHrefBefore's doc comment for what "associated with"
// covers) and returns the value of its attr attribute.
func extractAnchorAttrBefore(t *testing.T, body, marker, attr string) string {
	t.Helper()

	idx := strings.Index(body, marker)
	if idx < 0 {
		return ""
	}
	tagStart := strings.LastIndex(body[:idx], "<a ")
	if tagStart < 0 {
		t.Fatalf("no enclosing <a> tag found before marker %q", marker)
	}
	tagEnd := strings.Index(body[tagStart:], ">")
	if tagEnd < 0 {
		t.Fatalf("unterminated <a> tag near marker %q", marker)
	}
	tag := body[tagStart : tagStart+tagEnd]

	attrPrefix := attr + `="`
	attrIdx := strings.Index(tag, attrPrefix)
	if attrIdx < 0 {
		t.Fatalf("no %s attribute in anchor tag near marker %q: %s", attr, marker, tag)
	}
	rest := tag[attrIdx+len(attrPrefix):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("unterminated %s attribute near marker %q", attr, marker)
	}
	return rest[:end]
}

func assertHrefHasParam(t *testing.T, href, param, want string) {
	t.Helper()

	u, err := url.Parse(strings.ReplaceAll(href, "&amp;", "&"))
	if err != nil {
		t.Fatalf("unable to parse href %q: %v", href, err)
	}
	got := u.Query().Get(param)
	if got != want {
		t.Errorf("href %q: parameter %q = %q, want %q", href, param, got, want)
	}
}
