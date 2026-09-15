// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file is task 17's own required test coverage: every protected data
// endpoint (GET /api/search, /api/similar, /api/article) rejects a
// missing or unknown API key, accepts and correctly SCOPES a valid one,
// stops accepting a key the instant its api_keys row is gone (simulated
// here via fakeKeyValidator.revoke), and every endpoint this task
// deliberately left unauthenticated (server.go's own Server doc comment
// explains which, and why) stays that way. Per the brief: assert missing
// and unknown token cases PER endpoint, not once for the group -- this
// project has repeatedly shipped a guard that covered one call site out
// of several.
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

// newAuthTestServer builds a full Server over keys, wired with fresh
// fakeSearcher/fakeArticles so a subtest can assert whether they were
// actually reached. Unlike newTestSearchServer/newTestArticleServer, the
// returned handler is the RAW server handler -- NOT wrapped in
// authedHandler -- because these tests are specifically about what
// happens with no header, an unrecognised header, or a header that stops
// working mid-test.
func newAuthTestServer(t *testing.T, keys APIKeyValidator) (handler http.Handler, fs *fakeSearcher, fa *fakeArticles) {
	t.Helper()
	fs = &fakeSearcher{}
	fa = &fakeArticles{byID: map[int64]*store.ArticleDetail{42: {ID: 42, Title: "t"}}}
	srv, err := New(&fakeBackfill{}, nil, fs, nil, fa, nil, nil, keys)
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
			handler, fs, fa := newAuthTestServer(t, newFakeKeyValidator(map[string]int64{testAuthToken: testAuthUserID}))

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
			handler, fs, fa := newAuthTestServer(t, newFakeKeyValidator(map[string]int64{testAuthToken: testAuthUserID}))

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
			handler, _, _ := newAuthTestServer(t, newFakeKeyValidator(map[string]int64{testAuthToken: testAuthUserID}))

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
			handler, _, _ := newAuthTestServer(t, keys)

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
	handler, _, _ := newAuthTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets", nil)
	req.Header.Set(authTokenHeader, testAuthToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500 (fail closed, not open) with no APIKeyValidator configured", rec.Code, rec.Body.String())
	}
}

// TestControlAndStatusEndpointsRemainUnauthenticated pins this task's own
// deliberate decision (see server.go's Server doc comment for the
// reasoning): the status page, GET /api/status, and every backfill/
// embedder control endpoint stay open with NO X-Auth-Token header at
// all -- exactly as they behaved before this task. This exists so a
// later change that silently narrows that decision (accidentally routing
// one of these through requireAPIKey) is caught by a failing test, not
// discovered by an operator locked out of their own pause button.
func TestControlAndStatusEndpointsRemainUnauthenticated(t *testing.T) {
	fb := &fakeBackfill{cfg: indexer.RuntimeConfig{MinWorkers: 1, MaxWorkers: 2, BatchSize: 16, PageSize: 20}}
	manager := &fakeEmbedderManager{}
	// keys is nil here, deliberately: these routes must not even consult
	// a validator, whether or not one is configured.
	srv, err := New(fb, nil, nil, nil, nil, nil, manager, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := srv.Handler()

	cases := []struct {
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

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req *http.Request
			if c.body != "" {
				req = httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(c.method, c.target, nil)
			}
			// Deliberately NO X-Auth-Token header anywhere in this test.
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("status = 401 with no API key; this endpoint is supposed to remain unauthenticated (body: %s)", rec.Body.String())
			}
			if rec.Code >= 500 {
				t.Fatalf("status = %d, body = %s; expected this unauthenticated route to work, not fail server-side", rec.Code, rec.Body.String())
			}
		})
	}
}
