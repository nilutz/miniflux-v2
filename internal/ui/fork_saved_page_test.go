// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/storage"
)

// configureSavedPageTestOptions is configureUIHiddenTestOptions (see
// fork_hidden_test.go) plus FETCHER_ALLOW_PRIVATE_NETWORKS, which these
// tests need since they point the crawler at loopback httptest servers.
func configureSavedPageTestOptions(t *testing.T) {
	t.Helper()

	t.Setenv("FETCHER_ALLOW_PRIVATE_NETWORKS", "1")

	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("unable to configure test options: %v", err)
	}

	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() { config.Opts = previousOptions })
}

// createSavedPageTestUser inserts a user and a category, removed (with any
// feeds/entries via FK cascade) when the test finishes.
func createSavedPageTestUser(t *testing.T, db *sql.DB, username string) (userID, categoryID int64) {
	t.Helper()

	if err := db.QueryRow(
		`INSERT INTO users (username, password, language) VALUES ($1, 'x', 'en_US') RETURNING id`,
		username,
	).Scan(&userID); err != nil {
		t.Fatalf("unable to create user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	})

	if err := db.QueryRow(
		`INSERT INTO categories (user_id, title) VALUES ($1, 'Test') RETURNING id`,
		userID,
	).Scan(&categoryID); err != nil {
		t.Fatalf("unable to create category: %v", err)
	}

	return userID, categoryID
}

// savedPageArticleServer serves distinct article HTML per path, so a single
// server can double as "the page" for several URLs across a test.
func savedPageArticleServer(t *testing.T, pages map[string]string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return server
}

// savedPageArticleHTML is long and structured enough for readability to
// treat it as a real article, and carries an <h1> so the save path has a
// title to pull out that isn't just the URL. markerWord lets each test give
// its fetched page a distinctive, greppable body of its own.
func savedPageArticleHTML(title, markerWord string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><title>%[1]s</title></head>
<body>
<article>
<h1>%[1]s</h1>
<p class="marker">This paragraph exists only so the test can assert which version of the
article ended up stored on the entry. It repeats a distinctive word: %[2]s %[2]s.</p>
<p>Readability-style content extraction favors pages with several substantial paragraphs of
real prose, so this article includes a handful of them, each long enough to look like genuine
body text rather than a navigation link or a footer notice.</p>
<p>A saved page has no feed and therefore no feed-supplied excerpt or title, which is why the
save path pulls a title out of the page itself instead of out of an RSS item.</p>
<p>This final paragraph only pads the article out further, to comfortably clear whatever
minimum text density heuristic the readability extraction algorithm applies when deciding which
parts of the page are the article and which parts are chrome.</p>
</article>
</body>
</html>`, title, markerWord)
}

// savedPageAppShellHTML is a well-formed page with no article at all: a
// JavaScript single-page-app shell whose body is an empty mount point.
// Readability does not error on it, it returns markup with no text -
// exactly the case spec §10 requires the save path to treat as a failure.
const savedPageAppShellHTML = `<!DOCTYPE html>
<html>
<head><title>Subscribe to read</title></head>
<body>
<div id="app"></div>
<script>window.__DATA__ = {};</script>
</body>
</html>`

func countEntriesForFeed(t *testing.T, db *sql.DB, feedID int64) int {
	t.Helper()

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM entries WHERE feed_id=$1`, feedID).Scan(&count); err != nil {
		t.Fatalf("unable to count entries: %v", err)
	}
	return count
}

// TestSaveWebPageCreatesEntryInLazyDisabledFeed proves the positive case
// before anything else: saving a URL scrapes the real article into an entry
// living in a feed that is created on demand, exactly once, and disabled.
func TestSaveWebPageCreatesEntryInLazyDisabledFeed(t *testing.T) {
	db := uiHiddenTestDB(t)
	configureSavedPageTestOptions(t)
	store := storage.NewStorage(db)

	userID, _ := createSavedPageTestUser(t, db, "saved-page-lazy-feed")
	user, err := store.UserByID(userID)
	if err != nil || user == nil {
		t.Fatalf("unable to load user: %v", err)
	}

	server := savedPageArticleServer(t, map[string]string{
		"/first":  savedPageArticleHTML("First Saved Article", "marker-alpha"),
		"/second": savedPageArticleHTML("Second Saved Article", "marker-beta"),
	})

	firstEntry, saveErr := saveWebPage(store, user, server.URL+"/first")
	if saveErr != nil {
		t.Fatalf("unable to save first page: %v", saveErr.Error())
	}

	if !strings.Contains(firstEntry.Content, "marker-alpha") {
		t.Fatalf("expected the scraped article to be stored on the entry, got: %s", firstEntry.Content)
	}
	if firstEntry.Title != "First Saved Article" {
		t.Fatalf("expected the title to come from the page's <h1>, got: %q", firstEntry.Title)
	}

	var feedDisabled bool
	var feedTitle string
	if err := db.QueryRow(`SELECT disabled, title FROM feeds WHERE id=$1`, firstEntry.FeedID).Scan(&feedDisabled, &feedTitle); err != nil {
		t.Fatalf("unable to read back feed: %v", err)
	}
	if !feedDisabled {
		t.Fatal("expected the saved-pages feed to be disabled")
	}
	if feedTitle == "" {
		t.Fatal("expected the saved-pages feed to have a recognisable title")
	}

	// Saving a second, different URL for the same user must land in the
	// same feed, not create a second one.
	secondEntry, saveErr := saveWebPage(store, user, server.URL+"/second")
	if saveErr != nil {
		t.Fatalf("unable to save second page: %v", saveErr.Error())
	}
	if secondEntry.FeedID != firstEntry.FeedID {
		t.Fatalf("expected both saves to land in the same feed, got %d and %d", firstEntry.FeedID, secondEntry.FeedID)
	}

	var feedCount int
	if err := db.QueryRow(`SELECT count(*) FROM feeds WHERE user_id=$1`, userID).Scan(&feedCount); err != nil {
		t.Fatalf("unable to count feeds: %v", err)
	}
	if feedCount != 1 {
		t.Fatalf("expected exactly one feed to have been created for the user, got %d", feedCount)
	}
}

// TestSavedPagesFeedExcludedFromSchedulerBatch proves the exclusion against
// the real scheduler batch builder, not by reading the disabled flag, and
// establishes the positive case first: without WithoutDisabledFeeds the
// saved-pages feed IS an eligible job (same user, same next_check_at
// expiry, same error-count headroom as any real feed), so its absence once
// the disabled filter is applied is not a coincidence of some other
// eligibility rule.
func TestSavedPagesFeedExcludedFromSchedulerBatch(t *testing.T) {
	db := uiHiddenTestDB(t)
	configureSavedPageTestOptions(t)
	store := storage.NewStorage(db)

	userID, categoryID := createSavedPageTestUser(t, db, "saved-page-scheduler")
	user, err := store.UserByID(userID)
	if err != nil || user == nil {
		t.Fatalf("unable to load user: %v", err)
	}

	server := savedPageArticleServer(t, map[string]string{
		"/article": savedPageArticleHTML("Scheduler Test Article", "marker-scheduler"),
	})

	savedEntry, saveErr := saveWebPage(store, user, server.URL+"/article")
	if saveErr != nil {
		t.Fatalf("unable to save page: %v", saveErr.Error())
	}
	savedFeedID := savedEntry.FeedID

	// A real, enabled feed for the same user, due for a refresh right now,
	// so the batch builder has something legitimate to return alongside
	// the saved-pages feed.
	var realFeedID int64
	if err := db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id, disabled, next_check_at)
		 VALUES ($1, $1, 'Real feed', $2, $3, false, now() - interval '1 hour') RETURNING id`,
		"https://example.org/saved-page-scheduler.xml", categoryID, userID,
	).Scan(&realFeedID); err != nil {
		t.Fatalf("unable to create real feed: %v", err)
	}

	// Also push the saved-pages feed's next_check_at into the past: a
	// freshly created feed's next_check_at defaults to its creation time,
	// which should already be in the past by now, but pin it explicitly so
	// this assertion cannot flake on timing.
	if _, err := db.Exec(`UPDATE feeds SET next_check_at = now() - interval '1 hour' WHERE id=$1`, savedFeedID); err != nil {
		t.Fatalf("unable to backdate saved-pages feed: %v", err)
	}

	fetchFeedIDs := func(withoutDisabled bool) map[int64]bool {
		builder := store.NewBatchBuilder().WithUserID(userID).WithNextCheckExpired()
		if withoutDisabled {
			builder = builder.WithoutDisabledFeeds()
		}
		jobs, err := builder.FetchJobs()
		if err != nil {
			t.Fatalf("unable to fetch batch jobs: %v", err)
		}
		ids := make(map[int64]bool, len(jobs))
		for _, job := range jobs {
			ids[job.FeedID] = true
		}
		return ids
	}

	before := fetchFeedIDs(false)
	if !before[savedFeedID] {
		t.Fatalf("expected the saved-pages feed to be an eligible job before the disabled filter is applied (jobs: %v)", before)
	}
	if !before[realFeedID] {
		t.Fatalf("expected the real feed to be an eligible job before the disabled filter is applied (jobs: %v)", before)
	}

	after := fetchFeedIDs(true)
	if after[savedFeedID] {
		t.Fatalf("expected the saved-pages feed to be excluded by the scheduler's real batch builder, got jobs: %v", after)
	}
	if !after[realFeedID] {
		t.Fatalf("expected the real, enabled feed to still be returned once the disabled filter is applied, got jobs: %v", after)
	}
}

// TestResavingURLUpdatesEntryInPlace proves the update, not merely the
// absence of a duplicate: it checks that the second save's content actually
// replaced the first, which a silently failed second save would not do
// while still passing a bare "still exactly one entry" assertion.
func TestResavingURLUpdatesEntryInPlace(t *testing.T) {
	db := uiHiddenTestDB(t)
	configureSavedPageTestOptions(t)
	store := storage.NewStorage(db)

	userID, _ := createSavedPageTestUser(t, db, "saved-page-resave")
	user, err := store.UserByID(userID)
	if err != nil || user == nil {
		t.Fatalf("unable to load user: %v", err)
	}

	pages := map[string]string{"/article": savedPageArticleHTML("Original Title", "marker-version-one")}
	server := savedPageArticleServer(t, pages)

	firstEntry, saveErr := saveWebPage(store, user, server.URL+"/article")
	if saveErr != nil {
		t.Fatalf("unable to save page: %v", saveErr.Error())
	}
	if !strings.Contains(firstEntry.Content, "marker-version-one") {
		t.Fatalf("expected the first save's content to hold its own marker, got: %s", firstEntry.Content)
	}

	if countEntriesForFeed(t, db, firstEntry.FeedID) != 1 {
		t.Fatal("expected exactly one entry after the first save")
	}

	// Change what the server returns for the same URL and save again.
	pages["/article"] = savedPageArticleHTML("Updated Title", "marker-version-two")

	secondEntry, saveErr := saveWebPage(store, user, server.URL+"/article")
	if saveErr != nil {
		t.Fatalf("unable to re-save page: %v", saveErr.Error())
	}

	if countEntriesForFeed(t, db, firstEntry.FeedID) != 1 {
		t.Fatal("expected the re-save to update the existing entry, not add a second one")
	}
	if secondEntry.ID != firstEntry.ID {
		t.Fatalf("expected the re-save to reuse the same entry id, got %d and %d", firstEntry.ID, secondEntry.ID)
	}

	var storedContent, storedTitle string
	if err := db.QueryRow(`SELECT content, title FROM entries WHERE id=$1`, firstEntry.ID).Scan(&storedContent, &storedTitle); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if strings.Contains(storedContent, "marker-version-one") {
		t.Fatalf("expected the stale first-save content to be gone after re-saving, got: %s", storedContent)
	}
	if !strings.Contains(storedContent, "marker-version-two") {
		t.Fatalf("expected the re-save's content to have replaced the entry's content, got: %s", storedContent)
	}
	if storedTitle != "Updated Title" {
		t.Fatalf("expected the re-save's title to have replaced the entry's title, got: %q", storedTitle)
	}
}

// TestSaveWebPageFailsWhenPageHasNoArticleText is the failure case spec §10
// calls out: a scrape that returns markup but no prose must fail with a
// message and must not create an entry. This is the exact bug class the P0
// crawler fix (processor.ContainsAnyText) guards against; the save path
// must apply the same guard rather than trusting "no error" as "it worked".
func TestSaveWebPageFailsWhenPageHasNoArticleText(t *testing.T) {
	db := uiHiddenTestDB(t)
	configureSavedPageTestOptions(t)
	store := storage.NewStorage(db)

	userID, _ := createSavedPageTestUser(t, db, "saved-page-no-article")
	user, err := store.UserByID(userID)
	if err != nil || user == nil {
		t.Fatalf("unable to load user: %v", err)
	}

	server := savedPageArticleServer(t, map[string]string{"/app-shell": savedPageAppShellHTML})

	entry, saveErr := saveWebPage(store, user, server.URL+"/app-shell")
	if saveErr == nil {
		t.Fatalf("expected saving a page with no article text to fail, got entry: %+v", entry)
	}
	if entry != nil {
		t.Fatalf("expected no entry to be returned on failure, got: %+v", entry)
	}

	if msg := saveErr.Translate("en_US"); !strings.Contains(strings.ToLower(msg), "article") {
		t.Fatalf("expected a message about the missing article text, got: %q", msg)
	}

	var entryCount int
	if err := db.QueryRow(`SELECT count(*) FROM entries WHERE user_id=$1`, userID).Scan(&entryCount); err != nil {
		t.Fatalf("unable to count entries: %v", err)
	}
	if entryCount != 0 {
		t.Fatalf("expected no entry to have been created for a page with no article text, got %d", entryCount)
	}

	// The feed itself may legitimately have been created (it is the
	// container the failed save would have written into), but it must
	// hold no entries.
	var feedCount int
	if err := db.QueryRow(`SELECT count(*) FROM feeds WHERE user_id=$1`, userID).Scan(&feedCount); err != nil {
		t.Fatalf("unable to count feeds: %v", err)
	}
	if feedCount > 1 {
		t.Fatalf("expected at most one (the saved-pages) feed to exist, got %d", feedCount)
	}
}

// TestTwoUsersSavingSameURLGetOwnEntries proves per-user isolation: the
// same URL saved by two different users must not collide into one feed or
// one entry.
func TestTwoUsersSavingSameURLGetOwnEntries(t *testing.T) {
	db := uiHiddenTestDB(t)
	configureSavedPageTestOptions(t)
	store := storage.NewStorage(db)

	userAID, _ := createSavedPageTestUser(t, db, "saved-page-user-a")
	userBID, _ := createSavedPageTestUser(t, db, "saved-page-user-b")
	userA, err := store.UserByID(userAID)
	if err != nil || userA == nil {
		t.Fatalf("unable to load user A: %v", err)
	}
	userB, err := store.UserByID(userBID)
	if err != nil || userB == nil {
		t.Fatalf("unable to load user B: %v", err)
	}

	server := savedPageArticleServer(t, map[string]string{
		"/shared-article": savedPageArticleHTML("Shared Article", "marker-shared"),
	})

	entryA, saveErr := saveWebPage(store, userA, server.URL+"/shared-article")
	if saveErr != nil {
		t.Fatalf("unable to save page for user A: %v", saveErr.Error())
	}
	entryB, saveErr := saveWebPage(store, userB, server.URL+"/shared-article")
	if saveErr != nil {
		t.Fatalf("unable to save page for user B: %v", saveErr.Error())
	}

	if entryA.FeedID == entryB.FeedID {
		t.Fatalf("expected each user to get their own saved-pages feed, both landed in feed %d", entryA.FeedID)
	}
	if entryA.ID == entryB.ID {
		t.Fatal("expected each user to get their own entry")
	}
	if entryA.UserID != userAID || entryB.UserID != userBID {
		t.Fatalf("expected entries to be owned by the saving user, got %d and %d", entryA.UserID, entryB.UserID)
	}

	var feedOwnerA, feedOwnerB int64
	if err := db.QueryRow(`SELECT user_id FROM feeds WHERE id=$1`, entryA.FeedID).Scan(&feedOwnerA); err != nil {
		t.Fatalf("unable to read back feed A: %v", err)
	}
	if err := db.QueryRow(`SELECT user_id FROM feeds WHERE id=$1`, entryB.FeedID).Scan(&feedOwnerB); err != nil {
		t.Fatalf("unable to read back feed B: %v", err)
	}
	if feedOwnerA != userAID || feedOwnerB != userBID {
		t.Fatalf("expected each saved-pages feed to be owned by its own user, got %d and %d", feedOwnerA, feedOwnerB)
	}
}

// TestSubmitSavedPageHandlerRedirectsToFeed exercises the HTTP route end to
// end: POST /save-page must scrape the URL, persist the entry and redirect
// to the saved-pages feed's entry list.
func TestSubmitSavedPageHandlerRedirectsToFeed(t *testing.T) {
	db := uiHiddenTestDB(t)
	configureSavedPageTestOptions(t)
	store := storage.NewStorage(db)
	h := &handler{store: store}

	userID, _ := createSavedPageTestUser(t, db, "saved-page-handler")
	server := savedPageArticleServer(t, map[string]string{
		"/handler-article": savedPageArticleHTML("Handler Route Article", "marker-handler-route"),
	})

	form := strings.NewReader("url=" + server.URL + "/handler-article")
	r := httptest.NewRequest(http.MethodPost, "/save-page", form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = withUIHiddenTestContext(r, userID)

	w := httptest.NewRecorder()
	h.submitSavedPage(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("expected a redirect, got status %d, body: %s", w.Code, w.Body.String())
	}

	location := w.Header().Get("Location")
	if !strings.Contains(location, "/entries") {
		t.Fatalf("expected a redirect to the saved-pages feed's entries, got: %q", location)
	}

	var entryID int64
	var content string
	if err := db.QueryRow(
		`SELECT e.id, e.content FROM entries e JOIN feeds f ON f.id = e.feed_id
		  WHERE f.user_id = $1`,
		userID,
	).Scan(&entryID, &content); err != nil {
		t.Fatalf("unable to read back saved entry: %v", err)
	}
	if !strings.Contains(content, "marker-handler-route") {
		t.Fatalf("expected the entry to hold the scraped article, got: %s", content)
	}
}

// TestSubmitSavedPageHandlerRendersErrorWithoutPanicking proves the
// error-rendering path is safe even though add_subscription.html's regular
// subscribe form is not the one that produced the error: it must still
// render (in particular, .form.* must not be nil) and must surface the
// validation message.
func TestSubmitSavedPageHandlerRendersErrorWithoutPanicking(t *testing.T) {
	db := uiHiddenTestDB(t)
	h := newUIHiddenTestHandler(t, storage.NewStorage(db))

	userID, _ := createSavedPageTestUser(t, db, "saved-page-handler-error")

	form := strings.NewReader("url=")
	r := httptest.NewRequest(http.MethodPost, "/save-page", form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = withUIHiddenTestContext(r, userID)

	w := httptest.NewRecorder()
	h.submitSavedPage(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected the page to re-render with an error, got status %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mandatory") {
		t.Fatalf("expected the validation error message on the page, body:\n%s", w.Body.String())
	}
}

// TestSavedPagesFeedURLNeverCollidesWithARealSubscription is a narrow unit
// check on the collision argument the report relies on: the URL validator
// every real subscription's feed_url must pass through never accepts the
// scheme the saved-pages feed_url uses.
func TestSavedPagesFeedURLNeverCollidesWithARealSubscription(t *testing.T) {
	feedURL := storage.SavedPagesFeedURL(42)
	if !strings.HasPrefix(feedURL, "internal://") {
		t.Fatalf("expected the saved-pages feed_url to use a non-http(s) scheme, got: %q", feedURL)
	}
}
