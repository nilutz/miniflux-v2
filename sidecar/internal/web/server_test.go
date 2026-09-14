// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"miniflux.app/v2/sidecar/internal/indexer"
)

// fakeBackfill is a hermetic stand-in for *indexer.Backfill: it satisfies
// BackfillController without a store, an embedder, or a real Controller, so
// these tests never need SIDECAR_DATABASE_URL or a native ONNX Runtime
// library. Pause/Resume just flip a flag Stats() reflects, matching the
// contract the real Backfill.Pause/Resume/Stats documents.
type fakeBackfill struct {
	mu     sync.Mutex
	stats  indexer.Stats
	paused bool
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

func newTestServer(t *testing.T, fb *fakeBackfill) http.Handler {
	t.Helper()
	srv, err := New(fb)
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
