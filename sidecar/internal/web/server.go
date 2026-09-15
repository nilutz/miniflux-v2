// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package web serves the sidecar's status and admin page (spec §9.4): a
// progress/ETA/throughput view over the backfill lane's Stats(), plus
// pause/resume control. Plain net/http and html/template, matching
// Miniflux's own no-framework style — no JS framework, no CSS toolkit.
//
// This package must never import internal/embed/onnx: it is exercised by
// `go test ./internal/...` without -tags ORT and without libtokenizers.a
// linked, and only cmd/sidecar is allowed to pull the native ONNX Runtime
// dependency in. It depends only on internal/indexer's Stats type and the
// small BackfillController interface below, which *indexer.Backfill
// satisfies without this package ever constructing one.
package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"miniflux.app/v2/sidecar/internal/indexer"
)

// DefaultAddr is the address the admin server binds by default. This page
// has no authentication and exposes operational control (pause/resume,
// and any future live-editable knob), so it binds to loopback only —
// never a wildcard or externally reachable address — unless an operator
// deliberately overrides it (cmd/sidecar's SIDECAR_ADMIN_ADDR).
const DefaultAddr = "127.0.0.1:8081"

//go:embed templates/status.html
var templateFS embed.FS

// BackfillController is the subset of *indexer.Backfill this server needs:
// a progress snapshot plus operator pause/resume control. Defined as an
// interface, rather than depending on *indexer.Backfill directly, so tests
// can inject a fake lane with no store, no embedder and no real Controller
// (see server_test.go's fakeBackfill) — this package's own suite must stay
// hermetic and must run without SIDECAR_DATABASE_URL or a native ONNX
// Runtime build.
type BackfillController interface {
	Stats() indexer.Stats
	Pause()
	Resume()

	// RuntimeConfig and ApplyConfig are spec §9.2's "three knobs, all
	// live-editable without a restart". They live behind the same
	// interface as pause/resume because they are the same kind of
	// operation: something an operator does to a running backfill from
	// this page, between batches, without stopping it.
	RuntimeConfig() indexer.RuntimeConfig
	ApplyConfig(indexer.ConfigPatch) (indexer.RuntimeConfig, error)
}

// Server is the sidecar's status and admin HTTP server (spec §9.4), and,
// since task 7, its read-only search HTTP API (spec §6.3-6.4, §7).
type Server struct {
	backfill BackfillController
	searcher SearchService
	entries  EntryLookup
	tmpl     *template.Template
	mux      *http.ServeMux
}

// New builds a Server over backfill (control endpoints), searcher
// (GET /api/search and /api/similar) and entries (loaded per result to
// build a highlighted search.BuildSnippet — see search_handlers.go). It
// parses the embedded status page template eagerly so a malformed
// template fails at startup, not on the first request.
//
// searcher and entries may be nil in tests that never exercise the search
// routes (see server_test.go, which only cares about the backfill control
// endpoints); cmd/sidecar always supplies both.
func New(backfill BackfillController, searcher SearchService, entries EntryLookup) (*Server, error) {
	tmpl, err := template.New("status.html").Funcs(template.FuncMap{
		"comma": commaInt,
	}).ParseFS(templateFS, "templates/status.html")
	if err != nil {
		return nil, fmt.Errorf("web: unable to parse status template: %w", err)
	}

	s := &Server{backfill: backfill, searcher: searcher, entries: entries, tmpl: tmpl}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/status", s.handleAPIStatus)
	mux.HandleFunc("POST /api/backfill/pause", s.handlePause)
	mux.HandleFunc("POST /api/backfill/resume", s.handleResume)
	mux.HandleFunc("GET /api/backfill/config", s.handleGetConfig)
	mux.HandleFunc("POST /api/backfill/config", s.handleSetConfig)

	// GET /api/search and GET /api/similar (search_handlers.go) do NOT
	// carry sameOriginOrNoOrigin's check — see that function's own doc
	// comment on handleSearch/handleSimilar for why a read-only endpoint
	// on this loopback-bound server does not need it.
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/similar", s.handleSimilar)
	s.mux = mux

	return s, nil
}

// Handler returns the server's http.Handler, for tests (httptest) and for
// wrapping in an *http.Server in production.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe runs the admin HTTP server on addr until ctx is cancelled,
// then shuts it down gracefully and returns nil — mirroring RunLive and
// Backfill.Start's own contract: cancellation is the expected, clean way to
// stop, not an error to propagate.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("web: server exited: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("web: graceful shutdown failed: %w", err)
		}
		<-errCh
		return nil
	}
}

// statusView is Stats() reshaped for JSON and template rendering: the raw
// counters plus the derived fields the admin page needs (spec §9.4) —
// Total and an ETA — which Stats() itself deliberately does not compute
// (it has no opinion on presentation; see indexer.Stats' own doc comment).
type statusView struct {
	Indexed          int64            `json:"indexed"`
	Skipped          int64            `json:"skipped"`
	Failed           int64            `json:"failed"`
	Remaining        int64            `json:"remaining"`
	Total            int64            `json:"total"` // -1 if Remaining is unknown (a failed PendingEntryCount)
	SkippedByReason  map[string]int64 `json:"skipped_by_reason"`
	FailedByReason   map[string]int64 `json:"failed_by_reason"`
	Workers          int              `json:"workers"`
	ControllerReason string           `json:"controller_reason"`
	ThroughputPerSec float64          `json:"throughput_per_sec"`
	Paused           bool             `json:"paused"` // an OPERATOR pause; see EmbedderPaused for the lane's own automatic one (spec §13.1)

	// EmbedderPaused/EmbedderPauseReason distinguish "paused: embedder
	// unreachable" from an operator's own "paused by operator" (spec
	// §13.1) — an operator whose backfill stopped needs to know whether
	// to look at the network or the data.
	EmbedderPaused      bool   `json:"embedder_paused"`
	EmbedderPauseReason string `json:"embedder_pause_reason"`

	Done            bool      `json:"done"`
	ETA             string    `json:"eta"`              // human-readable, "unknown" or "done"
	PercentComplete float64   `json:"percent_complete"` // -1 if Total is unknown
	GeneratedAt     time.Time `json:"generated_at"`

	Config       indexer.RuntimeConfig `json:"config"`
	WindowLabel  string                `json:"window_label"` // Config's window as an operator writes it
	PollInterval string                `json:"poll_interval"`
	IdleResweep  string                `json:"idle_resweep"`
}

// buildView derives a statusView from a Stats snapshot and the lane's
// current live-editable configuration.
func buildView(st indexer.Stats, cfg indexer.RuntimeConfig) statusView {
	v := statusView{
		Indexed:             st.Indexed,
		Skipped:             st.Skipped,
		Failed:              st.Failed,
		Remaining:           st.Remaining,
		SkippedByReason:     st.SkippedByReason,
		FailedByReason:      st.FailedByReason,
		Workers:             st.Workers,
		ControllerReason:    st.ControllerReason,
		ThroughputPerSec:    st.ThroughputPerSec,
		Paused:              st.Paused,
		EmbedderPaused:      st.EmbedderPaused,
		EmbedderPauseReason: st.EmbedderPauseReason,
		Done:                st.Done,
		GeneratedAt:         time.Now(),
		Total:               -1,
		PercentComplete:     -1,
		Config:              cfg,
		WindowLabel:         cfg.WindowDescription(),
		PollInterval:        formatSeconds(cfg.PollIntervalSeconds),
		IdleResweep:         formatSeconds(cfg.IdleResweepIntervalSecs),
	}
	if v.SkippedByReason == nil {
		v.SkippedByReason = map[string]int64{}
	}
	if v.FailedByReason == nil {
		v.FailedByReason = map[string]int64{}
	}

	if st.Remaining >= 0 {
		v.Total = st.Indexed + st.Skipped + st.Failed + st.Remaining
		if v.Total > 0 {
			v.PercentComplete = 100 * float64(st.Indexed+st.Skipped+st.Failed) / float64(v.Total)
		} else {
			v.PercentComplete = 100
		}
	}

	switch {
	case st.Done:
		v.ETA = "done"
	case st.Remaining < 0 || st.ThroughputPerSec <= 0:
		v.ETA = "unknown"
	default:
		remaining := time.Duration(float64(st.Remaining)/st.ThroughputPerSec) * time.Second
		v.ETA = formatDuration(remaining)
	}

	return v
}

// formatDuration renders d as a short, human-readable approximation (e.g.
// "3h12m", "41h") suitable for an ETA display — spec §9.3 puts real runs in
// the hours-to-days range, so seconds-level precision would be noise.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return "<1m"
	}
	h := int64(d / time.Hour)
	m := int64((d % time.Hour) / time.Minute)
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// commaInt renders n with thousands separators (e.g. 1000 -> "1,000") for
// the status page template — the only formatting the template itself needs
// that html/template's defaults don't already provide.
func commaInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// view builds the current status view. Stats() and RuntimeConfig() are
// read one after the other rather than under a shared lock; they are
// independent snapshots of a lane that is changing anyway, and nothing
// rendered from them is a consistency claim about a single instant.
func (s *Server) view() statusView {
	return buildView(s.backfill.Stats(), s.backfill.RuntimeConfig())
}

// formatSeconds renders a duration expressed in seconds the short way an
// operator writes it, e.g. "5s", "15m", "1h30m".
func formatSeconds(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
	return formatDuration(d)
}

// configRequest is POST /api/backfill/config's wire format. Every field is
// optional and a field left out changes nothing, so an operator editing
// one knob from the admin page does not have to restate the others and
// cannot clobber a change someone else made in between.
type configRequest struct {
	Window              *string  `json:"window"` // "always", "02:00-07:00", "22-06"
	MinWorkers          *int     `json:"min_workers"`
	MaxWorkers          *int     `json:"max_workers"`
	LoadThreshold       *float64 `json:"load_threshold"`
	BatchSize           *int     `json:"batch_size"`
	PageSize            *int     `json:"page_size"`
	PollIntervalSeconds *float64 `json:"poll_interval_seconds"`
	IdleResweepSeconds  *float64 `json:"idle_resweep_interval_seconds"`
}

// toPatch converts a decoded request into an indexer.ConfigPatch. Only the
// schedule window can fail to convert; every numeric knob is clamped by
// ApplyConfig rather than rejected.
func (req configRequest) toPatch() (indexer.ConfigPatch, error) {
	var patch indexer.ConfigPatch

	if req.Window != nil {
		w, err := indexer.ParseWindow(*req.Window)
		if err != nil {
			return patch, err
		}
		patch.Window = &w
	}
	patch.MinWorkers = req.MinWorkers
	patch.MaxWorkers = req.MaxWorkers
	patch.LoadThreshold = req.LoadThreshold
	patch.BatchSize = req.BatchSize
	patch.PageSize = req.PageSize
	if req.PollIntervalSeconds != nil {
		d := time.Duration(*req.PollIntervalSeconds * float64(time.Second))
		patch.PollInterval = &d
	}
	if req.IdleResweepSeconds != nil {
		d := time.Duration(*req.IdleResweepSeconds * float64(time.Second))
		patch.IdleResweepInterval = &d
	}
	return patch, nil
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.backfill.RuntimeConfig())
}

// handleSetConfig applies spec §9.2's live-editable knobs to the running
// lane. It carries the same Origin check as pause/resume — it is a
// state-changing request against an unauthenticated loopback service, and
// concurrency and schedule are exactly the settings an attacker would want
// to change.
//
// The response is the configuration as it actually stands afterwards, not
// an echo of the request: values outside the permitted ranges are clamped
// (see indexer.ApplyConfig), and an operator has to be able to see that
// happen.
func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOrNoOrigin(r) {
		http.Error(w, "cross-origin requests are not permitted", http.StatusForbidden)
		return
	}

	var req configRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("unable to decode the request body: %v", err), http.StatusBadRequest)
		return
	}

	patch, err := req.toPatch()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if patch.IsEmpty() {
		http.Error(w, "the request changed nothing: every field was absent", http.StatusBadRequest)
		return
	}

	applied, err := s.backfill.ApplyConfig(patch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("web: backfill configuration changed via admin page",
		slog.String("window", applied.WindowDescription()),
		slog.Int("min_workers", applied.MinWorkers),
		slog.Int("max_workers", applied.MaxWorkers),
		slog.Int("batch_size", applied.BatchSize),
		slog.Int("page_size", applied.PageSize),
	)

	writeJSON(w, applied)
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("web: unable to encode response", slog.Any("error", err))
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	view := s.view()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, view); err != nil {
		slog.Error("web: unable to render status page", slog.Any("error", err))
	}
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	view := s.view()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(view); err != nil {
		slog.Error("web: unable to encode status response", slog.Any("error", err))
	}
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOrNoOrigin(r) {
		http.Error(w, "cross-origin requests are not permitted", http.StatusForbidden)
		return
	}
	s.backfill.Pause()
	slog.Info("web: backfill paused via admin page")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.view())
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOrNoOrigin(r) {
		http.Error(w, "cross-origin requests are not permitted", http.StatusForbidden)
		return
	}
	s.backfill.Resume()
	slog.Info("web: backfill resumed via admin page")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.view())
}

// sameOriginOrNoOrigin reports whether r may be trusted as a same-origin
// request: true when it carries no Origin header at all (a plain curl/CLI
// request, or a browser navigation/form submission old enough to omit it),
// or when it does and that Origin's host matches the request's own Host.
//
// This page has no authentication (bind-to-localhost is its only real
// protection), but localhost binding alone does not stop a page open in
// the operator's own browser, on any origin, from firing a same-site
// "simple" POST at http://127.0.0.1:8081/api/backfill/pause — a classic
// local-service CSRF vector. The admin page's own inline fetch() calls
// (templates/status.html) always set Origin on state-changing requests
// per the Fetch spec, so this check costs those calls nothing, while a
// cross-origin page's request is rejected outright.
func sameOriginOrNoOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}
