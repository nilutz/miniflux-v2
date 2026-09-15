// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/crypto"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/locale"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/processor"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/ui/form"
	"miniflux.app/v2/internal/ui/view"
)

// ErrSavedPageHasNoArticle is returned when a page was fetched without a
// transport or parsing error but holds no extractable article text.
//
// Spec §10's rule applies here exactly as it does to the crawler and the
// full-text backfill: readability never fails on a well-formed page with no
// article, it just returns markup holding no text (an empty <div></div>, a
// JavaScript app shell's mount point, ...). Treating "no error" as "it
// worked" would record an empty page as a saved entry.
var ErrSavedPageHasNoArticle = errors.New("ui: saved page holds no article text")

// submitSavedPage handles "save this page as a searchable entry, it has no
// feed" (spec §13.4), offered on the same page as the regular
// add-subscription form.
func (h *handler) submitSavedPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	categories, err := h.store.Categories(user.ID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	v := view.New(h.tpl, r)
	v.Set("categories", categories)
	v.Set("menu", "feeds")
	v.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	v.Set("countUnread", navMetadata.CountUnread)
	v.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	v.Set("defaultUserAgent", config.Opts.HTTPClientUserAgent())
	v.Set("hasProxyConfigured", config.Opts.HasHTTPClientProxyURLConfigured())
	// The page also renders the regular add-subscription form; give it its
	// GET-time defaults so the template's unconditional .form.* references
	// do not panic when we re-render add_subscription on a saved-page error.
	v.Set("form", &form.SubscriptionForm{CategoryID: 0, Crawler: config.Opts.CrawlerEnabledByDefault()})

	savedPageForm := form.NewSavedPageForm(r)
	v.Set("savedPageForm", savedPageForm)

	if validationErr := savedPageForm.Validate(); validationErr != nil {
		v.Set("savedPageErrorMessage", validationErr.Translate(user.Language))
		response.HTML(w, r, v.Render("add_subscription"))
		return
	}

	entry, saveErr := saveWebPage(h.store, user, savedPageForm.URL)
	if saveErr != nil {
		v.Set("savedPageErrorMessage", saveErr.Translate(user.Language))
		response.HTML(w, r, v.Render("add_subscription"))
		return
	}

	response.HTMLRedirect(w, r, h.routePath("/feed/%d/entries", entry.FeedID))
}

// saveWebPage fetches pageURL, extracts its article text through
// processor.ProcessEntryWebPage - the same crawler path the per-entry
// "fetch content" button and the full-text backfill use - and stores the
// result as an entry in user's saved-pages feed, creating that feed lazily
// on first use (spec §13.4).
//
// The entry hash is derived from the URL, so saving the same URL again goes
// through storage's existing entryExists path (via RefreshFeedEntries) and
// updates the entry in place instead of creating a duplicate.
func saveWebPage(store *storage.Storage, user *model.User, pageURL string) (*model.Entry, *locale.LocalizedErrorWrapper) {
	category, err := store.FirstCategory(user.ID)
	if err != nil {
		return nil, locale.NewLocalizedErrorWrapper(err, "error.database_error", err)
	}

	feedTitle := locale.NewPrinter(user.Language).Print("feed.saved_pages.title")
	feed, err := store.GetOrCreateSavedPagesFeed(user.ID, category.ID, feedTitle)
	if err != nil {
		return nil, locale.NewLocalizedErrorWrapper(err, "error.database_error", err)
	}

	entry := model.NewEntry()
	entry.UserID = user.ID
	entry.FeedID = feed.ID
	entry.Feed = feed
	entry.URL = pageURL
	entry.Hash = crypto.SHA256(pageURL)
	entry.Date = time.Now()
	// A placeholder in case the page holds no heading at all; overwritten
	// below from the scraped content once we have it.
	entry.Title = pageURL

	if scrapeErr := processor.ProcessEntryWebPage(feed, entry, user); scrapeErr != nil {
		return nil, locale.NewLocalizedErrorWrapper(scrapeErr, "error.http_client_error", scrapeErr)
	}

	if !processor.ContainsAnyText(entry.Content) {
		return nil, locale.NewLocalizedErrorWrapper(ErrSavedPageHasNoArticle, "error.saved_page_no_article")
	}

	entry.Title = titleFromScrapedContent(entry.Content, pageURL)

	if _, err := store.RefreshFeedEntries(user.ID, feed.ID, model.Entries{entry}, true); err != nil {
		return nil, locale.NewLocalizedErrorWrapper(err, "error.database_error", err)
	}

	return entry, nil
}

// titleFromScrapedContent pulls a title out of the article readability
// extracted, since the entry arrives with no feed-supplied title to fall
// back on. It reads the scraped content itself rather than fetching the
// page a second time to read <title>, matching the "reuse the existing
// scraper, do not fetch twice" rule. It falls back to the URL, the same
// convention every feed format parser (RSS, Atom, JSON Feed, RDF) already
// uses for an entry with no title.
func titleFromScrapedContent(content, fallbackURL string) string {
	document, err := goquery.NewDocumentFromReader(strings.NewReader(content))
	if err != nil {
		return fallbackURL
	}

	for _, selector := range []string{"h1", "h2"} {
		if title := strings.TrimSpace(document.Find(selector).First().Text()); title != "" {
			return title
		}
	}

	return fallbackURL
}
