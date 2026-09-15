// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"testing"
)

// createTestAdminUser is createTestUser plus is_admin=true.
func createTestAdminUser(t *testing.T, s *Store, username string) (userID int64) {
	t.Helper()

	if err := s.db.QueryRow(
		`INSERT INTO users (username, password, is_admin) VALUES ($1, 'x', true) RETURNING id`,
		username,
	).Scan(&userID); err != nil {
		t.Fatalf("unable to create admin user: %v", err)
	}
	t.Cleanup(func() {
		s.db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	})

	return userID
}

// TestIsAdminReturnsTrueForAnAdminUser is the basic positive case: a user
// created with is_admin=true reports true.
func TestIsAdminReturnsTrueForAnAdminUser(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	userID := createTestAdminUser(t, s, "admin-yes")

	isAdmin, err := s.IsAdmin(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isAdmin {
		t.Fatal("expected isAdmin=true for a user created with is_admin=true")
	}
}

// TestIsAdminReturnsFalseForANonAdminUser proves the check is a real
// read of is_admin, not a value that defaults to true: a plain user
// (is_admin defaults to false per the CREATE TABLE) reports false.
func TestIsAdminReturnsFalseForANonAdminUser(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	userID := createTestUser(t, s, "admin-no")

	isAdmin, err := s.IsAdmin(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isAdmin {
		t.Fatal("expected isAdmin=false for a plain user")
	}
}

// TestIsAdminReturnsFalseForUnknownUserID fails closed on a user id no
// row matches (a stale credential for a deleted user), rather than
// erroring or -- far worse -- reporting true.
func TestIsAdminReturnsFalseForUnknownUserID(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	isAdmin, err := s.IsAdmin(context.Background(), 9_999_999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isAdmin {
		t.Fatal("expected isAdmin=false for a user id that does not exist")
	}
}

// TestIsAdminDistinguishesTwoUsers seeds one admin and one non-admin and
// proves the check tells them apart, not a fixed answer that happens to
// match a single-user test.
func TestIsAdminDistinguishesTwoUsers(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	admin := createTestAdminUser(t, s, "admin-distinguish-admin")
	nonAdmin := createTestUser(t, s, "admin-distinguish-nonadmin")

	isAdmin, err := s.IsAdmin(context.Background(), admin)
	if err != nil || !isAdmin {
		t.Fatalf("admin user: isAdmin=%v err=%v, want true/nil", isAdmin, err)
	}

	isAdmin, err = s.IsAdmin(context.Background(), nonAdmin)
	if err != nil || isAdmin {
		t.Fatalf("non-admin user: isAdmin=%v err=%v, want false/nil", isAdmin, err)
	}
}
