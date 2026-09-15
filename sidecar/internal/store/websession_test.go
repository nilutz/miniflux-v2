// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"testing"
)

// createTestUser inserts a bare user row into the real Miniflux `users`
// table, mirroring createTestAPIKey's own technique (apikeys_test.go) --
// removed when the test finishes.
func createTestUser(t *testing.T, s *Store, username string) (userID int64) {
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

	return userID
}

// createTestWebSession inserts a web_sessions row bound to userID, with
// its secret_hash computed exactly the way internal/model/web_session.go's
// hashWebSessionSecret does (sha256, no salt) -- see websession.go's own
// doc comment for why this must mirror that file exactly rather than
// re-derive it. Returns the cookie value ValidateWebSessionCookie expects:
// "<sessionID>.<secret>", the same format
// internal/ui/web_session_middleware.go's loadWebSessionFromCookie parses.
//
// userID may be zero to create an anonymous (not-yet-logged-in) session,
// matching a fresh browser that has never signed in.
func createTestWebSession(t *testing.T, s *Store, sessionID, secret string, userID int64) (cookieValue string) {
	t.Helper()

	sum := sha256.Sum256([]byte(secret))

	var nullUserID sql.NullInt64
	if userID != 0 {
		nullUserID = sql.NullInt64{Int64: userID, Valid: true}
	}

	if _, err := s.db.Exec(
		`INSERT INTO web_sessions (id, secret_hash, user_id) VALUES ($1, $2, $3)`,
		sessionID, sum[:], nullUserID,
	); err != nil {
		t.Fatalf("unable to create web session: %v", err)
	}
	t.Cleanup(func() {
		s.db.Exec(`DELETE FROM web_sessions WHERE id=$1`, sessionID)
	})

	return sessionID + "." + secret
}

// TestWebSessionsTableHasTheShapeThisFileDependsOn is the required
// schema-shape guard (websession.go's package doc comment names
// the coupling risk this pins against): ValidateWebSessionCookie's raw
// SQL selects web_sessions.id/secret_hash/user_id by name, with no
// compile-time link to internal/database/migrations.go's CREATE TABLE on
// the other side of the module boundary. If a future rebase renames or
// retypes any of the three, THIS test fails first, by column name, rather
// than surfacing later as every browser session silently failing to
// authenticate with no clear cause (see websession.go's doc comment for
// what an operator sees and how they would diagnose it).
func TestWebSessionsTableHasTheShapeThisFileDependsOn(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	rows, err := s.db.Query(`
		SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'web_sessions'
	`)
	if err != nil {
		t.Fatalf("unable to introspect web_sessions: %v", err)
	}
	defer rows.Close()

	type column struct {
		dataType   string
		isNullable string
	}
	got := map[string]column{}
	for rows.Next() {
		var name string
		var c column
		if err := rows.Scan(&name, &c.dataType, &c.isNullable); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := map[string]column{
		"id":          {dataType: "text", isNullable: "NO"},
		"secret_hash": {dataType: "bytea", isNullable: "NO"},
		"user_id":     {dataType: "integer", isNullable: "YES"},
	}
	for col, wantCol := range want {
		gotCol, ok := got[col]
		if !ok {
			t.Fatalf("web_sessions has no column %q -- ValidateWebSessionCookie (websession.go) selects it by name and would fail at runtime with no compile error", col)
		}
		if gotCol.dataType != wantCol.dataType {
			t.Fatalf("web_sessions.%s has type %q, want %q", col, gotCol.dataType, wantCol.dataType)
		}
		if gotCol.isNullable != wantCol.isNullable {
			t.Fatalf("web_sessions.%s is_nullable=%q, want %q -- ValidateWebSessionCookie's NULL handling for user_id (sql.NullInt64) assumes this", col, gotCol.isNullable, wantCol.isNullable)
		}
	}
}

// TestValidateWebSessionCookieReturnsOwningUser is the basic, positive
// case: a session cookie for a real, authenticated session resolves to
// exactly the user id it was created under.
func TestValidateWebSessionCookieReturnsOwningUser(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	userID := createTestUser(t, s, "websession-basic")
	cookieValue := createTestWebSession(t, s, "websession-basic-id", "websession-basic-secret", userID)

	gotUserID, ok, err := s.ValidateWebSessionCookie(context.Background(), cookieValue)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for a valid, authenticated session cookie")
	}
	if gotUserID != userID {
		t.Fatalf("userID = %d, want %d", gotUserID, userID)
	}
}

// TestValidateWebSessionCookieRejectsWrongSecret proves the secret half
// of the cookie is actually checked, not just the session id: a correct
// session id paired with any other secret must not validate.
func TestValidateWebSessionCookieRejectsWrongSecret(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	userID := createTestUser(t, s, "websession-wrongsecret")
	createTestWebSession(t, s, "websession-wrongsecret-id", "the-real-secret", userID)

	gotUserID, ok, err := s.ValidateWebSessionCookie(context.Background(), "websession-wrongsecret-id.a-guessed-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a session id paired with the wrong secret")
	}
	if gotUserID != 0 {
		t.Fatalf("userID = %d, want 0", gotUserID)
	}
}

// TestValidateWebSessionCookieRejectsUnknownSessionID mirrors
// TestValidateAPIKeyReturnsFalseForUnknownToken's "do not leak whether it
// exists" case for sessions: a session id no row matches must fail
// exactly like a wrong secret, not with a different error or status.
func TestValidateWebSessionCookieRejectsUnknownSessionID(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	userID, ok, err := s.ValidateWebSessionCookie(context.Background(), "this-session-was-never-created.some-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a session id that does not exist")
	}
	if userID != 0 {
		t.Fatalf("userID = %d, want 0", userID)
	}
}

// TestValidateWebSessionCookieRejectsMalformedCookieValue proves a cookie
// value with no "." separator (or an empty id/secret half) is rejected
// with ok=false, nil error -- never a panic or a database round trip on
// obviously-invalid input.
func TestValidateWebSessionCookieRejectsMalformedCookieValue(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	for _, malformed := range []string{"", "no-dot-in-this-value", ".secret-with-empty-id", "id-with-empty-secret."} {
		userID, ok, err := s.ValidateWebSessionCookie(context.Background(), malformed)
		if err != nil {
			t.Fatalf("cookie %q: unexpected error: %v", malformed, err)
		}
		if ok {
			t.Fatalf("cookie %q: expected ok=false", malformed)
		}
		if userID != 0 {
			t.Fatalf("cookie %q: userID = %d, want 0", malformed, userID)
		}
	}
}

// TestValidateWebSessionCookieRejectsUnauthenticatedSession covers a
// session row that exists (Miniflux's web_session_middleware.go creates
// one for every first-time visitor) but has never been bound to a user
// -- user_id IS NULL, an anonymous, not-yet-logged-in browser. This must
// not resolve to user id 0 (a valid-looking ok=true, userID=0 would be a
// far worse bug than a plain rejection): it must be ok=false.
func TestValidateWebSessionCookieRejectsUnauthenticatedSession(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	cookieValue := createTestWebSession(t, s, "websession-anonymous-id", "websession-anonymous-secret", 0)

	userID, ok, err := s.ValidateWebSessionCookie(context.Background(), cookieValue)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a session with no bound user (user_id IS NULL)")
	}
	if userID != 0 {
		t.Fatalf("userID = %d, want 0", userID)
	}
}

// TestValidateWebSessionCookieReturnsFalseAfterSessionIsDeleted covers
// the required case for sessions, mirroring
// TestValidateAPIKeyReturnsFalseAfterKeyIsRevoked: a session that
// validates successfully must stop working the instant its web_sessions
// row is gone (Miniflux's own session cleanup, or a user signing out
// elsewhere, both delete the row) -- read live, on every call, with
// nothing cached that could serve a stale "still valid" answer.
func TestValidateWebSessionCookieReturnsFalseAfterSessionIsDeleted(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	userID := createTestUser(t, s, "websession-expiring")
	cookieValue := createTestWebSession(t, s, "websession-expiring-id", "websession-expiring-secret", userID)

	if _, ok, err := s.ValidateWebSessionCookie(context.Background(), cookieValue); err != nil || !ok {
		t.Fatalf("expected the session to validate before deletion: ok=%v err=%v", ok, err)
	}

	if _, err := s.db.Exec(`DELETE FROM web_sessions WHERE id=$1`, "websession-expiring-id"); err != nil {
		t.Fatalf("unable to delete the session: %v", err)
	}

	gotUserID, ok, err := s.ValidateWebSessionCookie(context.Background(), cookieValue)
	if err != nil {
		t.Fatalf("unexpected error after deletion: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false immediately after the web_sessions row was deleted")
	}
	if gotUserID != 0 {
		t.Fatalf("userID = %d, want 0 after deletion", gotUserID)
	}
}

// TestValidateWebSessionCookieDistinguishesTwoUsersSessions seeds two
// separate users, each with their own session, and proves the two
// cookies resolve to two DIFFERENT user ids -- the data-layer half of
// "a session belonging to user A must not return user B's entries",
// mirroring TestValidateAPIKeyDistinguishesTwoUsersTokens.
func TestValidateWebSessionCookieDistinguishesTwoUsersSessions(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	userA := createTestUser(t, s, "websession-two-users-a")
	userB := createTestUser(t, s, "websession-two-users-b")
	if userA == userB {
		t.Fatalf("fixture error: both users are %d", userA)
	}

	cookieA := createTestWebSession(t, s, "websession-two-users-a-id", "secret-a", userA)
	cookieB := createTestWebSession(t, s, "websession-two-users-b-id", "secret-b", userB)

	gotA, ok, err := s.ValidateWebSessionCookie(context.Background(), cookieA)
	if err != nil || !ok {
		t.Fatalf("validating cookie A: ok=%v err=%v", ok, err)
	}
	if gotA != userA {
		t.Fatalf("cookie A resolved to user %d, want %d", gotA, userA)
	}

	gotB, ok, err := s.ValidateWebSessionCookie(context.Background(), cookieB)
	if err != nil || !ok {
		t.Fatalf("validating cookie B: ok=%v err=%v", ok, err)
	}
	if gotB != userB {
		t.Fatalf("cookie B resolved to user %d, want %d", gotB, userB)
	}
}

// TestValidateWebSessionCookieDoesNotWriteWebSessions pins the explicit
// read-only requirement for sessions (mirroring
// TestValidateAPIKeyDoesNotUpdateLastUsedAt for api_keys): validating a
// cookie any number of times must never modify the underlying row --
// Miniflux owns web_sessions exclusively, and a write here (a rotation,
// a touched timestamp) would make the sidecar a writer of upstream
// session state for the first time.
func TestValidateWebSessionCookieDoesNotWriteWebSessions(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	userID := createTestUser(t, s, "websession-readonly")
	cookieValue := createTestWebSession(t, s, "websession-readonly-id", "websession-readonly-secret", userID)

	readRow := func(when string) (secretHash []byte, createdAt any) {
		t.Helper()
		var hash []byte
		var created any
		if err := s.db.QueryRow(`SELECT secret_hash, created_at FROM web_sessions WHERE id=$1`, "websession-readonly-id").Scan(&hash, &created); err != nil {
			t.Fatalf("%s: unable to read web session row: %v", when, err)
		}
		return hash, created
	}

	beforeHash, beforeCreated := readRow("before validation")

	if _, ok, err := s.ValidateWebSessionCookie(context.Background(), cookieValue); err != nil || !ok {
		t.Fatalf("validate: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.ValidateWebSessionCookie(context.Background(), cookieValue); err != nil || !ok {
		t.Fatalf("second validate: ok=%v err=%v", ok, err)
	}

	afterHash, afterCreated := readRow("after two validations")

	if string(beforeHash) != string(afterHash) {
		t.Fatalf("secret_hash changed: before=%x after=%x -- ValidateWebSessionCookie must never write web_sessions", beforeHash, afterHash)
	}
	if beforeCreated != afterCreated {
		t.Fatalf("created_at changed: before=%v after=%v -- ValidateWebSessionCookie must never write web_sessions", beforeCreated, afterCreated)
	}
}
