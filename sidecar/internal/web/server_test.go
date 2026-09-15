// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"miniflux.app/v2/sidecar/internal/indexer"
	"miniflux.app/v2/sidecar/internal/store"
)

// fakeBackfill is a hermetic stand-in for *indexer.Backfill: it satisfies
// BackfillController without a store, an embedder, or a real Controller, so
// these tests never need a database or a native ONNX Runtime
// library. Pause/Resume just flip a flag Stats() reflects, matching the
// contract the real Backfill.Pause/Resume/Stats documents.
type fakeBackfill struct {
	mu      sync.Mutex
	stats   indexer.Stats
	paused  bool
	cfg     indexer.RuntimeConfig
	patches []indexer.ConfigPatch
	cfgErr  error
}

func (f *fakeBackfill) Stats() indexer.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.stats
	st.Paused = f.paused
	return st
}

func (f *fakeBackfill) Pause() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
}

func (f *fakeBackfill) Resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = false
}

func (f *fakeBackfill) RuntimeConfig() indexer.RuntimeConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

// ApplyConfig records the patch it was handed and folds it into the
// reported configuration. It deliberately does NOT re-implement
// indexer.ApplyConfig's clamping — that is indexer's own tests' job; these
// tests are about the HTTP surface faithfully carrying a patch across and
// reporting back what the lane says, not about the clamp arithmetic.
func (f *fakeBackfill) ApplyConfig(patch indexer.ConfigPatch) (indexer.RuntimeConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, patch)
	if f.cfgErr != nil {
		return f.cfg, f.cfgErr
	}
	if patch.Window != nil {
		f.cfg.WindowStart, f.cfg.WindowEnd = patch.Window.Start, patch.Window.End
	}
	if patch.MinWorkers != nil {
		f.cfg.MinWorkers = *patch.MinWorkers
	}
	if patch.MaxWorkers != nil {
		f.cfg.MaxWorkers = *patch.MaxWorkers
	}
	if patch.BatchSize != nil {
		f.cfg.BatchSize = *patch.BatchSize
	}
	if patch.PageSize != nil {
		f.cfg.PageSize = *patch.PageSize
	}
	if patch.PollInterval != nil {
		f.cfg.PollIntervalSeconds = patch.PollInterval.Seconds()
	}
	if patch.IdleResweepInterval != nil {
		f.cfg.IdleResweepIntervalSecs = patch.IdleResweepInterval.Seconds()
	}
	return f.cfg, nil
}

func (f *fakeBackfill) appliedPatches() []indexer.ConfigPatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]indexer.ConfigPatch, len(f.patches))
	copy(out, f.patches)
	return out
}

// fakeLive is a hermetic stand-in for *indexer.LiveMonitor: it satisfies
// LiveLane by returning a fixed LiveStats snapshot, so tests can render
// the live lane's own pause state on the status page without a real
// runLive anywhere nearby.
type fakeLive struct {
	stats indexer.LiveStats
}

func (f *fakeLive) Stats() indexer.LiveStats { return f.stats }

// fakeMetrics is a hermetic stand-in for *store.Store's DatabaseMetrics
// method: it satisfies DatabaseMetricsSource by returning a fixed
// store.DatabaseMetrics (or a fixed error), so tests can render spec
// §13.2's database-size section without a real database anywhere nearby.
type fakeMetrics struct {
	metrics store.DatabaseMetrics
	err     error
}

func (f *fakeMetrics) DatabaseMetrics(context.Context) (store.DatabaseMetrics, error) {
	return f.metrics, f.err
}

func newTestServer(t *testing.T, fb *fakeBackfill) http.Handler {
	t.Helper()
	// nil for live: these tests exercise only the backfill control
	// endpoints and don't care what the live lane's own pause state
	// renders as (see TestStatusDistinguishesLiveLanePauseFromBackfill
	// for that, via newTestServerWithLive). nil, nil, nil: no search
	// service/entries/metrics source either -- see search_handlers_test.go
	// for the first two and TestStatusPage*Metrics* below for the third.
	srv, err := New(fb, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}

// newTestServerWithLive is newTestServer plus a real, non-nil LiveLane, for
// the tests that specifically check the live lane's own pause state
// renders distinctly from the backfill lane's.
func newTestServerWithLive(t *testing.T, fb *fakeBackfill, live LiveLane) http.Handler {
	t.Helper()
	srv, err := New(fb, live, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}

// newTestServerWithMetrics is newTestServer plus a real, non-nil
// DatabaseMetricsSource, for the tests that check spec §13.2's
// database-size section renders from it.
func newTestServerWithMetrics(t *testing.T, fb *fakeBackfill, metrics DatabaseMetricsSource) http.Handler {
	t.Helper()
	srv, err := New(fb, nil, nil, nil, metrics)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}

func TestAPIStatusReturnsProgressThroughputWorkersAndReason(t *testing.T) {
	fb := &fakeBackfill{stats: indexer.Stats{
		Indexed:          100,
		Skipped:          5,
		Failed:           2,
		Remaining:        900,
		SkippedByReason:  map[string]int64{"no usable text extracted from entry content": 5},
		FailedByReason:   map[string]int64{"embedding failed: boom": 2},
		Workers:          1,
		ControllerReason: "establishing latency baseline (1/3 batches)",
		ThroughputPerSec: 4.5,
	}}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var got struct {
		Indexed          int64            `json:"indexed"`
		Skipped          int64            `json:"skipped"`
		Failed           int64            `json:"failed"`
		Remaining        int64            `json:"remaining"`
		SkippedByReason  map[string]int64 `json:"skipped_by_reason"`
		FailedByReason   map[string]int64 `json:"failed_by_reason"`
		Workers          int              `json:"workers"`
		ControllerReason string           `json:"controller_reason"`
		ThroughputPerSec float64          `json:"throughput_per_sec"`
		Paused           bool             `json:"paused"`
		Done             bool             `json:"done"`
		Total            int64            `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v, body = %s", err, rec.Body.String())
	}

	if got.Indexed != 100 || got.Skipped != 5 || got.Failed != 2 || got.Remaining != 900 {
		t.Errorf("progress fields = %+v", got)
	}
	if got.Total != 1007 {
		t.Errorf("Total = %d, want 1007 (indexed+skipped+failed+remaining)", got.Total)
	}
	if got.Workers != 1 {
		t.Errorf("Workers = %d, want 1", got.Workers)
	}
	if got.ControllerReason != "establishing latency baseline (1/3 batches)" {
		t.Errorf("ControllerReason = %q", got.ControllerReason)
	}
	if got.ThroughputPerSec != 4.5 {
		t.Errorf("ThroughputPerSec = %v, want 4.5", got.ThroughputPerSec)
	}
	if got.SkippedByReason["no usable text extracted from entry content"] != 5 {
		t.Errorf("SkippedByReason = %+v", got.SkippedByReason)
	}
	if got.FailedByReason["embedding failed: boom"] != 2 {
		t.Errorf("FailedByReason = %+v", got.FailedByReason)
	}
	if got.Paused {
		t.Errorf("Paused = true before Pause() was ever called")
	}
}

func TestPauseThenStatusShowsPaused(t *testing.T) {
	fb := &fakeBackfill{stats: indexer.Stats{Workers: 2}}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodPost, "/api/backfill/pause", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d, body = %s", rec.Code, rec.Body.String())
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	statusRec := httptest.NewRecorder()
	handler.ServeHTTP(statusRec, statusReq)

	var got struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(statusRec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Paused {
		t.Errorf("Paused = false after POST /api/backfill/pause")
	}
}

func TestResumeThenStatusShowsRunning(t *testing.T) {
	fb := &fakeBackfill{stats: indexer.Stats{Workers: 2}, paused: true}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodPost, "/api/backfill/resume", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s", rec.Code, rec.Body.String())
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	statusRec := httptest.NewRecorder()
	handler.ServeHTTP(statusRec, statusReq)

	var got struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(statusRec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Paused {
		t.Errorf("Paused = true after POST /api/backfill/resume")
	}
}

// (Task 3, spec §13.1.) The status JSON and page must distinguish an
// embedder-triggered pause from an operator's own Pause(): "paused by
// operator" and "paused: embedder unreachable" are different states, and
// an operator whose backfill stopped needs to know which one it is.
func TestStatusDistinguishesEmbedderPauseFromOperatorPause(t *testing.T) {
	const reason = "embed/remote: request failed: dial tcp: connection refused"
	fb := &fakeBackfill{stats: indexer.Stats{
		EmbedderPaused:      true,
		EmbedderPauseReason: reason,
	}}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var got struct {
		Paused              bool   `json:"paused"`
		EmbedderPaused      bool   `json:"embedder_paused"`
		EmbedderPauseReason string `json:"embedder_pause_reason"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v, body = %s", err, rec.Body.String())
	}
	if got.Paused {
		t.Error("Paused (the OPERATOR pause) must be false: this is an embedder-triggered pause, a distinct state")
	}
	if !got.EmbedderPaused {
		t.Error("expected EmbedderPaused = true")
	}
	if got.EmbedderPauseReason != reason {
		t.Errorf("EmbedderPauseReason = %q, want %q", got.EmbedderPauseReason, reason)
	}

	htmlReq := httptest.NewRequest(http.MethodGet, "/", nil)
	htmlRec := httptest.NewRecorder()
	handler.ServeHTTP(htmlRec, htmlReq)
	body := htmlRec.Body.String()
	if !strings.Contains(body, "embedder unreachable") {
		t.Errorf("expected the status page to say the pause is due to the embedder, got:\n%s", body)
	}
	if !strings.Contains(body, reason) {
		t.Errorf("expected the status page to show the classified reason %q, got:\n%s", reason, body)
	}
	if strings.Contains(body, "paused by operator") {
		t.Errorf("expected the status page NOT to claim this is an operator pause, got:\n%s", body)
	}
}

// A plain operator Pause() must still render as "paused by operator", not
// be confused with an embedder pause -- the other half of the
// distinguishability requirement above.
func TestStatusPageRendersOperatorPauseDistinctly(t *testing.T) {
	fb := &fakeBackfill{stats: indexer.Stats{}, paused: true}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "paused by operator") {
		t.Errorf("expected the status page to say the pause is by the operator, got:\n%s", body)
	}
	if strings.Contains(body, "embedder unreachable") {
		t.Errorf("expected the status page NOT to claim an embedder pause for a plain operator Pause(), got:\n%s", body)
	}
}

// (Task 3 review round 2, spec §13.1 requirement 3.) The live lane's pause
// must be visible on the admin page at all -- and distinct from the
// backfill lane's, since the two can be paused independently, for
// independent reasons, at independent times. This asserts on the actual
// wiring (Server.view() reading s.live.Stats()), not merely that the
// template CAN render the field if handed it -- a regression that dropped
// s.live from the New()/view() plumbing entirely would still compile (a
// nil LiveLane renders as "not paused") and would be caught only by
// exercising a REAL, non-nil fakeLive through the full HTTP path, which is
// exactly what this does.
func TestStatusDistinguishesLiveLanePauseFromBackfillLane(t *testing.T) {
	const backfillReason = "embed/remote: request failed: dial tcp: connection refused"
	const liveReason = "embed/remote: request failed: read tcp: connection reset by peer"

	fb := &fakeBackfill{stats: indexer.Stats{
		EmbedderPaused:      true,
		EmbedderPauseReason: backfillReason,
	}}
	live := &fakeLive{stats: indexer.LiveStats{
		EmbedderPaused:      true,
		EmbedderPauseReason: liveReason,
	}}
	handler := newTestServerWithLive(t, fb, live)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var got struct {
		EmbedderPaused      bool   `json:"embedder_paused"`
		EmbedderPauseReason string `json:"embedder_pause_reason"`

		LiveEmbedderPaused      bool   `json:"live_embedder_paused"`
		LiveEmbedderPauseReason string `json:"live_embedder_pause_reason"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v, body = %s", err, rec.Body.String())
	}
	if !got.EmbedderPaused || got.EmbedderPauseReason != backfillReason {
		t.Errorf("expected the backfill lane's own pause/reason to come through unchanged, got paused=%v reason=%q",
			got.EmbedderPaused, got.EmbedderPauseReason)
	}
	if !got.LiveEmbedderPaused {
		t.Error("expected LiveEmbedderPaused = true -- the live lane's own pause state, wired in separately")
	}
	if got.LiveEmbedderPauseReason != liveReason {
		t.Errorf("LiveEmbedderPauseReason = %q, want %q -- the live lane's reason must not be conflated with the backfill lane's",
			got.LiveEmbedderPauseReason, liveReason)
	}

	htmlReq := httptest.NewRequest(http.MethodGet, "/", nil)
	htmlRec := httptest.NewRecorder()
	handler.ServeHTTP(htmlRec, htmlReq)
	body := htmlRec.Body.String()
	if !strings.Contains(body, "Backfill lane paused") || !strings.Contains(body, "Live lane paused") {
		t.Errorf("expected the status page to label the two lanes' pause rows separately, got:\n%s", body)
	}
	if !strings.Contains(body, backfillReason) {
		t.Errorf("expected the backfill lane's reason on the page, got:\n%s", body)
	}
	if !strings.Contains(body, liveReason) {
		t.Errorf("expected the live lane's OWN reason on the page (not just the backfill lane's), got:\n%s", body)
	}
}

// A pause that can never clear on its own (a mid-run model-identity
// change; spec §13.1) must be visibly distinct from an ordinary
// transient outage on the page, not just in the reason prose an operator
// would have to read to notice.
func TestStatusPageShowsRequiresRestartDistinctly(t *testing.T) {
	fb := &fakeBackfill{stats: indexer.Stats{
		EmbedderPaused:               true,
		EmbedderPauseReason:          "embed/remote: remote identity changed mid-run",
		EmbedderPauseRequiresRestart: true,
	}}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var got struct {
		EmbedderPauseRequiresRestart bool `json:"embedder_pause_requires_restart"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v, body = %s", err, rec.Body.String())
	}
	if !got.EmbedderPauseRequiresRestart {
		t.Error("expected embedder_pause_requires_restart = true in the JSON")
	}

	htmlReq := httptest.NewRequest(http.MethodGet, "/", nil)
	htmlRec := httptest.NewRecorder()
	handler.ServeHTTP(htmlRec, htmlReq)
	if body := htmlRec.Body.String(); !strings.Contains(body, "requires restart") {
		t.Errorf("expected the status page to visibly flag that this pause requires a restart, got:\n%s", body)
	}
}

// The counterpart: an ordinary transient outage (no identity change) must
// NOT show the requires-restart flag -- the two pause causes must not
// collapse into looking the same.
func TestStatusPageDoesNotShowRequiresRestartForPlainOutage(t *testing.T) {
	fb := &fakeBackfill{stats: indexer.Stats{
		EmbedderPaused:               true,
		EmbedderPauseReason:          "embed/remote: request failed: connection refused",
		EmbedderPauseRequiresRestart: false,
	}}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if body := rec.Body.String(); strings.Contains(body, "requires restart") {
		t.Errorf("expected no requires-restart flag for a plain transient outage, got:\n%s", body)
	}
}

func TestStatusPageRendersProgressFigure(t *testing.T) {
	fb := &fakeBackfill{stats: indexer.Stats{
		Indexed:          123,
		Remaining:        877,
		Workers:          2,
		ControllerReason: "batch latency and load average within bounds",
		ThroughputPerSec: 10,
	}}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "123") {
		t.Errorf("body does not contain the indexed count 123:\n%s", body)
	}
	if !strings.Contains(body, "1,000") && !strings.Contains(body, "1000") {
		t.Errorf("body does not contain the total (1000):\n%s", body)
	}
	if !strings.Contains(body, "batch latency and load average within bounds") {
		t.Errorf("body does not contain the controller's reason:\n%s", body)
	}
}

func TestPauseRejectsCrossOriginRequest(t *testing.T) {
	fb := &fakeBackfill{}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodPost, "/api/backfill/pause", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a cross-origin pause request", rec.Code)
	}
	fb.mu.Lock()
	paused := fb.paused
	fb.mu.Unlock()
	if paused {
		t.Errorf("a cross-origin request was allowed to pause the backfill")
	}
}

// TestBuildViewETAEdgeCases directly exercises buildView -- a pure function
// -- against the three cases the brief singled out for scrutiny (an unknown
// Remaining, a not-yet-established throughput, and Done overriding both)
// plus a normal in-progress case, none of which any other test in this file
// covered: they all go through the HTTP handlers with a single fixed Stats
// value, never varying Remaining/ThroughputPerSec/Done independently enough
// to hit these branches of buildView's own switch.
func TestBuildViewETAEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		stats       indexer.Stats
		wantETA     string
		wantTotal   int64
		wantPercent float64
	}{
		{
			name:        "remaining unknown (PendingEntryCount's documented -1 error sentinel)",
			stats:       indexer.Stats{Indexed: 10, Skipped: 1, Failed: 0, Remaining: -1, ThroughputPerSec: 5},
			wantETA:     "unknown",
			wantTotal:   -1,
			wantPercent: -1,
		},
		{
			name:        "throughput not yet established -- cannot derive an ETA even though Remaining is known",
			stats:       indexer.Stats{Indexed: 10, Skipped: 0, Failed: 0, Remaining: 90, ThroughputPerSec: 0},
			wantETA:     "unknown",
			wantTotal:   100,
			wantPercent: 10,
		},
		{
			name:        "done overrides throughput and remaining entirely",
			stats:       indexer.Stats{Indexed: 100, Skipped: 0, Failed: 0, Remaining: 0, ThroughputPerSec: 0, Done: true},
			wantETA:     "done",
			wantTotal:   100,
			wantPercent: 100,
		},
		{
			name:        "normal in-progress case renders a duration from remaining/throughput",
			stats:       indexer.Stats{Indexed: 100, Skipped: 0, Failed: 0, Remaining: 900, ThroughputPerSec: 5},
			wantETA:     "3m", // 900 remaining / 5 per sec = 180s = 3m
			wantTotal:   1000,
			wantPercent: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := buildView(tt.stats, indexer.LiveStats{}, indexer.RuntimeConfig{}, store.DatabaseMetrics{}, false)
			if v.ETA != tt.wantETA {
				t.Errorf("ETA = %q, want %q", v.ETA, tt.wantETA)
			}
			if v.Total != tt.wantTotal {
				t.Errorf("Total = %d, want %d", v.Total, tt.wantTotal)
			}
			if v.PercentComplete != tt.wantPercent {
				t.Errorf("PercentComplete = %v, want %v", v.PercentComplete, tt.wantPercent)
			}
		})
	}
}

func TestPauseWithGetMethodNotAllowed(t *testing.T) {
	fb := &fakeBackfill{}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodGet, "/api/backfill/pause", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// The runtime half of spec §9.2: POST /api/backfill/config carries a
// partial configuration change through to the lane and reports back what
// actually took effect (whole-branch fix wave, finding 1).
func TestConfigEndpointAppliesAPartialChange(t *testing.T) {
	fb := &fakeBackfill{cfg: indexer.RuntimeConfig{MinWorkers: 1, MaxWorkers: 2, BatchSize: 16, PageSize: 20}}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodPost, "/api/backfill/config",
		strings.NewReader(`{"window":"02:00-07:00","max_workers":3,"batch_size":8}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got indexer.RuntimeConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unable to decode the response: %v (body %s)", err, rec.Body.String())
	}
	if got.WindowStart != 2 || got.WindowEnd != 7 {
		t.Fatalf("expected the window 2-7 to be reported back, got %d-%d", got.WindowStart, got.WindowEnd)
	}
	if got.MaxWorkers != 3 || got.BatchSize != 8 {
		t.Fatalf("expected max_workers=3 and batch_size=8, got %d and %d", got.MaxWorkers, got.BatchSize)
	}

	patches := fb.appliedPatches()
	if len(patches) != 1 {
		t.Fatalf("expected exactly one ApplyConfig call, got %d", len(patches))
	}
	p := patches[0]
	if p.Window == nil || p.MaxWorkers == nil || p.BatchSize == nil {
		t.Fatalf("expected the three named fields to be present in the patch, got %+v", p)
	}
	// Fields the request left out must arrive as nil, so applying one
	// knob cannot clobber another.
	if p.MinWorkers != nil || p.PageSize != nil || p.PollInterval != nil || p.IdleResweepInterval != nil || p.LoadThreshold != nil {
		t.Fatalf("expected omitted fields to be absent from the patch, got %+v", p)
	}
}

func TestConfigEndpointRejectsAnUnparseableWindow(t *testing.T) {
	fb := &fakeBackfill{}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodPost, "/api/backfill/config", strings.NewReader(`{"window":"whenever"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unparseable window, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(fb.appliedPatches()) != 0 {
		t.Fatal("a rejected request must not reach the lane at all")
	}
}

func TestConfigEndpointRejectsAnEmptyPatch(t *testing.T) {
	fb := &fakeBackfill{}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodPost, "/api/backfill/config", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a patch that changes nothing, got %d", rec.Code)
	}
	if len(fb.appliedPatches()) != 0 {
		t.Fatal("an empty patch must not reach the lane")
	}
}

// The config endpoint changes operational state, so it carries the same
// Origin check pause/resume do.
func TestConfigEndpointRejectsCrossOriginRequest(t *testing.T) {
	fb := &fakeBackfill{}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodPost, "/api/backfill/config", strings.NewReader(`{"max_workers":8}`))
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a cross-origin config change, got %d", rec.Code)
	}
	if len(fb.appliedPatches()) != 0 {
		t.Fatal("a cross-origin request must never reach the lane")
	}
}

func TestConfigEndpointReportsCurrentConfiguration(t *testing.T) {
	fb := &fakeBackfill{cfg: indexer.RuntimeConfig{
		WindowStart: 22, WindowEnd: 6, MinWorkers: 1, MaxWorkers: 2, BatchSize: 16, PageSize: 20,
	}}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodGet, "/api/backfill/config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got indexer.RuntimeConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unable to decode: %v", err)
	}
	if got.WindowStart != 22 || got.WindowEnd != 6 {
		t.Fatalf("expected the lane's window 22-6, got %d-%d", got.WindowStart, got.WindowEnd)
	}
}

// The status page and its JSON both have to show the settings in force,
// or an operator cannot tell whether a change landed.
func TestStatusShowsTheConfigurationInForce(t *testing.T) {
	fb := &fakeBackfill{cfg: indexer.RuntimeConfig{
		WindowStart: 2, WindowEnd: 7, MinWorkers: 1, MaxWorkers: 2,
		BatchSize: 16, PageSize: 20, PollIntervalSeconds: 5, IdleResweepIntervalSecs: 900,
		MaxAllowedWorkersOnHost: 8, MinBatchSizeAllowed: 8, MaxBatchSizeAllowed: 32,
	}}
	handler := newTestServer(t, fb)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var view struct {
		Config      indexer.RuntimeConfig `json:"config"`
		WindowLabel string                `json:"window_label"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unable to decode: %v", err)
	}
	if view.WindowLabel != "02:00-07:00" {
		t.Fatalf("expected the window label 02:00-07:00, got %q", view.WindowLabel)
	}
	if view.Config.BatchSize != 16 {
		t.Fatalf("expected the batch size in the status JSON, got %d", view.Config.BatchSize)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"02:00-07:00", "Schedule window", "Batch size", "/api/backfill/config"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the status page to contain %q", want)
		}
	}
}

// (Task 4, spec §13.2.) The status page's database-size section must
// actually be wired to Server.metrics through view() and buildView, not
// merely renderable if handed the right statusView by hand — the same
// concern TestStatusDistinguishesLiveLanePauseFromBackfillLane raises for
// the live lane's own fields. This exercises a REAL, non-nil fakeMetrics
// through the full HTTP path (both /api/status and /) and asserts on
// specific values a hardcoded placeholder or a wrong-field mix-up would
// not reproduce.
func TestStatusPageRendersDatabaseMetrics(t *testing.T) {
	fb := &fakeBackfill{}
	metrics := &fakeMetrics{metrics: store.DatabaseMetrics{
		DatabaseSizeBytes:     117440512, // 112.0 MiB
		SearchSchemaSizeBytes: 16777216,  // 16.0 MiB
		HNSWIndexSizeBytes:    8388608,   // 8.0 MiB
		PassageCount:          5681,
		EntryCount:            436,
		PassagesPerEntry:      13.03,
		CountsAreEstimated:    true,
		DeadTuples:            1066,
	}}
	handler := newTestServerWithMetrics(t, fb, metrics)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var got struct {
		MetricsAvailable      bool    `json:"metrics_available"`
		DatabaseSizeBytes     int64   `json:"database_size_bytes"`
		SearchSchemaSizeBytes int64   `json:"search_schema_size_bytes"`
		HNSWIndexSizeBytes    int64   `json:"hnsw_index_size_bytes"`
		PassageCount          int64   `json:"passage_count"`
		EntryCount            int64   `json:"entry_count"`
		PassagesPerEntry      float64 `json:"passages_per_entry"`
		CountsAreEstimated    bool    `json:"counts_are_estimated"`
		DeadTuples            int64   `json:"dead_tuples_passages"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v, body = %s", err, rec.Body.String())
	}
	if !got.MetricsAvailable {
		t.Fatal("expected metrics_available = true with a real DatabaseMetricsSource")
	}
	if got.DatabaseSizeBytes != 117440512 || got.SearchSchemaSizeBytes != 16777216 || got.HNSWIndexSizeBytes != 8388608 {
		t.Errorf("size fields = %+v", got)
	}
	if got.PassageCount != 5681 || got.EntryCount != 436 {
		t.Errorf("count fields = %+v", got)
	}
	if got.PassagesPerEntry != 13.03 {
		t.Errorf("PassagesPerEntry = %v, want 13.03", got.PassagesPerEntry)
	}
	if !got.CountsAreEstimated {
		t.Error("expected counts_are_estimated = true")
	}
	if got.DeadTuples != 1066 {
		t.Errorf("DeadTuples = %d, want 1066", got.DeadTuples)
	}

	htmlReq := httptest.NewRequest(http.MethodGet, "/", nil)
	htmlRec := httptest.NewRecorder()
	handler.ServeHTTP(htmlRec, htmlReq)
	body := htmlRec.Body.String()
	for _, want := range []string{
		"112.0 MB",  // total database size
		"16.0 MB",   // search schema size
		"8.0 MB",    // HNSW index size specifically
		"5,681",     // passage count
		"436",       // entry count
		"13.03",     // passages per entry -- the ratio the task exists to surface
		"estimated", // counts are estimates, not exact
		"1,066",     // dead tuple count
		"VACUUM",    // the stale-VACUUM/HNSW-truncation warning
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected the status page to contain %q, got:\n%s", want, body)
		}
	}
}

// A nil DatabaseMetricsSource (no store wired in at all, e.g. a test
// harness) must render gracefully -- no panic, and no false zeroes
// presented as a real reading.
func TestStatusPageShowsMetricsUnavailableWithoutSource(t *testing.T) {
	fb := &fakeBackfill{}
	handler := newTestServer(t, fb) // metrics is nil here

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var got struct {
		MetricsAvailable bool `json:"metrics_available"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v, body = %s", err, rec.Body.String())
	}
	if got.MetricsAvailable {
		t.Fatal("expected metrics_available = false with no DatabaseMetricsSource")
	}

	htmlReq := httptest.NewRequest(http.MethodGet, "/", nil)
	htmlRec := httptest.NewRecorder()
	handler.ServeHTTP(htmlRec, htmlReq)
	if body := htmlRec.Body.String(); !strings.Contains(body, "unavailable") {
		t.Errorf("expected the status page to say the database metrics are unavailable, got:\n%s", body)
	}
}

// A DatabaseMetricsSource that errors (a transient database hiccup) must
// degrade the same way a nil one does -- MetricsAvailable false, not a
// false zero reading presented as real. This is the case that would slip
// through if view() ever stopped checking the error from
// s.metrics.DatabaseMetrics and just always reported dmOK=true.
func TestStatusPageShowsMetricsUnavailableOnError(t *testing.T) {
	fb := &fakeBackfill{}
	metrics := &fakeMetrics{err: errors.New("connection refused")}
	handler := newTestServerWithMetrics(t, fb, metrics)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var got struct {
		MetricsAvailable bool  `json:"metrics_available"`
		DeadTuples       int64 `json:"dead_tuples_passages"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v, body = %s", err, rec.Body.String())
	}
	if got.MetricsAvailable {
		t.Fatal("expected metrics_available = false when the metrics source errors")
	}
	if got.DeadTuples != 0 {
		t.Errorf("expected the zero value (not a stale reading) when the source errors, got DeadTuples=%d", got.DeadTuples)
	}
}
