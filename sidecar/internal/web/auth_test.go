// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file is the required test coverage for the sidecar's
// authentication and authorisation gates: every protected data endpoint
// (GET /api/search, /api/similar, /api/article) rejects a missing or
// unknown credential (API key OR session cookie), accepts and correctly
// SCOPES a valid one of either kind, and stops accepting a credential the
// instant its underlying row (api_keys or web_sessions) is gone
// (simulated here via fakeKeyValidator.revoke / fakeSessionValidator.expire).
// It also covers every control endpoint and the status page, which
// require an ADMIN credential specifically
// (TestControlAndStatusEndpointsRequireAdmin -- see server.go's Server
// doc comment for why). Per the brief: assert every case PER endpoint,
// not once for the group -- this project has repeatedly shipped a guard
// that covered one call site out of several.
package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"miniflux.app/v2/sidecar/internal/indexer"
	"miniflux.app/v2/sidecar/internal/store"
)

// protectedRequest describes one protected-endpoint request for the
// table-driven tests below.
type protectedRequest struct {
	name   string
	method string
	target string
}

var protectedRequests = []protectedRequest{
	{name: "search", method: http.MethodGet, target: "/api/search?q=widgets"},
	{name: "similar", method: http.MethodGet, target: "/api/similar?entry_id=42"},
	{name: "article", method: http.MethodGet, target: "/api/article?entry_id=42"},
}

// newAuthTestServer builds a full Server over keys and sessions, wired
// with fresh fakeSearcher/fakeArticles so a subtest can assert whether
// they were actually reached. Unlike newTestSearchServer/
// newTestArticleServer, the returned handler is the RAW server handler --
// NOT wrapped in authedHandler -- because these tests are specifically
// about what happens with no credential, an unrecognised one, or one
// that stops working mid-test.
func newAuthTestServer(t *testing.T, keys APIKeyValidator, sessions SessionValidator) (handler http.Handler, fs *fakeSearcher, fa *fakeArticles) {
	t.Helper()
	fs = &fakeSearcher{}
	fa = &fakeArticles{byID: map[int64]*store.ArticleDetail{42: {ID: 42, Title: "t"}}}
	srv, err := New(&fakeBackfill{}, nil, fs, nil, fa, nil, nil, keys, sessions, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler(), fs, fa
}

// TestProtectedEndpointsRejectMissingHeader is the brief's own required
// case, asserted per endpoint (a t.Run subtest per route) rather than
// once for the group: a request with no X-Auth-Token header at all must
// get 401 on EVERY protected route, and the downstream fake must never be
// reached.
func TestProtectedEndpointsRejectMissingHeader(t *testing.T) {
	for _, pr := range protectedRequests {
		t.Run(pr.name, func(t *testing.T) {
			handler, fs, fa := newAuthTestServer(t, newFakeKeyValidator(map[string]int64{testAuthToken: testAuthUserID}), nil)

			req := httptest.NewRequest(pr.method, pr.target, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body = %s, want 401", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), authTokenHeader) {
				t.Errorf("error body = %q, want it to name the expected header %q", rec.Body.String(), authTokenHeader)
			}
			if fs.lastRequest.Query != "" || fs.similarCalled {
				t.Errorf("the searcher was reached despite a missing header: %+v / similarCalled=%v", fs.lastRequest, fs.similarCalled)
			}
			if fa.lastUserID != 0 {
				t.Errorf("the article lookup was reached despite a missing header: lastUserID=%d", fa.lastUserID)
			}
		})
	}
}

// TestProtectedEndpointsRejectUnknownToken is
// TestProtectedEndpointsRejectMissingHeader's counterpart: a header that
// IS present, but names a token the validator has no row for, must also
// be 401 on every protected route, and must look the same to the caller
// as a missing header -- § Behaviour's "do not leak whether a token
// exists".
func TestProtectedEndpointsRejectUnknownToken(t *testing.T) {
	for _, pr := range protectedRequests {
		t.Run(pr.name, func(t *testing.T) {
			handler, fs, fa := newAuthTestServer(t, newFakeKeyValidator(map[string]int64{testAuthToken: testAuthUserID}), nil)

			req := httptest.NewRequest(pr.method, pr.target, nil)
			req.Header.Set(authTokenHeader, "this-token-was-never-issued")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body = %s, want 401", rec.Code, rec.Body.String())
			}
			if fs.lastRequest.Query != "" || fs.similarCalled {
				t.Errorf("the searcher was reached despite an unknown token: %+v / similarCalled=%v", fs.lastRequest, fs.similarCalled)
			}
			if fa.lastUserID != 0 {
				t.Errorf("the article lookup was reached despite an unknown token: lastUserID=%d", fa.lastUserID)
			}
		})
	}
}

// TestProtectedEndpointsAcceptValidToken proves a valid token gets past
// the gate on every protected route (the status-code half of "valid
// token -> 200 and results scoped to that key's user" -- the scoping half
// is covered separately, per endpoint, by
// TestSearchWithoutUserParameterIsScopedToTheAuthenticatedUser,
// TestSimilarWithoutUserParameterIsScopedToTheAuthenticatedUser and
// TestArticleScopesLookupToTheAuthenticatedUser in
// search_handlers_test.go/article_handler_test.go).
func TestProtectedEndpointsAcceptValidToken(t *testing.T) {
	for _, pr := range protectedRequests {
		t.Run(pr.name, func(t *testing.T) {
			handler, _, _ := newAuthTestServer(t, newFakeKeyValidator(map[string]int64{testAuthToken: testAuthUserID}), nil)

			req := httptest.NewRequest(pr.method, pr.target, nil)
			req.Header.Set(authTokenHeader, testAuthToken)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s, want 200 for a valid token", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestProtectedEndpointsReject401AfterKeyIsRevoked is the brief's own
// required case: a token that validated successfully must stop working
// the moment its underlying api_keys row is gone -- simulated here via
// fakeKeyValidator.revoke, which is exactly what store.Store.ValidateAPIKey
// does for a real DELETE (see internal/store/apikeys_test.go's
// TestValidateAPIKeyReturnsFalseAfterKeyIsRevoked for the data-layer
// proof of that). Checked on every protected route: nothing caches a
// validation result across requests today (server.go's own doc comment
// on New explains there is nothing to invalidate), so a stale "still
// valid" answer on any one route would be a real regression.
func TestProtectedEndpointsReject401AfterKeyIsRevoked(t *testing.T) {
	for _, pr := range protectedRequests {
		t.Run(pr.name, func(t *testing.T) {
			keys := newFakeKeyValidator(map[string]int64{testAuthToken: testAuthUserID})
			handler, _, _ := newAuthTestServer(t, keys, nil)

			okReq := httptest.NewRequest(pr.method, pr.target, nil)
			okReq.Header.Set(authTokenHeader, testAuthToken)
			okRec := httptest.NewRecorder()
			handler.ServeHTTP(okRec, okReq)
			if okRec.Code != http.StatusOK {
				t.Fatalf("expected 200 before revocation, got %d: %s", okRec.Code, okRec.Body.String())
			}

			keys.revoke(testAuthToken)

			revokedReq := httptest.NewRequest(pr.method, pr.target, nil)
			revokedReq.Header.Set(authTokenHeader, testAuthToken)
			revokedRec := httptest.NewRecorder()
			handler.ServeHTTP(revokedRec, revokedReq)
			if revokedRec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 on the request immediately after revocation, got %d: %s", revokedRec.Code, revokedRec.Body.String())
			}
		})
	}
}

// TestProtectedEndpointFailsClosedWithoutKeyValidator proves a Server
// built with keys == nil (a misconfiguration -- cmd/sidecar always
// supplies one) fails CLOSED with 500 on a protected route rather than
// silently admitting every caller. This is the concrete guard behind this
// task's "no configuration flag disables authentication" constraint: an
// unconfigured validator must never be indistinguishable from "auth
// turned off".
func TestProtectedEndpointFailsClosedWithoutKeyValidator(t *testing.T) {
	handler, _, _ := newAuthTestServer(t, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets", nil)
	req.Header.Set(authTokenHeader, testAuthToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500 (fail closed, not open) with no APIKeyValidator configured", rec.Code, rec.Body.String())
	}
}

// --- session-cookie authentication on the data endpoints ---

// testSessionCookieValue/testSessionUserID and testOtherSessionCookieValue/
// testOtherSessionUserID are the fixed session-cookie fixtures the tests
// below use, the cookie-based counterpart of testAuthToken/testAuthUserID
// and testOtherAuthToken/testOtherUserID (server_test.go).
const (
	testSessionCookieValue            = "sidecar-test-session-id.sidecar-test-session-secret"
	testSessionUserID           int64 = 7770
	testOtherSessionCookieValue       = "sidecar-test-session-id-other.sidecar-test-session-secret-other"
	testOtherSessionUserID      int64 = 8880
)

// withSessionCookie sets Miniflux's own session cookie on req -- the
// browser-side counterpart of setting the X-Auth-Token header.
func withSessionCookie(req *http.Request, value string) {
	req.AddCookie(&http.Cookie{Name: minifluxSessionCookieName, Value: value})
}

// TestProtectedEndpointsAcceptValidSessionCookie is
// TestProtectedEndpointsAcceptValidToken's session-cookie counterpart
// (the "no login" requirement): a request carrying no X-Auth-Token header
// at all, but a valid MinifluxSessionID cookie, must pass the same gate
// every protected data endpoint uses.
func TestProtectedEndpointsAcceptValidSessionCookie(t *testing.T) {
	for _, pr := range protectedRequests {
		t.Run(pr.name, func(t *testing.T) {
			sessions := newFakeSessionValidator(map[string]int64{testSessionCookieValue: testSessionUserID})
			handler, _, _ := newAuthTestServer(t, nil, sessions)

			req := httptest.NewRequest(pr.method, pr.target, nil)
			withSessionCookie(req, testSessionCookieValue)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s, want 200 for a valid session cookie", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestProtectedEndpointsRejectUnknownSessionCookie mirrors
// TestProtectedEndpointsRejectUnknownToken for the cookie credential: a
// cookie present but naming a session no row matches must be 401 on
// every protected route, indistinguishable from a missing credential.
func TestProtectedEndpointsRejectUnknownSessionCookie(t *testing.T) {
	for _, pr := range protectedRequests {
		t.Run(pr.name, func(t *testing.T) {
			sessions := newFakeSessionValidator(map[string]int64{testSessionCookieValue: testSessionUserID})
			handler, fs, fa := newAuthTestServer(t, nil, sessions)

			req := httptest.NewRequest(pr.method, pr.target, nil)
			withSessionCookie(req, "this-session-was-never-created.some-secret")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body = %s, want 401", rec.Code, rec.Body.String())
			}
			if fs.lastRequest.Query != "" || fs.similarCalled {
				t.Errorf("the searcher was reached despite an unknown session cookie: %+v / similarCalled=%v", fs.lastRequest, fs.similarCalled)
			}
			if fa.lastUserID != 0 {
				t.Errorf("the article lookup was reached despite an unknown session cookie: lastUserID=%d", fa.lastUserID)
			}
		})
	}
}

// TestProtectedEndpointsReject401AfterSessionCookieExpires is
// TestProtectedEndpointsReject401AfterKeyIsRevoked's session-cookie
// counterpart, and the brief's own required "expired or deleted session
// -> 401 on the next request" case: a session that validates
// successfully must stop working the instant its underlying
// web_sessions row is gone (sign-out, or Miniflux's own cleanup sweep;
// simulated here via fakeSessionValidator.expire, which is exactly what
// store.Store.ValidateWebSessionCookie does for a real DELETE -- see
// internal/store/websession_test.go's
// TestValidateWebSessionCookieReturnsFalseAfterSessionIsDeleted for the
// data-layer proof of that).
func TestProtectedEndpointsReject401AfterSessionCookieExpires(t *testing.T) {
	for _, pr := range protectedRequests {
		t.Run(pr.name, func(t *testing.T) {
			sessions := newFakeSessionValidator(map[string]int64{testSessionCookieValue: testSessionUserID})
			handler, _, _ := newAuthTestServer(t, nil, sessions)

			okReq := httptest.NewRequest(pr.method, pr.target, nil)
			withSessionCookie(okReq, testSessionCookieValue)
			okRec := httptest.NewRecorder()
			handler.ServeHTTP(okRec, okReq)
			if okRec.Code != http.StatusOK {
				t.Fatalf("expected 200 before expiry, got %d: %s", okRec.Code, okRec.Body.String())
			}

			sessions.expire(testSessionCookieValue)

			expiredReq := httptest.NewRequest(pr.method, pr.target, nil)
			withSessionCookie(expiredReq, testSessionCookieValue)
			expiredRec := httptest.NewRecorder()
			handler.ServeHTTP(expiredRec, expiredReq)
			if expiredRec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 on the request immediately after the session expired, got %d: %s", expiredRec.Code, expiredRec.Body.String())
			}
		})
	}
}

// TestSessionCookieScopesArticleLookupToItsOwnUser proves a session
// cookie is resolved to the RIGHT user, not just SOME user: two distinct
// sessions, for two distinct users, each of whom owns a different entry,
// must each only be able to read their own via GET /api/article -- the
// brief's own required "a session belonging to user A must not return
// user B's entries" case, exercised through the cookie credential
// specifically (the equivalent API-key case is
// article_handler_test.go's TestArticleDoesNotReturnAnotherUsersEntry).
func TestSessionCookieScopesArticleLookupToItsOwnUser(t *testing.T) {
	sessions := newFakeSessionValidator(map[string]int64{
		testSessionCookieValue:      testSessionUserID,
		testOtherSessionCookieValue: testOtherSessionUserID,
	})
	fa := &fakeArticles{
		byID: map[int64]*store.ArticleDetail{
			42: {ID: 42, Title: "user A's article"},
			43: {ID: 43, Title: "user B's article"},
		},
		ownerOf: map[int64]int64{42: testSessionUserID, 43: testOtherSessionUserID},
	}
	srv, err := New(&fakeBackfill{}, nil, nil, nil, fa, nil, nil, nil, sessions, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := srv.Handler()

	// User A's session can read entry 42 (their own)...
	reqOwn := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=42", nil)
	withSessionCookie(reqOwn, testSessionCookieValue)
	recOwn := httptest.NewRecorder()
	handler.ServeHTTP(recOwn, reqOwn)
	if recOwn.Code != http.StatusOK {
		t.Fatalf("user A reading their own entry: status = %d, body = %s", recOwn.Code, recOwn.Body.String())
	}

	// ...but not entry 43, which belongs to user B.
	reqOther := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=43", nil)
	withSessionCookie(reqOther, testSessionCookieValue)
	recOther := httptest.NewRecorder()
	handler.ServeHTTP(recOther, reqOther)
	if recOther.Code != http.StatusNotFound {
		t.Fatalf("user A reading user B's entry: status = %d, body = %s, want 404 (indistinguishable from non-existent)", recOther.Code, recOther.Body.String())
	}

	// Symmetrically, user B's OWN, DIFFERENT session cookie can read
	// their own entry 43...
	reqBOwn := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=43", nil)
	withSessionCookie(reqBOwn, testOtherSessionCookieValue)
	recBOwn := httptest.NewRecorder()
	handler.ServeHTTP(recBOwn, reqBOwn)
	if recBOwn.Code != http.StatusOK {
		t.Fatalf("user B reading their own entry: status = %d, body = %s", recBOwn.Code, recBOwn.Body.String())
	}

	// ...but not entry 42, which belongs to user A -- proving the two
	// DISTINCT cookies actually resolve to two DISTINCT user ids (a bug
	// that resolved every cookie to the same user, or swapped the two,
	// would still pass the two checks above in isolation but fails this
	// one).
	reqBOther := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=42", nil)
	withSessionCookie(reqBOther, testOtherSessionCookieValue)
	recBOther := httptest.NewRecorder()
	handler.ServeHTTP(recBOther, reqBOther)
	if recBOther.Code != http.StatusNotFound {
		t.Fatalf("user B reading user A's entry: status = %d, body = %s, want 404", recBOther.Code, recBOther.Body.String())
	}
}

// --- requireAdmin on every control endpoint and the status page ---

// controlAndStatusRequests is every route gated behind requireAdmin --
// see server.go's Server doc comment for why these endpoints require an
// admin credential rather than staying unauthenticated or accepting any
// valid one.
var controlAndStatusRequests = []struct {
	name   string
	method string
	target string
	body   string
}{
	{name: "status page", method: http.MethodGet, target: "/"},
	{name: "api status", method: http.MethodGet, target: "/api/status"},
	{name: "backfill pause", method: http.MethodPost, target: "/api/backfill/pause"},
	{name: "backfill resume", method: http.MethodPost, target: "/api/backfill/resume"},
	{name: "backfill get config", method: http.MethodGet, target: "/api/backfill/config"},
	{name: "backfill set config", method: http.MethodPost, target: "/api/backfill/config", body: `{"max_workers":2}`},
	{name: "get embedder", method: http.MethodGet, target: "/api/embedder"},
	{name: "embedder preview", method: http.MethodPost, target: "/api/embedder/preview", body: `{"kind":"local"}`},
	{name: "embedder switch", method: http.MethodPost, target: "/api/embedder/switch", body: `{"kind":"local","confirm":true}`},
}

// newControlTestServer builds a Server over fb/manager plus keys+admins
// fixtures with exactly two known users: adminUserID (is_admin=true) and
// nonAdminUserID (present in keys, absent -- so false -- from admins).
// adminToken/nonAdminToken authenticate as each via X-Auth-Token.
const (
	adminToken     = "sidecar-test-admin-token"
	adminUserID    = 9001
	nonAdminToken  = "sidecar-test-nonadmin-token"
	nonAdminUserID = 9002
)

func newControlTestServer(t *testing.T, fb *fakeBackfill, manager EmbedderManager) http.Handler {
	t.Helper()
	keys := newFakeKeyValidator(map[string]int64{
		adminToken:    adminUserID,
		nonAdminToken: nonAdminUserID,
	})
	admins := newFakeAdminChecker(map[int64]bool{
		adminUserID: true,
		// nonAdminUserID is deliberately absent -- a map lookup miss
		// reports false, exactly like store.Store.IsAdmin does for a real
		// row with is_admin=false.
	})
	srv, err := New(fb, nil, nil, nil, nil, nil, manager, keys, nil, admins)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}

func newControlRequest(c struct {
	name   string
	method string
	target string
	body   string
}) *http.Request {
	if c.body != "" {
		req := httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	return httptest.NewRequest(c.method, c.target, nil)
}

// TestControlAndStatusEndpointsRequireAdmin is the central authorisation
// guard for these routes (see server.go's Server doc comment for why they
// require admin). Asserted PER endpoint, PER case, exactly as the brief
// requires ("this project has repeatedly shipped guards covering one
// route out of several, every time caught only by mutation"):
//
//   - no credential at all -> 401
//   - a valid, non-admin credential -> 403, never 200
//   - a valid admin credential -> 200 (or at least not 401/403/5xx)
func TestControlAndStatusEndpointsRequireAdmin(t *testing.T) {
	newServer := func(t *testing.T) http.Handler {
		fb := &fakeBackfill{cfg: indexer.RuntimeConfig{MinWorkers: 1, MaxWorkers: 2, BatchSize: 16, PageSize: 20}}
		manager := &fakeEmbedderManager{}
		return newControlTestServer(t, fb, manager)
	}

	for _, c := range controlAndStatusRequests {
		t.Run(c.name+"/no credential", func(t *testing.T) {
			handler := newServer(t)
			req := newControlRequest(c)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body = %s, want 401 with no credential", rec.Code, rec.Body.String())
			}
		})

		t.Run(c.name+"/non-admin credential", func(t *testing.T) {
			handler := newServer(t)
			req := newControlRequest(c)
			req.Header.Set(authTokenHeader, nonAdminToken)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s, want 403 for a valid non-admin credential", rec.Code, rec.Body.String())
			}
		})

		t.Run(c.name+"/admin credential", func(t *testing.T) {
			handler := newServer(t)
			req := newControlRequest(c)
			req.Header.Set(authTokenHeader, adminToken)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Fatalf("status = %d, body = %s; an admin credential must not be rejected as unauthenticated/unauthorised", rec.Code, rec.Body.String())
			}
			if rec.Code >= 500 {
				t.Fatalf("status = %d, body = %s; expected this admin-authenticated route to work, not fail server-side", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestControlAndStatusEndpointsFailClosedWithoutAdminChecker mirrors
// TestProtectedEndpointFailsClosedWithoutKeyValidator for the admin gate:
// a Server built with admins == nil (a misconfiguration -- cmd/sidecar
// always supplies one) must fail CLOSED with 500 for a credential that
// otherwise validates, never fall through to 200 because there was
// nothing to check admin status against.
func TestControlAndStatusEndpointsFailClosedWithoutAdminChecker(t *testing.T) {
	fb := &fakeBackfill{}
	keys := newFakeKeyValidator(map[string]int64{adminToken: adminUserID})
	srv, err := New(fb, nil, nil, nil, nil, nil, nil, keys, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set(authTokenHeader, adminToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500 (fail closed, not open) with no AdminChecker configured", rec.Code, rec.Body.String())
	}
}
