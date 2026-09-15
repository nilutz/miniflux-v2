// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main // import "miniflux.app/v2/sidecar/cmd/sidecar"

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The startup half of spec §9.2's live-editable knobs. Range checking is
// indexer.ApplyConfig's job — this is only about reading the environment
// faithfully, and about a typo being a startup error rather than a
// silently ignored line.
func TestLoadBackfillPatchReadsTheEnvironment(t *testing.T) {
	t.Setenv("SIDECAR_BACKFILL_WINDOW", "02:00-07:00")
	t.Setenv("SIDECAR_BACKFILL_MIN_WORKERS", "1")
	t.Setenv("SIDECAR_BACKFILL_MAX_WORKERS", "2")
	t.Setenv("SIDECAR_BACKFILL_LOAD_THRESHOLD", "0.75")
	t.Setenv("SIDECAR_BACKFILL_BATCH_SIZE", "8")
	t.Setenv("SIDECAR_BACKFILL_PAGE_SIZE", "40")
	t.Setenv("SIDECAR_BACKFILL_POLL_INTERVAL", "10s")
	t.Setenv("SIDECAR_BACKFILL_IDLE_RESWEEP", "30m")

	patch, err := loadBackfillPatch()
	if err != nil {
		t.Fatalf("loadBackfillPatch failed: %v", err)
	}

	if patch.Window == nil || patch.Window.Start != 2 || patch.Window.End != 7 {
		t.Fatalf("expected the window 2-7, got %+v", patch.Window)
	}
	if patch.MinWorkers == nil || *patch.MinWorkers != 1 {
		t.Fatalf("expected MinWorkers 1, got %v", patch.MinWorkers)
	}
	if patch.MaxWorkers == nil || *patch.MaxWorkers != 2 {
		t.Fatalf("expected MaxWorkers 2, got %v", patch.MaxWorkers)
	}
	if patch.LoadThreshold == nil || *patch.LoadThreshold != 0.75 {
		t.Fatalf("expected LoadThreshold 0.75, got %v", patch.LoadThreshold)
	}
	if patch.BatchSize == nil || *patch.BatchSize != 8 {
		t.Fatalf("expected BatchSize 8, got %v", patch.BatchSize)
	}
	if patch.PageSize == nil || *patch.PageSize != 40 {
		t.Fatalf("expected PageSize 40, got %v", patch.PageSize)
	}
	if patch.PollInterval == nil || *patch.PollInterval != 10*time.Second {
		t.Fatalf("expected a 10s poll interval, got %v", patch.PollInterval)
	}
	if patch.IdleResweepInterval == nil || *patch.IdleResweepInterval != 30*time.Minute {
		t.Fatalf("expected a 30m idle re-sweep interval, got %v", patch.IdleResweepInterval)
	}
}

func TestLoadBackfillPatchIsEmptyWhenNothingIsSet(t *testing.T) {
	// t.Setenv registers the restore; os.Unsetenv then makes the variable
	// genuinely absent for the duration of this test, which is what
	// "nothing is set" means to os.LookupEnv.
	for _, name := range []string{
		"SIDECAR_BACKFILL_WINDOW", "SIDECAR_BACKFILL_MIN_WORKERS", "SIDECAR_BACKFILL_MAX_WORKERS",
		"SIDECAR_BACKFILL_LOAD_THRESHOLD", "SIDECAR_BACKFILL_BATCH_SIZE", "SIDECAR_BACKFILL_PAGE_SIZE",
		"SIDECAR_BACKFILL_POLL_INTERVAL", "SIDECAR_BACKFILL_IDLE_RESWEEP",
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unable to unset %s: %v", name, err)
		}
	}

	patch, err := loadBackfillPatch()
	if err != nil {
		t.Fatalf("loadBackfillPatch failed: %v", err)
	}
	if !patch.IsEmpty() {
		t.Fatalf("expected an empty patch when nothing is set, got %+v", patch)
	}
}

func TestLoadBackfillPatchRejectsMalformedValues(t *testing.T) {
	cases := []struct{ name, value string }{
		{"SIDECAR_BACKFILL_WINDOW", "overnight"},
		{"SIDECAR_BACKFILL_WINDOW", "02:30-07:00"},
		{"SIDECAR_BACKFILL_MAX_WORKERS", "two"},
		{"SIDECAR_BACKFILL_BATCH_SIZE", "8.5"},
		{"SIDECAR_BACKFILL_LOAD_THRESHOLD", "high"},
		{"SIDECAR_BACKFILL_POLL_INTERVAL", "30"},
		{"SIDECAR_BACKFILL_IDLE_RESWEEP", "half an hour"},
	}

	for _, tc := range cases {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := loadBackfillPatch(); err == nil {
				t.Fatalf("expected %s=%q to be a startup error, not silently ignored", tc.name, tc.value)
			}
		})
	}
}

// configEnvVars is every environment variable loadConfig reads that this
// suite needs to control precisely, for both this file's tests and the
// backfill ones above. Clearing all of them at the start of a subtest
// means each one starts from "nothing is set", not whatever the previous
// subtest (or a stray variable in the real environment) left behind.
var configEnvVars = []string{
	"SIDECAR_DATABASE_URL", "SIDECAR_EMBEDDER", "SIDECAR_MODEL_PATH", "SIDECAR_ONNX_LIB_DIR",
	"SIDECAR_REMOTE_EMBEDDER_URL", "SIDECAR_REMOTE_EMBEDDER_TIMEOUT", "SIDECAR_ADMIN_ADDR",
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range configEnvVars {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unable to unset %s: %v", name, err)
		}
	}
}

// TestLoadConfigEmbedderKind covers loadConfig's SIDECAR_EMBEDDER
// branching: which variables are required for "local" vs "remote", the
// unset-defaults-to-local guarantee an existing deployment depends on,
// and an unrecognised value being a startup error rather than silently
// falling through to one backend or the other.
func TestLoadConfigEmbedderKind(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		wantErr     bool
		errContains string
		wantKind    string
	}{
		{
			name: "unset SIDECAR_EMBEDDER defaults to local",
			env: map[string]string{
				"SIDECAR_DATABASE_URL": "postgres://x",
				"SIDECAR_MODEL_PATH":   "/models/model.onnx",
			},
			wantKind: "local",
		},
		{
			name: "explicit local requires SIDECAR_MODEL_PATH",
			env: map[string]string{
				"SIDECAR_DATABASE_URL": "postgres://x",
				"SIDECAR_EMBEDDER":     "local",
				"SIDECAR_MODEL_PATH":   "/models/model.onnx",
			},
			wantKind: "local",
		},
		{
			name: "explicit local without SIDECAR_MODEL_PATH is an error",
			env: map[string]string{
				"SIDECAR_DATABASE_URL": "postgres://x",
				"SIDECAR_EMBEDDER":     "local",
			},
			wantErr:     true,
			errContains: "SIDECAR_MODEL_PATH",
		},
		{
			name: "remote with a URL",
			env: map[string]string{
				"SIDECAR_DATABASE_URL":        "postgres://x",
				"SIDECAR_EMBEDDER":            "remote",
				"SIDECAR_REMOTE_EMBEDDER_URL": "http://gpu-host:9000",
			},
			wantKind: "remote",
		},
		{
			name: "remote without a URL is an error",
			env: map[string]string{
				"SIDECAR_DATABASE_URL": "postgres://x",
				"SIDECAR_EMBEDDER":     "remote",
			},
			wantErr:     true,
			errContains: "SIDECAR_REMOTE_EMBEDDER_URL",
		},
		{
			name: "unrecognised value is an error",
			env: map[string]string{
				"SIDECAR_DATABASE_URL": "postgres://x",
				"SIDECAR_EMBEDDER":     "bogus",
			},
			wantErr:     true,
			errContains: "SIDECAR_EMBEDDER",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg, err := loadConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got nil (cfg=%+v)", cfg)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not mention %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig failed: %v", err)
			}
			if cfg.embedderKind != tc.wantKind {
				t.Fatalf("embedderKind = %q, want %q", cfg.embedderKind, tc.wantKind)
			}
		})
	}
}
