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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/albumtracks"
	"github.com/RequiDev/encore/internal/artistalbums"
	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/httpapi"
	"github.com/RequiDev/encore/internal/importer"
	"github.com/RequiDev/encore/internal/metrics"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/stats"
	encoresync "github.com/RequiDev/encore/internal/sync"
	"github.com/RequiDev/encore/test/harness"
)

// --- the fake Spotify -------------------------------------------------------

type spotifyStub struct {
	server *httptest.Server

	// profile is what /v1/me returns; tests change it to sign in as someone else.
	profile map[string]any
	// plays is what recently-played returns on the next call.
	plays []map[string]any

	// grantedScopes is what /api/token reports back. Tests widen it to stand in
	// for a listener granting playlist access.
	grantedScopes []string
	// playlistItems records what was sent to each playlist, so a test can assert
	// on the tracks rather than only on the response.
	playlistItems map[string][]string
	playlistCalls int

	// albumTracks is what GET /v1/albums/{id}/tracks answers for one album,
	// keyed by id. A test sets it directly rather than driving pagination,
	// since paging through several pages is the Spotify client's own concern
	// (internal/spotify/albumtracks_test.go) and not something this stub needs
	// to reproduce: every album here answers in one page, "next": null.
	albumTracks map[string][]map[string]any
	// albumTrackReqs counts requests to that endpoint, so a test can prove the
	// lease held: a page that polls three times must still only make Spotify
	// answer once. Written from the detached fetch goroutine and read from the
	// test goroutine, with no other synchronisation between the two visible at
	// this layer — a real happens-before edge likely exists via the pool and
	// the socket, but it is incidental rather than designed, and this project
	// cannot run -race locally to lean on it. atomic.Int64 makes it correct
	// regardless.
	albumTrackReqs atomic.Int64

	// artistAlbums is what GET /v1/artists/{id}/albums answers for one artist.
	// An id no test told it about comes back as an empty page, which is the shape
	// that makes the discography unavailable — see the test that relies on it.
	artistAlbums    map[string][]map[string]any
	artistAlbumReqs atomic.Int64
}

func newSpotifyStub(t *testing.T) *spotifyStub {
	t.Helper()
	s := &spotifyStub{
		profile:       meProfile("listener-one", "Listener One"),
		grantedScopes: config.DefaultScopes(),
		playlistItems: map[string][]string{},
		albumTracks:   map[string][]map[string]any{},
		artistAlbums:  map[string][]map[string]any{},
	}
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
			"scope":         strings.Join(s.grantedScopes, " "),
			"expires_in":    3600,
		})
	})

	mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.profile)
	})

	// Playlist creation. The path carries the Spotify user id, which is how a
	// test checks Encore created it on the right account.
	mux.HandleFunc("POST /v1/users/{id}/playlists", func(w http.ResponseWriter, r *http.Request) {
		s.playlistCalls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := fmt.Sprintf("pl%08d", s.playlistCalls)
		s.playlistItems[id] = nil
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{
			"id": id, "name": body["name"], "uri": "spotify:playlist:" + id,
			"external_urls": map[string]any{"spotify": "https://open.spotify.test/playlist/" + id},
		})
	})

	// Replace and append, the two halves of a rebuild.
	playlistTracks := func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			URIs []string `json:"uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method == http.MethodPut {
			s.playlistItems[id] = nil
		}
		s.playlistItems[id] = append(s.playlistItems[id], body.URIs...)
		writeJSON(w, map[string]any{"snapshot_id": "snap"})
	}
	mux.HandleFunc("PUT /v1/playlists/{id}/tracks", playlistTracks)
	mux.HandleFunc("POST /v1/playlists/{id}/tracks", playlistTracks)

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

	// An album's own track listing, one page, "next": null. What it answers for
	// one album is whatever a test put in albumTracks. An id the test never
	// seeded answers {"items":null} — an empty listing, which
	// TestAlbumTracklistUnavailableIsA200NotAnErrorEnvelope uses deliberately:
	// internal/albumtracks records a 200 carrying no tracks as a failure (see
	// errEmptyListing), which is the doorway into "unavailable" that needs no
	// waiting at all.
	mux.HandleFunc("/v1/albums/{id}/tracks", func(w http.ResponseWriter, r *http.Request) {
		s.albumTrackReqs.Add(1)
		writeJSON(w, map[string]any{"items": s.albumTracks[r.PathValue("id")], "next": nil})
	})

	// An artist's own discography, one page, "next": null. Same shape as the
	// album track listing above: an id the test never seeded answers
	// {"items":null}, which is the doorway into "unavailable" that needs no
	// waiting at all.
	mux.HandleFunc("/v1/artists/{id}/albums", func(w http.ResponseWriter, r *http.Request) {
		s.artistAlbumReqs.Add(1)
		writeJSON(w, map[string]any{"items": s.artistAlbums[r.PathValue("id")], "next": nil})
	})

	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

// albumTrackCalls reports how many times GET /v1/albums/{id}/tracks has been
// served, across every album. It exists so a test can prove the lease held: a
// page that polls the tracklist endpoint several times must still only make
// this instance ask Spotify once per album.
func (s *spotifyStub) albumTrackCalls() int { return int(s.albumTrackReqs.Load()) }

// artistAlbumCalls reports how many times GET /v1/artists/{id}/albums has been
// served, which is how the tests below prove a walk happened exactly once.
func (s *spotifyStub) artistAlbumCalls() int { return int(s.artistAlbumReqs.Load()) }

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
	client  *spotify.Client
	poller  *encoresync.Poller
	baseURL string
}

func newInstance(t *testing.T) *instance { return newInstanceWith(t, nil) }

// newInstanceWith is newInstance with configuration overrides merged over the
// defaults every test otherwise gets — the operator's switch
// (ENCORE_ALBUM_TRACKS_ENABLED, say) being the reason this exists at all: that
// is a wiring property config.Load through albumtracks.New through the
// handler, which no unit test constructing its own config by hand can see.
func newInstanceWith(t *testing.T, overrides map[string]string) *instance {
	t.Helper()
	stub := newSpotifyStub(t)
	rig := harness.NewRig(t, nil)
	env := rig.Env

	key := strings.Repeat("k", 32)
	envMap := map[string]string{
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
	}
	for k, v := range overrides {
		envMap[k] = v
	}
	cfg, err := config.LoadFrom(envMap)
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

	albumTracks, err := albumtracks.New(cfg.AlbumTracks, albumtracks.Deps{
		Catalog: env.Catalog, Spotify: client,
		Writer: albumtracks.StoreWriter{Store: env.Store},
		Logger: harness.Discard(),
	})
	if err != nil {
		t.Fatalf("build album tracks service: %v", err)
	}
	t.Cleanup(albumTracks.Close)

	artistAlbums, err := artistalbums.New(cfg.ArtistAlbums, artistalbums.Deps{
		Catalog: env.Catalog, Spotify: client,
		Writer: artistalbums.StoreWriter{Store: env.Store},
		Logger: harness.Discard(),
	})
	if err != nil {
		t.Fatalf("build artist albums service: %v", err)
	}
	t.Cleanup(artistAlbums.Close)

	api, err := httpapi.New(httpapi.Deps{
		Config: cfg, Store: env.Store, Accounts: env.Accounts, Catalog: env.Catalog,
		Listens: env.Listens, Imports: env.Imports, Stats: stats.New(env.Store),
		Intake: intake, Spotify: client, Metrics: metrics.New(),
		Logger: harness.Discard(), Version: "test",
		UserToken:    poller.AccessToken,
		AlbumTracks:  albumTracks,
		ArtistAlbums: artistAlbums,
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
		client: client, poller: poller, baseURL: srv.URL}
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

// seedAlbumWithPlays gives one album a Spotify track listing of `total` tracks
// and a play recorded for the first `played` of them. Track ids are derived
// from the album id, so the played tracks and the listed tracks name the same
// identifiers without a lookup table — track n is "<albumID>T<n>" in both the
// recently-played page synced through the real import path and the album
// track listing the stub answers when the tracklist endpoint fetches it.
func (i *instance) seedAlbumWithPlays(b *browser, albumID string, total, played int) {
	i.t.Helper()
	if played > total {
		i.t.Fatalf("seedAlbumWithPlays: played (%d) exceeds total (%d)", played, total)
	}
	trackID := func(n int) string { return fmt.Sprintf("%sT%02d", albumID, n) }

	at := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	items := make([]map[string]any, 0, played)
	for n := 1; n <= played; n++ {
		items = append(items, samePlayItem(trackID(n), albumID, at.Add(time.Duration(n)*time.Minute)))
	}
	i.stub.plays = items
	syncResult := decode[map[string]any](i.t, b.postJSON("/api/sync/now", nil), http.StatusOK)
	if n, _ := syncResult["imported"].(float64); int(n) != played {
		i.t.Fatalf("sync reported %v imported while seeding, want %d", syncResult["imported"], played)
	}

	listing := make([]map[string]any, 0, total)
	for n := 1; n <= total; n++ {
		listing = append(listing, map[string]any{
			"id": trackID(n), "name": "Track " + trackID(n),
			"disc_number": 1, "track_number": n,
		})
	}
	i.stub.albumTracks[albumID] = listing
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

// samePlayItem is playItem with the album id pinned rather than derived from
// the track id, so two plays can be made to land on the same album. That is
// the one shape the completion statistics need and playItem cannot produce.
func samePlayItem(trackID, albumID string, at time.Time) map[string]any {
	return map[string]any{
		"played_at": at.Format(time.RFC3339),
		"track": map[string]any{
			"id": trackID, "name": "Track " + trackID, "duration_ms": 210000,
			"album": map[string]any{
				"id": albumID, "name": "Album", "album_type": "album",
				"release_date": "2019-06-01", "release_date_precision": "day",
				"artists": []any{map[string]any{"id": "art" + albumID, "name": "Artist"}},
			},
			"artists": []any{map[string]any{"id": "art" + albumID, "name": "Artist"}},
		},
	}
}

// artistPlayItem is samePlayItem with the artist id pinned rather than derived
// from the album id, so every play can be attributed to the one artist whose
// discography a test is building — seedArtistWithPlays needs several albums to
// all name the same artist, which samePlayItem's own "art"+albumID derivation
// cannot produce.
func artistPlayItem(trackID, albumID, artistID string, at time.Time) map[string]any {
	return map[string]any{
		"played_at": at.Format(time.RFC3339),
		"track": map[string]any{
			"id": trackID, "name": "Track " + trackID, "duration_ms": 210000,
			"album": map[string]any{
				"id": albumID, "name": "Album", "album_type": "album",
				"release_date": "2019-06-01", "release_date_precision": "day",
				"artists": []any{map[string]any{"id": artistID, "name": "Artist"}},
			},
			"artists": []any{map[string]any{"id": artistID, "name": "Artist"}},
		},
	}
}

// TestAlbumDetailReportsCompletion pins the one thing a service-level test of
// stats.Service.AlbumCompletion cannot see: that the figure actually reaches the
// album detail response as JSON. The arithmetic itself is covered exhaustively
// in test/integration/completion_test.go; this is the wiring.
func TestAlbumDetailReportsCompletion(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	albumID := "e2ealbumcomplete0001"
	at := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{
		samePlayItem("e2etrackcomplete001a", albumID, at),
		samePlayItem("e2etrackcomplete001b", albumID, at.Add(5*time.Minute)),
	}
	syncResult := decode[map[string]any](t, b.postJSON("/api/sync/now", nil), http.StatusOK)
	if n, _ := syncResult["imported"].(float64); int(n) != 2 {
		t.Fatalf("sync reported %v imported, want 2", syncResult["imported"])
	}

	// Import leaves total_tracks at its unenriched default of 0; set it as if
	// enrichment had already resolved it, exactly as completion_test.go does at
	// the service level.
	inst.env.Exec(`UPDATE albums SET total_tracks = 2 WHERE id = $1`, albumID)

	detail := decode[httpapi.AlbumDetail](t, b.get("/api/albums/"+albumID), http.StatusOK)
	if detail.Completion == nil {
		t.Fatal("album detail carries no completion field at all")
	}
	if !detail.Completion.Known || detail.Completion.Heard != 2 || detail.Completion.Total != 2 {
		t.Fatalf("completion = %+v, want {Heard:2 Total:2 Known:true}", detail.Completion)
	}
}

// TestAlbumCompletionIgnoresTheRange pins the property the whole album-
// completion feature rests on at the one layer that can actually break it.
// stats.Service.AlbumCompletion takes no range argument, so a test that calls
// it directly can never show its answer is independent of a range — it can
// only call it the same way twice. The real risk is in handleAlbum
// (internal/httpapi/entities.go), which has a domain.TimeRange in scope from
// callerAndRange and simply chooses not to pass it to AlbumCompletion; a
// future edit that "tidies up" that inconsistent-looking call by threading the
// range through would pass every existing test in this repo. This fetches the
// same album under two disjoint from/to windows — one containing the plays,
// one well before them — and requires the completion payload to come back
// identical either way.
func TestAlbumCompletionIgnoresTheRange(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	albumID := "e2ealbumcompletionrng"
	at := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{
		samePlayItem("e2etrkcompletionrng1a", albumID, at),
		samePlayItem("e2etrkcompletionrng1b", albumID, at.Add(5*time.Minute)),
	}
	syncResult := decode[map[string]any](t, b.postJSON("/api/sync/now", nil), http.StatusOK)
	if n, _ := syncResult["imported"].(float64); int(n) != 2 {
		t.Fatalf("sync reported %v imported, want 2", syncResult["imported"])
	}
	inst.env.Exec(`UPDATE albums SET total_tracks = 2 WHERE id = $1`, albumID)

	albumPath := func(from, to time.Time) string {
		return fmt.Sprintf("/api/albums/%s?from=%s&to=%s", albumID,
			url.QueryEscape(from.Format(time.RFC3339)), url.QueryEscape(to.Format(time.RFC3339)))
	}
	// A window containing both plays, and one well before either of them.
	containing := albumPath(at.Add(-24*time.Hour), time.Now().Add(24*time.Hour))
	excluding := albumPath(at.Add(-240*time.Hour), at.Add(-192*time.Hour))

	within := decode[httpapi.AlbumDetail](t, b.get(containing), http.StatusOK)
	outside := decode[httpapi.AlbumDetail](t, b.get(excluding), http.StatusOK)

	if within.Completion == nil || outside.Completion == nil {
		t.Fatal("album detail carries no completion field at all")
	}
	if *within.Completion != *outside.Completion {
		t.Fatalf("completion moved with the range: %+v (plays inside) vs %+v (plays outside)",
			within.Completion, outside.Completion)
	}
	if !within.Completion.Known || within.Completion.Heard != 2 || within.Completion.Total != 2 {
		t.Fatalf("completion = %+v, want {Heard:2 Total:2 Known:true}", within.Completion)
	}
}

// TestStatsExtrasReportsAlbumsCompleted is the same wiring check for the
// range-scoped aggregate. The arithmetic is covered by
// TestCompletedAlbumsIsRangeScoped in the integration suite; this pins that the
// extras endpoint actually serialises what the service computes.
func TestStatsExtrasReportsAlbumsCompleted(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	albumID := "e2ealbumcompleted002"
	at := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{
		samePlayItem("e2etrackcompleted02a", albumID, at),
	}
	syncResult := decode[map[string]any](t, b.postJSON("/api/sync/now", nil), http.StatusOK)
	if n, _ := syncResult["imported"].(float64); int(n) != 1 {
		t.Fatalf("sync reported %v imported, want 1", syncResult["imported"])
	}
	// One track played, and Spotify says the album has exactly one: complete.
	inst.env.Exec(`UPDATE albums SET total_tracks = 1 WHERE id = $1`, albumID)

	from := at.Add(-24 * time.Hour).Format(time.RFC3339)
	to := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	extras := decode[httpapi.StatsExtras](t, b.get(
		fmt.Sprintf("/api/stats/extras?from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to))),
		http.StatusOK)
	if extras.AlbumsCompleted == nil {
		t.Fatal("extras carries no albumsCompleted field at all")
	}
	if extras.AlbumsCompleted.Albums != 1 || extras.AlbumsCompleted.Complete != 1 {
		t.Fatalf("albumsCompleted = %+v, want {Complete:1 Albums:1}", extras.AlbumsCompleted)
	}
}

// TestLibraryStatsNeverSyncedRendersNullSyncedAt pins the wire-level contract
// for the state every upgraded instance starts in: an account the library
// worker has never enumerated. A struct-level assertion cannot tell "the
// server sent null" apart from "the server omitted the field" — encoding/json
// unmarshals both into the same nil pointer — so this checks the raw response
// text, exactly as TestMeReportsNoMissingScopesForACurrentGrant checks for []
// rather than null for the same reason.
func TestLibraryStatsNeverSyncedRendersNullSyncedAt(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	resp := b.get("/api/stats/library")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats/library = %d, want 200; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"syncedAt":null`) {
		t.Errorf(`a never-synced account must report "syncedAt":null verbatim, got: %s`, body)
	}
	for _, list := range []string{"savedNeverPlayed", "playedNeverSaved", "dormantFollows"} {
		if !strings.Contains(string(body), fmt.Sprintf(`"%s":[]`, list)) {
			t.Errorf("%s must serialise as [] and not null, got: %s", list, body)
		}
	}

	var lib httpapi.LibraryStatsResponse
	if err := json.Unmarshal(body, &lib); err != nil {
		t.Fatalf("decode library stats: %v; body: %s", err, body)
	}
	if lib.SyncedAt != nil {
		t.Errorf("synced at = %v, want nil", *lib.SyncedAt)
	}
	if lib.SavedTracks != 0 || lib.SavedAlbums != 0 || lib.FollowedArtists != 0 {
		t.Errorf("counts = %+v, want all zero for a never-synced account", lib)
	}
}

// TestLibraryStatsResolvesEntitiesAndSyncState is the wiring check for the
// happy path. The arithmetic behind each list is covered exhaustively by
// test/integration/librarystats_test.go; what only an HTTP-level test can see
// is that the endpoint actually resolves every identifier to a name — through
// resolveRefs, exactly as handleAlbum does for every other list in the API —
// and reports the true sync watermark rather than a placeholder.
func TestLibraryStatsResolvesEntitiesAndSyncState(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	me := inst.signIn(b)
	userID := uuid.MustParse(me["user"].(map[string]any)["id"].(string))
	ctx, db := inst.env.Ctx(), inst.env.Store.DB()

	// A track saved but never played. Seeded straight into the catalogue,
	// exactly as the library worker's own reconciliation would leave it, since
	// it carries no listen at all.
	savedTrackID := "e2elibsavedtrack0001"
	if err := inst.env.Catalog.UpsertTracks(ctx, db, []domain.Track{
		{ID: savedTrackID, Name: "Saved Never Played"},
	}); err != nil {
		t.Fatalf("seed saved track: %v", err)
	}
	inst.env.Exec(`INSERT INTO user_saved_tracks (user_id, track_id) VALUES ($1, $2)`,
		userID, savedTrackID)

	// A followed artist with no listen at all, so it reads as dormant with no
	// LastPlayedAt.
	followedArtistID := "e2elibfollowedartist1"
	if err := inst.env.Catalog.UpsertArtists(ctx, db, []domain.Artist{
		{ID: followedArtistID, Name: "Dormant Artist"},
	}); err != nil {
		t.Fatalf("seed followed artist: %v", err)
	}
	inst.env.Exec(`INSERT INTO user_followed_artists (user_id, artist_id) VALUES ($1, $2)`,
		userID, followedArtistID)

	// A play, which the sync gives its own catalogue row and which is
	// therefore played-never-saved.
	playedTrackID := "e2elibplayedtrack0001"
	at := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{playItem(playedTrackID, at)}
	syncResult := decode[map[string]any](t, b.postJSON("/api/sync/now", nil), http.StatusOK)
	if n, _ := syncResult["imported"].(float64); int(n) != 1 {
		t.Fatalf("sync reported %v imported, want 1", syncResult["imported"])
	}

	syncedAt := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	if err := inst.env.Accounts.Credentials.MarkLibrarySynced(ctx, db, userID, syncedAt); err != nil {
		t.Fatalf("mark library synced: %v", err)
	}

	from := at.Add(-24 * time.Hour).Format(time.RFC3339)
	to := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	lib := decode[httpapi.LibraryStatsResponse](t, b.get(
		fmt.Sprintf("/api/stats/library?from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to))),
		http.StatusOK)

	if lib.SyncedAt == nil || *lib.SyncedAt != syncedAt.Format(time.RFC3339) {
		t.Fatalf("synced at = %v, want %v", lib.SyncedAt, syncedAt.Format(time.RFC3339))
	}
	if lib.SavedTracks != 1 {
		t.Errorf("saved tracks = %d, want 1", lib.SavedTracks)
	}
	if lib.FollowedArtists != 1 {
		t.Errorf("followed artists = %d, want 1", lib.FollowedArtists)
	}

	if len(lib.SavedNeverPlayed) != 1 || lib.SavedNeverPlayed[0].Entity.ID != savedTrackID ||
		lib.SavedNeverPlayed[0].Entity.Name != "Saved Never Played" {
		t.Fatalf("savedNeverPlayed = %+v, want one entry naming %q", lib.SavedNeverPlayed, savedTrackID)
	}

	found := false
	for _, e := range lib.PlayedNeverSaved {
		if e.Entity.ID == playedTrackID {
			found = true
			if e.Entity.Name != "Track "+playedTrackID {
				t.Errorf("played track name = %q, want %q", e.Entity.Name, "Track "+playedTrackID)
			}
			if e.Plays != 1 {
				t.Errorf("played track plays = %d, want 1", e.Plays)
			}
		}
	}
	if !found {
		t.Fatalf("playedNeverSaved = %+v, want an entry for %q", lib.PlayedNeverSaved, playedTrackID)
	}

	if len(lib.DormantFollows) != 1 || lib.DormantFollows[0].Entity.ID != followedArtistID ||
		lib.DormantFollows[0].Entity.Name != "Dormant Artist" {
		t.Fatalf("dormantFollows = %+v, want one entry naming %q", lib.DormantFollows, followedArtistID)
	}
	if lib.DormantFollows[0].LastPlayedAt != nil {
		t.Errorf("dormant artist last played at = %v, want nil: it has never played",
			*lib.DormantFollows[0].LastPlayedAt)
	}
}

// TestTopDiffNeverCapturedRendersNullCapturedAt pins the wire-level contract
// for the state every (kind, range) set starts in until the daily worker
// captures it: a struct-level assertion cannot tell "the server sent null"
// apart from "the server omitted the field" - encoding/json unmarshals both
// into the same nil pointer - so this checks the raw response text, exactly
// as TestLibraryStatsNeverSyncedRendersNullSyncedAt does for the same reason.
func TestTopDiffNeverCapturedRendersNullCapturedAt(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	resp := b.get("/api/stats/top-diff?kind=artist&range=short_term")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats/top-diff = %d, want 200; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"capturedAt":null`) {
		t.Errorf(`a never-captured (kind, range) must report "capturedAt":null verbatim, got: %s`, body)
	}
	if !strings.Contains(string(body), `"entries":[]`) {
		t.Errorf("entries must serialise as [] and not null, got: %s", body)
	}

	var diff httpapi.TopDiffResponse[httpapi.ArtistRef]
	if err := json.Unmarshal(body, &diff); err != nil {
		t.Fatalf("decode top diff: %v; body: %s", err, body)
	}
	if diff.CapturedAt != nil {
		t.Errorf("captured at = %v, want nil", *diff.CapturedAt)
	}
	if diff.TimeRange != "short_term" {
		t.Errorf("time range = %q, want short_term", diff.TimeRange)
	}
}

// TestTopDiffResolvesEntitiesAndValidatesParameters is the wiring check the
// unit suite cannot make on its own: that the endpoint actually resolves a
// captured entity id to a name through resolveRefs, exactly as handleLibrary
// does, and that kind and range are both validated against their fixed sets
// before ever reaching stats.TopDiff. The arithmetic behind the comparison
// itself - both ranks present, one side only, the blacklist, the
// per-time-range window - is covered exhaustively by
// test/integration/topdiff_test.go; nothing here repeats it.
func TestTopDiffResolvesEntitiesAndValidatesParameters(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	me := inst.signIn(b)
	userID := uuid.MustParse(me["user"].(map[string]any)["id"].(string))

	// A play, so Encore ranks this artist too: playItem derives the artist id
	// from the track id, the same convention TestSyncFlowThroughTheAPI uses. A
	// sync only mints the artist as an unnamed, pending catalogue row (see
	// internal/sync/ingest.go: ingestion writes pending rows and nothing else,
	// same as an import); UpsertArtists stands in for the enrichment worker
	// this harness never starts, giving the row the name resolveRefs must
	// then thread through to the response.
	trackID := "e2etopdifftrack00001"
	artistID := "art" + trackID[3:]
	at := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{playItem(trackID, at)}
	syncResult := decode[map[string]any](t, b.postJSON("/api/sync/now", nil), http.StatusOK)
	if n, _ := syncResult["imported"].(float64); int(n) != 1 {
		t.Fatalf("sync reported %v imported, want 1", syncResult["imported"])
	}
	ctx, db := inst.env.Ctx(), inst.env.Store.DB()
	if err := inst.env.Catalog.UpsertArtists(ctx, db, []domain.Artist{
		{ID: artistID, Name: "Top Diff Artist"},
	}); err != nil {
		t.Fatalf("seed artist name: %v", err)
	}

	// Spotify's own captured ranking for the same artist, seeded directly the
	// way the library worker would leave it after a real /me/top/artists call.
	capturedAt := time.Now().UTC().Truncate(time.Second)
	inst.env.Exec(`INSERT INTO spotify_top_snapshots (user_id, kind, time_range, position, entity_id, captured_at)
		VALUES ($1, 'artist', 'short_term', 1, $2, $3)`, userID, artistID, capturedAt)

	diff := decode[httpapi.TopDiffResponse[httpapi.ArtistRef]](t, b.get(
		"/api/stats/top-diff?kind=artist&range=short_term"), http.StatusOK)

	if diff.CapturedAt == nil || *diff.CapturedAt != capturedAt.Format(time.RFC3339) {
		t.Fatalf("captured at = %v, want %v", diff.CapturedAt, capturedAt.Format(time.RFC3339))
	}
	if diff.TimeRange != "short_term" {
		t.Fatalf("time range = %q, want short_term", diff.TimeRange)
	}

	var found *httpapi.TopDiffEntry[httpapi.ArtistRef]
	for i := range diff.Entries {
		if diff.Entries[i].Entity.ID == artistID {
			found = &diff.Entries[i]
		}
	}
	if found == nil {
		t.Fatalf("entries = %+v, want an entry for %q", diff.Entries, artistID)
	}
	if found.Entity.Name != "Top Diff Artist" {
		t.Fatalf("entity name = %q, want %q: the endpoint must resolve the id through resolveRefs",
			found.Entity.Name, "Top Diff Artist")
	}
	if found.SpotifyRank == nil || *found.SpotifyRank != 1 {
		t.Fatalf("spotify rank = %v, want 1", found.SpotifyRank)
	}
	if found.EncoreRank == nil || *found.EncoreRank != 1 {
		t.Fatalf("encore rank = %v, want 1", found.EncoreRank)
	}

	// kind and range are both validated against their fixed sets: neither ever
	// reaches stats.TopDiff, let alone SQL, as a bare caller-supplied string.
	for _, path := range []string{
		"/api/stats/top-diff?kind=album&range=short_term",
		"/api/stats/top-diff?kind=artist&range=this_year",
		"/api/stats/top-diff",
	} {
		resp := b.get(path)
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400; body: %s", path, resp.StatusCode, respBody)
		}
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

// TestStatusReportsEnrichmentProgressAndRateLimiting covers the endpoint behind
// the Settings page's metadata panel.
//
// The panel exists because "the artists are blank" is the first thing a fresh
// self-hosted instance shows, and the honest answer — Spotify has rate limited
// this application, listening data is unaffected, it fixes itself — used to live
// only in the worker's logs. If this endpoint under-reports the pause, the page
// silently goes back to telling the user nothing.
func TestStatusReportsEnrichmentProgressAndRateLimiting(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	// Signing in is required: this is instance-wide operational state.
	anon := inst.browser()
	if resp := anon.get("/api/status"); resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("an anonymous request to /api/status got %d, want 401", resp.StatusCode)
	}

	type entity struct {
		Total, Resolved, Pending, Failed, Unavailable, Named int64
	}
	type status struct {
		Catalogue struct {
			Tracks, Artists, Albums entity
			AliasesTotal            int64 `json:"aliasesTotal"`
			AliasesPending          int64 `json:"aliasesPending"`
		}
		Metadata struct {
			Outstanding int64
			Complete    bool
			Paused      bool
			PausedUntil *time.Time `json:"pausedUntil"`
		}
	}

	// An empty instance has nothing outstanding, and must not claim to be paused.
	empty := decode[status](t, b.get("/api/status"), http.StatusOK)
	if !empty.Metadata.Complete || empty.Metadata.Outstanding != 0 {
		t.Fatalf("an empty catalogue reports %d outstanding (complete=%v), want 0/true",
			empty.Metadata.Outstanding, empty.Metadata.Complete)
	}
	if empty.Metadata.Paused {
		t.Fatal("a fresh instance reports itself rate limited")
	}

	// An elapsed pause is history, not a warning: the panel must not nag about a
	// window that has already passed. Checked before a live pause is recorded
	// because a stored pause is never shortened.
	if err := inst.env.Accounts.Settings.SetSpotifyPausedUntil(
		inst.env.Ctx(), inst.env.Store.DB(), time.Now().Add(-time.Minute).UTC()); err != nil {
		t.Fatalf("record an elapsed pause: %v", err)
	}
	if cleared := decode[status](t, b.get("/api/status"), http.StatusOK); cleared.Metadata.Paused {
		t.Fatalf("a pause that ended at %v is still reported as active",
			cleared.Metadata.PausedUntil)
	}

	// A sync fills the catalogue. The stub answers every lookup, so the entities
	// it created are resolved rather than queued.
	at := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{playItem("sta00000000000000001a", at)}
	decode[map[string]any](t, b.postJSON("/api/sync/now", nil), http.StatusOK)

	filled := decode[status](t, b.get("/api/status"), http.StatusOK)
	if filled.Catalogue.Tracks.Total == 0 {
		t.Fatal("after a sync the status reports an empty catalogue")
	}
	if filled.Catalogue.Tracks.Named == 0 {
		t.Fatal("the status reports no named tracks, so the page would claim nothing is displayable")
	}
	if filled.Catalogue.Artists.Total == 0 {
		t.Fatal("the status reports no artists at all")
	}

	// Now Spotify rate limits the whole application. This is the state the panel
	// exists for, and it must be visible through the API.
	resume := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	if err := inst.env.Accounts.Settings.SetSpotifyPausedUntil(
		inst.env.Ctx(), inst.env.Store.DB(), resume); err != nil {
		t.Fatalf("record a pause: %v", err)
	}

	paused := decode[status](t, b.get("/api/status"), http.StatusOK)
	if !paused.Metadata.Paused {
		t.Fatal("Spotify is rate limiting the instance but the status does not say so; " +
			"the page would show blank artists with no explanation")
	}
	if paused.Metadata.PausedUntil == nil || !paused.Metadata.PausedUntil.Equal(resume) {
		t.Fatalf("the status reports the pause ending at %v, want %s",
			paused.Metadata.PausedUntil, resume)
	}
}

// TestSignInWorksWhileSpotifyIsRateLimitingTheInstance is the regression test
// for the failure that locked people out of their own Encore.
//
// A large import exhausts a development-mode application's daily quota. Spotify
// answers with a Retry-After of most of a day, and the limiter honours it for
// the whole process — which is right for catalogue reads and was catastrophic
// for the two calls behind the sign-in button. Signing in blocked in the
// limiter until the browser or the reverse proxy gave up, so an import that
// cost a day of metadata also cost a day of being able to log in.
//
// The redirect to Spotify never broke, which is what made it confusing: the
// first hop is pure string building and answers in milliseconds. Everything
// after it hung.
func TestSignInWorksWhileSpotifyIsRateLimitingTheInstance(t *testing.T) {
	inst := newInstance(t)

	// Exactly the state an exhausted daily quota leaves behind.
	inst.client.Limiter().Pause(time.Now().Add(20 * time.Hour))

	done := make(chan map[string]any, 1)
	go func() {
		defer func() {
			// A hang shows up as a nil send rather than a deadlocked test.
			if r := recover(); r != nil {
				done <- nil
			}
		}()
		done <- inst.signIn(inst.browser())
	}()

	select {
	case me := <-done:
		if me == nil {
			t.Fatal("signing in failed while Spotify was rate limiting the instance")
		}
		user, _ := me["user"].(map[string]any)
		if user == nil || user["spotifyUserId"] == "" {
			t.Fatalf("signed in but got no user back: %v", me)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("signing in blocked behind the catalogue rate limit; a background " +
			"import must never be able to lock somebody out of their own instance")
	}

	// And the catalogue budget is still held back, which is the whole reason the
	// pause exists. Fixing the lockout must not have spent the protection.
	if until := inst.client.Limiter().PausedUntil(); !until.After(time.Now()) {
		t.Fatal("the catalogue pause was cleared by a sign-in")
	}
}

// TestManualSyncRefusesQuicklyWhileRateLimited: the same trap, one button along.
//
// A manual sync runs the ordinary poller, whose calls queue on the shared
// limiter and would wait out the whole ban. Somebody who has just pressed a
// button gets an answer instead, and one that says the history is fine.
func TestManualSyncRefusesQuicklyWhileRateLimited(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	if err := inst.env.Accounts.Settings.SetSpotifyPausedUntil(
		inst.env.Ctx(), inst.env.Store.DB(), time.Now().Add(20*time.Hour)); err != nil {
		t.Fatalf("record a pause: %v", err)
	}

	start := time.Now()
	resp := b.postJSON("/api/sync/now", nil)
	elapsed := time.Since(start)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("sync during a rate limit returned %d, want 409", resp.StatusCode)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the refusal took %s; the person is watching a spinner", elapsed)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unaffected") {
		t.Fatalf("the refusal does not say the listening history is safe: %s", body)
	}
}

// --- sharing ----------------------------------------------------------------

// TestSharedLinkShowsAggregatesToAnybodyHoldingIt is the happy path: a link
// works with no session at all, which is the entire point of it.
func TestSharedLinkShowsAggregatesToAnybodyHoldingIt(t *testing.T) {
	inst := newInstance(t)
	owner := inst.browser()
	inst.signIn(owner)

	at := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{
		playItem("shr00000000000000001a", at),
		playItem("shr00000000000000002b", at.Add(30*time.Minute)),
	}
	decode[map[string]any](t, owner.postJSON("/api/sync/now", nil), http.StatusOK)

	created := decode[map[string]any](t, owner.postJSON("/api/shares",
		map[string]any{"label": "My year"}), http.StatusCreated)

	token, _ := created["token"].(string)
	if token == "" {
		t.Fatal("creating a link returned no token; it is the only time it exists")
	}
	if url, _ := created["url"].(string); !strings.Contains(url, "/share/"+token) {
		t.Fatalf("share url is %q, want it to carry the token", created["url"])
	}

	// A stranger: no session, no cookies, nothing.
	stranger := inst.browser()
	shared := decode[map[string]any](t, stranger.get("/api/share/"+token), http.StatusOK)

	if shared["label"] != "My year" {
		t.Fatalf("label = %v, want the one the owner set", shared["label"])
	}
	summary, _ := shared["summary"].(map[string]any)
	if summary == nil {
		t.Fatalf("the shared payload carries no summary: %v", shared)
	}
	if n, _ := summary["listens"].(float64); int(n) != 2 {
		t.Fatalf("shared summary reports %v listens, want 2", summary["listens"])
	}
	if _, ok := shared["tracks"].(map[string]any); !ok {
		t.Fatal("the shared payload carries no top tracks")
	}
}

// TestSharedLinkCarriesGenresAndObscurity checks the two aggregate additions
// reach a shared page. They are the same data class as the top lists beside
// them: what somebody's taste is, never when they were listening.
func TestSharedLinkCarriesGenresAndObscurity(t *testing.T) {
	inst := newInstance(t)
	owner := inst.browser()
	inst.signIn(owner)

	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{playItem("shr00000000000000004a", at)}
	decode[map[string]any](t, owner.postJSON("/api/sync/now", nil), http.StatusOK)

	created := decode[map[string]any](t, owner.postJSON("/api/shares", map[string]any{}), http.StatusCreated)
	token := created["token"].(string)

	shared := decode[map[string]any](t, inst.browser().get("/api/share/"+token), http.StatusOK)
	if _, ok := shared["genres"].(map[string]any); !ok {
		t.Errorf("the shared payload carries no genre ranking: %v", shared)
	}
	if _, ok := shared["taste"].(map[string]any); !ok {
		t.Errorf("the shared payload carries no taste scores: %v", shared)
	}
}

// TestASharedLinkCannotReachTheListeningHistory is the privacy boundary, and the
// reason the share endpoint composes its own payload instead of reusing the
// statistics handlers behind a shared authentication path.
//
// Aggregates say what somebody listens to. The history feed says when they were
// awake, which is a different thing to hand a stranger.
func TestASharedLinkCannotReachTheListeningHistory(t *testing.T) {
	inst := newInstance(t)
	owner := inst.browser()
	inst.signIn(owner)

	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{playItem("shr00000000000000003c", at)}
	decode[map[string]any](t, owner.postJSON("/api/sync/now", nil), http.StatusOK)

	created := decode[map[string]any](t, owner.postJSON("/api/shares", map[string]any{}), http.StatusCreated)
	token := created["token"].(string)

	shared := decode[map[string]any](t, inst.browser().get("/api/share/"+token), http.StatusOK)
	// The playback-context statistics are withheld deliberately: device and
	// country say what hardware somebody owns and where they have travelled,
	// which is a different disclosure from a favourite artist.
	for _, forbidden := range []string{
		"history", "listens", "plays", "items",
		"endReasons", "endReasonCoverage", "skipRate", "shuffleRate",
		"platforms", "platformCoverage", "countries", "countryCoverage",
		"offlineRate", "incognitoRate",
	} {
		if _, present := shared[forbidden]; present {
			t.Fatalf("the shared payload carries a %q field; a share must expose "+
				"aggregates and never individual plays", forbidden)
		}
	}

	// And holding a link grants nothing else on the instance.
	stranger := inst.browser()
	for _, path := range []string{"/api/me", "/api/history?limit=10", "/api/imports", "/api/users"} {
		resp := stranger.get(path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s returned %d to a stranger holding a share link, want 401",
				path, resp.StatusCode)
		}
	}
}

// TestRevokingALinkStopsItImmediately: revocation is the only recourse once a
// link has been sent to somebody, so it has to be instant and complete.
func TestRevokingALinkStopsItImmediately(t *testing.T) {
	inst := newInstance(t)
	owner := inst.browser()
	inst.signIn(owner)

	created := decode[map[string]any](t, owner.postJSON("/api/shares",
		map[string]any{"label": "temporary"}), http.StatusCreated)
	token := created["token"].(string)
	id := created["id"].(string)

	visitor := inst.browser()
	resp := visitor.get("/api/share/" + token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a fresh link returned %d", resp.StatusCode)
	}

	del := owner.do(http.MethodDelete, "/api/shares/"+id, nil, "")
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking returned %d, want 204", del.StatusCode)
	}

	resp = visitor.get("/api/share/" + token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a revoked link returned %d, want 404", resp.StatusCode)
	}

	// And it is gone from the owner's list rather than lingering as clutter.
	list := decode[[]map[string]any](t, owner.get("/api/shares"), http.StatusOK)
	for _, l := range list {
		if l["id"] == id {
			t.Fatal("a revoked link is still listed")
		}
	}
}

// TestOneUserCannotRevokeAnotherUsersLink: the id is not a secret, so the
// statement that acts on it has to be scoped by owner.
func TestOneUserCannotRevokeAnotherUsersLink(t *testing.T) {
	inst := newInstance(t)
	owner := inst.browser()
	inst.signIn(owner)
	created := decode[map[string]any](t, owner.postJSON("/api/shares", map[string]any{}), http.StatusCreated)
	id := created["id"].(string)
	token := created["token"].(string)

	// A second account on the same instance.
	inst.stub.profile = meProfile("intruder", "Intruder")
	other := inst.browser()
	inst.signIn(other)

	resp := other.do(http.MethodDelete, "/api/shares/"+id, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another user revoking a link got %d, want 404", resp.StatusCode)
	}
	// Still working for the owner.
	live := inst.browser().get("/api/share/" + token)
	live.Body.Close()
	if live.StatusCode != http.StatusOK {
		t.Fatal("the link stopped working after another user tried to revoke it")
	}
}

// TestListingLinksNeverReturnsTheToken: only the hash is stored, so a listing
// that appeared to carry a URL would be offering one that cannot work.
func TestListingLinksNeverReturnsTheToken(t *testing.T) {
	inst := newInstance(t)
	owner := inst.browser()
	inst.signIn(owner)
	decode[map[string]any](t, owner.postJSON("/api/shares", map[string]any{}), http.StatusCreated)

	list := decode[[]map[string]any](t, owner.get("/api/shares"), http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("%d links listed, want 1", len(list))
	}
	if tok, present := list[0]["token"]; present && tok != "" {
		t.Fatalf("the listing returned a token (%v); it exists only at creation", tok)
	}
	if u, present := list[0]["url"]; present && u != "" {
		t.Fatalf("the listing returned a url (%v) it cannot reconstruct", u)
	}
}

// TestAnUnknownShareTokenIs404: a wrong token must not be distinguishable from a
// revoked or expired one.
func TestAnUnknownShareTokenIs404(t *testing.T) {
	inst := newInstance(t)
	resp := inst.browser().get("/api/share/" + strings.Repeat("a", 43))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown token returned %d, want 404", resp.StatusCode)
	}
	if resp.Header.Get("X-Robots-Tag") != "" {
		t.Log("note: the noindex header is only set on a served share")
	}
}

// --- playlists ---------------------------------------------------------------

// seedPlays syncs a set of tracks, each played `times` times, so a playlist has
// something to rank.
func seedPlays(t *testing.T, inst *instance, b *browser, tracks map[string]int) {
	t.Helper()
	at := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	offset := 0
	items := make([]map[string]any, 0, 64)
	for id, times := range tracks {
		for range times {
			items = append(items, playItem(id, at.Add(time.Duration(offset)*7*time.Minute)))
			offset++
		}
	}
	inst.stub.plays = items
	decode[map[string]any](t, b.postJSON("/api/sync/now", nil), http.StatusOK)
}

// TestPlaylistNeedsPermissionBeforeItIsAsked is the reason the scope is
// incremental.
//
// Encore signs everybody in with read-only access. Somebody who never makes a
// playlist never grants write access at all, and the one who does is told what
// to do rather than shown a Spotify error they cannot act on.
func TestPlaylistNeedsPermissionBeforeItIsAsked(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	resp := b.postJSON("/api/playlists", map[string]any{
		"name": "Top tracks", "mode": "top", "limit": 10,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("creating a playlist without the scope returned %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "permission") {
		t.Fatalf("the refusal does not say permission is needed: %s", body)
	}
	if inst.stub.playlistCalls != 0 {
		t.Fatal("Encore called Spotify before it had permission to")
	}

	// And the way to grant it is a normal OAuth journey that asks for exactly one
	// more scope.
	auth := b.get("/api/auth/spotify/playlists")
	auth.Body.Close()
	if auth.StatusCode != http.StatusFound {
		t.Fatalf("the authorisation route returned %d, want a redirect", auth.StatusCode)
	}
	loc := auth.Header.Get("Location")
	if !strings.Contains(loc, "playlist-modify-private") {
		t.Fatalf("the authorisation url does not ask for the playlist scope: %s", loc)
	}
	if !strings.Contains(loc, "user-read-recently-played") {
		t.Fatalf("the authorisation url dropped the read scopes: %s", loc)
	}
}

// TestPlaylistIsCreatedAndRebuiltInPlace covers the whole loop.
func TestPlaylistIsCreatedAndRebuiltInPlace(t *testing.T) {
	inst := newInstance(t)
	inst.stub.grantedScopes = append(config.DefaultScopes(), "playlist-modify-private")
	b := inst.browser()
	inst.signIn(b)

	seedPlays(t, inst, b, map[string]int{
		"pl000000000000000001a": 5,
		"pl000000000000000002b": 3,
		"pl000000000000000003c": 1,
	})

	created := decode[map[string]any](t, b.postJSON("/api/playlists", map[string]any{
		"name": "Heavy rotation", "mode": "min_plays", "minPlays": 3, "limit": 50,
	}), http.StatusCreated)

	if n, _ := created["trackCount"].(float64); int(n) != 2 {
		t.Fatalf("playlist holds %v tracks, want the 2 that cleared the bar", created["trackCount"])
	}
	spotifyID, _ := created["spotifyId"].(string)
	if spotifyID == "" {
		t.Fatal("no spotify id recorded for the playlist")
	}
	if url, _ := created["spotifyUrl"].(string); url == "" {
		t.Fatal("no spotify url recorded; the owner has no way to open it")
	}

	got := inst.stub.playlistItems[spotifyID]
	if len(got) != 2 {
		t.Fatalf("Spotify was sent %d uris, want 2: %v", len(got), got)
	}
	for _, uri := range got {
		if !strings.HasPrefix(uri, "spotify:track:") {
			t.Fatalf("sent %q, want a track uri", uri)
		}
	}

	// A rebuild replaces in place: the same playlist, not a second one.
	id := created["id"].(string)
	rebuilt := decode[map[string]any](t, b.postJSON("/api/playlists/"+id+"/rebuild", nil), http.StatusOK)
	if rebuilt["spotifyId"] != spotifyID {
		t.Fatalf("a rebuild made a new playlist (%v), want the same one", rebuilt["spotifyId"])
	}
	if inst.stub.playlistCalls != 1 {
		t.Fatalf("%d playlists created, want 1: a rebuild must not make another",
			inst.stub.playlistCalls)
	}
	if len(inst.stub.playlistItems[spotifyID]) != 2 {
		t.Fatalf("after a rebuild the playlist holds %d uris, want 2 — the replace did not "+
			"clear the previous contents", len(inst.stub.playlistItems[spotifyID]))
	}

	// Listed, and forgetting it leaves Spotify alone.
	list := decode[[]map[string]any](t, b.get("/api/playlists"), http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("%d playlists listed, want 1", len(list))
	}
	del := b.do(http.MethodDelete, "/api/playlists/"+id, nil, "")
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("forgetting returned %d, want 204", del.StatusCode)
	}
	if _, gone := inst.stub.playlistItems[spotifyID]; !gone {
		t.Fatal("forgetting a playlist deleted it from Spotify; it belongs to the listener")
	}
}

// TestPlaylistRefusesWhenNothingMatches: an empty playlist in somebody's library
// is worse than a refusal that says why.
func TestPlaylistRefusesWhenNothingMatches(t *testing.T) {
	inst := newInstance(t)
	inst.stub.grantedScopes = append(config.DefaultScopes(), "playlist-modify-private")
	b := inst.browser()
	inst.signIn(b)
	seedPlays(t, inst, b, map[string]int{"pl000000000000000004d": 1})

	resp := b.postJSON("/api/playlists", map[string]any{
		"name": "Impossible", "mode": "min_plays", "minPlays": 500, "limit": 50,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a definition matching nothing returned %d, want 400", resp.StatusCode)
	}
	if inst.stub.playlistCalls != 0 {
		t.Fatal("an empty playlist was created on Spotify")
	}
}

// TestForgottenFavouritesNeedsARange: the mode is defined by what a period is
// missing, so an all-time range has nothing to be missing from. Saying that is
// better than silently returning nothing.
func TestForgottenFavouritesNeedsARange(t *testing.T) {
	inst := newInstance(t)
	inst.stub.grantedScopes = append(config.DefaultScopes(), "playlist-modify-private")
	b := inst.browser()
	inst.signIn(b)

	resp := b.postJSON("/api/playlists", map[string]any{
		"name": "Forgotten", "mode": "forgotten", "limit": 50,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("forgotten favourites over all time returned %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "period") {
		t.Fatalf("the error does not explain that a period is needed: %s", body)
	}
}

// TestOneUserCannotRebuildAnotherUsersPlaylist keeps the ownership scope honest.
func TestOneUserCannotRebuildAnotherUsersPlaylist(t *testing.T) {
	inst := newInstance(t)
	inst.stub.grantedScopes = append(config.DefaultScopes(), "playlist-modify-private")
	owner := inst.browser()
	inst.signIn(owner)
	seedPlays(t, inst, owner, map[string]int{"pl000000000000000005e": 4})

	created := decode[map[string]any](t, owner.postJSON("/api/playlists", map[string]any{
		"name": "Mine", "mode": "top", "limit": 10,
	}), http.StatusCreated)
	id := created["id"].(string)

	inst.stub.profile = meProfile("someone-else", "Someone Else")
	other := inst.browser()
	inst.signIn(other)

	for _, resp := range []*http.Response{
		other.postJSON("/api/playlists/"+id+"/rebuild", nil),
		other.do(http.MethodDelete, "/api/playlists/"+id, nil, ""),
	} {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("another user reached %s and got %d, want 404",
				resp.Request.URL.Path, resp.StatusCode)
		}
	}
}

// TestPlaylistPreviewTouchesNothingAndNeedsNoPermission is the point of the
// preview: it answers "what would this make" before anything is made, and before
// Encore has any way to make it.
//
// Not behind the write scope on purpose. Seeing the selection is how somebody
// decides whether to grant write access at all, so requiring the grant first
// would put the decision after the thing it decides.
func TestPlaylistPreviewTouchesNothingAndNeedsNoPermission(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b) // read-only scopes, as everybody starts

	seedPlays(t, inst, b, map[string]int{
		"pv000000000000000001a": 5,
		"pv000000000000000002b": 3,
		"pv000000000000000003c": 1,
	})

	preview := decode[map[string]any](t, b.postJSON("/api/playlists/preview", map[string]any{
		"name": "ignored for a preview", "mode": "min_plays", "minPlays": 3, "limit": 50,
	}), http.StatusOK)

	tracks, _ := preview["tracks"].([]any)
	if len(tracks) != 2 {
		t.Fatalf("preview holds %d tracks, want the 2 that cleared the bar", len(tracks))
	}
	if n, _ := preview["matched"].(float64); int(n) != 2 {
		t.Fatalf("matched = %v, want 2", preview["matched"])
	}

	// Ranked, named, and carrying why each one qualified.
	first, _ := tracks[0].(map[string]any)
	if r, _ := first["rank"].(float64); int(r) != 1 {
		t.Fatalf("first entry has rank %v, want 1", first["rank"])
	}
	if p, _ := first["plays"].(float64); int(p) != 5 {
		t.Fatalf("first entry reports %v plays, want the most-played track first", first["plays"])
	}
	track, _ := first["track"].(map[string]any)
	if track == nil || track["name"] == "" {
		t.Fatalf("preview entry carries no track: %v", first)
	}

	// Nothing was created, and creating still needs permission.
	if inst.stub.playlistCalls != 0 {
		t.Fatal("a preview created something on Spotify")
	}
	list := decode[[]map[string]any](t, b.get("/api/playlists"), http.StatusOK)
	if len(list) != 0 {
		t.Fatalf("%d playlists exist after a preview, want none", len(list))
	}
	refused := b.postJSON("/api/playlists", map[string]any{
		"name": "Real", "mode": "min_plays", "minPlays": 3, "limit": 50,
	})
	refused.Body.Close()
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("creating without the scope returned %d, want 403", refused.StatusCode)
	}
}

// TestPlaylistPreviewMatchesWhatIsCreated: a preview nobody can rely on is
// worse than none, so the two paths must resolve the same definition to the
// same tracks in the same order.
func TestPlaylistPreviewMatchesWhatIsCreated(t *testing.T) {
	inst := newInstance(t)
	inst.stub.grantedScopes = append(config.DefaultScopes(), "playlist-modify-private")
	b := inst.browser()
	inst.signIn(b)

	seedPlays(t, inst, b, map[string]int{
		"pv000000000000000004d": 7,
		"pv000000000000000005e": 4,
		"pv000000000000000006f": 2,
	})

	body := map[string]any{"name": "Top three", "mode": "top", "limit": 2}

	preview := decode[map[string]any](t, b.postJSON("/api/playlists/preview", body), http.StatusOK)
	previewed := make([]string, 0, 2)
	for _, raw := range preview["tracks"].([]any) {
		entry := raw.(map[string]any)
		previewed = append(previewed, entry["track"].(map[string]any)["id"].(string))
	}
	// The limit cut the list, and the preview says what it cut.
	if len(previewed) != 2 {
		t.Fatalf("preview holds %d tracks, want the limit of 2", len(previewed))
	}
	if n, _ := preview["matched"].(float64); int(n) != 3 {
		t.Fatalf("matched = %v, want the 3 that qualified before the limit", preview["matched"])
	}

	created := decode[map[string]any](t, b.postJSON("/api/playlists", body), http.StatusCreated)
	sent := inst.stub.playlistItems[created["spotifyId"].(string)]

	if len(sent) != len(previewed) {
		t.Fatalf("preview showed %d tracks and %d were sent", len(previewed), len(sent))
	}
	for i, id := range previewed {
		if want := "spotify:track:" + id; sent[i] != want {
			t.Fatalf("position %d: preview said %s, Spotify got %s", i, want, sent[i])
		}
	}
}

// TestAlbumTracklistFillsInWithoutBlockingThePage walks the whole feature: the
// first request answers immediately without a listing, the fetch lands behind
// it, and a later request names the tracks that were never played.
func TestAlbumTracklistFillsInWithoutBlockingThePage(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)
	// An album of twelve tracks, of which the listener has played nine.
	const albumID = "e2etrklstfillsin00001"
	inst.seedAlbumWithPlays(b, albumID, 12, 9)

	start := time.Now()
	first := decode[httpapi.AlbumTrackList](t, b.get("/api/albums/"+albumID+"/tracklist"), http.StatusOK)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the first request took %s; it waited for Spotify", elapsed)
	}
	if first.State != "pending" {
		t.Fatalf("first state = %q, want \"pending\"", first.State)
	}
	if len(first.Missing) != 0 {
		t.Fatalf("first response named %d missing tracks before any fetch finished", len(first.Missing))
	}

	var got httpapi.AlbumTrackList
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got = decode[httpapi.AlbumTrackList](t, b.get("/api/albums/"+albumID+"/tracklist"), http.StatusOK)
		if got.State == "ready" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "ready" {
		t.Fatalf("state never became ready; last was %q", got.State)
	}
	if got.Coverage.Total != 12 || got.Coverage.Covered != 9 {
		t.Fatalf("coverage = %d/%d, want 9/12", got.Coverage.Covered, got.Coverage.Total)
	}
	if len(got.Missing) != 3 {
		t.Fatalf("named %d missing tracks, want 3", len(got.Missing))
	}
	if got.FetchedAt == nil {
		t.Fatal("fetchedAt is absent on a ready listing; the page cannot say how old it is")
	}
	if n := inst.stub.albumTrackCalls(); n != 1 {
		t.Fatalf("the stub served %d album-track requests, want 1: the poll refetched", n)
	}
}

// TestAlbumTracklistRefusesAnAlbumNobodyHasPlayed keeps an arbitrary id in the
// URL from spending a Spotify request.
func TestAlbumTracklistRefusesAnAlbumNobodyHasPlayed(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	resp := b.get("/api/albums/1BBBBBBBBBBBBBBBBBBBBB/tracklist")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an album not in the catalogue", resp.StatusCode)
	}
	if n := inst.stub.albumTrackCalls(); n != 0 {
		t.Fatalf("the stub served %d requests for an unknown album, want 0", n)
	}
}

// TestAlbumTracklistDisabledAnswersWithoutTouchingSpotify walks the operator's
// switch end to end, which is the only place the configuration, the service and
// the handler are proved to agree about it.
func TestAlbumTracklistDisabledAnswersWithoutTouchingSpotify(t *testing.T) {
	inst := newInstanceWith(t, map[string]string{"ENCORE_ALBUM_TRACKS_ENABLED": "false"})
	b := inst.browser()
	inst.signIn(b)
	const albumID = "e2etrklstdisabled0001"
	inst.seedAlbumWithPlays(b, albumID, 12, 9)

	resp := b.get("/api/albums/" + albumID + "/tracklist")
	got := decode[httpapi.AlbumTrackList](t, resp, http.StatusOK)
	if got.State != "disabled" {
		t.Fatalf("state = %q, want \"disabled\"", got.State)
	}
	if n := inst.stub.albumTrackCalls(); n != 0 {
		t.Fatalf("the stub served %d album-track requests on a disabled instance, want 0", n)
	}

	// And the lease was never even claimed: no unattended write, either.
	var rows int
	if err := inst.env.Pool.QueryRow(inst.env.Ctx(),
		`SELECT count(*)::int FROM album_track_fetches`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d album_track_fetches rows were written on a disabled instance, want 0", rows)
	}
}

// TestAlbumTracklistUnavailableIsA200NotAnErrorEnvelope proves the one state
// whose whole contract is "stop polling" actually arrives as an ordinary 200
// carrying {"state":"unavailable",...} rather than writeError's envelope —
// the seam a future change to error classification could break silently,
// since no other test here would catch it.
//
// "unavailable" needs no waiting at all. It is the fifteen-minute window
// immediately *after* a recorded failure, not the fifteen minutes after that
// (which is what returns the album to "pending" — see albumtracks.due). A
// Spotify response carrying zero tracks is exactly such a failure: fetch
// records it under errEmptyListing precisely because there is no such record
// as an album with no tracks. Leaving an album unseeded in stub.albumTracks
// is what produces that response, since the stub answers {"items":null} for
// any id nobody told it about.
func TestAlbumTracklistUnavailableIsA200NotAnErrorEnvelope(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	// One play puts the album in the catalogue, which the endpoint requires
	// before it does anything else; stub.albumTracks is deliberately left with
	// no entry for it.
	const albumID = "e2etrklstunavail00001"
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{samePlayItem("e2etrkunavailabl0001a", albumID, at)}
	syncResult := decode[map[string]any](t, b.postJSON("/api/sync/now", nil), http.StatusOK)
	if n, _ := syncResult["imported"].(float64); int(n) != 1 {
		t.Fatalf("sync reported %v imported while seeding, want 1", syncResult["imported"])
	}

	var got httpapi.AlbumTrackList
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp := b.get("/api/albums/" + albumID + "/tracklist")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tracklist answered %d while the fetch was resolving, want 200 throughout every state; body: %s",
				resp.StatusCode, body)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode tracklist: %v; body: %s", err, body)
		}
		if got.State == "unavailable" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "unavailable" {
		t.Fatalf("state never became unavailable; last was %q", got.State)
	}
	if got.Coverage.Total != 0 || got.Coverage.Covered != 0 {
		t.Fatalf("coverage = %+v, want zero on both sides with no listing ever stored", got.Coverage)
	}
	if len(got.Missing) != 0 {
		t.Fatalf("missing has %d entries with no listing ever stored, want 0", len(got.Missing))
	}
	if n := inst.stub.albumTrackCalls(); n != 1 {
		t.Fatalf("the stub served %d album-track requests, want exactly 1: a recorded failure "+
			"must not be retried inside its fifteen-minute backoff", n)
	}
}

// seedArtistWithPlays gives one artist a Spotify discography of `albums`
// album-group releases plus `singles` singles, and plays one track from the
// first `played` albums, so the discography endpoint has a real numerator and a
// real denominator to work with.
func (i *instance) seedArtistWithPlays(b *browser, artistID string, albums, singles, played int) {
	i.t.Helper()
	if played > albums {
		i.t.Fatalf("seedArtistWithPlays: played (%d) exceeds albums (%d)", played, albums)
	}
	// One play per album puts the album (and the artist) in the catalogue.
	plays := make([]map[string]any, 0, played)
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	listing := make([]map[string]any, 0, albums+singles)
	for n := range albums {
		albumID := fmt.Sprintf("%salb%02d", artistID[:14], n)
		listing = append(listing, map[string]any{
			"id": albumID, "name": fmt.Sprintf("Album %d", n), "album_group": "album",
			"release_date": fmt.Sprintf("%d", 2010+n), "release_date_precision": "year",
		})
		if n < played {
			plays = append(plays, artistPlayItem(
				fmt.Sprintf("%strk%02d", artistID[:14], n), albumID, artistID, at.Add(-time.Duration(n)*time.Minute)))
		}
	}
	for n := range singles {
		listing = append(listing, map[string]any{
			"id": fmt.Sprintf("%ssng%02d", artistID[:14], n), "name": fmt.Sprintf("Single %d", n),
			"album_group": "single", "release_date": "2021", "release_date_precision": "year",
		})
	}
	i.stub.artistAlbums[artistID] = listing
	i.stub.plays = plays
	res := decode[map[string]any](i.t, b.postJSON("/api/sync/now", nil), http.StatusOK)
	if n, _ := res["imported"].(float64); int(n) != played {
		i.t.Fatalf("sync reported %v imported while seeding, want %d", res["imported"], played)
	}
}

// TestArtistDiscographyFillsInWithoutBlockingThePage walks the whole feature:
// the first request answers immediately without a discography, the walk lands
// behind it, and a later request names the albums that were never played.
func TestArtistDiscographyFillsInWithoutBlockingThePage(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)
	const artistID = "e2ediscofillsin000001"
	// Eleven albums, four played, and forty singles nobody counts.
	inst.seedArtistWithPlays(b, artistID, 11, 40, 4)

	start := time.Now()
	first := decode[httpapi.ArtistDiscography](t,
		b.get("/api/artists/"+artistID+"/discography"), http.StatusOK)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the first request took %s; it waited for Spotify", elapsed)
	}
	if first.State != "pending" {
		t.Fatalf("first state = %q, want \"pending\"", first.State)
	}
	if len(first.Missing) != 0 {
		t.Fatalf("first response named %d missing albums before any walk finished", len(first.Missing))
	}

	var got httpapi.ArtistDiscography
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got = decode[httpapi.ArtistDiscography](t,
			b.get("/api/artists/"+artistID+"/discography"), http.StatusOK)
		if got.State == "ready" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "ready" {
		t.Fatalf("state never became ready; last was %q", got.State)
	}
	// The sentence §5.2 asks for, end to end.
	if got.Coverage.Total != 11 || got.Coverage.Covered != 4 {
		t.Fatalf("coverage = %d/%d, want 4/11: singles must not enter the denominator",
			got.Coverage.Covered, got.Coverage.Total)
	}
	if len(got.Missing) != 7 {
		t.Fatalf("named %d missing albums, want 7", len(got.Missing))
	}
	if got.Excluded.Singles != 40 {
		t.Fatalf("excluded.singles = %d, want 40: the page cannot say what it set aside without this",
			got.Excluded.Singles)
	}
	if got.FetchedAt == nil {
		t.Fatal("fetchedAt is absent on a ready discography; the page cannot say how old it is")
	}
	if n := inst.stub.artistAlbumCalls(); n != 1 {
		t.Fatalf("the stub served %d discography requests, want 1: the poll refetched", n)
	}
}

// TestArtistDiscographyRefusesAnArtistNobodyHasPlayed keeps an arbitrary id in
// the URL from spending up to forty Spotify requests.
func TestArtistDiscographyRefusesAnArtistNobodyHasPlayed(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	resp := b.get("/api/artists/1BBBBBBBBBBBBBBBBBBBBB/discography")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an artist not in the catalogue", resp.StatusCode)
	}
	if n := inst.stub.artistAlbumCalls(); n != 0 {
		t.Fatalf("the stub served %d requests for an unknown artist, want 0", n)
	}
}

// TestArtistDiscographyDisabledAnswersWithoutTouchingSpotify walks the
// operator's switch end to end, which is the only place the configuration, the
// service and the handler are proved to agree about it — and the only place the
// two switches are proved independent at runtime rather than in config parsing.
func TestArtistDiscographyDisabledAnswersWithoutTouchingSpotify(t *testing.T) {
	inst := newInstanceWith(t, map[string]string{"ENCORE_ARTIST_ALBUMS_ENABLED": "false"})
	b := inst.browser()
	inst.signIn(b)
	const artistID = "e2ediscodisabled00001"
	inst.seedArtistWithPlays(b, artistID, 11, 0, 4)

	got := decode[httpapi.ArtistDiscography](t,
		b.get("/api/artists/"+artistID+"/discography"), http.StatusOK)
	if got.State != "disabled" {
		t.Fatalf("state = %q, want \"disabled\"", got.State)
	}
	if n := inst.stub.artistAlbumCalls(); n != 0 {
		t.Fatalf("the stub served %d discography requests on a disabled instance, want 0", n)
	}
	var rows int
	if err := inst.env.Pool.QueryRow(inst.env.Ctx(),
		`SELECT count(*)::int FROM artist_album_fetches`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d artist_album_fetches rows were written on a disabled instance, want 0", rows)
	}
	// And the album endpoint is untouched by the artist switch, which is the
	// whole point of two keys.
	const albumID = "e2ediscodisabled0alb0"
	inst.seedAlbumWithPlays(b, albumID, 12, 9)
	tl := decode[httpapi.AlbumTrackList](t, b.get("/api/albums/"+albumID+"/tracklist"), http.StatusOK)
	if tl.State == "disabled" {
		t.Fatal("turning off discographies also turned off album track listings; the two keys exist " +
			"precisely so an operator can keep the cheap one")
	}
}

// TestArtistDiscographyAllSinglesIsReadyNotAFailure is the state with no
// counterpart on the album endpoint, walked end to end because it is the one
// place a guard on the filtered set — rather than on the whole response — would
// show up as a user-visible lie.
func TestArtistDiscographyAllSinglesIsReadyNotAFailure(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)
	const artistID = "e2ediscoallsingles001"
	// One album so the artist reaches the catalogue at all, then the stub's
	// listing is replaced with singles only: the artist Spotify knows has
	// released nothing it calls an album.
	inst.seedArtistWithPlays(b, artistID, 1, 0, 1)
	inst.stub.artistAlbums[artistID] = []map[string]any{
		{"id": "e2ediscoallsingsng01", "name": "One", "album_group": "single",
			"release_date": "2021", "release_date_precision": "year"},
		{"id": "e2ediscoallsingsng02", "name": "Two", "album_group": "single",
			"release_date": "2022", "release_date_precision": "year"},
	}

	var got httpapi.ArtistDiscography
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got = decode[httpapi.ArtistDiscography](t,
			b.get("/api/artists/"+artistID+"/discography"), http.StatusOK)
		if got.State == "ready" || got.State == "unavailable" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "ready" {
		t.Fatalf("state = %q, want \"ready\": an artist who has only released singles was read "+
			"successfully, and calling that a failure tells them Spotify would not answer about "+
			"somebody Spotify answered about at length", got.State)
	}
	if got.Coverage.Total != 0 || len(got.Missing) != 0 {
		t.Fatalf("coverage = %+v with %d missing, want nothing counted", got.Coverage, len(got.Missing))
	}
	if got.Excluded.Singles != 2 {
		t.Fatalf("excluded.singles = %d, want 2", got.Excluded.Singles)
	}
}

// TestArtistDiscographyUnavailableIsA200NotAnErrorEnvelope proves the one state
// whose whole contract is "stop polling" arrives as an ordinary 200 carrying
// {"state":"unavailable",...} rather than writeError's envelope. Leaving the
// artist unseeded in stub.artistAlbums is what produces it: the stub answers
// {"items":null} for any id nobody told it about, which the service records as a
// failure because there is no such artist as one who has released nothing.
func TestArtistDiscographyUnavailableIsA200NotAnErrorEnvelope(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)
	const artistID = "e2ediscounavail000001"
	inst.seedArtistWithPlays(b, artistID, 1, 0, 1)
	delete(inst.stub.artistAlbums, artistID)

	var got httpapi.ArtistDiscography
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp := b.get("/api/artists/" + artistID + "/discography")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("discography answered %d while the walk was resolving, want 200 throughout every "+
				"state; body: %s", resp.StatusCode, body)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode discography: %v; body: %s", err, body)
		}
		if got.State == "unavailable" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "unavailable" {
		t.Fatalf("state never became unavailable; last was %q", got.State)
	}
	if got.Coverage.Total != 0 || len(got.Missing) != 0 {
		t.Fatalf("coverage = %+v with %d missing, want nothing with no discography ever stored",
			got.Coverage, len(got.Missing))
	}
	if n := inst.stub.artistAlbumCalls(); n != 1 {
		t.Fatalf("the stub served %d discography requests, want exactly 1: a recorded failure must "+
			"not be retried inside its fifteen-minute backoff", n)
	}
}
