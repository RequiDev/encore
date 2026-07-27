package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// TestSecurityHeaders pins the headers docs/security.md promises. They are on
// every response, including the operational ones, because a browser that
// somehow reaches /healthz is still a browser.
func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.do(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"X-Frame-Options":        "SAMEORIGIN",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("Content-Security-Policy %q is missing %q", csp, directive)
		}
	}
	if rec.Header().Get(RequestIDHeaderName) == "" {
		t.Error("the response carries no request id")
	}
}

// TestFrameAncestorsComeFromConfiguration proves the CSP is driven by
// ENCORE_FRAME_ANCESTORS rather than hard-coded.
func TestFrameAncestorsComeFromConfiguration(t *testing.T) {
	ts := newTestServer(t)
	ts.cfg.HTTP.FrameAncestors = []string{"https://dashboard.example.com"}
	ts.handler = ts.buildHandler()

	rec := ts.do(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors https://dashboard.example.com") {
		t.Fatalf("Content-Security-Policy = %q, want the configured frame ancestor", csp)
	}
}

// TestCSRFAcceptsAMatchingToken is the happy path: the header equals both the
// cookie and the token bound to the session row.
func TestCSRFAcceptsAMatchingToken(t *testing.T) {
	ts := newTestServer(t)

	r := ts.signedIn(httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil))
	r.Header.Set(CSRFHeaderName, ts.sessions.session.CSRFToken)

	rec := ts.do(r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /api/auth/logout = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if len(ts.sessions.deleted) != 1 {
		t.Fatalf("the session was not deleted: %v", ts.sessions.deleted)
	}
}

// TestCSRFRejections walks every way the double-submit check can fail. All of
// them must fail closed with the same code, and none of them may reach the
// handler.
func TestCSRFRejections(t *testing.T) {
	cases := []struct {
		name   string
		header string
		cookie string
	}{
		{"no header at all", "", "csrf-token-value"},
		{"no cookie at all", "csrf-token-value", ""},
		{"header does not match the cookie", "another-token", "csrf-token-value"},
		{"header and cookie agree but the session does not", "forged-token", "forged-token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestServer(t)

			r := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
			r.AddCookie(&http.Cookie{Name: ts.cfg.Security.CookieName, Value: "session-cookie-value"})
			if c.cookie != "" {
				r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: c.cookie})
			}
			if c.header != "" {
				r.Header.Set(CSRFHeaderName, c.header)
			}

			rec := ts.do(r)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if code := errorCodeOf(t, rec); code != CodeCSRF {
				t.Fatalf("code = %q, want %q", code, CodeCSRF)
			}
			if len(ts.sessions.deleted) != 0 {
				t.Fatal("the handler ran despite the CSRF failure")
			}
		})
	}
}

// TestCSRFIgnoresSafeMethods proves a read is never asked for a token; the
// bootstrap call would otherwise be impossible, since the client learns the
// token from it.
func TestCSRFIgnoresSafeMethods(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.do(ts.signedIn(httptest.NewRequest(http.MethodGet, "/api/me", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"csrfToken":"csrf-token-value"`) {
		t.Fatalf("the bootstrap call did not carry the session's csrf token: %s", rec.Body.String())
	}
}

// TestAnonymousRequestsAreUnauthenticated checks that a caller with no session
// is told so, rather than being reported as a CSRF failure.
func TestAnonymousRequestsAreUnauthenticated(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.do(httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/me = %d, want 401", rec.Code)
	}
	if code := errorCodeOf(t, rec); code != CodeUnauthenticated {
		t.Fatalf("code = %q, want %q", code, CodeUnauthenticated)
	}
}

// TestDisabledAccountIsRefusedAtTheDoor checks that a session belonging to a
// deactivated account is refused with the code the client can explain, and that
// its cookies are cleared on the way out.
func TestDisabledAccountIsRefusedAtTheDoor(t *testing.T) {
	ts := newTestServer(t)
	ts.sessions.lookup = domain.ErrAccountDisabled

	rec := ts.do(ts.signedIn(httptest.NewRequest(http.MethodGet, "/api/me", nil)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := errorCodeOf(t, rec); code != CodeAccountDisabled {
		t.Fatalf("code = %q, want %q", code, CodeAccountDisabled)
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("the session cookie was not cleared")
	}
}

// TestSessionIsTouchedAtMostOncePerMinute pins the rule that keeps a read-only
// dashboard poll from turning into a write on every request.
func TestSessionIsTouchedAtMostOncePerMinute(t *testing.T) {
	ts := newTestServer(t)

	for range 3 {
		ts.do(ts.signedIn(httptest.NewRequest(http.MethodGet, "/api/me", nil)))
	}
	if ts.sessions.touches != 1 {
		t.Fatalf("session touched %d times in the same minute, want 1", ts.sessions.touches)
	}

	ts.clock = ts.clock.Add(touchInterval + time.Second)
	ts.do(ts.signedIn(httptest.NewRequest(http.MethodGet, "/api/me", nil)))
	if ts.sessions.touches != 2 {
		t.Fatalf("session touched %d times after the interval elapsed, want 2", ts.sessions.touches)
	}
}

// TestUnknownRoutesUseTheErrorEnvelope keeps clients from having to parse a
// second shape for the paths nothing is mounted on.
func TestUnknownRoutesUseTheErrorEnvelope(t *testing.T) {
	ts := newTestServer(t)

	for _, path := range []string{"/api/nothing-here", "/nothing-here"} {
		rec := ts.do(httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, rec.Code)
		}
		if code := errorCodeOf(t, rec); code != CodeNotFound {
			t.Fatalf("GET %s code = %q, want %q", path, code, CodeNotFound)
		}
	}
}

// TestCORSIsANoOpWithoutAnAllowList is the bundled same-origin deployment: no
// origin is echoed because none is configured.
func TestCORSIsANoOpWithoutAnAllowList(t *testing.T) {
	ts := newTestServer(t)

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("Origin", "https://evil.example.com")

	rec := ts.do(r)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want no header at all", got)
	}
}

// TestCORSEchoesOnlyAllowedOrigins proves the allow-list is exact: there is no
// wildcard and no suffix matching.
func TestCORSEchoesOnlyAllowedOrigins(t *testing.T) {
	ts := newTestServer(t)
	ts.cfg.HTTP.CORSOrigins = []string{"https://client.example.com"}
	ts.handler = ts.buildHandler()

	cases := map[string]string{
		"https://client.example.com":      "https://client.example.com",
		"https://notclient.example.com":   "",
		"https://client.example.com.evil": "",
		"https://client.example.com:8443": "",
	}
	for origin, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.Header.Set("Origin", origin)

		rec := ts.do(r)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("origin %s echoed %q, want %q", origin, got, want)
		}
	}
}

// TestRecovererTurnsAPanicIntoA500 checks that a panicking handler produces the
// error envelope and never a stack trace.
func TestRecovererTurnsAPanicIntoA500(t *testing.T) {
	ts := newTestServer(t)
	handler := ts.recoverer(ts.requestID(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("something came apart")
	})))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := errorCodeOf(t, rec); code != CodeInternal {
		t.Fatalf("code = %q, want %q", code, CodeInternal)
	}
	if body := rec.Body.String(); strings.Contains(body, "something came apart") || strings.Contains(body, "goroutine") {
		t.Fatalf("the response leaked the panic: %s", body)
	}
}

// TestSanitiseRequestID keeps a forged header out of the log line it would
// otherwise be able to shape.
func TestSanitiseRequestID(t *testing.T) {
	cases := map[string]string{
		"abc-123_.4":   "abc-123_.4",
		"":             "",
		"has space":    "",
		"has\nnewline": "",
		strings.Repeat("a", maxRequestIDLength+1): "",
	}
	for in, want := range cases {
		if got := sanitiseRequestID(in); got != want {
			t.Errorf("sanitiseRequestID(%q) = %q, want %q", in, got, want)
		}
	}
}
