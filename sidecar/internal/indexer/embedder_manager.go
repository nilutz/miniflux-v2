// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"miniflux.app/v2/sidecar/internal/embed"
	"miniflux.app/v2/sidecar/internal/embed/remote"
	"miniflux.app/v2/sidecar/internal/store"
)

// schemaDimensions reads the fixed vector width search.passages.embedding
// requires from internal/embed/remote's own exported constant, rather
// than hardcoding 384 a second time here: internal/embed/remote is pure
// Go (no CGO, no native ONNX Runtime dependency), so importing it from
// this package costs nothing in build tags or test hermeticity -- and it
// means the nomic migration plan's 768-dimension schema change only has
// to update remote.WantDimensions once for every reader, this one
// included, to agree.
func schemaDimensions() int { return remote.WantDimensions }

// EmbedderFactory constructs a fresh embed.Embedder for the given kind
// ("local" or "remote") and, for "remote", the candidate URL. timeout
// bounds a remote construction's own HTTP requests (including its
// startup identity probe); zero means "use the operationally configured
// default" (cmd/sidecar's SIDECAR_REMOTE_EMBEDDER_TIMEOUT, or
// remote.DefaultTimeout) -- Manager passes a short, fixed timeout instead
// only for its own background reachability check (see Info), so a down
// remote cannot make every admin page load hang for
// remote.DefaultTimeout.
//
// cmd/sidecar supplies the concrete implementation, closing over the
// model path/ONNX library dir read from the environment for "local" and
// the configured request timeout for "remote" -- the only package
// allowed to import internal/embed/onnx, which requires CGO/-tags ORT.
// Manager is injected this rather than importing internal/embed/onnx or
// internal/embed/remote itself, so `go test ./internal/indexer/...` never
// needs the native ONNX Runtime / libtokenizers libraries, exactly like
// every other embedder dependency in this package.
//
// backend is "" for a remote embedder, and reports which local backend
// actually built ("ORT" or the pure-Go fallback's name) for a local one
// -- the admin page's Model section shows it because omitting -tags ORT
// silently selects the pure-Go fallback, roughly 10x slower (spec §6.7),
// with no error anywhere.
type EmbedderFactory func(ctx context.Context, kind, remoteURL string, timeout time.Duration) (e embed.Embedder, backend string, err error)

// reachabilityProbeTimeout bounds Info's own background reachability
// check against the currently configured remote embedder -- short and
// fixed, deliberately independent of the remote's operationally
// configured request timeout (which can be up to
// remote.DefaultTimeout=30s): the admin page's Model section is read on
// every page load, and a down remote must not make that load hang for
// anywhere near that long.
const reachabilityProbeTimeout = 5 * time.Second

// measuredPassagesPerSecond and measuredPassagesPerEntry are the task 9
// brief's own measured throughput figures (spec §13.1's re-index cost
// estimate): ~33.9 passages/sec, 13 passages per entry, used only to
// estimate how long re-indexing the corpus will take after a model
// switch -- shown to the operator BEFORE they confirm (section 5: "the
// confirmation must state the actual entry count and the estimate").
const (
	measuredPassagesPerSecond = 33.9
	measuredPassagesPerEntry  = 13.0
)

// EmbedderInfo is what the admin page's Model section (Task 9, spec
// §13.1) shows about the embedder currently in use: kind, backend,
// identity, dimensions, and for a remote embedder its URL and whether it
// is currently reachable.
type EmbedderInfo struct {
	Kind       string `json:"kind"`    // "local" or "remote"
	Backend    string `json:"backend"` // local only: "ORT" or the pure-Go fallback's name
	Identity   string `json:"identity"`
	Dimensions int    `json:"dimensions"`
	RemoteURL  string `json:"remote_url"` // remote only

	// SchemaDimensions is the fixed width search.passages.embedding
	// requires right now (remote.WantDimensions) -- shown so an operator
	// can see for themselves why a candidate was refused, not just read a
	// refusal message.
	SchemaDimensions int `json:"schema_dimensions"`

	// ReachabilityChecked/Reachable/ReachabilityError describe a fresh,
	// short-timeout probe against RemoteURL performed while building this
	// snapshot (remote only; always false/empty for local, which is
	// always reachable by construction -- it runs in this same process).
	ReachabilityChecked bool   `json:"reachability_checked"`
	Reachable           bool   `json:"reachable"`
	ReachabilityError   string `json:"reachability_error"`
}

// ProbeResult is what a candidate remote embedder reports before
// anything is swapped (Task 9, section 3: "test before committing").
// Error is non-empty, and every other field is the zero value, when the
// candidate could not be constructed at all -- unreachable, or (via
// EmbedderFactory delegating to remote.New) a dimension mismatch against
// the fixed schema width, refused here rather than after a switch has
// already started re-embedding.
type ProbeResult struct {
	Reachable  bool   `json:"reachable"`
	Identity   string `json:"identity"`
	Dimensions int    `json:"dimensions"`
	Backend    string `json:"backend"`
	Error      string `json:"error"`
}

// SwitchPreview is what the admin page shows an operator BEFORE they
// confirm a switch (Task 9, section 5): the candidate's own ProbeResult,
// plus how many entries the corpus has right now and how long
// re-indexing all of them is estimated to take at the measured
// throughput -- unless SameIdentity is true (the "same model on another
// machine" case), in which case nothing needs re-indexing at all.
type SwitchPreview struct {
	ProbeResult
	Kind      string `json:"kind"`
	RemoteURL string `json:"remote_url"`

	// SameIdentity is true only when the candidate reports EXACTLY the
	// identity the currently active embedder already has -- the one case
	// spec §13.1 calls out as not needing a re-index. It will not be true
	// by default for a local -> remote switch even of "the same model",
	// because the ONNX implementation's revision is a sha256 of the model
	// file while a remote's is whatever the server reports; it takes a
	// remote deliberately configured to report the identical identity.
	SameIdentity bool `json:"same_identity"`

	EntryCount        int64         `json:"entry_count"`
	EntryCountUnknown bool          `json:"entry_count_unknown"`
	EstimatedDuration time.Duration `json:"estimated_duration_ns"`
}

// SwitchResult is what Manager.Switch reports after actually performing a
// switch.
type SwitchResult struct {
	OldIdentity  string `json:"old_identity"`
	NewIdentity  string `json:"new_identity"`
	SameIdentity bool   `json:"same_identity"`
}

// ErrSwitchNotConfirmed is returned by Switch when confirm is false --
// section 5's "the switch must not proceed without explicit
// confirmation", enforced here rather than trusted to the caller (the
// admin page's own JS dialog) alone.
var ErrSwitchNotConfirmed = fmt.Errorf("indexer: embedder switch requires explicit confirmation")

// SettingsStore is the subset of *store.Store Manager needs to persist an
// operator's embedder choice (spec §13.1: "the persisted row wins; the
// environment variables are the initial default used only when no row
// exists").
type SettingsStore interface {
	DatabaseMetrics(ctx context.Context) (store.DatabaseMetrics, error)
	SetEmbedderSettings(ctx context.Context, kind, remoteURL string) error
}

// Manager owns the live-swappable embedder end to end (Task 9, spec
// §13.1): showing what is currently configured, testing a candidate
// remote before committing, and performing the swap itself -- quiesce
// both lanes, guarantee no in-flight embed call before closing the old
// embedder, publish the new one through Indexer's synchronised accessor,
// record its identity with the store, persist the choice, and resume.
// internal/web drives it entirely through this type so that package never
// has to reach into *Backfill/*LiveMonitor/*Indexer's swap internals
// itself.
type Manager struct {
	idx      *Indexer
	backfill *Backfill
	live     *LiveMonitor
	settings SettingsStore
	factory  EmbedderFactory

	mu   sync.Mutex
	info EmbedderInfo // kind/backend/URL -- metadata a bare embed.Embedder cannot self-report
}

// NewManager builds a Manager. initial describes the embedder idx was
// already constructed with (cmd/sidecar resolves this from the persisted
// settings row, falling back to the startup environment variables when
// none exists -- spec §13.1).
func NewManager(idx *Indexer, backfill *Backfill, live *LiveMonitor, settings SettingsStore, factory EmbedderFactory, initial EmbedderInfo) *Manager {
	return &Manager{idx: idx, backfill: backfill, live: live, settings: settings, factory: factory, info: initial}
}

// Info returns a snapshot of the currently configured embedder --
// identity and dimensions read live from the Indexer (so this can never
// drift from what Switch actually set), kind/backend/URL from what the
// Manager was last told (construction or the last Switch), and, for a
// remote embedder, a fresh short-timeout reachability probe so the page
// shows current state, not what was true when the process started or the
// last switch happened.
func (m *Manager) Info(ctx context.Context) EmbedderInfo {
	m.mu.Lock()
	info := m.info
	m.mu.Unlock()

	info.SchemaDimensions = schemaDimensions()

	if e := m.idx.Embedder(); e != nil {
		info.Identity = e.Identity()
		info.Dimensions = e.Dimensions()
	}

	if info.Kind == "remote" && info.RemoteURL != "" && m.factory != nil {
		probeCtx, cancel := context.WithTimeout(ctx, reachabilityProbeTimeout)
		result := m.probe(probeCtx, "remote", info.RemoteURL)
		cancel()
		info.ReachabilityChecked = true
		info.Reachable = result.Error == ""
		info.ReachabilityError = result.Error
	}

	return info
}

// Preview probes a candidate embedder (Task 9, section 3: "without
// swapping anything") and, when it is reachable, adds section 5's
// re-index cost estimate against the corpus's current entry count. It
// swaps nothing and touches neither lane -- see Switch for the operation
// that actually applies a choice.
func (m *Manager) Preview(ctx context.Context, kind, remoteURL string) SwitchPreview {
	result := m.probe(ctx, kind, remoteURL)
	preview := SwitchPreview{ProbeResult: result, Kind: kind, RemoteURL: remoteURL}
	if result.Error != "" {
		return preview
	}

	if current := m.idx.Embedder(); current != nil {
		preview.SameIdentity = current.Identity() == result.Identity
	}

	if m.settings == nil {
		preview.EntryCountUnknown = true
		return preview
	}

	dm, err := m.settings.DatabaseMetrics(ctx)
	switch {
	case err != nil:
		slog.Error("indexer: unable to read entry count for a switch preview", slog.Any("error", err))
		preview.EntryCountUnknown = true
	case dm.EntryCountUnknown:
		preview.EntryCountUnknown = true
	default:
		preview.EntryCount = dm.EntryCount
		preview.EstimatedDuration = estimateReindexDuration(dm.EntryCount)
	}

	return preview
}

// estimateReindexDuration applies section 5's measured throughput figures
// to entryCount, rounding up to the nearest second so a nonzero-but-tiny
// corpus never estimates to 0s.
func estimateReindexDuration(entryCount int64) time.Duration {
	if entryCount <= 0 {
		return 0
	}
	seconds := float64(entryCount) * measuredPassagesPerEntry / measuredPassagesPerSecond
	return time.Duration(seconds*float64(time.Second)) + time.Second/2
}

// probe constructs a candidate embedder via the factory and immediately
// closes it -- it never becomes the active embedder. A dimension
// mismatch against the fixed schema width surfaces as an ordinary error
// here (EmbedderFactory's "remote" case delegates to remote.New, which
// already refuses one against remote.WantDimensions), so this needs no
// separate width check of its own; ProbeResult.Error carries that message
// verbatim.
func (m *Manager) probe(ctx context.Context, kind, remoteURL string) ProbeResult {
	if m.factory == nil {
		return ProbeResult{Error: "indexer: no embedder factory configured"}
	}
	e, backend, err := m.factory(ctx, kind, remoteURL, reachabilityProbeTimeout)
	if err != nil {
		return ProbeResult{Error: err.Error()}
	}
	defer e.Close()
	return ProbeResult{Reachable: true, Identity: e.Identity(), Dimensions: e.Dimensions(), Backend: backend}
}

// switchTimeout bounds how long Switch waits for the quiesce-and-swap
// sequence below before giving up and reporting a timeout error. The
// swap itself (Indexer.SetEmbedder) has no way to be cancelled once
// started -- sync.RWMutex.Lock has no context-aware variant -- so a
// timeout here does not abort it; it can only stop WAITING and let the
// caller (the HTTP handler) return an answer instead of hanging
// indefinitely. The swap keeps running in the background and completes
// on its own once whatever in-flight batch it is waiting on finishes.
const switchTimeout = 60 * time.Second

// Switch constructs kind/remoteURL as a candidate, quiesces both lanes,
// guarantees no in-flight embed call before closing the previous
// embedder, publishes the new one, records its identity with the store,
// persists the choice, and resumes both lanes (Task 9, section 4).
//
// confirm must be true or Switch returns ErrSwitchNotConfirmed without
// touching anything -- section 5's "the switch must not proceed without
// explicit confirmation", enforced server-side rather than trusted
// entirely to the admin page's own confirmation dialog.
func (m *Manager) Switch(ctx context.Context, kind, remoteURL string, confirm bool) (SwitchResult, error) {
	if !confirm {
		return SwitchResult{}, ErrSwitchNotConfirmed
	}
	if m.factory == nil {
		return SwitchResult{}, fmt.Errorf("indexer: no embedder factory configured")
	}

	newEmbedder, backend, err := m.factory(ctx, kind, remoteURL, 0)
	if err != nil {
		return SwitchResult{}, err
	}

	oldIdentity := ""
	if e := m.idx.Embedder(); e != nil {
		oldIdentity = e.Identity()
	}

	// Quiesce both lanes (spec §13.1's requirement; reuses Task 3's own
	// pause/resume machinery on both -- Backfill.Pause/Resume unmodified,
	// LiveMonitor.Pause/Resume added by this task following the identical
	// pattern) so neither starts NEW work while the swap below is in
	// progress. This is defense in depth, not the correctness mechanism
	// itself -- SetEmbedder below is what actually proves no in-flight
	// call remains, via sync.RWMutex -- but it bounds how much work either
	// lane can pile up waiting on the write lock, and it is what makes
	// Stats().Paused honestly reflect "a switch is in progress" for
	// anyone watching the admin page mid-swap.
	m.backfill.Pause()
	m.live.Pause()
	defer func() {
		m.backfill.Resume()
		m.live.Resume()
	}()

	old, err := m.setEmbedderWithTimeout(ctx, newEmbedder)
	if err != nil {
		newEmbedder.Close()
		return SwitchResult{}, err
	}

	store.SetModelIdentity(newEmbedder.Identity())

	if old != nil {
		if closeErr := old.Close(); closeErr != nil {
			slog.Warn("indexer: closing previous embedder after switch", slog.Any("error", closeErr))
		}
	}

	if m.settings != nil {
		if err := m.settings.SetEmbedderSettings(ctx, kind, remoteURL); err != nil {
			// The switch itself already happened -- the identity is
			// recorded with the store and both lanes are indexing under
			// the new embedder -- so this is logged, not returned as a
			// failure of the switch. Left unpersisted, the NEXT restart
			// would revert to the environment variables' default (spec
			// §13.1's own risk this table exists to close), so it is
			// still worth surfacing loudly.
			slog.Error("indexer: switch succeeded but the choice could not be persisted -- a restart will revert to the environment variables' default",
				slog.Any("error", err))
		}
	}

	newIdentity := newEmbedder.Identity()
	m.mu.Lock()
	m.info = EmbedderInfo{Kind: kind, Backend: backend, RemoteURL: remoteURL}
	m.mu.Unlock()

	slog.Info("indexer: embedder switched via admin page",
		slog.String("kind", kind),
		slog.String("old_identity", oldIdentity),
		slog.String("new_identity", newIdentity),
		slog.Bool("same_identity", oldIdentity == newIdentity),
	)

	return SwitchResult{OldIdentity: oldIdentity, NewIdentity: newIdentity, SameIdentity: oldIdentity == newIdentity}, nil
}

// setEmbedderWithTimeout runs Indexer.SetEmbedder in a goroutine and waits
// for it, ctx, or switchTimeout, whichever comes first -- see
// switchTimeout's own doc comment for why a timeout here can only stop
// waiting, not cancel the swap itself.
func (m *Manager) setEmbedderWithTimeout(ctx context.Context, e embed.Embedder) (embed.Embedder, error) {
	done := make(chan embed.Embedder, 1)
	go func() { done <- m.idx.SetEmbedder(e) }()

	select {
	case old := <-done:
		return old, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("indexer: switch cancelled while waiting for in-flight embedding to finish: %w", ctx.Err())
	case <-time.After(switchTimeout):
		return nil, fmt.Errorf("indexer: timed out after %s waiting for in-flight embedding to finish before switching embedders -- it may still complete in the background", switchTimeout)
	}
}
