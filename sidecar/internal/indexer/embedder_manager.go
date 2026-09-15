// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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

// reachabilityCacheTTL bounds how often Info actually re-probes the
// configured remote for reachability, rather than serving a memoised
// result. Without this, GET / and GET /api/embedder each triggered a
// fresh network round trip on every call, and the admin page's own
// 10-second meta-refresh means any open tab re-triggers one every 10s
// indefinitely -- continuous load on the operator's remote host
// regardless of whether indexing is even running, and worse for a SLOW
// remote than a down one: a down remote fails fast, a slow one pays the
// full reachabilityProbeTimeout on every single refresh. 20s is short
// enough that "reachable" flips to "unreachable" within about two page
// refreshes of an outage starting, and long enough to cut the refresh
// cadence's own load by half or more.
//
// A var, not the constant directly, purely so this package's own tests
// can shrink it and observe the cache actually expiring without a real
// 20-second sleep (the same convention internal/store's pipelineVersion
// uses for the identical reason); production never changes it.
var reachabilityCacheTTL = 20 * time.Second

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

// QueryCacheInvalidator is the subset of *search.Searcher Manager needs
// to keep a live embedder switch correct end to end. internal/search's
// queryCache caches EmbedQuery's output keyed on query TEXT alone, with
// no notion of which model produced a cached vector -- a live switch
// (this file) changes what EmbedQuery returns for the identical text,
// and nothing else invalidates that cache (a review-round finding: the
// first version of this file fixed the embedder REFERENCE via
// Indexer.AsEmbedder but left every already-cached query vector from the
// old model being served, silently, after a perfectly successful
// switch -- spec §13.1's cross-model corruption, reopened one layer up
// from search.passages, which the content-hash mechanism does nothing to
// protect).
//
// An interface, not *search.Searcher directly: internal/indexer must not
// import internal/search (a layering internal/search itself avoids the
// other way too -- search.WithEmbedder takes embed.Embedder, not
// *indexer.Indexer), and this package's own tests must stay hermetic.
type QueryCacheInvalidator interface {
	ClearQueryCache()
}

// ErrSwitchInProgress is returned by Switch when another Switch call is
// already running (a double-click, two admin-page tabs, a retried
// request). Without this, two overlapping Switch calls could each
// construct a candidate, each pause/resume the lanes independently, and
// each call store.SetModelIdentity and persist settings in an
// unspecified interleaving -- the recorded identity ending up not
// matching whichever embedder actually ends up installed. Only one
// Switch may be in flight at a time; a second is rejected outright
// rather than queued, so an operator sees the conflict immediately
// instead of it resolving silently in whatever order the two happened to
// interleave.
var ErrSwitchInProgress = fmt.Errorf("indexer: another embedder switch is already in progress")

// Manager owns the live-swappable embedder end to end (Task 9, spec
// §13.1): showing what is currently configured, testing a candidate
// remote before committing, and performing the swap itself -- quiesce
// both lanes, guarantee no in-flight embed call before closing the old
// embedder, publish the new one through Indexer's synchronised accessor,
// record its identity with the store, invalidate the query cache,
// persist the choice, and resume. internal/web drives it entirely
// through this type so that package never has to reach into
// *Backfill/*LiveMonitor/*Indexer's swap internals itself.
type Manager struct {
	idx        *Indexer
	backfill   *Backfill
	live       *LiveMonitor
	settings   SettingsStore
	factory    EmbedderFactory
	queryCache QueryCacheInvalidator // may be nil (tests, or no search configured)

	// switchMu enforces ErrSwitchInProgress: TryLock'd by Switch, and
	// released by installEmbedder once the background goroutine it starts
	// has COMPLETELY finished -- installed-and-closed-old, or
	// abandoned-and-closed-candidate -- never by Switch's own return,
	// which can happen (ctx cancelled/timeout) long before that
	// background work is actually done. Releasing it on Switch's return
	// instead would let a second Switch start while the first is still
	// resolving, exactly the interleaving ErrSwitchInProgress exists to
	// prevent.
	switchMu sync.Mutex

	mu   sync.Mutex
	info EmbedderInfo // kind/backend/URL -- metadata a bare embed.Embedder cannot self-report

	// reachMu/reachURL/reachResult/reachCachedAt back cachedReachability
	// -- see reachabilityCacheTTL's own doc comment for why Info does not
	// probe the remote fresh on every call.
	reachMu       sync.Mutex
	reachURL      string // the URL reachResult is a reading FOR; a switch to a different URL invalidates the cache outright
	reachResult   ProbeResult
	reachCachedAt time.Time
}

// NewManager builds a Manager. initial describes the embedder idx was
// already constructed with (cmd/sidecar resolves this from the persisted
// settings row, falling back to the startup environment variables when
// none exists -- spec §13.1). queryCache may be nil (see
// QueryCacheInvalidator).
func NewManager(idx *Indexer, backfill *Backfill, live *LiveMonitor, settings SettingsStore, factory EmbedderFactory, initial EmbedderInfo, queryCache QueryCacheInvalidator) *Manager {
	return &Manager{idx: idx, backfill: backfill, live: live, settings: settings, factory: factory, info: initial, queryCache: queryCache}
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
		result := m.cachedReachability(ctx, info.RemoteURL)
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

// cachedReachability returns a reachability probe against url, reusing a
// result already fetched within the last reachabilityCacheTTL for that
// SAME url instead of hitting the network again -- see that constant's
// own doc comment for why. A url that differs from what the cache was
// last populated for (an operator just switched, say) is never served a
// stale cached result for the wrong remote: the cache is keyed on url and
// simply misses when it changes, exactly as expired.
func (m *Manager) cachedReachability(ctx context.Context, url string) ProbeResult {
	m.reachMu.Lock()
	fresh := m.reachURL == url && !m.reachCachedAt.IsZero() && time.Since(m.reachCachedAt) < reachabilityCacheTTL
	cached := m.reachResult
	m.reachMu.Unlock()
	if fresh {
		return cached
	}

	probeCtx, cancel := context.WithTimeout(ctx, reachabilityProbeTimeout)
	result := m.probe(probeCtx, "remote", url)
	cancel()

	m.reachMu.Lock()
	m.reachURL = url
	m.reachResult = result
	m.reachCachedAt = time.Now()
	m.reachMu.Unlock()

	return result
}

// switchTimeout bounds how long Switch's CALLER waits for the
// quiesce-and-swap sequence before giving up and reporting a timeout
// error. The swap itself (SetEmbedderIfNotAbandoned) has no way to be
// cancelled once blocked on embedderMu's write lock -- sync.RWMutex.Lock
// has no context-aware variant -- so a timeout here does not abort it; it
// only stops the CALLER's wait. installEmbedder keeps running in the
// background regardless, and is solely responsible for either finishing
// the swap or disposing of the candidate it never got to install -- see
// installEmbedder's own doc comment for why that decision cannot safely
// be made here, in Switch, at all.
const switchTimeout = 60 * time.Second

// switchOutcome is what installEmbedder reports back to Switch over
// outcomeCh when it reaches a result BEFORE Switch's own select has
// already moved on (ctx cancelled or switchTimeout elapsed) -- Switch is
// the only reader, and only reads if it is still waiting.
type switchOutcome struct {
	result SwitchResult
	err    error
}

// Switch constructs kind/remoteURL as a candidate, quiesces both lanes,
// and starts installEmbedder to perform the actual swap, waiting up to
// switchTimeout (or ctx) for it to finish (Task 9, section 4).
//
// confirm must be true or Switch returns ErrSwitchNotConfirmed without
// touching anything -- section 5's "the switch must not proceed without
// explicit confirmation", enforced server-side rather than trusted
// entirely to the admin page's own confirmation dialog.
//
// Only one Switch may run at a time (ErrSwitchInProgress) -- see
// switchMu's own doc comment for why the lock it holds is released by
// installEmbedder, not here.
//
// Switch itself NEVER closes an embedder it constructed: not the
// candidate (installEmbedder decides, atomically with the install
// decision, whether that is ever safe), and not the previous one either
// (only installEmbedder, immediately after actually installing the
// candidate, knows a fresh old value exists to close). A prior version of
// this function closed the candidate here on a ctx-cancelled/timeout
// path, racing installEmbedder's own still-pending attempt to install
// that exact value -- fixed after a review round found a deterministic
// reproduction: block an in-flight embed to hold the RLock, call Switch
// with an already-cancelled context, release the block, and the active
// embedder came back closed.
func (m *Manager) Switch(ctx context.Context, kind, remoteURL string, confirm bool) (SwitchResult, error) {
	if !confirm {
		return SwitchResult{}, ErrSwitchNotConfirmed
	}
	if m.factory == nil {
		return SwitchResult{}, fmt.Errorf("indexer: no embedder factory configured")
	}

	if !m.switchMu.TryLock() {
		return SwitchResult{}, ErrSwitchInProgress
	}
	// switchMu.Unlock() is NOT deferred here -- see the field's own doc
	// comment. installEmbedder unlocks it once it has completely
	// finished, on every path, including this function's own early
	// returns below (which unlock immediately, since installEmbedder was
	// never started on those paths).

	newEmbedder, backend, err := m.factory(ctx, kind, remoteURL, 0)
	if err != nil {
		m.switchMu.Unlock()
		return SwitchResult{}, err
	}

	// Quiesce both lanes (spec §13.1's requirement; reuses Task 3's own
	// pause/resume machinery on both -- Backfill.Pause/Resume unmodified,
	// LiveMonitor.Pause/Resume added by this task following the identical
	// pattern) so neither starts NEW work while the swap below is in
	// progress. This is defense in depth, not the correctness mechanism
	// itself -- SetEmbedderIfNotAbandoned is what actually proves no
	// in-flight call remains, via sync.RWMutex -- but it bounds how much
	// work either lane can pile up waiting on the write lock. Resuming
	// happens when THIS function returns, even if installEmbedder is
	// still running in the background: new attempts from either lane
	// simply queue up behind installEmbedder's still-pending write lock
	// (RWMutex's own fairness guarantee -- see embedderMu's doc comment),
	// so resuming early is safe, and it is what keeps a slow switch from
	// leaving the lanes paused indefinitely.
	m.backfill.Pause()
	m.live.Pause()
	defer func() {
		m.backfill.Resume()
		m.live.Resume()
	}()

	var abandoned atomic.Bool
	outcomeCh := make(chan switchOutcome, 1)
	go m.installEmbedder(kind, remoteURL, backend, newEmbedder, &abandoned, outcomeCh)

	select {
	case o := <-outcomeCh:
		return o.result, o.err
	case <-ctx.Done():
		abandoned.Store(true)
		return SwitchResult{}, fmt.Errorf("indexer: switch cancelled while waiting for in-flight embedding to finish: %w", ctx.Err())
	case <-time.After(switchTimeout):
		abandoned.Store(true)
		return SwitchResult{}, fmt.Errorf("indexer: timed out after %s waiting for in-flight embedding to finish before switching embedders -- it may still complete in the background", switchTimeout)
	}
}

// installEmbedder is Switch's actual swap, always run in its own
// goroutine, and the SOLE owner of two decisions a prior version of this
// file split unsafely across two goroutines: whether newEmbedder ever
// gets installed at all, and what happens to whichever of
// (newEmbedder, old) does NOT end up active.
//
// It calls SetEmbedderIfNotAbandoned, which makes the install-or-abandon
// decision atomically with the write lock -- see that method's own doc
// comment. Exactly one of the two branches below runs:
//
//   - installed: newEmbedder is now active. old (if any) is safe to Close
//     immediately -- SetEmbedderIfNotAbandoned's own guarantee. This
//     branch does everything a successful switch requires: records the
//     new identity with the store, invalidates the query cache (a
//     review-round finding: without this, a cached pre-switch query
//     vector keeps being served under the new model's identity, spec
//     §13.1's cross-model corruption one layer up from search.passages),
//     persists the choice, and updates Manager's own metadata -- ALL of
//     it here, unconditionally, regardless of whether Switch's own caller
//     is still waiting on outcomeCh or gave up long ago. That is
//     deliberate: the swap either fully happened or it fully did not,
//     never partially depending on whether an HTTP request was still
//     open to see it through.
//   - not installed (abandoned): newEmbedder was never published, so
//     nothing else can be using it -- closing it here, now, is safe and
//     is this goroutine's job, because Switch's own caller no longer
//     knows whether that is still true (see Switch's own doc comment for
//     the bug this replaced).
//
// switchMu is released here, on every path, exactly once -- see that
// field's own doc comment for why Switch itself must not release it.
func (m *Manager) installEmbedder(kind, remoteURL, backend string, newEmbedder embed.Embedder, abandoned *atomic.Bool, outcomeCh chan<- switchOutcome) {
	defer m.switchMu.Unlock()

	old, installed := m.idx.SetEmbedderIfNotAbandoned(newEmbedder, abandoned.Load)
	if !installed {
		if err := newEmbedder.Close(); err != nil {
			slog.Warn("indexer: closing an abandoned switch candidate", slog.Any("error", err))
		}
		slog.Warn("indexer: embedder switch abandoned before it could complete -- the caller gave up waiting; nothing was installed",
			slog.String("kind", kind))
		trySend(outcomeCh, switchOutcome{err: fmt.Errorf("indexer: switch abandoned before it could complete")})
		return
	}

	oldIdentity := ""
	if old != nil {
		oldIdentity = old.Identity()
	}
	newIdentity := newEmbedder.Identity()

	store.SetModelIdentity(newIdentity)

	if old != nil {
		if closeErr := old.Close(); closeErr != nil {
			slog.Warn("indexer: closing previous embedder after switch", slog.Any("error", closeErr))
		}
	}

	if m.queryCache != nil {
		// Must run on every successful install, whether or not Switch's
		// caller is still waiting -- a query embedded under the OLD
		// model must never keep being served once the new one is active,
		// with or without an HTTP response to report it on.
		m.queryCache.ClearQueryCache()
	}

	if m.settings != nil {
		if err := m.settings.SetEmbedderSettings(context.Background(), kind, remoteURL); err != nil {
			// The switch itself already happened -- the identity is
			// recorded with the store and both lanes are indexing under
			// the new embedder -- so this is logged, not returned as a
			// failure of the switch. Left unpersisted, the NEXT restart
			// would revert to the environment variables' default (spec
			// §13.1's own risk this table exists to close), so it is
			// still worth surfacing loudly. context.Background(), not a
			// ctx threaded from Switch: that ctx may already be Done by
			// the time this runs (the abandoned path proves it can be),
			// and a persistence write following a successful in-memory
			// swap must not be skipped just because the ORIGINAL request
			// that triggered it is gone.
			slog.Error("indexer: switch succeeded but the choice could not be persisted -- a restart will revert to the environment variables' default",
				slog.Any("error", err))
		}
	}

	m.mu.Lock()
	m.info = EmbedderInfo{Kind: kind, Backend: backend, RemoteURL: remoteURL}
	m.mu.Unlock()

	slog.Info("indexer: embedder switched via admin page",
		slog.String("kind", kind),
		slog.String("old_identity", oldIdentity),
		slog.String("new_identity", newIdentity),
		slog.Bool("same_identity", oldIdentity == newIdentity),
		slog.Bool("caller_still_waiting", !abandoned.Load()),
	)

	trySend(outcomeCh, switchOutcome{result: SwitchResult{OldIdentity: oldIdentity, NewIdentity: newIdentity, SameIdentity: oldIdentity == newIdentity}})
}

// trySend delivers v on ch without blocking -- ch is always a
// buffered-by-1 channel installEmbedder owns exclusively, so this always
// succeeds; the select/default shape only exists so a future change to
// that invariant fails safe (drops the value) instead of leaking this
// goroutine on a blocked send nobody will ever read.
func trySend[T any](ch chan<- T, v T) {
	select {
	case ch <- v:
	default:
	}
}
