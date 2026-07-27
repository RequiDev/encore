package httpapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/crypto"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
)

// Header and cookie names shared with the web client. They are part of the
// contract in docs/api.md.
const (
	// CSRFCookieName is the non-HttpOnly companion to the session cookie.
	CSRFCookieName = "encore_csrf"
	// CSRFHeaderName is where the client echoes that cookie back.
	CSRFHeaderName = "X-CSRF-Token"
	// RequestIDHeaderName correlates a response with its log line.
	RequestIDHeaderName = "X-Request-Id"
)

// maxRequestIDLength bounds an inbound request id. A proxy may supply one, and
// it ends up in every log record for the request, so it is length-limited and
// stripped of anything that could forge a log line.
const maxRequestIDLength = 128

// callbackPath is the one unsafe-method-exempt route: the OAuth callback is a
// top-level GET navigation from Spotify, which carries no CSRF token and needs
// none, since it authenticates itself with a single-use state parameter.
const callbackPath = "/api/auth/spotify/callback"

// --- request context -------------------------------------------------------

type ctxKeyRequestID struct{}
type ctxKeyAuth struct{}
type ctxKeyRoute struct{}

// authContext is the caller's identity for the duration of one request.
type authContext struct {
	session domain.Session
	user    domain.User
}

// authFrom returns the signed-in caller, if there is one.
func authFrom(ctx context.Context) (authContext, bool) {
	a, ok := ctx.Value(ctxKeyAuth{}).(authContext)
	return a, ok
}

// RequestIDFrom returns the request id attached to a context, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return id
}

// routeRecorder carries the matched route template back out to the middleware
// that logs and measures the request.
//
// It is a pointer stored once at the top of the chain and filled in by the
// router, because the concrete path carries user and entity identifiers: a
// metric labelled with those grows a series per user and eventually costs more
// than the application it observes.
type routeRecorder struct{ template string }

func routeFrom(ctx context.Context) *routeRecorder {
	rec, _ := ctx.Value(ctxKeyRoute{}).(*routeRecorder)
	return rec
}

// routeTemplate reports the matched route, or a fixed placeholder when nothing
// matched, so an unrouted request cannot create a label per bad URL.
func routeTemplate(ctx context.Context) string {
	if rec := routeFrom(ctx); rec != nil && rec.template != "" {
		return rec.template
	}
	return "unmatched"
}

// --- response recording ----------------------------------------------------

// statusRecorder remembers the status a handler wrote and whether anything has
// gone out yet, which is what lets the panic handler decide between sending a
// 500 and simply logging a stream that died half-way.
//
// It forwards the optional interfaces a handler may reach for and implements
// Unwrap, so http.ResponseController keeps working and the streaming export can
// still flush.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
	bytes   int64
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (w *statusRecorder) WriteHeader(code int) {
	// 1xx responses are informational; net/http allows several before the real
	// status and none of them is the one worth recording.
	if code >= 100 && code < 200 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.written {
		return
	}
	w.status, w.written = code, true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.written {
		w.status, w.written = http.StatusOK, true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.written {
			w.status, w.written = http.StatusOK, true
		}
		f.Flush()
	}
}

func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpapi: the wrapped ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

func (w *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !w.written {
		w.status, w.written = http.StatusOK, true
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		w.bytes += n
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, src)
	w.bytes += n
	return n, err
}

// --- middleware ------------------------------------------------------------

// recoverer turns a panic into a 500 and a logged stack trace.
//
// The stack never reaches the client: it names internal paths and can quote
// arguments. A response that has already begun streaming cannot be turned into
// an error, so in that case the failure is logged and the connection is left to
// be cut, which is what tells the client the body is incomplete.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := newStatusRecorder(w)
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			// A client that disconnects mid-write makes net/http panic with
			// ErrAbortHandler by design; it is not a bug and must not be logged
			// as one.
			if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(p)
			}
			logging.FromContext(r.Context()).Error("handler panicked",
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())))
			if !rec.written {
				writeJSON(rec, r, http.StatusInternalServerError, ErrorBody{Error: ErrorPayload{
					Code: CodeInternal, Message: vagueInternalMessage,
				}})
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// requestID attaches an identifier to the request, the response and the
// context logger, and installs the route recorder the rest of the chain reads.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitiseRequestID(r.Header.Get(RequestIDHeaderName))
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set(RequestIDHeaderName, id)

		ctx := context.WithValue(r.Context(), ctxKeyRequestID{}, id)
		ctx = context.WithValue(ctx, ctxKeyRoute{}, &routeRecorder{})
		ctx = logging.WithLogger(ctx, s.log.With(slog.String("request_id", id)))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sanitiseRequestID keeps an inbound identifier only when it is short and made
// of characters that cannot forge a log record or a header.
func sanitiseRequestID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxRequestIDLength {
		return ""
	}
	for i := range v {
		c := v[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_', c == '.':
		default:
			return ""
		}
	}
	return v
}

// requestLogger emits exactly one structured line per request.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := newStatusRecorder(w)
		next.ServeHTTP(rec, r)

		lg := logging.FromContext(r.Context())
		attrs := []any{
			slog.String("method", r.Method),
			slog.String("route", routeTemplate(r.Context())),
			slog.Int("status", rec.status),
			slog.Duration("duration", s.now().Sub(start)),
		}
		switch {
		case rec.status >= 500:
			lg.Error("request", attrs...)
		case rec.status >= 400:
			lg.Warn("request", attrs...)
		default:
			lg.Info("request", attrs...)
		}
	})
}

// observeMetrics counts and times every request against its route template.
func (s *Server) observeMetrics(next http.Handler) http.Handler {
	if s.metrics == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.metrics.IncInFlight()
		defer s.metrics.DecInFlight()

		start := s.now()
		rec := newStatusRecorder(w)
		// Deferred so that a panicking handler is recorded as a slow 500 rather
		// than as a gap in the series.
		defer func() { s.metrics.ObserveHTTP(r.Method, routeTemplate(r.Context()), rec.status, s.now().Sub(start)) }()

		next.ServeHTTP(rec, r)
	})
}

// cors applies the exact-origin allow-list.
//
// It is a no-op when ENCORE_CORS_ORIGINS is empty, which is the correct setting
// for the bundled deployment where nginx serves the client from the same origin.
// There is no wildcard: credentials travel on every request, and "*" cannot be
// combined with credentials in any browser.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := s.cfg.HTTP.CORSOrigins
	if len(allowed) == 0 {
		return next
	}
	index := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		index[strings.ToLower(strings.TrimRight(o, "/"))] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Vary is set whatever the outcome, so a cache never serves one origin's
		// response to another.
		w.Header().Add("Vary", "Origin")

		if origin != "" {
			if _, ok := index[strings.ToLower(strings.TrimRight(origin, "/"))]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}

		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+CSRFHeaderName+", "+RequestIDHeaderName)
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets the response headers documented in docs/security.md.
//
// The policy is as tight as an API can make it: this surface serves JSON and
// redirects, so nothing may be loaded, framed or submitted from it at all.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	frameAncestors := "'none'"
	if len(s.cfg.HTTP.FrameAncestors) > 0 {
		frameAncestors = strings.Join(s.cfg.HTTP.FrameAncestors, " ")
	}
	csp := "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors " + frameAncestors

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// X-Frame-Options is the legacy sibling of frame-ancestors, kept for the
		// browsers that never learned the newer directive. Where the two disagree
		// the CSP wins, so the configured ancestor list remains authoritative.
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

// limitBody caps how much a client may send.
//
// Uploads get the much larger import limit, because they are the one route
// whose body is legitimately gigabytes; everything else is a small JSON
// document and is held to ENCORE_HTTP_MAX_REQUEST_BYTES.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			limit := s.cfg.HTTP.MaxRequestBytes
			if isUploadRequest(r) {
				limit = s.cfg.Import.MaxUploadBytes
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// isUploadRequest reports whether this is the streamed import upload. The path
// is compared literally rather than by prefix so that no other route can inherit
// the larger budget.
func isUploadRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.TrimSuffix(r.URL.Path, "/") == "/api/imports"
}

// session resolves the session cookie into a caller.
//
// A missing or lapsed cookie leaves the request anonymous; the handlers decide
// whether that is allowed. A cookie belonging to a deactivated account is
// refused outright, because every later answer would be an error anyway and the
// user deserves to be told why.
func (s *Server) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(s.cfg.Security.CookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		ctx := r.Context()
		sess, user, err := s.sessions.GetByTokenHash(ctx, s.querier, crypto.HashToken(cookie.Value))
		if err != nil {
			if errors.Is(err, domain.ErrAccountDisabled) {
				s.clearAuthCookies(w)
				writeError(w, r, err)
				return
			}
			if !errors.Is(err, domain.ErrNotFound) {
				writeError(w, r, err)
				return
			}
			// Unknown or expired: drop the cookie so the browser stops sending it
			// and continue as an anonymous request.
			s.clearAuthCookies(w)
			next.ServeHTTP(w, r)
			return
		}

		// last_seen_at is refreshed at most once a minute per session: it exists
		// so a person can see when a session was last used, not so every
		// dashboard poll becomes a write.
		if s.touched.due(sess.ID, s.now()) {
			if err := s.sessions.Touch(ctx, s.querier, sess.ID); err != nil {
				logging.FromContext(ctx).Warn("could not record session activity", logging.Err(err))
			}
		}

		ctx = context.WithValue(ctx, ctxKeyAuth{}, authContext{session: sess, user: user})
		ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With(slog.String("user_id", user.ID.String())))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// csrf enforces the double-submit check on every state-changing request.
//
// The header must equal both the non-HttpOnly cookie and the token bound to the
// session row, compared in constant time. It fails closed: a request with no
// token at all is rejected rather than allowed.
//
// An anonymous request is not checked, because CSRF is an attack on ambient
// authority and there is none to abuse; those requests are refused by the
// handler with a 401, which is the more useful answer.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) || r.URL.Path == callbackPath {
			next.ServeHTTP(w, r)
			return
		}
		auth, ok := authFrom(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		header := r.Header.Get(CSRFHeaderName)
		cookie, err := r.Cookie(CSRFCookieName)
		if err != nil || header == "" || cookie.Value == "" ||
			!crypto.EqualTokens(header, cookie.Value) ||
			!crypto.EqualTokens(header, auth.session.CSRFToken) {
			writeError(w, r, ErrCSRF())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// safeMethod reports whether a method is free of side effects and therefore
// exempt from the CSRF check.
func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// --- helpers used by handlers ----------------------------------------------

// requireUser returns the signed-in caller or the 401 the contract demands.
func requireUser(r *http.Request) (domain.User, error) {
	auth, ok := authFrom(r.Context())
	if !ok {
		return domain.User{}, ErrUnauthorized()
	}
	return auth.user, nil
}

// clientIP is the address recorded on a session.
//
// X-Forwarded-For is believed only when ENCORE_TRUST_PROXY_HEADERS says a proxy
// we control rewrites it; any client can otherwise set that header to anything.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.HTTP.TrustProxyHeaders {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if first, _, ok := strings.Cut(fwd, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(fwd)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// userAgent bounds what is stored from the User-Agent header.
func userAgent(r *http.Request) string {
	const maxLen = 400
	ua := strings.TrimSpace(r.UserAgent())
	if len(ua) > maxLen {
		return ua[:maxLen]
	}
	return ua
}

// sameSite maps the configured name onto the http package's value.
func (s *Server) sameSite() http.SameSite {
	switch s.cfg.Security.CookieSameSite {
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}

// setAuthCookies issues the session cookie and its CSRF companion.
//
// The session cookie is HttpOnly so that script cannot read it; the CSRF cookie
// deliberately is not, because the client has to read it to echo it back. That
// is safe: on its own the CSRF token authenticates nothing.
func (s *Server) setAuthCookies(w http.ResponseWriter, sessionToken, csrfToken string, expires time.Time) {
	sec := s.cfg.Security
	http.SetCookie(w, &http.Cookie{
		Name:     sec.CookieName,
		Value:    sessionToken,
		Path:     sec.CookiePath,
		Domain:   sec.CookieDomain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   sec.CookieSecure,
		SameSite: s.sameSite(),
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    csrfToken,
		Path:     sec.CookiePath,
		Domain:   sec.CookieDomain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: false,
		Secure:   sec.CookieSecure,
		SameSite: s.sameSite(),
	})
}

// clearAuthCookies expires both cookies with the same attributes they were set
// with, which is what browsers require in order to actually remove them.
func (s *Server) clearAuthCookies(w http.ResponseWriter) {
	sec := s.cfg.Security
	for _, name := range []string{sec.CookieName, CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     sec.CookiePath,
			Domain:   sec.CookieDomain,
			Expires:  time.Unix(0, 0),
			MaxAge:   -1,
			HttpOnly: name == sec.CookieName,
			Secure:   sec.CookieSecure,
			SameSite: s.sameSite(),
		})
	}
}
