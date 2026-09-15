// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"database/sql"
	"testing"
)

// createTestAPIKey inserts a user and an api_keys row for it into the real
// Miniflux tables, mirroring createTestEntry's own technique
// (entries_test.go) -- everything it creates is removed when the test
// finishes. Returns the token and the id of the user it belongs to.
func createTestAPIKey(t *testing.T, s *Store, username, token string) (userID int64) {
	t.Helper()

	if err := s.db.QueryRow(
		`INSERT INTO users (username, password) VALUES ($1, 'x') RETURNING id`,
		username,
	).Scan(&userID); err != nil {
		t.Fatalf("unable to create user: %v", err)
	}
	t.Cleanup(func() {
		s.db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	})

	if _, err := s.db.Exec(
		`INSERT INTO api_keys (user_id, token, description) VALUES ($1, $2, 'sidecar test key')`,
		userID, token,
	); err != nil {
		t.Fatalf("unable to create api key: %v", err)
	}

	return userID
}

// TestValidateAPIKeyReturnsOwningUser is the basic, positive case: a
// token that exists resolves to exactly the user id it was created under.
func TestValidateAPIKeyReturnsOwningUser(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	const token = "apikeys-basic-token"
	userID := createTestAPIKey(t, s, "apikeys-basic", token)

	gotUserID, ok, err := s.ValidateAPIKey(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for a token that exists")
	}
	if gotUserID != userID {
		t.Fatalf("userID = %d, want %d", gotUserID, userID)
	}
}

// TestValidateAPIKeyReturnsFalseForUnknownToken pins the "do not leak
// whether a token exists" requirement's simplest case: a token that was
// never issued must come back as ok=false with a nil error, not an error
// a caller might render differently than the revoked case below.
func TestValidateAPIKeyReturnsFalseForUnknownToken(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	userID, ok, err := s.ValidateAPIKey(context.Background(), "apikeys-never-issued-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a token that was never issued")
	}
	if userID != 0 {
		t.Fatalf("userID = %d, want 0", userID)
	}
}

// TestValidateAPIKeyReturnsFalseAfterKeyIsRevoked is the task's own
// required case: Miniflux revokes a key by deleting its api_keys row, and
// the very next validation against that same token must fail -- read
// live, on every call, with nothing cached that could serve a stale
// "still valid" answer past the revocation.
func TestValidateAPIKeyReturnsFalseAfterKeyIsRevoked(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	const token = "apikeys-revoked-token"
	createTestAPIKey(t, s, "apikeys-revoked", token)

	if _, ok, err := s.ValidateAPIKey(context.Background(), token); err != nil || !ok {
		t.Fatalf("expected the key to validate before revocation: ok=%v err=%v", ok, err)
	}

	if _, err := s.db.Exec(`DELETE FROM api_keys WHERE token=$1`, token); err != nil {
		t.Fatalf("unable to revoke (delete) the key: %v", err)
	}

	userID, ok, err := s.ValidateAPIKey(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error after revocation: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false immediately after the api_keys row was deleted")
	}
	if userID != 0 {
		t.Fatalf("userID = %d, want 0 after revocation", userID)
	}
}

// TestValidateAPIKeyDistinguishesTwoUsersTokens seeds two separate users,
// each with their own key, and proves the two tokens resolve to two
// DIFFERENT user ids -- the data-layer half of the task's "a token
// belonging to user A must not return user B's entries" requirement. A
// bug that returned a fixed or swapped user id would still pass a
// single-user test like TestValidateAPIKeyReturnsOwningUser above; it
// cannot pass this one.
func TestValidateAPIKeyDistinguishesTwoUsersTokens(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	const tokenA = "apikeys-two-users-token-a"
	const tokenB = "apikeys-two-users-token-b"
	userA := createTestAPIKey(t, s, "apikeys-two-users-a", tokenA)
	userB := createTestAPIKey(t, s, "apikeys-two-users-b", tokenB)

	if userA == userB {
		t.Fatalf("fixture error: both keys belong to user %d", userA)
	}

	gotA, ok, err := s.ValidateAPIKey(context.Background(), tokenA)
	if err != nil || !ok {
		t.Fatalf("validating token A: ok=%v err=%v", ok, err)
	}
	if gotA != userA {
		t.Fatalf("token A resolved to user %d, want %d", gotA, userA)
	}

	gotB, ok, err := s.ValidateAPIKey(context.Background(), tokenB)
	if err != nil || !ok {
		t.Fatalf("validating token B: ok=%v err=%v", ok, err)
	}
	if gotB != userB {
		t.Fatalf("token B resolved to user %d, want %d", gotB, userB)
	}
}

// TestValidateAPIKeyDoesNotUpdateLastUsedAt pins the brief's explicit
// read-only requirement: the sidecar must never write api_keys.last_used_at
// -- that column belongs to Miniflux (internal/api/middleware.go updates
// it on the fork's own REST API), and writing it here would make the
// sidecar a writer of upstream state for the first time. A key created
// with a NULL last_used_at (the column's default -- it is only ever set by
// Miniflux's own middleware on first use there) must still be NULL after
// any number of sidecar-side validations.
func TestValidateAPIKeyDoesNotUpdateLastUsedAt(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	const token = "apikeys-readonly-token"
	createTestAPIKey(t, s, "apikeys-readonly", token)

	assertLastUsedAtIsNull := func(when string) {
		t.Helper()
		var lastUsedAt sql.NullTime
		if err := s.db.QueryRow(`SELECT last_used_at FROM api_keys WHERE token=$1`, token).Scan(&lastUsedAt); err != nil {
			t.Fatalf("%s: unable to read last_used_at: %v", when, err)
		}
		if lastUsedAt.Valid {
			t.Fatalf("%s: last_used_at = %v, want NULL -- the sidecar must never write this column", when, lastUsedAt.Time)
		}
	}

	assertLastUsedAtIsNull("before validation")

	if _, ok, err := s.ValidateAPIKey(context.Background(), token); err != nil || !ok {
		t.Fatalf("validate: ok=%v err=%v", ok, err)
	}

	assertLastUsedAtIsNull("after one validation")

	// A second call, for good measure -- if anything ever set it on a
	// first call, a UNIQUE index or an UPDATE side effect might not
	// surface until a repeat call re-touches the same row.
	if _, ok, err := s.ValidateAPIKey(context.Background(), token); err != nil || !ok {
		t.Fatalf("second validate: ok=%v err=%v", ok, err)
	}

	assertLastUsedAtIsNull("after two validations")
}
