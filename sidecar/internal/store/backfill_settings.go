// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BackfillSettings is the operator's persisted backfill runtime
// configuration (spec §9.2): the schedule window, the worker floor/
// ceiling, the load threshold, the embedding batch size, the pagination
// page size, and the idle poll/resweep intervals -- every field
// indexer.RuntimeConfig itself reports, minus the two fields
// (MaxAllowedWorkersOnHost, MinBatchSizeAllowed/MaxBatchSizeAllowed) that
// describe this HOST rather than the operator's own choice and so have
// nothing to persist. See search.backfill_settings' own migration comment
// for why this is a singleton row rather than a general-purpose settings
// table -- the same reasoning search.embedder_settings already uses.
type BackfillSettings struct {
	WindowStart int
	WindowEnd   int

	MinWorkers    int
	MaxWorkers    int
	LoadThreshold float64

	BatchSize int
	PageSize  int

	PollIntervalSeconds     float64
	IdleResweepIntervalSecs float64

	UpdatedAt time.Time
}

// GetBackfillSettings returns the persisted backfill runtime
// configuration, or nil (and no error) if an operator has never changed it
// from the admin page -- a distinct, meaningful state from any row's own
// content: it is what lets cmd/sidecar fall back to the
// SIDECAR_BACKFILL_* environment variables as the *initial* default,
// mirroring GetEmbedderSettings' own contract (spec §13.1: "the persisted
// row wins; the environment variables are the initial default used only
// when no row exists"). Callers must not confuse a nil result with an
// error.
func (s *Store) GetBackfillSettings(ctx context.Context) (*BackfillSettings, error) {
	var row BackfillSettings
	err := s.db.QueryRowContext(ctx, `
		SELECT window_start, window_end, min_workers, max_workers, load_threshold,
		       batch_size, page_size, poll_interval_seconds, idle_resweep_interval_seconds,
		       updated_at
		FROM search.backfill_settings
		WHERE id = 1
	`).Scan(
		&row.WindowStart, &row.WindowEnd, &row.MinWorkers, &row.MaxWorkers, &row.LoadThreshold,
		&row.BatchSize, &row.PageSize, &row.PollIntervalSeconds, &row.IdleResweepIntervalSecs,
		&row.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: unable to read backfill settings: %w", err)
	}
	return &row, nil
}

// SetBackfillSettings persists settings as the operator's backfill runtime
// configuration, overwriting whatever (if anything) was recorded before.
// It is the only way a configuration change made through the admin page's
// POST /api/backfill/config survives a restart -- without it, the compose
// stack's `restart: unless-stopped` (and every ordinary process restart,
// including one forced by defect 1's crash) silently reverts to the
// SIDECAR_BACKFILL_* environment variables' default the next time the
// process starts, changing the throttle back with nobody asking -- see
// this table's own migration comment for the exact log line that surfaced
// this.
func (s *Store) SetBackfillSettings(ctx context.Context, settings BackfillSettings) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO search.backfill_settings (
			id, window_start, window_end, min_workers, max_workers, load_threshold,
			batch_size, page_size, poll_interval_seconds, idle_resweep_interval_seconds, updated_at
		)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (id) DO UPDATE
		SET window_start = EXCLUDED.window_start,
		    window_end = EXCLUDED.window_end,
		    min_workers = EXCLUDED.min_workers,
		    max_workers = EXCLUDED.max_workers,
		    load_threshold = EXCLUDED.load_threshold,
		    batch_size = EXCLUDED.batch_size,
		    page_size = EXCLUDED.page_size,
		    poll_interval_seconds = EXCLUDED.poll_interval_seconds,
		    idle_resweep_interval_seconds = EXCLUDED.idle_resweep_interval_seconds,
		    updated_at = EXCLUDED.updated_at
	`,
		settings.WindowStart, settings.WindowEnd, settings.MinWorkers, settings.MaxWorkers, settings.LoadThreshold,
		settings.BatchSize, settings.PageSize, settings.PollIntervalSeconds, settings.IdleResweepIntervalSecs,
	)
	if err != nil {
		return fmt.Errorf("store: unable to persist backfill settings: %w", err)
	}
	return nil
}
