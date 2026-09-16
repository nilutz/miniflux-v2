// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"miniflux.app/v2/internal/model"
)

// savedPagesFeedURLPrefix identifies a user's synthetic saved-pages feed
// (spec §13.4, "Saving a URL that has no feed"). It deliberately uses a
// scheme no real subscription's feed_url can ever carry: both the
// add-subscription form (urllib.IsAbsoluteURL) and reader/handler.CreateFeed
// (which re-validates the effective URL after following redirects) require
// an "http://" or "https://" prefix before a feed_url is ever looked up or
// stored. A user_id-scoped "internal://" value is therefore guaranteed
// distinct from any feed_url a real subscription could ever be given, on
// top of the (user_id, feed_url) uniqueness the schema already enforces.
const savedPagesFeedURLPrefix = "internal://saved-pages/user/"

// SavedPagesFeedURL returns the feed_url identifying userID's saved-pages
// feed.
func SavedPagesFeedURL(userID int64) string {
	return savedPagesFeedURLPrefix + strconv.FormatInt(userID, 10)
}

// GetOrCreateSavedPagesFeed returns userID's saved-pages feed, creating it
// lazily and disabled the first time it is needed (spec §13.4: one feed per
// user, disabled so the scheduler's batch builder - which filters on
// "disabled IS false" - never refreshes it).
//
// The insert uses ON CONFLICT DO NOTHING followed by a re-select on
// conflict, so the feed is created exactly once per user even if two saves
// from the same user race each other.
func (s *Storage) GetOrCreateSavedPagesFeed(userID, categoryID int64, title string) (*model.Feed, error) {
	feedURL := SavedPagesFeedURL(userID)

	if feed, err := s.feedByUserAndFeedURL(userID, feedURL); err != nil || feed != nil {
		return feed, err
	}

	if _, err := s.db.Exec(`
		INSERT INTO feeds (feed_url, site_url, title, category_id, user_id, disabled)
		VALUES ($1, $1, $2, $3, $4, true)
		ON CONFLICT (user_id, feed_url) DO NOTHING
	`, feedURL, title, categoryID, userID); err != nil {
		return nil, fmt.Errorf("store: unable to create saved-pages feed for user #%d: %w", userID, err)
	}

	feed, err := s.feedByUserAndFeedURL(userID, feedURL)
	if err != nil {
		return nil, err
	}
	if feed == nil {
		return nil, fmt.Errorf("store: saved-pages feed missing after creation for user #%d", userID)
	}
	return feed, nil
}

// SavedPagesFeed returns userID's saved-pages feed if the user has ever
// saved a page, or nil if they have not. Unlike GetOrCreateSavedPagesFeed,
// it never creates the feed: a page that only lists saved pages (rather
// than storing a new one) must use this lookup, or merely visiting that
// page would conjure an empty synthetic feed into the feed list of every
// user who opens it, including users who have never saved anything.
func (s *Storage) SavedPagesFeed(userID int64) (*model.Feed, error) {
	return s.feedByUserAndFeedURL(userID, SavedPagesFeedURL(userID))
}

func (s *Storage) feedByUserAndFeedURL(userID int64, feedURL string) (*model.Feed, error) {
	var feed model.Feed
	feed.Category = &model.Category{}

	err := s.db.QueryRow(
		`SELECT id, user_id, feed_url, site_url, title, disabled, category_id
		   FROM feeds
		  WHERE user_id=$1 AND feed_url=$2`,
		userID, feedURL,
	).Scan(&feed.ID, &feed.UserID, &feed.FeedURL, &feed.SiteURL, &feed.Title, &feed.Disabled, &feed.Category.ID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: unable to fetch saved-pages feed for user #%d: %w", userID, err)
	default:
		return &feed, nil
	}
}
