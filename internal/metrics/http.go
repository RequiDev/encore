package metrics

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// scrapeTimeout bounds one scrape. A gather that hangs would otherwise hold a
// server worker for as long as the scraper is willing to wait.
const scrapeTimeout = 10 * time.Second

// maxConcurrentScrapes limits how many scrapes gather at once. Prometheus
// federation and a misconfigured second scraper can otherwise multiply the cost
// of collection during exactly the incident the metrics are needed for.
const maxConcurrentScrapes = 4

// Handler returns the /metrics endpoint.
//
// A non-empty username enables HTTP basic auth. Credentials are compared as
// SHA-256 digests in constant time, so neither the length nor the content of the
// configured values can be recovered by timing the response. An empty username
// exposes the endpoint unauthenticated, which is only appropriate when the
// listener is not reachable from outside the deployment.
func (r *Registry) Handler(username, password string) http.Handler {
	h := promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		// Report a gather failure to the scraper rather than serving a partial
		// exposition that would silently misreport the instance.
		ErrorHandling:       promhttp.HTTPErrorOnError,
		Registry:            r.reg,
		MaxRequestsInFlight: maxConcurrentScrapes,
		Timeout:             scrapeTimeout,
		EnableOpenMetrics:   true,
	})
	if username == "" {
		return h
	}
	return basicAuth(username, password, h)
}

// basicAuth wraps h in a constant-time HTTP basic auth check.
func basicAuth(username, password string, h http.Handler) http.Handler {
	// Digesting first makes the comparison independent of credential length,
	// which subtle.ConstantTimeCompare alone does not guarantee: it returns early
	// when the two slices differ in size.
	wantUser := sha256.Sum256([]byte(username))
	wantPass := sha256.Sum256([]byte(password))

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotUser, gotPass, ok := req.BasicAuth()
		if !ok {
			denied(w)
			return
		}
		haveUser := sha256.Sum256([]byte(gotUser))
		havePass := sha256.Sum256([]byte(gotPass))
		// Both comparisons always run: short-circuiting on the username would
		// leak, through timing, whether the username alone was correct.
		userOK := subtle.ConstantTimeCompare(haveUser[:], wantUser[:])
		passOK := subtle.ConstantTimeCompare(havePass[:], wantPass[:])
		if userOK&passOK != 1 {
			denied(w)
			return
		}
		h.ServeHTTP(w, req)
	})
}

func denied(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="encore metrics", charset="UTF-8"`)
	http.Error(w, "unauthorised", http.StatusUnauthorized)
}

// InstrumentHandler wraps h so that every request it serves is counted, timed
// and reflected in the in-flight gauge.
//
// route is the template the handler is mounted on, supplied by the caller rather
// than read from the request, because the request path carries identifiers and
// would give the metric one series per user.
func (r *Registry) InstrumentHandler(route string, h http.Handler) http.Handler {
	rt := label(route, "unknown")
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.IncInFlight()
		defer r.DecInFlight()

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		t := NewTimer(nil)
		// The deferred observation also records requests whose handler panics, so
		// a crash shows up as a slow 500 rather than as a gap in the series.
		defer func() { r.ObserveHTTP(req.Method, rt, rec.status, t.Stop()) }()

		h.ServeHTTP(rec, req)
	})
}

// recorder remembers the status code a handler wrote.
//
// It forwards the optional interfaces a handler may reach for, and implements
// Unwrap so http.ResponseController keeps working through the wrapper. Without
// that, adding instrumentation to a route would quietly break streaming or a
// WebSocket upgrade on it.
type recorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *recorder) WriteHeader(code int) {
	// 1xx responses are informational: net/http allows several before the real
	// status, and none of them is the one worth labelling.
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

func (w *recorder) Write(b []byte) (int, error) {
	w.implicitOK()
	return w.ResponseWriter.Write(b)
}

// implicitOK records the 200 that net/http sends on the first write when the
// handler never called WriteHeader.
func (w *recorder) implicitOK() {
	if !w.written {
		w.status, w.written = http.StatusOK, true
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		w.implicitOK()
		f.Flush()
	}
}

func (w *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("metrics: the wrapped ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

func (w *recorder) ReadFrom(src io.Reader) (int64, error) {
	w.implicitOK()
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(w.ResponseWriter, src)
}
