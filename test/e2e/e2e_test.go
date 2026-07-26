//go:build integration

// Package e2e drives Encore the way a browser and a worker container do: over
// real HTTP, against a real PostgreSQL, through the real router and the real
// import pipeline.
//
// Only Spotify is faked, because it is the one thing a test cannot own. The
// fake speaks the real protocol — an authorisation redirect, a code exchange, a
// /v1/me profile, a recently-played page — so the flows exercised here are the
// same ones a live instance runs.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/httpapi"
	"github.com/requi/encore/internal/importer"
	"github.com/requi/encore/internal/metrics"
	"github.com/requi/encore/internal/spotify"
	"github.com/requi/encore/internal/stats"
	encoresync "github.com/requi/encore/internal/sync"
	"github.com/requi/encore/test/harness"
)

// --- the fake Spotify -------------------------------------------------------

type spotifyStub struct {
	server *httptest.Server

	// profile is what /v1/me returns; tests change it to sign in as someone else.
	profile map[string]any
	// plays is what recently-played returns on the next call.
	plays []map[string]any
}

func newSpotifyStub(t *testing.T) *spotifyStub {
	t.Helper()
	s := &spotifyStub{profile: meProfile("listener-one", "Listener One")}
	mux := http.NewServeMux()

	// The authorisation screen. A real one asks the human; this one bounces
	// straight back with a code, which is what makes the flow testable.
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		redirect := r.URL.Query().Get("redirect_uri")
		state := r.URL.Query().Get("state")
		if r.URL.Query().Get("code_challenge_method") != "S256" {
			http.Error(w, "Encore must use PKCE with S256", http.StatusBadRequest)
			return
		}
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("code", "the-authorisation-code")
		q.Set("state", state)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})

	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		writeJSON(w, map[string]any{
			"access_token":  "user-access-token",
			"refresh_token": "user-refresh-token",
			"token_type":    "Bearer",
			"scope":         strings.Join(config.DefaultScopes(), " "),
			"expires_in":    3600,
		})
	})

	mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.profile)
	})

	mux.HandleFunc("/v1/me/player/recently-played", func(w http.ResponseWriter, r *http.Request) {
		items := s.plays
		s.plays = nil // one page, then exhausted
		writeJSON(w, map[string]any{"items": items, "cursors": map[string]any{}})
	})

	// The catalogue endpoints answer for any id, so enrichment has something to
	// find if a test chooses to run it.
	catalogue := func(key string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ids := strings.Split(r.URL.Query().Get("ids"), ",")
			out := make([]any, 0, len(ids))
			for _, id := range ids {
				if id == "" {
					continue
				}
				out = append(out, map[string]any{"id": id, "name": "Name " + id})
			}
			writeJSON(w, map[string]any{key: out})
		}
	}
	mux.HandleFunc("/v1/tracks", catalogue("tracks"))
	mux.HandleFunc("/v1/albums", catalogue("albums"))
	mux.HandleFunc("/v1/artists", catalogue("artists"))

	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func meProfile(id, name string) map[string]any {
	return map[string]any{
		"id": id, "display_name": name, "email": id + "@example.test",
		"product": "premium",
		"images":  []any{map[string]any{"url": "https://i.example/" + id}},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- the instance under test ------------------------------------------------

type instance struct {
	t       *testing.T
	env     *harness.Env
	server  *httptest.Server
	stub    *spotifyStub
	cfg     *config.Config
	rig     *harness.Rig
	poller  *encoresync.Poller
	baseURL string
}

func newInstance(t *testing.T) *instance {
	t.Helper()
	stub := newSpotifyStub(t)
	rig := harness.NewRig(t, nil)
	env := rig.Env

	key := strings.Repeat("k", 32)
	cfg, err := config.LoadFrom(map[string]string{
		"ENCORE_ENV":                   "development",
		"ENCORE_PUBLIC_URL":            "http://api.test",
		"ENCORE_WEB_URL":               "http://web.test",
		"ENCORE_DATABASE_URL":          os.Getenv(harness.TestDatabaseEnv),
		"ENCORE_SPOTIFY_CLIENT_ID":     "client-id",
		"ENCORE_SPOTIFY_CLIENT_SECRET": "client-secret",
		"ENCORE_ENCRYPTION_KEY":        encodeKey(key),
		"ENCORE_SPOTIFY_API_BASE_URL":  stub.server.URL,
		"ENCORE_SPOTIFY_AUTH_BASE_URL": stub.server.URL,
		"ENCORE_IMPORT_DIR":            rig.Cfg.Dir,
		"ENCORE_COOKIE_SECURE":         "false",
		"ENCORE_METRICS_ENABLED":       "true",
	})
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}

	client := spotify.NewClient(cfg.Spotify, harness.Discard(), spotify.WithBaseURL(stub.server.URL))
	poller, err := encoresync.NewPoller(cfg.Sync, encoresync.Deps{
		Store: env.Store, Accounts: env.Accounts, Listens: env.Listens,
		Catalog: env.Catalog, Spotify: client, Logger: harness.Discard(),
	})
	if err != nil {
		t.Fatalf("build poller: %v", err)
	}

	intake, err := importer.NewIntake(rig.Cfg, env.Store, env.Imports, harness.Discard())
	if err != nil {
		t.Fatalf("build intake: %v", err)
	}

	api, err := httpapi.New(httpapi.Deps{
		Config: cfg, Store: env.Store, Accounts: env.Accounts, Catalog: env.Catalog,
		Listens: env.Listens, Imports: env.Imports, Stats: stats.New(env.Store),
		Intake: intake, Spotify: client, Metrics: metrics.New(),
		Logger: harness.Discard(), Version: "test",
		SyncNow: func(ctx context.Context, userID uuid.UUID) (httpapi.SyncOutcome, error) {
			res, err := poller.SyncUser(ctx, userID)
			out := httpapi.SyncOutcome{
				Fetched: res.Fetched, Imported: res.Imported,
				Duplicates: res.Duplicates, Skipped: res.Skipped,
			}
			if !res.NewestPlayedAt.IsZero() {
				newest := res.NewestPlayedAt.UTC()
				out.NewestAt = &newest
			}
			return out, err
		},
	})
	if err != nil {
		t.Fatalf("build api: %v", err)
	}

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	return &instance{t: t, env: env, server: srv, stub: stub, cfg: cfg, rig: rig,
		poller: poller, baseURL: srv.URL}
}

func encodeKey(raw string) string {
	// config accepts base64; the test key is 32 ASCII bytes.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := []byte(raw)
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		out.WriteByte(alphabet[(v>>18)&63])
		out.WriteByte(alphabet[(v>>12)&63])
		if n > 1 {
			out.WriteByte(alphabet[(v>>6)&63])
		} else {
			out.WriteByte('=')
		}
		if n > 2 {
			out.WriteByte(alphabet[v&63])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

// --- a browser --------------------------------------------------------------

// browser is an HTTP client with a cookie jar that does not follow redirects,
// so a test can inspect each hop of the OAuth journey.
type browser struct {
	t    *testing.T
	http *http.Client
	base string
}

func (i *instance) browser() *browser {
	i.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		i.t.Fatalf("cookie jar: %v", err)
	}
	return &browser{t: i.t, base: i.baseURL, http: &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (b *browser) do(method, path string, body io.Reader, contentType string) *http.Response {
	b.t.Helper()
	req, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		b.t.Fatalf("build request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if method != http.MethodGet && method != http.MethodHead {
		if token := b.csrf(); token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
	}
	resp, err := b.http.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// csrf reads the non-HttpOnly companion cookie, exactly as the real client does.
func (b *browser) csrf() string {
	u, _ := url.Parse(b.base)
	for _, c := range b.http.Jar.Cookies(u) {
		if c.Name == httpapi.CSRFCookieName {
			return c.Value
		}
	}
	return ""
}

func (b *browser) get(path string) *http.Response { return b.do(http.MethodGet, path, nil, "") }

func (b *browser) postJSON(path string, body any) *http.Response {
	b.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		b.t.Fatalf("encode body: %v", err)
	}
	return b.do(http.MethodPost, path, strings.NewReader(string(raw)), "application/json")
}

func (b *browser) patchJSON(path string, body any) *http.Response {
	b.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		b.t.Fatalf("encode body: %v", err)
	}
	return b.do(http.MethodPatch, path, strings.NewReader(string(raw)), "application/json")
}

// decode reads a JSON response and fails the test on a non-2xx status.
func decode[T any](t *testing.T, resp *http.Response, want int) T {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s: status %d, want %d; body: %s", resp.Request.URL.Path, resp.StatusCode, want, body)
	}
	var out T
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode %s: %v; body: %s", resp.Request.URL.Path, err, body)
		}
	}
	return out
}

// signIn walks the whole OAuth journey and leaves the browser authenticated.
func (i *instance) signIn(b *browser) map[string]any {
	i.t.Helper()

	// 1. Encore redirects to Spotify.
	resp := b.get("/api/auth/spotify/login")
	if resp.StatusCode != http.StatusFound {
		i.t.Fatalf("login returned %d, want a redirect", resp.StatusCode)
	}
	authURL := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.HasPrefix(authURL, i.stub.server.URL) {
		i.t.Fatalf("login redirected to %q, want the Spotify authorisation endpoint", authURL)
	}

	// 2. Spotify sends the browser back with a code and the state.
	authResp, err := b.http.Get(authURL)
	if err != nil {
		i.t.Fatalf("follow authorisation redirect: %v", err)
	}
	callback := authResp.Header.Get("Location")
	authResp.Body.Close()
	if callback == "" {
		i.t.Fatal("the authorisation screen did not redirect back")
	}
	cbURL, err := url.Parse(callback)
	if err != nil {
		i.t.Fatalf("parse callback: %v", err)
	}

	// 3. Encore consumes the state, exchanges the code and issues a session.
	cb := b.get(cbURL.Path + "?" + cbURL.RawQuery)
	defer cb.Body.Close()
	if cb.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(cb.Body)
		i.t.Fatalf("callback returned %d, want a redirect into the web client; body: %s", cb.StatusCode, body)
	}
	if loc := cb.Header.Get("Location"); !strings.HasPrefix(loc, i.cfg.Instance.WebURL) {
		i.t.Fatalf("callback redirected to %q, want somewhere under %q", loc, i.cfg.Instance.WebURL)
	}

	return decode[map[string]any](i.t, b.get("/api/me"), http.StatusOK)
}

// --- the tests --------------------------------------------------------------

func TestSignInWithSpotifyCreatesTheFirstAdministrator(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()

	if resp := b.get("/api/me"); resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("an anonymous /api/me returned %d, want 401", resp.StatusCode)
	}

	me := inst.signIn(b)
	user, _ := me["user"].(map[string]any)
	if user["spotifyUserId"] != "listener-one" {
		t.Fatalf("signed in as %v, want listener-one", user["spotifyUserId"])
	}
	if user["role"] != "admin" {
		t.Fatalf("the first user has role %v, want admin", user["role"])
	}
	spot, _ := me["spotify"].(map[string]any)
	if spot["connected"] != true {
		t.Fatal("the session reports Spotify as not connected after signing in through it")
	}
	if me["csrfToken"] == nil || me["csrfToken"] == "" {
		t.Fatal("no CSRF token was issued")
	}

	// The tokens are in the database, and they are not in plaintext.
	var sealed []byte
	if err := inst.env.Pool.QueryRow(inst.env.Ctx(),
		`SELECT access_token_enc FROM spotify_credentials`).Scan(&sealed); err != nil {
		t.Fatalf("read stored credentials: %v", err)
	}
	if strings.Contains(string(sealed), "user-access-token") {
		t.Fatal("the Spotify access token is stored in plaintext")
	}
}

func TestReplayedOAuthStateIsRefused(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()

	resp := b.get("/api/auth/spotify/login")
	authURL := resp.Header.Get("Location")
	resp.Body.Close()

	authResp, err := b.http.Get(authURL)
	if err != nil {
		t.Fatalf("authorise: %v", err)
	}
	callback := authResp.Header.Get("Location")
	authResp.Body.Close()
	cbURL, _ := url.Parse(callback)
	path := cbURL.Path + "?" + cbURL.RawQuery

	first := b.get(path)
	first.Body.Close()
	if first.StatusCode != http.StatusFound {
		t.Fatalf("first callback returned %d", first.StatusCode)
	}

	// The same state again. It was consumed by the first call, so this must not
	// mint a second session.
	second := b.get(path)
	defer second.Body.Close()
	loc := second.Header.Get("Location")
	if second.StatusCode == http.StatusFound && !strings.Contains(loc, "error=") {
		t.Fatalf("a replayed OAuth state was accepted (redirected to %q); "+
			"state must be single use", loc)
	}
}

func TestCSRFIsRequiredForStateChangingRequests(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	// With the header, it works.
	ok := b.patchJSON("/api/me", map[string]any{"timezone": "Europe/Berlin"})
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("a well-formed PATCH returned %d, want 200", ok.StatusCode)
	}

	// Without it, it must fail closed.
	req, _ := http.NewRequest(http.MethodPatch, inst.baseURL+"/api/me",
		strings.NewReader(`{"timezone":"UTC"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a PATCH with no CSRF token returned %d, want 403", resp.StatusCode)
	}
}

func TestSyncFlowThroughTheAPI(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	at := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{
		playItem("e2e00000000000000001a", at),
		playItem("e2e00000000000000002b", at.Add(20*time.Minute)),
	}

	resp := b.postJSON("/api/sync/now", nil)
	result := decode[map[string]any](t, resp, http.StatusOK)
	if n, _ := result["imported"].(float64); int(n) != 2 {
		t.Fatalf("sync reported %v imported, want 2 (full result: %v)", result["imported"], result)
	}
	if result["newestAt"] == nil {
		t.Fatal("sync did not report the newest play it accounted for")
	}

	me := decode[map[string]any](t, b.get("/api/me"), http.StatusOK)
	user := me["user"].(map[string]any)
	userID := uuid.MustParse(user["id"].(string))
	if got := inst.env.CountListens(userID); got != 2 {
		t.Fatalf("the database holds %d listens after a sync, want 2", got)
	}

	// And the statistics endpoint sees them.
	from := at.Add(-24 * time.Hour).Format(time.RFC3339)
	to := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	summary := decode[map[string]any](t, b.get(
		fmt.Sprintf("/api/stats/summary?from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to))),
		http.StatusOK)
	if n, _ := summary["listens"].(float64); int(n) != 2 {
		t.Fatalf("the summary reports %v listens, want 2", summary["listens"])
	}
}

func playItem(trackID string, at time.Time) map[string]any {
	return map[string]any{
		"played_at": at.Format(time.RFC3339),
		"track": map[string]any{
			"id": trackID, "name": "Track " + trackID, "duration_ms": 210000,
			"album": map[string]any{
				"id": "alb" + trackID[3:], "name": "Album", "album_type": "album",
				"release_date": "2019-06-01", "release_date_precision": "day",
				"artists": []any{map[string]any{"id": "art" + trackID[3:], "name": "Artist"}},
			},
			"artists": []any{map[string]any{"id": "art" + trackID[3:], "name": "Artist"}},
		},
	}
}

func TestImportFlowThroughTheAPI(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	me := inst.signIn(b)
	userID := uuid.MustParse(me["user"].(map[string]any)["id"].(string))

	path := filepath.Join(t.TempDir(), "Streaming_History_Audio_2015-2017_0.json")
	total := harness.WriteExtendedExport(t, path, harness.GenOptions{
		Records: 400, Seed: 4242, PodcastEvery: 20,
	})

	// Upload, as the browser does: multipart, streamed.
	body, contentType := multipartFile(t, path)
	resp := b.do(http.MethodPost, "/api/imports", body, contentType)
	created := decode[map[string]any](t, resp, http.StatusAccepted)
	job := created["job"].(map[string]any)
	jobID := uuid.MustParse(job["id"].(string))
	if job["status"] != string(domain.ImportQueued) {
		t.Fatalf("a new job has status %v, want queued", job["status"])
	}

	// The worker picks it up. This is the real Runner, in-process.
	inst.rig.Drain(inst.env.Ctx())

	detail := decode[map[string]any](t, b.get("/api/imports/"+jobID.String()), http.StatusOK)
	if detail["status"] != string(domain.ImportCompleted) {
		t.Fatalf("job status %v (%v: %v), want completed",
			detail["status"], detail["errorCode"], detail["errorMessage"])
	}
	counters := detail["counters"].(map[string]any)
	imported := int64(counters["imported"].(float64))
	processed := imported +
		int64(counters["duplicates"].(float64)) +
		int64(counters["skipped"].(float64)) +
		int64(counters["rejected"].(float64))
	if processed != int64(total) {
		t.Fatalf("the API reports %d records accounted for, want %d", processed, total)
	}
	if got := inst.env.CountListens(userID); got != imported {
		t.Fatalf("the API claims %d imported but the database holds %d", imported, got)
	}

	// Another user's job must be invisible, not merely unlisted.
	other := inst.browser()
	inst.stub.profile = meProfile("listener-two", "Listener Two")
	inst.signIn(other)
	forbidden := other.get("/api/imports/" + jobID.String())
	forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusNotFound {
		t.Fatalf("another user reading the job got %d, want 404", forbidden.StatusCode)
	}
}

func multipartFile(t *testing.T, path string) (io.Reader, string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("files", filepath.Base(path))
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return strings.NewReader(buf.String()), mw.FormDataContentType()
}

func TestAdminControlsRegistrations(t *testing.T) {
	inst := newInstance(t)
	admin := inst.browser()
	inst.signIn(admin) // the first user, therefore the administrator

	settings := decode[map[string]any](t, admin.get("/api/admin/settings"), http.StatusOK)
	if settings["registrationsEnabled"] != true {
		t.Fatal("a fresh instance should start open to registrations")
	}

	closed := admin.patchJSON("/api/admin/settings", map[string]any{"registrationsEnabled": false})
	closed.Body.Close()
	if closed.StatusCode != http.StatusOK {
		t.Fatalf("closing registrations returned %d", closed.StatusCode)
	}

	// A different Spotify identity is now refused.
	inst.stub.profile = meProfile("listener-three", "Listener Three")
	stranger := inst.browser()
	resp := stranger.get("/api/auth/spotify/login")
	authURL := resp.Header.Get("Location")
	resp.Body.Close()
	authResp, err := stranger.http.Get(authURL)
	if err != nil {
		t.Fatalf("authorise: %v", err)
	}
	callback := authResp.Header.Get("Location")
	authResp.Body.Close()
	cbURL, _ := url.Parse(callback)
	cb := stranger.get(cbURL.Path + "?" + cbURL.RawQuery)
	cb.Body.Close()

	if loc := cb.Header.Get("Location"); !strings.Contains(loc, "error=") {
		t.Fatalf("an unknown identity signed in while registrations were closed (redirected to %q)", loc)
	}
	if got := inst.env.ScalarInt(`SELECT count(*) FROM users`); got != 1 {
		t.Fatalf("%d users exist, want 1: registration was closed", got)
	}

	// A non-administrator cannot reach the settings at all.
	inst.stub.profile = meProfile("listener-one", "Listener One")
	admin2 := inst.browser()
	inst.signIn(admin2)
	if _, err := inst.env.Accounts.Users.SetRole(inst.env.Ctx(), inst.env.Store.DB(),
		userIDOf(t, inst, "listener-one"), domain.RoleUser); err != nil {
		t.Fatalf("demote: %v", err)
	}
	denied := admin2.get("/api/admin/settings")
	denied.Body.Close()
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("a demoted user reading admin settings got %d, want 403: the guard must "+
			"re-read the role rather than trust the session", denied.StatusCode)
	}
}

func userIDOf(t *testing.T, inst *instance, spotifyID string) uuid.UUID {
	t.Helper()
	u, err := inst.env.Accounts.Users.GetBySpotifyUserID(inst.env.Ctx(), inst.env.Store.DB(), spotifyID)
	if err != nil {
		t.Fatalf("look up %s: %v", spotifyID, err)
	}
	return u.ID
}

func TestHealthAndReadiness(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()

	health := b.get("/healthz")
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("/healthz returned %d, want 200", health.StatusCode)
	}

	ready := b.get("/readyz")
	defer ready.Body.Close()
	if ready.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(ready.Body)
		t.Fatalf("/readyz returned %d, want 200; body: %s", ready.StatusCode, body)
	}

	m := b.get("/metrics")
	defer m.Body.Close()
	if m.StatusCode != http.StatusOK {
		t.Fatalf("/metrics returned %d, want 200", m.StatusCode)
	}
	body, _ := io.ReadAll(m.Body)
	if !strings.Contains(string(body), "encore_") {
		t.Fatal("/metrics served nothing recognisably Encore's")
	}
}
