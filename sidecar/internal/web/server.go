// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package web serves the sidecar's status and admin page (spec §9.4): a
// progress/ETA/throughput view over the backfill lane's Stats(), plus
// pause/resume control, plus (spec §13.1) the live lane's own embedder-
// pause status. Plain net/http and html/template, matching Miniflux's own
// no-framework style — no JS framework, no CSS toolkit.
//
// This package must never import internal/embed/onnx: it is exercised by
// `go test ./internal/...` without -tags ORT and without libtokenizers.a
// linked, and only cmd/sidecar is allowed to pull the native ONNX Runtime
// dependency in. It depends only on internal/indexer's Stats/LiveStats
// types and the small BackfillController/LiveLane interfaces below, which
// *indexer.Backfill and *indexer.LiveMonitor satisfy without this package
// ever constructing either.
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
	"strings"
	"time"

	"miniflux.app/v2/sidecar/internal/indexer"
	"miniflux.app/v2/sidecar/internal/store"
)

// DefaultAddr is the address the admin server binds by default. This page
// exposes operational control (pause/resume, and any future live-editable
// knob) behind requireAdmin, but stays loopback-only by default anyway —
// defence in depth, not the only protection — never a wildcard or
// externally reachable address — unless an operator deliberately
// overrides it (cmd/sidecar's SIDECAR_ADMIN_ADDR; the production compose
// stack does this deliberately, to publish the port — see
// docker-compose.yaml's own comment on the sidecar service).
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

// LiveLane is the subset of the live lane's status the admin page needs
// (spec §13.1's requirement 3: the page must say which lane is paused and
// why). Unlike BackfillController, there is no Pause()/Resume() here — the
// live lane has no operator pause concept of its own, only the automatic
// embedder-outage one — and no progress/throughput snapshot, since the
// live lane has no backlog to page through. *indexer.LiveMonitor satisfies
// this without internal/web ever constructing one, the same pattern
// BackfillController already uses for *indexer.Backfill.
type LiveLane interface {
	Stats() indexer.LiveStats
}

// DatabaseMetricsSource is the subset of *store.Store the admin page needs
// for spec §13.2's database-size section: total database size, the
// search schema's own size, the HNSW index specifically, passage/entry
// counts and the dead-tuple count for search.passages, all in one round
// trip. Defined as an interface, not *store.Store directly, for the same
// reason BackfillController and LiveLane are: this package's own test
// suite must stay hermetic, with no real database, by injecting a fake
// (see server_test.go's fakeMetrics).
type DatabaseMetricsSource interface {
	DatabaseMetrics(ctx context.Context) (store.DatabaseMetrics, error)
}

// EmbedderManager is the subset of *indexer.Manager the admin page's Model
// section needs (spec §13.1): what is currently configured
// (Info), testing a candidate before committing (Preview), and applying
// a switch (Switch). Defined as an interface, not *indexer.Manager
// directly, for the same reason BackfillController/LiveLane/
// DatabaseMetricsSource are: this package's own test suite must stay
// hermetic, with no real store, embedder or native ONNX Runtime library,
// by injecting a fake (see server_test.go's fakeEmbedderManager).
type EmbedderManager interface {
	Info(ctx context.Context) indexer.EmbedderInfo
	Preview(ctx context.Context, kind, remoteURL string) indexer.SwitchPreview
	Switch(ctx context.Context, kind, remoteURL string, confirm bool) (indexer.SwitchResult, error)
}

// ArticleLookup is the subset of *store.Store that GET /api/article needs:
// one entry's full reader-facing content by id. Defined as an interface,
// not *store.Store directly, for the same hermetic-tests reason every
// other narrow interface in this file exists (see BackfillController's own
// doc comment) — this package's suite must run with no database.
// *store.Store satisfies it with no adaptation, the same way it already
// satisfies EntryLookup and DatabaseMetricsSource.
type ArticleLookup interface {
	EntryArticle(ctx context.Context, entryID, userID int64) (*store.ArticleDetail, error)
}

// Server is the sidecar's status and admin HTTP server (spec §9.4), its
// read-only search HTTP API (spec §6.3-6.4, §7), and GET /api/article —
// the full-content read the MCP server's fetch_article tool wraps.
//
// GET /api/search, /api/similar and /api/article require a valid Miniflux
// credential (requireAuthenticatedUser, auth.go) and scope their result to
// that credential's user. The credential may be either a Miniflux API key
// (X-Auth-Token) or a Miniflux web session cookie (MinifluxSessionID) — see
// auth.go's package doc comment and session_auth.go.
//
// The status page (GET /{$}), GET /api/status, and every control endpoint
// (backfill pause/resume/config, the embedder-switch endpoints) require
// requireAdmin (auth.go) rather than staying unauthenticated or accepting
// any valid credential: a Miniflux API key carries no admin/operator
// distinction of its own, but public.users.is_admin
// (internal/database/migrations.go's initial migration) is one join away
// from either credential's resolved user id, and gives these endpoints a
// real distinction to gate on. ANY valid credential authenticates a
// caller, but only one whose user is_admin may reach these operator
// surfaces (AdminChecker / store.Store.IsAdmin, read-only against
// public.users.is_admin). A valid non-admin credential gets 403, not 200;
// no credential at all gets 401; nothing here is optional or behind a
// flag.
//
// See auth_test.go's TestControlAndStatusEndpointsRequireAdmin, which pins
// this per-endpoint, per-case (no credential / non-admin / admin) so a
// later change cannot silently narrow it by accident.
type Server struct {
	backfill BackfillController
	live     LiveLane
	searcher SearchService
	entries  EntryLookup
	articles ArticleLookup
	metrics  DatabaseMetricsSource
	manager  EmbedderManager
	keys     APIKeyValidator
	sessions SessionValidator
	admins   AdminChecker
	tmpl     *template.Template
	mux      *http.ServeMux

	// serviceToken is the shared secret internal/searchclient (the
	// fork's own HTTP client for this sidecar) presents via
	// serviceTokenHeader (auth.go) -- empty by default, which disables
	// that credential entirely (see serviceTokenMatches: fail closed, not
	// "accept a matching empty header"). Set via SetServiceToken, not a
	// New() parameter: unlike every other field above (all interfaces
	// satisfied by *store.Store or similar, wired once at construction),
	// this is a plain secret cmd/sidecar reads from its own environment,
	// and giving it a dedicated setter keeps New()'s already-long
	// parameter list from growing for a field most callers (every test in
	// this package) never need to set at all.
	serviceToken string
}

// SetServiceToken configures the shared secret internal/searchclient
// authenticates with (see serviceTokenHeader and credentialService's own
// doc comments in auth.go). token == "" (New's default) disables the
// credential entirely -- fail closed, not "accept an empty header".
func (s *Server) SetServiceToken(token string) {
	s.serviceToken = token
}

// New builds a Server over backfill (control endpoints), live (the live
// lane's own pause status — spec §13.1), searcher (GET /api/search and
// /api/similar), entries (loaded per result to build a highlighted
// search.BuildSnippet — see search_handlers.go), articles (GET
// /api/article's full-content read — see article_handler.go), metrics (the
// database-size and health section — spec §13.2), manager (the Model
// section's embedder controls — spec §13.1), keys (the API key
// validator), sessions (the Miniflux web session cookie validator) and
// admins (the is_admin check guarding the status page and every control
// endpoint — see this type's own doc comment for which endpoints require
// which gate, and why). It parses the embedded status page template
// eagerly so a malformed template fails at startup, not on the first
// request.
//
// live, searcher, entries, articles, metrics, manager, keys, sessions and
// admins may all be nil in tests that don't care about what they cover
// (see server_test.go); cmd/sidecar always supplies all ten. A nil live
// renders as "not paused" — the zero value of indexer.LiveStats — rather
// than panicking; a nil (or erroring) metrics source renders the
// database-size section as unavailable rather than panicking or showing
// zeroes as if they were real (see view()); a nil manager renders the
// Model section as unavailable the same way (see modelView()); a nil keys
// or sessions fails every request presenting that credential kind closed
// with 500 (see resolveCredential in auth.go) rather than silently
// admitting every caller; a nil admins fails every request to an
// admin-gated route closed with 500 (see requireAdmin in auth.go) the
// same way, regardless of whether the credential itself was valid.
func New(backfill BackfillController, live LiveLane, searcher SearchService, entries EntryLookup, articles ArticleLookup, metrics DatabaseMetricsSource, manager EmbedderManager, keys APIKeyValidator, sessions SessionValidator, admins AdminChecker) (*Server, error) {
	tmpl, err := template.New("status.html").Funcs(template.FuncMap{
		"comma":    commaInt,
		"bytesize": formatBytes,
	}).ParseFS(templateFS, "templates/status.html")
	if err != nil {
		return nil, fmt.Errorf("web: unable to parse status template: %w", err)
	}

	s := &Server{backfill: backfill, live: live, searcher: searcher, entries: entries, articles: articles, metrics: metrics, manager: manager, keys: keys, sessions: sessions, admins: admins, tmpl: tmpl}

	mux := http.NewServeMux()
	// GET /{$} and every route below down to POST /api/embedder/switch are
	// admin-gated operator surfaces (requireAdmin, auth.go) — see this
	// type's own doc comment for why.
	mux.HandleFunc("GET /{$}", s.requireAdmin(s.handleIndex))
	mux.HandleFunc("GET /api/status", s.requireAdmin(s.handleAPIStatus))
	mux.HandleFunc("POST /api/backfill/pause", s.requireAdmin(s.handlePause))
	mux.HandleFunc("POST /api/backfill/resume", s.requireAdmin(s.handleResume))
	mux.HandleFunc("GET /api/backfill/config", s.requireAdmin(s.handleGetConfig))
	mux.HandleFunc("POST /api/backfill/config", s.requireAdmin(s.handleSetConfig))

	mux.HandleFunc("GET /api/embedder", s.requireAdmin(s.handleGetEmbedder))
	mux.HandleFunc("POST /api/embedder/preview", s.requireAdmin(s.handleEmbedderPreview))
	mux.HandleFunc("POST /api/embedder/switch", s.requireAdmin(s.handleEmbedderSwitch))

	// GET /api/search, GET /api/similar (search_handlers.go) and GET
	// /api/article (article_handler.go) do NOT carry sameOriginOrNoOrigin's
	// check — see handleSearch/handleSimilar's own doc comment for why a
	// read-only endpoint on this loopback-bound server does not need it;
	// handleArticle is the same shape of endpoint for the same reason. They
	// DO carry requireAuthenticatedUser (auth.go) — these are the
	// sidecar's data endpoints, gated on ANY valid credential with no
	// admin requirement, unlike the routes above.
	mux.HandleFunc("GET /api/search", s.requireAuthenticatedUser(s.handleSearch))
	mux.HandleFunc("GET /api/similar", s.requireAuthenticatedUser(s.handleSimilar))
	mux.HandleFunc("GET /api/article", s.requireAuthenticatedUser(s.handleArticle))
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
	// to look at the network or the data. These three all describe the
	// BACKFILL lane specifically — see LiveEmbedderPaused etc. below for
	// the live lane's own, entirely separate, state.
	EmbedderPaused      bool   `json:"embedder_paused"`
	EmbedderPauseReason string `json:"embedder_pause_reason"`

	// EmbedderPauseRequiresRestart is true when the backfill lane's
	// current embedder pause can never clear on its own (the remote
	// changed identity mid-run; spec §13.1) — as opposed to an ordinary
	// transient outage, which resolves once the network/remote recovers
	// with no operator action. Distinct from the reason PROSE so the page
	// can render "wait" vs. "go restart the sidecar" as a fact to scan,
	// not a sentence to parse.
	EmbedderPauseRequiresRestart bool `json:"embedder_pause_requires_restart"`

	// LiveEmbedderPaused/LiveEmbedderPauseReason/
	// LiveEmbedderPauseRequiresRestart mirror the three fields above, but
	// for the LIVE lane (indexer.RunLive) rather than the backfill lane.
	// They are deliberately named and rendered separately, never folded
	// into the backfill fields above or into one shared boolean: the two
	// lanes can be paused independently, for independent reasons, at
	// independent times, and an operator reading this page must be able
	// to tell which lane stopped without reading logs (spec §13.1,
	// requirement 3 — "the live lane behaves the same way [...] the page
	// must say why").
	LiveEmbedderPaused               bool   `json:"live_embedder_paused"`
	LiveEmbedderPauseReason          string `json:"live_embedder_pause_reason"`
	LiveEmbedderPauseRequiresRestart bool   `json:"live_embedder_pause_requires_restart"`

	// LivePaused mirrors Paused above, but for the live lane (it has an
	// operator Pause()/Resume() too, so Manager.Switch can quiesce
	// both lanes the same way) rather than the backfill lane.
	LivePaused bool `json:"live_paused"`

	Done            bool      `json:"done"`
	ETA             string    `json:"eta"`              // human-readable, "unknown" or "done"
	PercentComplete float64   `json:"percent_complete"` // -1 if Total is unknown
	GeneratedAt     time.Time `json:"generated_at"`

	Config       indexer.RuntimeConfig `json:"config"`
	WindowLabel  string                `json:"window_label"` // Config's window as an operator writes it
	PollInterval string                `json:"poll_interval"`
	IdleResweep  string                `json:"idle_resweep"`

	// MetricsAvailable is false when the Server was built with a nil
	// DatabaseMetricsSource, or the last read of it failed (a transient
	// database hiccup, say) — the page must say "unavailable" rather than
	// render zeroes as if they were a real, current database-size reading
	// (see view()). The fields below are the zero DatabaseMetrics when
	// this is false.
	MetricsAvailable bool `json:"metrics_available"`

	// DatabaseSizeBytes, SearchSchemaSizeBytes and HNSWIndexSizeBytes are
	// spec §13.2's size numbers: the whole database, the search schema's
	// own share, and the HNSW vector index specifically — called out on
	// its own because it grows fastest and the spec's own §5.5 estimate
	// folded it into a table total.
	DatabaseSizeBytes     int64 `json:"database_size_bytes"`
	SearchSchemaSizeBytes int64 `json:"search_schema_size_bytes"`
	HNSWIndexSizeBytes    int64 `json:"hnsw_index_size_bytes"`

	// PassageCount, EntryCount and PassagesPerEntry are the numbers spec
	// §13.2 exists to surface: the measured corpus is 13 passages per
	// entry, not the 4-6 §5.5 assumed, which is what made §5.5's disk
	// estimate wrong by roughly 5x. CountsAreEstimated is true whenever
	// PassageCount/EntryCount come from pg_class.reltuples rather than an
	// exact count (see store.DatabaseMetrics' own doc comment for why) —
	// always true today — and the page must render the word "estimated"
	// next to them when it is, rather than presenting an approximation as
	// an exact count.
	PassageCount       int64   `json:"passage_count"`
	EntryCount         int64   `json:"entry_count"`
	PassagesPerEntry   float64 `json:"passages_per_entry"`
	CountsAreEstimated bool    `json:"counts_are_estimated"`

	// PassageCountUnknown/EntryCountUnknown/PassagesPerEntryUnknown mirror
	// store.DatabaseMetrics' own fields of the same name: pg_class.reltuples
	// is -1 for a relation Postgres has never ANALYZEd, which
	// search.passages hits for real right at the start of a fresh backfill
	// -- exactly when an operator is most likely watching. Without these, a
	// 0 rendered here is indistinguishable from "the backfill is producing
	// nothing"; the template must check these before rendering a number at
	// all.
	PassageCountUnknown     bool `json:"passage_count_unknown"`
	EntryCountUnknown       bool `json:"entry_count_unknown"`
	PassagesPerEntryUnknown bool `json:"passages_per_entry_unknown"`

	// DeadTuples is search.passages' own dead-tuple count
	// (pg_stat_user_tables.n_dead_tup). Not routine: a stale VACUUM
	// silently truncates HNSW index scans, and in this project 1,066
	// dead tuples once produced a constant 31-row deficit in search
	// results with no error anywhere. Read live on every render (see
	// view()'s own doc comment for why no caching is needed here), so an
	// operator watching this number while they run VACUUM sees it change.
	DeadTuples int64 `json:"dead_tuples_passages"`
}

// buildView derives a statusView from the backfill lane's Stats snapshot,
// the live lane's own LiveStats snapshot, the backfill lane's current
// live-editable configuration (the live lane has no configuration of its
// own to show), and the database-size/health numbers (spec §13.2)
// dmOK reports whether dm is a genuine, freshly-read reading (false when
// there was no DatabaseMetricsSource or the last read of it failed) — the
// zero DatabaseMetrics is indistinguishable from "everything is really
// zero", so this cannot be inferred from dm alone.
func buildView(st indexer.Stats, liveSt indexer.LiveStats, cfg indexer.RuntimeConfig, dm store.DatabaseMetrics, dmOK bool) statusView {
	v := statusView{
		Indexed:                      st.Indexed,
		Skipped:                      st.Skipped,
		Failed:                       st.Failed,
		Remaining:                    st.Remaining,
		SkippedByReason:              st.SkippedByReason,
		FailedByReason:               st.FailedByReason,
		Workers:                      st.Workers,
		ControllerReason:             st.ControllerReason,
		ThroughputPerSec:             st.ThroughputPerSec,
		Paused:                       st.Paused,
		EmbedderPaused:               st.EmbedderPaused,
		EmbedderPauseReason:          st.EmbedderPauseReason,
		EmbedderPauseRequiresRestart: st.EmbedderPauseRequiresRestart,

		LiveEmbedderPaused:               liveSt.EmbedderPaused,
		LiveEmbedderPauseReason:          liveSt.EmbedderPauseReason,
		LiveEmbedderPauseRequiresRestart: liveSt.EmbedderPauseRequiresRestart,
		LivePaused:                       liveSt.Paused,

		Done:            st.Done,
		GeneratedAt:     time.Now(),
		Total:           -1,
		PercentComplete: -1,
		Config:          cfg,
		WindowLabel:     cfg.WindowDescription(),
		PollInterval:    formatSeconds(cfg.PollIntervalSeconds),
		IdleResweep:     formatSeconds(cfg.IdleResweepIntervalSecs),

		MetricsAvailable:        dmOK,
		DatabaseSizeBytes:       dm.DatabaseSizeBytes,
		SearchSchemaSizeBytes:   dm.SearchSchemaSizeBytes,
		HNSWIndexSizeBytes:      dm.HNSWIndexSizeBytes,
		PassageCount:            dm.PassageCount,
		EntryCount:              dm.EntryCount,
		PassagesPerEntry:        dm.PassagesPerEntry,
		CountsAreEstimated:      dm.CountsAreEstimated,
		PassageCountUnknown:     dm.PassageCountUnknown,
		EntryCountUnknown:       dm.EntryCountUnknown,
		PassagesPerEntryUnknown: dm.PassagesPerEntryUnknown,
		DeadTuples:              dm.DeadTuples,
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

// formatBytes renders n bytes as a short human-readable size (e.g.
// "112.4 MB", "1.8 GB") for the status page's database-size section (spec
// §13.2) — the numbers involved range from kilobytes to the hundreds-of-
// gigabytes spec §13.2 itself extrapolates to, and a raw byte count is not
// something an operator scans at a glance at that range.
func formatBytes(n int64) string {
	const unit = 1024.0
	if n < 0 {
		return fmt.Sprintf("-%s", formatBytes(-n))
	}
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := float64(unit), 0
	for f := float64(n) / unit; f >= unit; f /= unit {
		div *= unit
		exp++
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	return fmt.Sprintf("%.1f %s", float64(n)/div, units[exp])
}

// view builds the current status view. Stats(), the live lane's Stats()
// and RuntimeConfig() are read one after the other rather than under a
// shared lock; they are independent snapshots of lanes that are changing
// anyway, and nothing rendered from them is a consistency claim about a
// single instant.
//
// The database-size/health numbers (spec §13.2) are read fresh here, on
// every call, with no caching or TTL: unlike store.PendingEntryCount —
// which this project already had to fix exactly this mistake for once,
// by caching it behind a TTL (see indexer.Backfill.cachedRemaining) —
// every primitive store.DatabaseMetrics reads is a catalog or statistics
// lookup (pg_database_size, pg_total_relation_size, pg_relation_size,
// pg_class.reltuples, pg_stat_user_tables.n_dead_tup) whose cost does not
// grow with the size of entries or search.passages. Measured with EXPLAIN
// (ANALYZE, BUFFERS) against a live corpus, the combined query runs in
// single-digit milliseconds — safely inside the 10-second refresh this
// page uses (spec §9.4) with nothing memoised. A
// failed read (a transient database hiccup) is logged and rendered as
// "unavailable" rather than as a false zero.
func (s *Server) view(ctx context.Context) statusView {
	var liveSt indexer.LiveStats
	if s.live != nil {
		liveSt = s.live.Stats()
	}

	var dm store.DatabaseMetrics
	dmOK := false
	if s.metrics != nil {
		m, err := s.metrics.DatabaseMetrics(ctx)
		if err != nil {
			slog.Error("web: unable to read database metrics for the status page", slog.Any("error", err))
		} else {
			dm = m
			dmOK = true
		}
	}

	return buildView(s.backfill.Stats(), liveSt, s.backfill.RuntimeConfig(), dm, dmOK)
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
// state-changing, admin-gated request, and concurrency and schedule are
// exactly the settings an attacker who somehow reached this far would want
// to change; sameOriginOrNoOrigin is defence in depth on top of
// requireAdmin, not a substitute for it (see that function's own doc
// comment).
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

// modelView is the admin page's Model section (spec §13.1), derived from
// EmbedderManager.Info. Available is false when the Server
// was built with a nil EmbedderManager -- the section must say so rather
// than render the zero EmbedderInfo as if it were a real reading, the
// same degrade-honestly rule MetricsAvailable already follows for the
// Database section: a section whose data is unavailable must say so
// rather than render an empty panel.
type modelView struct {
	Available bool
	indexer.EmbedderInfo
}

// modelInfoView builds the Model section's view, or the unavailable zero
// value when s.manager is nil.
func (s *Server) modelInfoView(ctx context.Context) modelView {
	if s.manager == nil {
		return modelView{}
	}
	return modelView{Available: true, EmbedderInfo: s.manager.Info(ctx)}
}

// pageView is what the template actually renders: the existing
// status/database/indexing view (Status, unchanged in shape -- GET
// /api/status still serves it directly, see handleAPIStatus) plus the
// Model section. Kept as a separate top-level field rather than folding
// Model's fields into statusView so that /api/status' existing JSON shape
// does not change.
type pageView struct {
	Status statusView
	Model  modelView
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	view := pageView{Status: s.view(r.Context()), Model: s.modelInfoView(r.Context())}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, view); err != nil {
		slog.Error("web: unable to render status page", slog.Any("error", err))
	}
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	view := s.view(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(view); err != nil {
		slog.Error("web: unable to encode status response", slog.Any("error", err))
	}
}

// embedderRequest is POST /api/embedder/preview and .../switch's shared
// wire format. Confirm is ignored by preview -- the preview probe never
// swaps anything regardless of what a caller sends -- and required true by
// switch ("the switch must not proceed without explicit confirmation"),
// enforced again server-side by indexer.Manager.Switch itself, not only
// here.
type embedderRequest struct {
	Kind      string `json:"kind"`
	RemoteURL string `json:"remote_url"`
	Confirm   bool   `json:"confirm"`
}

// decodeEmbedderRequest reads and validates the shared request shape for
// both embedder endpoints: kind must be "local" or "remote", and a
// "remote" kind requires a non-empty URL. Returns the normalised
// (lower-cased, trimmed) kind, the trimmed URL and the raw Confirm field,
// or writes a 400 response and reports false.
func decodeEmbedderRequest(w http.ResponseWriter, r *http.Request) (kind, remoteURL string, confirm, ok bool) {
	var req embedderRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("unable to decode the request body: %v", err), http.StatusBadRequest)
		return "", "", false, false
	}
	kind = strings.ToLower(strings.TrimSpace(req.Kind))
	if kind != "local" && kind != "remote" {
		http.Error(w, `"kind" must be "local" or "remote"`, http.StatusBadRequest)
		return "", "", false, false
	}
	remoteURL = strings.TrimSpace(req.RemoteURL)
	if kind == "remote" && remoteURL == "" {
		http.Error(w, `"remote_url" is required when "kind" is "remote"`, http.StatusBadRequest)
		return "", "", false, false
	}
	return kind, remoteURL, req.Confirm, true
}

// handleGetEmbedder serves the Model section's current state as JSON --
// the same data handleIndex renders into the page, for a caller that
// wants it directly.
func (s *Server) handleGetEmbedder(w http.ResponseWriter, r *http.Request) {
	if s.manager == nil {
		http.Error(w, "no embedder manager configured", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, s.manager.Info(r.Context()))
}

// handleEmbedderPreview probes a candidate embedder and, for a reachable
// one, adds the re-index cost estimate before committing to anything. It
// carries the same Origin check as pause/resume/config: a state-CHANGING
// request would need it for CSRF, and although Preview itself swaps
// nothing, it does make an outbound network request to whatever URL the
// caller supplies -- treating it like the other POST endpoints costs
// nothing and avoids this becoming an exception an operator has to
// remember.
func (s *Server) handleEmbedderPreview(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOrNoOrigin(r) {
		http.Error(w, "cross-origin requests are not permitted", http.StatusForbidden)
		return
	}
	if s.manager == nil {
		http.Error(w, "no embedder manager configured", http.StatusServiceUnavailable)
		return
	}
	kind, remoteURL, _, ok := decodeEmbedderRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, buildPreviewView(s.manager.Preview(r.Context(), kind, remoteURL)))
}

// previewView reshapes indexer.SwitchPreview for JSON/template rendering
// the same way statusView reshapes indexer.Stats: EstimatedDurationLabel
// is the human-readable rendering of EstimatedDuration (formatDuration,
// the same helper the ETA row already uses) -- indexer.SwitchPreview
// itself carries only the raw time.Duration, since presentation is not
// that package's concern.
type previewView struct {
	indexer.SwitchPreview
	EstimatedDurationLabel string `json:"estimated_duration_label"`
}

func buildPreviewView(p indexer.SwitchPreview) previewView {
	label := ""
	if p.Error == "" && !p.SameIdentity && !p.EntryCountUnknown {
		label = formatDuration(p.EstimatedDuration)
	}
	return previewView{SwitchPreview: p, EstimatedDurationLabel: label}
}

// handleEmbedderSwitch applies an operator's embedder choice. confirm=false
// is rejected with 428 Precondition Required rather than silently
// no-oping, so a client bug that drops the field fails loudly instead of
// quietly doing nothing.
func (s *Server) handleEmbedderSwitch(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOrNoOrigin(r) {
		http.Error(w, "cross-origin requests are not permitted", http.StatusForbidden)
		return
	}
	if s.manager == nil {
		http.Error(w, "no embedder manager configured", http.StatusServiceUnavailable)
		return
	}
	kind, remoteURL, confirm, ok := decodeEmbedderRequest(w, r)
	if !ok {
		return
	}

	result, err := s.manager.Switch(r.Context(), kind, remoteURL, confirm)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, indexer.ErrSwitchNotConfirmed) {
			status = http.StatusPreconditionRequired
		}
		http.Error(w, err.Error(), status)
		return
	}

	slog.Info("web: embedder switched via admin page",
		slog.String("kind", kind),
		slog.String("old_identity", result.OldIdentity),
		slog.String("new_identity", result.NewIdentity),
		slog.Bool("same_identity", result.SameIdentity),
	)

	writeJSON(w, result)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOrNoOrigin(r) {
		http.Error(w, "cross-origin requests are not permitted", http.StatusForbidden)
		return
	}
	s.backfill.Pause()
	slog.Info("web: backfill paused via admin page")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.view(r.Context()))
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOrNoOrigin(r) {
		http.Error(w, "cross-origin requests are not permitted", http.StatusForbidden)
		return
	}
	s.backfill.Resume()
	slog.Info("web: backfill resumed via admin page")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.view(r.Context()))
}

// sameOriginOrNoOrigin reports whether r may be trusted as a same-origin
// request: true when it carries no Origin header at all (a plain curl/CLI
// request, or a browser navigation/form submission old enough to omit it),
// or when it does and that Origin's host matches the request's own Host.
//
// These routes also require requireAdmin (auth.go) — but that alone does
// not stop a page open in the operator's own browser, on any origin, from
// firing a same-site "simple" POST at
// http://127.0.0.1:8081/api/backfill/pause and having the browser attach
// the operator's own MinifluxSessionID cookie automatically: a classic
// CSRF vector, and especially relevant given that a browser cookie is one
// of the two credentials these routes accept. (SameSite=Lax already
// blocks the cookie on a genuinely cross-SITE POST in current browsers —
// see internal/ui/auth.go's setSessionCookie — but this check is defence
// in depth on top of that, not a replacement for it, the same way it is
// defence in depth on top of loopback binding.) The admin page's own
// inline fetch() calls (templates/status.html) always set Origin on
// state-changing requests per the Fetch spec, so this check costs those
// calls nothing, while a cross-origin page's request is rejected outright.
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
