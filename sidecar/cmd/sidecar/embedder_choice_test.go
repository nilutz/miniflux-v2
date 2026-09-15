// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main // import "miniflux.app/v2/sidecar/cmd/sidecar"

import (
	"testing"

	"miniflux.app/v2/sidecar/internal/store"
)

// TestResolveEmbedderChoicePrefersPersistedRowOverEnvironmentVariable is
// the task 9 brief's own named trap: "a test asserting 'the persisted
// setting wins over the environment variable' passes trivially if the
// environment variable was never set in the test." cfg and persisted are
// set here to DIFFERENT, genuinely conflicting values -- kind AND
// remoteURL both disagree -- so this only passes if resolveEmbedderChoice
// actually prefers the persisted row, not merely "whichever argument
// happens to be non-empty".
func TestResolveEmbedderChoicePrefersPersistedRowOverEnvironmentVariable(t *testing.T) {
	cfg := config{
		embedderKind: "local",
		remoteURL:    "http://env-var-host:9000",
	}
	persisted := &store.EmbedderSettings{
		Kind:      "remote",
		RemoteURL: "http://persisted-host:9000",
	}

	got := resolveEmbedderChoice(cfg, persisted)

	if got.kind != "remote" {
		t.Fatalf("kind = %q, want %q (the persisted row's kind) -- the environment variable %q must not have won", got.kind, "remote", cfg.embedderKind)
	}
	if got.remoteURL != "http://persisted-host:9000" {
		t.Fatalf("remoteURL = %q, want %q (the persisted row's URL) -- the environment variable %q must not have won", got.remoteURL, "http://persisted-host:9000", cfg.remoteURL)
	}
}

// TestResolveEmbedderChoiceFallsBackToEnvironmentWhenNoRowExists is spec
// §13.1's other half: "the environment variables are the initial default
// used only when no row exists" -- persisted is nil (store.
// GetEmbedderSettings' own contract for "an operator has never switched
// embedders"), and the environment-derived config must be exactly what
// is used.
func TestResolveEmbedderChoiceFallsBackToEnvironmentWhenNoRowExists(t *testing.T) {
	cfg := config{
		embedderKind: "remote",
		remoteURL:    "http://env-var-host:9000",
	}

	got := resolveEmbedderChoice(cfg, nil)

	if got.kind != "remote" {
		t.Fatalf("kind = %q, want %q (cfg's own value, no persisted row)", got.kind, "remote")
	}
	if got.remoteURL != "http://env-var-host:9000" {
		t.Fatalf("remoteURL = %q, want %q (cfg's own value, no persisted row)", got.remoteURL, "http://env-var-host:9000")
	}
}
