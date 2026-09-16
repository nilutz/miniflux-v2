// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// testServiceToken is the shared secret every service-credential test in
// this file configures the server with, via newTestSearchServerWithServiceToken.
const testServiceToken = "sidecar-test-service-secret"

// newTestSearchServerWithServiceToken is newTestSearchServer plus a
// configured service credential (SetServiceToken) and an AdminChecker
// naming testAuthUserID an administrator -- the fixture
// TestServiceCredentialNeverReachesAdminEndpoints needs to prove the
// service credential is rejected even when it asserts a REAL
// administrator's own user id.
func newTestSearchServerWithServiceToken(t *testing.T, searcher SearchService, entries EntryLookup) http.Handler {
	t.Helper()
	keys := newFakeKeyValidator(map[string]int64{
		testAuthToken:      testAuthUserID,
		testOtherAuthToken: testOtherUserID,
	})
	admins := newFakeAdminChecker(map[int64]bool{testAuthUserID: true})
	srv, err := New(&fakeBackfill{}, nil, searcher, entries, nil, nil, nil, keys, nil, admins)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.SetServiceToken(testServiceToken)
	return srv.Handler()
}

// TestServiceCredentialMayAssertAnyUserID is defect 5's fix, as a test:
// internal/searchclient has no end-user credential of its own to forward
// (it renders results on behalf of whichever Miniflux user is browsing),
// so the shared service secret must let it assert that user's id via the
// "user" parameter -- the one thing presenting the secret authorises, per
// the brief. No X-Auth-Token is sent at all; only the service header.
func TestServiceCredentialMayAssertAnyUserID(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServerWithServiceToken(t, fs, nil)

	assertedUserID := int64(4242)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/search?q=widgets&user=%d", assertedUserID), nil)
	req.Header.Set(serviceTokenHeader, testServiceToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if fs.lastRequest.Filters.UserID != assertedUserID {
		t.Fatalf("Filters.UserID sent to Search = %d, want %d (the asserted user)", fs.lastRequest.Filters.UserID, assertedUserID)
	}
}

// TestServiceCredentialWithoutUserParameterIsBadRequest: a service
// credential has no user of its own to fall back on, so omitting "user"
// is a 400, not an unscoped search across every user's content.
func TestServiceCredentialWithoutUserParameterIsBadRequest(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServerWithServiceToken(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets", nil)
	req.Header.Set(serviceTokenHeader, testServiceToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
	if fs.lastRequest.Query != "" {
		t.Fatalf("expected the Searcher to never be called without a \"user\" parameter, but it received %+v", fs.lastRequest)
	}
}

// TestEndUserAPIKeyStillCannotAssertADifferentUserWhenServiceTokenIsConfigured
// is the asymmetry the brief asks to be tested explicitly: an end-user API
// key must be rejected for asserting someone else's user id, EVEN THOUGH a
// service credential is configured on this same server -- configuring a
// service credential must not loosen what an ordinary API key may do.
// (TestSearchUserParameterDisagreeingWithAuthenticatedUserIsRejected
// already covers this without a service token configured at all; this is
// the same check with one configured, so the two paths are proven not to
// interact.)
func TestEndUserAPIKeyStillCannotAssertADifferentUserWhenServiceTokenIsConfigured(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServerWithServiceToken(t, fs, nil)

	otherUserID := testAuthUserID + 1
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/search?q=widgets&user=%d", otherUserID), nil)
	req.Header.Set(authTokenHeader, testAuthToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
	if fs.lastRequest.Query != "" {
		t.Fatalf("expected the Searcher to never be called for a disagreeing user parameter, but it received %+v", fs.lastRequest)
	}
}

// TestServiceCredentialWrongSecretIsUnauthorized proves a caller cannot
// simply guess or reuse the header name without the real secret.
func TestServiceCredentialWrongSecretIsUnauthorized(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServerWithServiceToken(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets&user=1", nil)
	req.Header.Set(serviceTokenHeader, "not-the-real-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s, want 401", rec.Code, rec.Body.String())
	}
}

// TestServiceCredentialDisabledWhenNoTokenIsConfigured is the "fail
// closed" requirement: an unconfigured secret (serviceToken == "", New's
// default) must not silently accept the header at all -- not even a
// present-but-empty one. Deliberately NOT wrapped in authedHandler (which
// would inject a valid X-Auth-Token whenever none is set, masking exactly
// the no-credential-at-all case this test needs): no credential of any
// kind is presented here beyond the service header under test.
func TestServiceCredentialDisabledWhenNoTokenIsConfigured(t *testing.T) {
	fs := &fakeSearcher{}
	keys := newFakeKeyValidator(map[string]int64{testAuthToken: testAuthUserID})
	srv, err := New(&fakeBackfill{}, nil, fs, nil, nil, nil, nil, keys, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// serviceToken left at its zero value ("") deliberately -- no
	// SetServiceToken call.
	handler := srv.Handler()

	for _, presented := range []string{"anything", testServiceToken} {
		req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets&user=1", nil)
		req.Header.Set(serviceTokenHeader, presented)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("presented=%q: status = %d, body = %s, want 401 (service credential must be disabled entirely when unconfigured)",
				presented, rec.Code, rec.Body.String())
		}
	}
}

// TestServiceCredentialNeverReachesAdminEndpoints is the privilege-
// escalation check the brief calls out explicitly: presenting the shared
// secret must never grant admin, even when it asserts the user id of a
// REAL administrator (testAuthUserID, configured as one by
// newTestSearchServerWithServiceToken) -- Task 18's is_admin gating on
// control endpoints and the status page must stay exactly as it is. No
// admin-gated route reads a "user"-style assertion today (GET /api/status
// takes no parameters at all), so resolveCredential's own contract --
// asserted in TestResolveCredentialNeverAssignsAServiceCredentialANonZeroUserID
// below -- is what actually keeps this closed structurally; this test
// covers the route itself, end to end, against a regression in either
// piece.
func TestServiceCredentialNeverReachesAdminEndpoints(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServerWithServiceToken(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set(serviceTokenHeader, testServiceToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403 -- a service credential must never reach an admin-gated endpoint", rec.Code, rec.Body.String())
	}
}

// TestResolveCredentialNeverAssignsAServiceCredentialANonZeroUserID is the
// discriminating unit underneath requireAdmin's own explicit
// kind == credentialService rejection (see that check's doc comment): a
// service credential can only ever escalate to admin if resolveCredential
// ever handed it a REAL user id (a real administrator's, guessed or
// known) to carry into requireAdmin's IsAdmin lookup. It never does --
// this fails loudly the moment that stops being true, rather than relying
// on no admin route happening to read a "user" parameter today.
func TestResolveCredentialNeverAssignsAServiceCredentialANonZeroUserID(t *testing.T) {
	keys := newFakeKeyValidator(map[string]int64{testAuthToken: testAuthUserID})
	admins := newFakeAdminChecker(map[int64]bool{testAuthUserID: true})
	srv, err := New(&fakeBackfill{}, nil, nil, nil, nil, nil, nil, keys, nil, admins)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.SetServiceToken(testServiceToken)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	// Asking for testAuthUserID (a real administrator) via the "user"
	// query parameter is deliberate: even though no admin route reads it
	// today, resolveCredential must not be the thing an admin route could
	// ever be tricked into trusting -- see this test's own doc comment.
	req.URL.RawQuery = "user=" + strconv.FormatInt(testAuthUserID, 10)
	req.Header.Set(serviceTokenHeader, testServiceToken)

	userID, kind, ok, err := srv.resolveCredential(req)
	if err != nil {
		t.Fatalf("resolveCredential returned an error: %v", err)
	}
	if !ok {
		t.Fatalf("expected resolveCredential to accept the valid service token, got ok=false")
	}
	if kind != credentialService {
		t.Fatalf("kind = %v, want credentialService", kind)
	}
	if userID != 0 {
		t.Fatalf("userID = %d, want 0 -- a service credential must never resolve to a real user id, admin or not, no matter what the request's own \"user\" parameter names", userID)
	}
}

// TestServiceCredentialSimilarMayAssertAnyUserID is
// TestServiceCredentialMayAssertAnyUserID's counterpart for
// GET /api/similar, the other endpoint internal/searchclient calls.
func TestServiceCredentialSimilarMayAssertAnyUserID(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServerWithServiceToken(t, fs, nil)

	assertedUserID := int64(4242)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/similar?entry_id=42&user=%d", assertedUserID), nil)
	req.Header.Set(serviceTokenHeader, testServiceToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if fs.lastFilters.UserID != assertedUserID {
		t.Fatalf("Filters.UserID sent to Similar = %d, want %d (the asserted user)", fs.lastFilters.UserID, assertedUserID)
	}
}
