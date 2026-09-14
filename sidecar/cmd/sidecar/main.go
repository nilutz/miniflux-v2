// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command sidecar runs the search sidecar: it connects to Postgres, runs
// migrations, constructs the ONNX embedder, and runs three things
// concurrently until it receives SIGINT or SIGTERM — the always-on live
// indexing lane, the throttled backfill lane, and the status/admin HTTP
// server (spec §9.4).
//
// This is the only place in the module that imports internal/embed/onnx —
// everything else depends only on the embed.Embedder interface, so that
// `go test ./internal/...` never needs to link the native ONNX Runtime /
// libtokenizers libraries this package requires. Build with -tags ORT (see
// the Makefile); without it, hugot silently falls back to a pure-Go
// backend roughly 10x slower, which is exactly what the startup log line
// below exists to catch.
package main // import "miniflux.app/v2/sidecar/cmd/sidecar"

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"miniflux.app/v2/sidecar/internal/embed/onnx"
	"miniflux.app/v2/sidecar/internal/indexer"
	"miniflux.app/v2/sidecar/internal/store"
	"miniflux.app/v2/sidecar/internal/web"
)

// liveLanePollInterval is how often the live lane checks for newly
// arrived entries. At a few hundred entries a day (spec §9.1) a few
// seconds is more than tight enough; it is not a value under load
// pressure the way the backfill lane's worker count is.
const liveLanePollInterval = 5 * time.Second

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "sidecar runs the Miniflux search sidecar: live indexing, throttled backfill, and the status/admin page.\n\n")
		fmt.Fprintf(os.Stderr, "Configuration is via environment variables:\n")
		fmt.Fprintf(os.Stderr, "  SIDECAR_DATABASE_URL   Postgres DSN (required)\n")
		fmt.Fprintf(os.Stderr, "  SIDECAR_MODEL_PATH     path to the quantized ONNX model file (required)\n")
		fmt.Fprintf(os.Stderr, "  SIDECAR_ONNX_LIB_DIR   directory containing the native ONNX Runtime library (optional)\n")
		fmt.Fprintf(os.Stderr, "  SIDECAR_ADMIN_ADDR     address for the status/admin HTTP server (optional, default %s)\n", web.DefaultAddr)
		fmt.Fprintf(os.Stderr, "\nBackfill throttle (spec §9.2; all optional, and all live-editable afterwards\n")
		fmt.Fprintf(os.Stderr, "via POST /api/backfill/config on the admin server):\n")
		fmt.Fprintf(os.Stderr, "  SIDECAR_BACKFILL_WINDOW        hours the backfill may run: \"always\" (default) or e.g. \"02:00-07:00\"\n")
		fmt.Fprintf(os.Stderr, "  SIDECAR_BACKFILL_MIN_WORKERS   worker floor (default %d)\n", indexer.DefaultControllerConfig().MinWorkers)
		fmt.Fprintf(os.Stderr, "  SIDECAR_BACKFILL_MAX_WORKERS   worker ceiling (default %d, host maximum %d)\n", indexer.DefaultControllerConfig().MaxWorkers, indexer.MaxAllowedWorkers())
		fmt.Fprintf(os.Stderr, "  SIDECAR_BACKFILL_LOAD_THRESHOLD  per-core 1-minute load average to back off at (default %.2f)\n", indexer.DefaultControllerConfig().LoadThreshold)
		fmt.Fprintf(os.Stderr, "  SIDECAR_BACKFILL_BATCH_SIZE    passages per forward pass, %d-%d (default %d)\n", indexer.MinBatchSize, indexer.MaxBatchSize, indexer.DefaultBatchSize)
		fmt.Fprintf(os.Stderr, "  SIDECAR_BACKFILL_PAGE_SIZE     entries fetched per batch (default %d)\n", indexer.DefaultBackfillPageSize)
		fmt.Fprintf(os.Stderr, "  SIDECAR_BACKFILL_POLL_INTERVAL idle poll interval, a Go duration (default %s)\n", indexer.DefaultBackfillPollInterval)
		fmt.Fprintf(os.Stderr, "  SIDECAR_BACKFILL_IDLE_RESWEEP  wait before re-sweeping a drained corpus, a Go duration (default %s)\n", indexer.DefaultIdleResweepInterval)
	}
	flag.Parse()

	if err := run(); err != nil {
		slog.Error("sidecar: fatal error", slog.Any("error", err))
		os.Exit(1)
	}
}

// config is the sidecar's environment-variable-driven configuration.
type config struct {
	databaseURL string
	modelPath   string
	onnxLibDir  string
	adminAddr   string

	// backfill is the startup half of spec §9.2's "three knobs, all
	// live-editable without a restart". Startup configuration and the
	// admin server's POST /api/backfill/config produce the same
	// indexer.ConfigPatch and go through the same validation, so the two
	// entry points cannot disagree about what a legal value is.
	backfill indexer.ConfigPatch
}

// loadConfig reads configuration from the environment. SIDECAR_DATABASE_URL
// and SIDECAR_MODEL_PATH are required; SIDECAR_ONNX_LIB_DIR is optional —
// ONNXConfig.ONNXLibraryDir is ignored when empty, and only matters on
// platforms (macOS) where the native library isn't found at hugot's
// Linux-only default search path. SIDECAR_ADMIN_ADDR is also optional and
// defaults to web.DefaultAddr (loopback-only): the status/admin page has
// no authentication, so binding it anywhere reachable off the local
// machine is an operator's deliberate override, never this binary's
// default.
func loadConfig() (config, error) {
	cfg := config{
		databaseURL: os.Getenv("SIDECAR_DATABASE_URL"),
		modelPath:   os.Getenv("SIDECAR_MODEL_PATH"),
		onnxLibDir:  os.Getenv("SIDECAR_ONNX_LIB_DIR"),
		adminAddr:   os.Getenv("SIDECAR_ADMIN_ADDR"),
	}
	if cfg.adminAddr == "" {
		cfg.adminAddr = web.DefaultAddr
	}

	var missing []string
	if cfg.databaseURL == "" {
		missing = append(missing, "SIDECAR_DATABASE_URL")
	}
	if cfg.modelPath == "" {
		missing = append(missing, "SIDECAR_MODEL_PATH")
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("sidecar: missing required environment variable(s): %v", missing)
	}

	patch, err := loadBackfillPatch()
	if err != nil {
		return config{}, err
	}
	cfg.backfill = patch

	return cfg, nil
}

// loadBackfillPatch reads the backfill throttle's environment variables
// into a patch. An unset variable leaves that setting at its default;
// a set but unparseable one is a startup error rather than a silently
// ignored line, because the whole point of these is that an operator who
// sets a nightly window gets a nightly window.
//
// Range checking is deliberately NOT done here — indexer.ApplyConfig owns
// it, so that the environment and the admin endpoint clamp identically.
func loadBackfillPatch() (indexer.ConfigPatch, error) {
	var patch indexer.ConfigPatch

	if raw, ok := os.LookupEnv("SIDECAR_BACKFILL_WINDOW"); ok {
		w, err := indexer.ParseWindow(raw)
		if err != nil {
			return patch, fmt.Errorf("sidecar: SIDECAR_BACKFILL_WINDOW: %w", err)
		}
		patch.Window = &w
	}

	intVars := []struct {
		name string
		dst  **int
	}{
		{"SIDECAR_BACKFILL_MIN_WORKERS", &patch.MinWorkers},
		{"SIDECAR_BACKFILL_MAX_WORKERS", &patch.MaxWorkers},
		{"SIDECAR_BACKFILL_BATCH_SIZE", &patch.BatchSize},
		{"SIDECAR_BACKFILL_PAGE_SIZE", &patch.PageSize},
	}
	for _, v := range intVars {
		raw, ok := os.LookupEnv(v.name)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return patch, fmt.Errorf("sidecar: %s must be a whole number, got %q", v.name, raw)
		}
		*v.dst = &n
	}

	if raw, ok := os.LookupEnv("SIDECAR_BACKFILL_LOAD_THRESHOLD"); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return patch, fmt.Errorf("sidecar: SIDECAR_BACKFILL_LOAD_THRESHOLD must be a number, got %q", raw)
		}
		patch.LoadThreshold = &f
	}

	durationVars := []struct {
		name string
		dst  **time.Duration
	}{
		{"SIDECAR_BACKFILL_POLL_INTERVAL", &patch.PollInterval},
		{"SIDECAR_BACKFILL_IDLE_RESWEEP", &patch.IdleResweepInterval},
	}
	for _, v := range durationVars {
		raw, ok := os.LookupEnv(v.name)
		if !ok {
			continue
		}
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return patch, fmt.Errorf("sidecar: %s must be a Go duration such as \"30m\", got %q", v.name, raw)
		}
		*v.dst = &d
	}

	return patch, nil
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	s, err := store.New(cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("sidecar: unable to open store: %w", err)
	}
	defer s.Close()

	if err := s.Ping(); err != nil {
		return fmt.Errorf("sidecar: unable to reach database: %w", err)
	}

	if err := s.Migrate(); err != nil {
		return fmt.Errorf("sidecar: unable to run migrations: %w", err)
	}

	embedder, err := onnx.NewONNX(onnx.ONNXConfig{
		ModelPath:      cfg.modelPath,
		ONNXLibraryDir: cfg.onnxLibDir,
	})
	if err != nil {
		return fmt.Errorf("sidecar: unable to create embedder: %w", err)
	}
	defer embedder.Close()

	// Logged unconditionally on every startup. In production, a line
	// reading "GoMLX" here instead of "ORT" is the only visible symptom
	// that this binary was built without -tags ORT and is running roughly
	// 10x slower than expected — see internal/embed/onnx/backend_noort.go.
	slog.Info("sidecar: embedder ready", slog.String("backend", onnx.BackendName()))

	ix := indexer.New(s, embedder)

	controller := indexer.NewController(indexer.DefaultControllerConfig())
	backfill := indexer.NewBackfill(ix, controller, indexer.BackfillConfig{})

	// Startup configuration goes through exactly the same validation and
	// clamping as a later admin-page change, and the result is logged: an
	// operator who asked for something out of range needs to see what
	// they actually got, not discover it 40 hours later.
	effective, err := backfill.ApplyConfig(cfg.backfill)
	if err != nil {
		return fmt.Errorf("sidecar: invalid backfill configuration: %w", err)
	}
	slog.Info("sidecar: backfill throttle configured",
		slog.String("window", effective.WindowDescription()),
		slog.Int("min_workers", effective.MinWorkers),
		slog.Int("max_workers", effective.MaxWorkers),
		slog.Float64("load_threshold_per_core", effective.LoadThreshold),
		slog.Int("batch_size", effective.BatchSize),
		slog.Int("page_size", effective.PageSize),
		slog.Float64("poll_interval_seconds", effective.PollIntervalSeconds),
		slog.Float64("idle_resweep_interval_seconds", effective.IdleResweepIntervalSecs),
	)

	adminServer, err := web.New(backfill)
	if err != nil {
		return fmt.Errorf("sidecar: unable to build admin server: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The live lane and the backfill lane are started together, every
	// process run, unconditionally — this is the coordination invariant
	// documented on RunLive and Backfill.start: RunLive only ever covers
	// entries created after THIS startup (its cursor seeds from the
	// current max entry id), so it is safe only because Backfill always
	// re-sweeps everything else from cursor 0 right here, on every start,
	// regardless of what any earlier run's Stats() reported. Backfill
	// keeps no cursor of its own across restarts and Start is never
	// skipped because a previous Done was seen (there is nowhere such a
	// thing is even recorded) — breaking either half of that would make
	// an entry created during a previous shutdown's window invisible to
	// both lanes, permanently and silently.
	//
	// All three goroutines share ctx, and any one of them returning an
	// error calls stop() to cancel it — signal.NotifyContext's stop both
	// unregisters the signal handler and cancels ctx (safe to call more
	// than once, so this races harmlessly with the deferred call above
	// and with SIGINT/SIGTERM arriving independently). Without this, an
	// early failure in just one lane (the admin port already in use, say)
	// would leave the healthy lanes running forever with nothing to
	// surface the failure short of reading logs — the process would never
	// exit non-zero, and an orchestrator would never know to restart it.
	var wg sync.WaitGroup
	errs := make(chan error, 3)

	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("sidecar: starting live indexing lane",
			slog.Duration("poll_interval", liveLanePollInterval),
		)
		if err := indexer.RunLive(ctx, ix, liveLanePollInterval); err != nil {
			errs <- fmt.Errorf("sidecar: live lane exited with error: %w", err)
			stop()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("sidecar: starting backfill lane")
		if err := backfill.Start(ctx); err != nil {
			errs <- fmt.Errorf("sidecar: backfill lane exited with error: %w", err)
			stop()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("sidecar: starting admin server", slog.String("addr", cfg.adminAddr))
		if err := adminServer.ListenAndServe(ctx, cfg.adminAddr); err != nil {
			errs <- fmt.Errorf("sidecar: admin server exited with error: %w", err)
			stop()
		}
	}()

	wg.Wait()
	close(errs)

	var firstErr error
	for err := range errs {
		slog.Error("sidecar: lane reported an error", slog.Any("error", err))
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}

	slog.Info("sidecar: shutdown complete")
	return nil
}
