// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"fmt"
	"time"

	"miniflux.app/v2/internal/model"
)

// HideFeedBacklog hides every entry currently belonging to feedID and marks
// them hidden_reason = 'bulk'.
//
// It exists for exactly one caller: feed creation, immediately after the
// first fetch's entries are inserted, when the "hide existing articles"
// checkbox was ticked (spec §13.3). At that point every row for feedID *is*
// the backlog — the feed cannot yet have been refreshed — so there is no
// status or published_at condition to add, unlike the mark-all-as-hidden
// family below which must avoid touching entries a later refresh added.
func (s *Storage) HideFeedBacklog(userID, feedID int64) error {
	query := `
		UPDATE entries
		SET hidden = true, hidden_reason = $1, changed_at = now()
		WHERE user_id = $2 AND feed_id = $3
	`
	if _, err := s.db.Exec(query, model.EntryHiddenReasonBulk, userID, feedID); err != nil {
		return fmt.Errorf(`store: unable to hide backlog of feed #%d: %v`, feedID, err)
	}

	return nil
}

// MarkFeedAsHidden hides every unread, not-yet-hidden entry of feedID
// published before the given time and marks them hidden_reason = 'bulk'.
//
// This mirrors MarkFeedAsRead exactly, including the before cutoff (normally
// the feed's checked_at), so that entries a concurrent refresh adds while
// this bulk action runs are never swept up. The additional `hidden IS FALSE`
// guard is what MarkFeedAsRead does not need but this does: without it, a
// mark-all-as-hidden click would silently overwrite an entry a person hid by
// hand (hidden_reason NULL) with 'bulk', destroying a real preference signal.
func (s *Storage) MarkFeedAsHidden(userID, feedID int64, before time.Time) error {
	query := `
		UPDATE entries
		SET hidden = true, hidden_reason = $1, changed_at = now()
		WHERE user_id = $2 AND feed_id = $3 AND status = $4 AND hidden IS FALSE AND published_at < $5
	`
	if _, err := s.db.Exec(query, model.EntryHiddenReasonBulk, userID, feedID, model.EntryStatusUnread, before); err != nil {
		return fmt.Errorf(`store: unable to mark feed entries as hidden: %v`, err)
	}

	return nil
}

// MarkCategoryAsHidden hides every unread, not-yet-hidden entry of
// categoryID published before the given time and marks them
// hidden_reason = 'bulk'. See MarkFeedAsHidden for why the extra
// `hidden IS FALSE` guard, absent from MarkCategoryAsRead, is required here.
func (s *Storage) MarkCategoryAsHidden(userID, categoryID int64, before time.Time) error {
	query := `
		UPDATE entries
		SET hidden = true, hidden_reason = $1, changed_at = now()
		FROM feeds
		WHERE
			entries.feed_id = feeds.id
			AND feeds.user_id = $2
			AND entries.status = $3
			AND entries.hidden IS FALSE
			AND entries.published_at < $4
			AND feeds.category_id = $5
	`
	if _, err := s.db.Exec(query, model.EntryHiddenReasonBulk, userID, model.EntryStatusUnread, before, categoryID); err != nil {
		return fmt.Errorf(`store: unable to mark category entries as hidden: %v`, err)
	}

	return nil
}

// MarkGloballyVisibleFeedsAsHidden hides every unread, not-yet-hidden entry
// visible in the global unread view (belonging to a feed and category that
// are both not hide_globally) and marks them hidden_reason = 'bulk'. This
// mirrors MarkGloballyVisibleFeedsAsRead, the storage call behind the
// top-level "mark all as read" route, for its "mark all as hidden" sibling.
func (s *Storage) MarkGloballyVisibleFeedsAsHidden(userID int64) error {
	query := `
		UPDATE entries
		SET hidden = true, hidden_reason = $1, changed_at = now()
		FROM feeds
			JOIN categories ON (categories.id = feeds.category_id)
		WHERE
			entries.feed_id = feeds.id
			AND entries.user_id = $2
			AND entries.status = $3
			AND entries.hidden IS FALSE
			AND feeds.hide_globally IS FALSE
			AND categories.hide_globally IS FALSE
	`
	if _, err := s.db.Exec(query, model.EntryHiddenReasonBulk, userID, model.EntryStatusUnread); err != nil {
		return fmt.Errorf(`store: unable to mark globally visible feeds as hidden: %v`, err)
	}

	return nil
}
