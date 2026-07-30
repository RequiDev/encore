package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
)

// The tests in this package exercise the parts of the HTTP surface that need no
// database and no network: the middleware chain, the parameter parsing, the
// redirect validator, the error mapping and the administrator guard. Anything
// that needs a live schema belongs to the integration suite.

// testConfig builds a valid configuration without touching the environment.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(map[string]string{
		"ENCORE_ENV":                   "development",
		"ENCORE_PUBLIC_URL":            "https://encore.example.com",
		"ENCORE_WEB_URL":               "https://encore.example.com/app",
		"ENCORE_DATABASE_URL":          "postgres://encore@localhost:5432/encore",
		"ENCORE_ENCRYPTION_KEY":        base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"ENCORE_SPOTIFY_CLIENT_ID":     "client-id",
		"ENCORE_SPOTIFY_CLIENT_SECRET": "client-secret",
	})
	if err != nil {
		t.Fatalf("build test configuration: %v", err)
	}
	return cfg
}

// --- fakes -----------------------------------------------------------------

// fakeSessions stands in for accounts.Sessions. It records how often a session
// was touched, which is how the once-a-minute rule is asserted.
type fakeSessions struct {
	session domain.Session
	user    domain.User
	lookup  error

	touches int
	deleted []uuid.UUID
}

func (f *fakeSessions) Create(context.Context, store.Querier, uuid.UUID, []byte, string, time.Time, string, string) (domain.Session, error) {
	return f.session, nil
}

func (f *fakeSessions) GetByTokenHash(context.Context, store.Querier, []byte) (domain.Session, domain.User, error) {
	if f.lookup != nil {
		return domain.Session{}, domain.User{}, f.lookup
	}
	return f.session, f.user, nil
}

func (f *fakeSessions) Touch(context.Context, store.Querier, uuid.UUID) error {
	f.touches++
	return nil
}

func (f *fakeSessions) Delete(_ context.Context, _ store.Querier, id uuid.UUID) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// fakeUsers stands in for accounts.Users, answering only from a map.
// fakeListens answers the one question /api/me asks of the listening history:
// how far back it goes. Returning nothing is the honest shape for a user who has
// not imported anything, which is what these tests set up.
type fakeListens struct {
	first, last *time.Time
	count       int64
}

func (f *fakeListens) Bounds(context.Context, store.Querier, uuid.UUID) (*time.Time, *time.Time, error) {
	return f.first, f.last, nil
}

func (f *fakeListens) CountListensForUser(context.Context, store.Querier, uuid.UUID) (int64, error) {
	return f.count, nil
}

type fakeUsers struct {
	byID   map[uuid.UUID]domain.User
	admins int64
	err    error

	roleSet   map[uuid.UUID]domain.Role
	activeSet map[uuid.UUID]bool
	deleted   []uuid.UUID
}

func newFakeUsers(users ...domain.User) *fakeUsers {
	f := &fakeUsers{
		byID:      map[uuid.UUID]domain.User{},
		roleSet:   map[uuid.UUID]domain.Role{},
		activeSet: map[uuid.UUID]bool{},
	}
	for _, u := range users {
		f.byID[u.ID] = u
		if u.Role.IsAdmin() && u.IsActive {
			f.admins++
		}
	}
	return f
}

func (f *fakeUsers) GetByID(_ context.Context, _ store.Querier, id uuid.UUID) (domain.User, error) {
	if f.err != nil {
		return domain.User{}, f.err
	}
	u, ok := f.byID[id]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}

func (f *fakeUsers) ListUsers(context.Context, store.Querier, int, int) ([]domain.User, int64, error) {
	out := make([]domain.User, 0, len(f.byID))
	for _, u := range f.byID {
		out = append(out, u)
	}
	return out, int64(len(out)), nil
}

func (f *fakeUsers) UpsertFromSpotify(context.Context, store.Querier, accounts.SpotifyProfile, string, bool) (domain.User, bool, error) {
	return domain.User{}, false, domain.ErrNotFound
}

func (f *fakeUsers) SetTimezone(_ context.Context, _ store.Querier, id uuid.UUID, tz string) (domain.User, error) {
	u := f.byID[id]
	u.Timezone = tz
	f.byID[id] = u
	return u, nil
}

func (f *fakeUsers) SetRole(_ context.Context, _ store.Querier, id uuid.UUID, role domain.Role) (domain.User, error) {
	f.roleSet[id] = role
	u := f.byID[id]
	u.Role = role
	f.byID[id] = u
	return u, nil
}

func (f *fakeUsers) SetActive(_ context.Context, _ store.Querier, id uuid.UUID, active bool) (domain.User, error) {
	f.activeSet[id] = active
	u := f.byID[id]
	u.IsActive = active
	f.byID[id] = u
	return u, nil
}

func (f *fakeUsers) DeleteUser(_ context.Context, _ store.Querier, id uuid.UUID) error {
	f.deleted = append(f.deleted, id)
	delete(f.byID, id)
	return nil
}

func (f *fakeUsers) CountAdmins(context.Context, store.Querier) (int64, error) { return f.admins, nil }

// fakeCredentials stands in for accounts.Credentials.
type fakeCredentials struct {
	creds domain.SpotifyCredentials
	err   error
}

func (f *fakeCredentials) Get(context.Context, store.Querier, uuid.UUID) (domain.SpotifyCredentials, error) {
	if f.err != nil {
		return domain.SpotifyCredentials{}, f.err
	}
	return f.creds, nil
}

func (f *fakeCredentials) Upsert(_ context.Context, _ store.Querier, creds domain.SpotifyCredentials) error {
	f.creds = creds
	return nil
}

// fakeSettings stands in for accounts.Settings.
type fakeSettings struct {
	registrations bool
	pausedUntil   time.Time
}

func (f *fakeSettings) RegistrationsEnabled(context.Context, store.Querier) (bool, error) {
	return f.registrations, nil
}

func (f *fakeSettings) SetRegistrationsEnabled(_ context.Context, _ store.Querier, enabled bool) error {
	f.registrations = enabled
	return nil
}

func (f *fakeSettings) SpotifyPausedUntil(context.Context, store.Querier) (time.Time, error) {
	return f.pausedUntil, nil
}

// --- server harness --------------------------------------------------------

// testServer is a Server wired to fakes, with the fakes kept to hand so a test
// can assert on what the handlers did to them.
type testServer struct {
	*Server
	sessions    *fakeSessions
	users       *fakeUsers
	credentials *fakeCredentials
	settings    *fakeSettings
	listens     *fakeListens
	clock       time.Time
}

// newTestServer builds a server with no database behind it. New itself insists
// on real repositories, so the struct is assembled directly; the middleware and
// the handlers under test reach only for what is filled in here.
func newTestServer(t *testing.T) *testServer {
	t.Helper()

	user := domain.User{
		ID:            uuid.New(),
		SpotifyUserID: "listener",
		DisplayName:   "Listener",
		Role:          domain.RoleUser,
		IsActive:      true,
		Timezone:      "UTC",
		CreatedAt:     time.Date(2026, time.January, 4, 10, 0, 0, 0, time.UTC),
	}
	session := domain.Session{
		ID:        uuid.New(),
		UserID:    user.ID,
		CSRFToken: "csrf-token-value",
		ExpiresAt: time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC),
	}

	ts := &testServer{
		sessions:    &fakeSessions{session: session, user: user},
		users:       newFakeUsers(user),
		credentials: &fakeCredentials{err: domain.ErrNotFound},
		settings:    &fakeSettings{registrations: true},
		listens:     &fakeListens{},
		clock:       time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
	}
	s := &Server{
		cfg:         testConfig(t),
		log:         slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		version:     "test",
		now:         func() time.Time { return ts.clock },
		users:       ts.users,
		sessions:    ts.sessions,
		credentials: ts.credentials,
		settings:    ts.settings,
		listens:     ts.listens,
		syncing:     newInFlight(),
		touched:     newTouchTracker(),
		ready:       &readyCache{},
	}
	s.handler = s.buildHandler()
	ts.Server = s
	return ts
}

// signedIn adds the cookies a signed-in browser would send.
func (ts *testServer) signedIn(r *http.Request) *http.Request {
	r.AddCookie(&http.Cookie{Name: ts.cfg.Security.CookieName, Value: "session-cookie-value"})
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: ts.sessions.session.CSRFToken})
	return r
}

// do serves one request through the whole middleware chain.
func (ts *testServer) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, r)
	return rec
}

// errorCodeOf reads the code out of an error envelope, failing the test when the
// body is not one.
func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not an error envelope: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Code
}

// TestNewStatsRoutesRequireASession keeps the new endpoints inside the same
// session and CSRF envelope as every other statistic.
func TestNewStatsRoutesRequireASession(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{
		"/api/stats/genres",
		"/api/stats/genres/timeline",
		"/api/stats/taste",
		"/api/stats/context",
	} {
		rec := ts.do(httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, rec.Code)
		}
	}
}

// TestMeReportsMissingScopes is what drives the re-consent prompt.
//
// An account connected before the scope set grew has a grant that will never
// widen on its own — a refresh token carries the scopes it was issued with for
// ever — and Spotify answers 403 for anything needing the new ones. The
// shortfall has to reach the client or the failure is invisible.
func TestMeReportsMissingScopes(t *testing.T) {
	ts := newTestServer(t)
	ts.credentials.err = nil
	ts.credentials.creds = domain.SpotifyCredentials{
		UserID:         ts.sessions.user.ID,
		AccessToken:    "token",
		RefreshToken:   "refresh",
		TokenExpiresAt: ts.clock.Add(time.Hour),
		// The three scopes every account connected before this change holds.
		Scopes:    []string{"user-read-recently-played", "user-read-private", "user-read-email"},
		SyncState: domain.SyncStateOK,
	}

	rec := ts.do(ts.signedIn(httptest.NewRequest(http.MethodGet, "/api/me", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var body MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a me payload: %v (%s)", err, rec.Body.String())
	}

	want := []string{
		"user-top-read", "user-library-read", "user-follow-read",
		"playlist-read-private", "user-read-playback-state",
	}
	if !slices.Equal(body.Spotify.MissingScopes, want) {
		t.Errorf("missingScopes =\n  %v\nwant\n  %v", body.Spotify.MissingScopes, want)
	}
}

// TestMeReportsNoMissingScopesForACurrentGrant guards the other direction. A
// freshly connected account must never be nagged, and an empty result must
// serialise as [] rather than null so the client can test its length.
func TestMeReportsNoMissingScopesForACurrentGrant(t *testing.T) {
	ts := newTestServer(t)
	ts.credentials.err = nil
	ts.credentials.creds = domain.SpotifyCredentials{
		UserID:         ts.sessions.user.ID,
		AccessToken:    "token",
		RefreshToken:   "refresh",
		TokenExpiresAt: ts.clock.Add(time.Hour),
		Scopes:         config.DefaultScopes(),
		SyncState:      domain.SyncStateOK,
	}

	rec := ts.do(ts.signedIn(httptest.NewRequest(http.MethodGet, "/api/me", nil)))
	var body MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a me payload: %v (%s)", err, rec.Body.String())
	}
	if len(body.Spotify.MissingScopes) != 0 {
		t.Errorf("a current grant reported missing scopes: %v", body.Spotify.MissingScopes)
	}
	if !strings.Contains(rec.Body.String(), `"missingScopes":[]`) {
		t.Error("an empty missingScopes must serialise as [] and not null")
	}
}
