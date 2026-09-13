// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"fmt"
	"time"

	"github.com/lib/pq"
)

// EntryIDsWithoutFullText returns up to limit entry IDs greater than afterID
// that have never been successfully scraped for full text, in ascending id
// order so the backfill can checkpoint on the last id it processed.
func (s *Storage) EntryIDsWithoutFullText(afterID int64, limit int) ([]int64, error) {
	query := `
		SELECT id
		FROM entries
		WHERE full_text_fetched_at IS NULL AND id > $1
		ORDER BY id ASC
		LIMIT $2
	`

	rows, err := s.db.Query(query, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch entry ids without full text: %v`, err)
	}
	defer rows.Close()

	var entryIDs []int64
	for rows.Next() {
		var entryID int64
		if err := rows.Scan(&entryID); err != nil {
			return nil, fmt.Errorf(`store: unable to scan entry id: %v`, err)
		}
		entryIDs = append(entryIDs, entryID)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(`store: unable to read entry ids without full text: %v`, err)
	}

	return entryIDs, nil
}

// MarkFullTextFetched records that an entry's full text has been successfully
// scraped and stored.
func (s *Storage) MarkFullTextFetched(entryID int64, fetchedAt time.Time) error {
	query := `UPDATE entries SET full_text_fetched_at=$1 WHERE id=$2`

	if _, err := s.db.Exec(query, fetchedAt, entryID); err != nil {
		return fmt.Errorf(`store: unable to mark entry #%d as fetched: %v`, entryID, err)
	}

	return nil
}

// MarkFullTextFetchedByHashes records that the given entries of a feed hold
// full text scraped from their original web page.
//
// The live crawler path (processor.ProcessFeedEntries) scrapes entries before
// they are persisted, so it has no entry ids to work with; (feed_id, hash) is
// the natural key those entries are stored under. Hashes that match no row
// (a tombstoned entry, for instance) are silently ignored.
func (s *Storage) MarkFullTextFetchedByHashes(feedID int64, entryHashes []string, fetchedAt time.Time) error {
	if len(entryHashes) == 0 {
		return nil
	}

	query := `UPDATE entries SET full_text_fetched_at=$1 WHERE feed_id=$2 AND hash=ANY($3)`

	if _, err := s.db.Exec(query, fetchedAt, feedID, pq.Array(entryHashes)); err != nil {
		return fmt.Errorf(`store: unable to mark entries of feed #%d as fetched: %v`, feedID, err)
	}

	return nil
}

// EntryOwner returns the user id and feed id that own the given entry. It
// exists so that a caller holding only an entry id (such as the full-text
// backfill, which discovers ids via EntryIDsWithoutFullText) can build the
// per-user EntryQueryBuilder that GetEntry requires, without another way to
// look up a single entry across users.
func (s *Storage) EntryOwner(entryID int64) (userID, feedID int64, err error) {
	query := `SELECT user_id, feed_id FROM entries WHERE id=$1`

	if err := s.db.QueryRow(query, entryID).Scan(&userID, &feedID); err != nil {
		return 0, 0, fmt.Errorf(`store: unable to fetch owner of entry #%d: %v`, entryID, err)
	}

	return userID, feedID, nil
}
