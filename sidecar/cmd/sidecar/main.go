// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command sidecar runs the search sidecar's always-on live indexing lane:
// it connects to Postgres, runs migrations, constructs the ONNX embedder,
// and indexes newly arrived entries until it receives SIGINT or SIGTERM.
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
	"syscall"
	"time"

	"miniflux.app/v2/sidecar/internal/embed/onnx"
	"miniflux.app/v2/sidecar/internal/indexer"
	"miniflux.app/v2/sidecar/internal/store"
)

// liveLanePollInterval is how often the live lane checks for newly
// arrived entries. At a few hundred entries a day (spec §9.1) a few
// seconds is more than tight enough; it is not a value under load
// pressure the way the backfill lane's worker count is.
const liveLanePollInterval = 5 * time.Second

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "sidecar runs the Miniflux search sidecar's live indexing lane.\n\n")
		fmt.Fprintf(os.Stderr, "Configuration is via environment variables:\n")
		fmt.Fprintf(os.Stderr, "  SIDECAR_DATABASE_URL   Postgres DSN (required)\n")
		fmt.Fprintf(os.Stderr, "  SIDECAR_MODEL_PATH     path to the quantized ONNX model file (required)\n")
		fmt.Fprintf(os.Stderr, "  SIDECAR_ONNX_LIB_DIR   directory containing the native ONNX Runtime library (optional)\n")
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
}

// loadConfig reads configuration from the environment. SIDECAR_DATABASE_URL
// and SIDECAR_MODEL_PATH are required; SIDECAR_ONNX_LIB_DIR is optional —
// ONNXConfig.ONNXLibraryDir is ignored when empty, and only matters on
// platforms (macOS) where the native library isn't found at hugot's
// Linux-only default search path.
func loadConfig() (config, error) {
	cfg := config{
		databaseURL: os.Getenv("SIDECAR_DATABASE_URL"),
		modelPath:   os.Getenv("SIDECAR_MODEL_PATH"),
		onnxLibDir:  os.Getenv("SIDECAR_ONNX_LIB_DIR"),
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

	return cfg, nil
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("sidecar: starting live indexing lane",
		slog.Duration("poll_interval", liveLanePollInterval),
	)
	if err := indexer.RunLive(ctx, ix, liveLanePollInterval); err != nil {
		return fmt.Errorf("sidecar: live lane exited with error: %w", err)
	}

	slog.Info("sidecar: shutdown complete")
	return nil
}
