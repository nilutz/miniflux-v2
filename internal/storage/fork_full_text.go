// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"fmt"
	"time"
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
