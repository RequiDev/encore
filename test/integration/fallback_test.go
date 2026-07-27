//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/enrich"
	"github.com/RequiDev/encore/internal/metadata"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store/listens"
	"github.com/RequiDev/encore/test/harness"
)

// fakeFallback implements docs/metadata-fallback.md and nothing else.
//
// It is written against the document rather than against the client, so if the
// two ever disagree these tests are what notices.
type fakeFallback struct {
	server *httptest.Server
	// known is the set of ids this source can describe. Everything else is null.
	known map[string]bool
	token string

	calls atomic.Int32
	// lastIDs records what the most recent request asked for, which is how a test
	// checks that the fallback was asked only about what Spotify could not serve.
	lastIDs atomic.Value
}

func newFakeFallback(t *testing.T, token string, known ...string) *fakeFallback {
	t.Helper()
	f := &fakeFallback{known: map[string]bool{}, token: token}
	for _, id := range known {
		f.known[id] = true
	}

	handle := func(key string, object func(id string) map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.calls.Add(1)
			if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			ids := strings.Split(r.URL.Query().Get("ids"), ",")
			f.lastIDs.Store(append([]string(nil), ids...))

			items := make([]any, 0, len(ids))
			for _, id := range ids {
				if !f.known[id] {
					items = append(items, nil)
					continue
				}
				items = append(items, object(id))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{key: items})
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tracks", handle("tracks", func(id string) map[string]any {
		return map[string]any{
			"id": id, "name": "Mirrored " + id, "duration_ms": 180000,
			"album":   map[string]any{"id": albumIDFor(id)},
			"artists": []any{map[string]any{"id": artistIDFor(id), "name": "Mirrored artist"}},
		}
	}))
	mux.HandleFunc("/v1/artists", handle("artists", func(id string) map[string]any {
		return map[string]any{"id": id, "name": "Mirrored artist " + id, "genres": []string{"mirror"}}
	}))
	mux.HandleFunc("/v1/albums", handle("albums", func(id string) map[string]any {
		return map[string]any{
			"id": id, "name": "Mirrored album " + id, "album_type": "album",
			"release_date": "2011", "release_date_precision": "year",
		}
	}))

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeFallback) requestedIDs() []string {
	v, _ := f.lastIDs.Load().([]string)
	return v
}

// newChainedWorker builds an enrichment worker whose catalogue reads go through
// a Spotify client and a fallback, exactly as the worker binary wires them.
func newChainedWorker(
	t *testing.T,
	env *harness.Env,
	fake *fakeSpotify,
	fb *fakeFallback,
	token string,
) (*enrich.Worker, *spotify.Client) {
	t.Helper()

	client := fake.client(t)
	mirror, err := metadata.NewMirror(config.MetadataFallback{
		URL: fb.server.URL, Token: token, Timeout: 5 * time.Second, BatchSize: 50,
	}, metadata.WithHTTPClient(fb.server.Client()), metadata.WithLogger(harness.Discard()))
	if err != nil {
		t.Fatalf("build mirror: %v", err)
	}

	w, err := enrich.New(config.Enrich{
		Enabled: true, Interval: 10 * time.Millisecond, BatchSize: 50,
		AliasEnabled: true, AliasRate: 1000,
		RepairInterval: time.Hour, RollupInterval: time.Hour,
	}, enrich.Deps{
		Store: env.Store, Catalog: env.Catalog, Listens: env.Listens,
		Accounts: env.Accounts, Stats: stats.New(env.Store),
		Spotify: client,
		Catalogue: metadata.NewChain(client, mirror,
			metadata.WithChainLogger(harness.Discard()),
			metadata.WithPauseCheck(client.Limiter().PausedUntil)),
		Logger: harness.Discard(),
		Rand:   func() float64 { return 0 },
	})
	if err != nil {
		t.Fatalf("build enrichment worker: %v", err)
	}
	return w, client
}

// seedListensFor queues track ids for enrichment the way ingestion does: the
// ids are written in the pending state without any Spotify call.
func seedListensFor(t *testing.T, env *harness.Env, userID uuid.UUID, ids ...string) {
	t.Helper()
	if err := env.Listens.EnsureTracks(env.Ctx(), env.Store.DB(), trackSeeds(ids...)); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	batch := make([]listens.StagedListen, 0, len(ids))
	at := time.Date(2024, time.March, 1, 9, 0, 0, 0, time.UTC)
	for i, id := range ids {
		batch = append(batch, listens.Stage(domain.Listen{
			UserID:    userID,
			PlayedAt:  at.Add(time.Duration(i) * time.Hour),
			Precision: domain.PrecisionSecond,
			Identity:  domain.TrackIdentityFromID(id),
			MsPlayed:  200_000,
			Source:    domain.SourceExtended,
		}, nil))
	}
	if _, err := env.Listens.InsertListens(env.Ctx(), env.Store.DB(), batch, "UTC"); err != nil {
		t.Fatalf("seed listens: %v", err)
	}
}

// TestFallbackFillsWhatSpotifyWillNotServe is the quiet half of the feature.
//
// A track Spotify answers with null is marked unavailable, which is terminal —
// the repair pass deliberately never revisits it. Before a fallback existed that
// was correct, because there was nowhere else to ask. Now there is, and the id
// must reach it before being written off for good.
func TestFallbackFillsWhatSpotifyWillNotServe(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	// The fallback knows precisely the track Spotify refuses to serve.
	fb := newFakeFallback(t, "", deletedTrackID)
	worker, _ := newChainedWorker(t, env, fake, fb, "")
	user := env.NewUser("fallbackuser")

	live := "aaaaaaaaaaaaaaaaaaaaa1"
	seedListensFor(t, env, user.ID, live, deletedTrackID)

	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("run tracks: %v", err)
	}

	// Both tracks are resolved: one by Spotify, one by the fallback.
	if got := env.ScalarInt(`SELECT count(*) FROM tracks WHERE metadata_state = 'resolved'`); got != 2 {
		t.Fatalf("%d tracks resolved, want 2 — the fallback did not fill the gap", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE metadata_state = 'unavailable'`); got != 0 {
		t.Fatalf("%d tracks written off although the fallback served them", got)
	}
	var name string
	if err := env.Store.DB().QueryRow(env.Ctx(),
		`SELECT name FROM tracks WHERE id = $1`, deletedTrackID).Scan(&name); err != nil {
		t.Fatalf("read the mirrored track: %v", err)
	}
	if !strings.HasPrefix(name, "Mirrored ") {
		t.Fatalf("track name is %q, want the fallback's", name)
	}

	// And the fallback was asked only about the id Spotify declined — not the
	// whole batch, which would double every enrichment request on the instance.
	if got := fb.requestedIDs(); len(got) != 1 || got[0] != deletedTrackID {
		t.Fatalf("the fallback was asked for %v, want only %q", got, deletedTrackID)
	}
}

// TestFallbackCarriesEnrichmentThroughARateLimit is the case the feature was
// asked for: Spotify has stopped answering for the day, and metadata keeps
// arriving anyway.
func TestFallbackCarriesEnrichmentThroughARateLimit(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	live := "aaaaaaaaaaaaaaaaaaaaa1"
	fb := newFakeFallback(t, "", live)
	worker, client := newChainedWorker(t, env, fake, fb, "")
	user := env.NewUser("pauseduser")

	seedListensFor(t, env, user.ID, live)

	// The state an exhausted daily quota leaves behind, and the state a restart
	// restores from app_settings: the limiter is held back for hours.
	//
	// It is set directly rather than provoked with a 429 because the client's own
	// retry loop would then wait the pause out inside the first call — which is
	// precisely the stall this feature exists to route around, and not something
	// a test should sit through.
	client.Limiter().Pause(time.Now().Add(time.Hour))
	before := fake.trackCalls.Load()

	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("run tracks while paused: %v", err)
	}
	if after := fake.trackCalls.Load(); after != before {
		t.Fatalf("Spotify was called %d more times while paused; the batch would have "+
			"blocked for the length of the pause", after-before)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM tracks WHERE metadata_state = 'resolved'`); got != 1 {
		t.Fatalf("%d tracks resolved from the fallback while Spotify was paused, want 1", got)
	}
}

// TestAPausedPrimaryNeverWritesOffAnID is the destructive case, checked against
// the real schema rather than in isolation.
//
// While Spotify is paused it has not declined anything. An id the fallback
// happens not to know must stay queued; marking it unavailable would blank it
// for the life of the instance, because nothing revisits that state.
func TestAPausedPrimaryNeverWritesOffAnID(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	unknown := "bbbbbbbbbbbbbbbbbbbbb2"
	fb := newFakeFallback(t, "") // knows nothing at all
	worker, client := newChainedWorker(t, env, fake, fb, "")
	user := env.NewUser("neverwriteoff")

	seedListensFor(t, env, user.ID, unknown)

	client.Limiter().Pause(time.Now().Add(time.Hour))
	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("run tracks while paused: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE metadata_state = 'unavailable'`); got != 0 {
		t.Fatal("an id was marked permanently unavailable while Spotify was paused; " +
			"Spotify never declined it, and nothing revisits that state")
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE id = $1 AND metadata_state = 'pending'`, unknown); got != 1 {
		t.Fatal("the id is no longer pending, so it will never be asked of Spotify again")
	}
}

// TestFallbackAuthenticationIsSent proves the bearer token reaches the source,
// since a fallback that silently 401s looks exactly like one that is empty.
func TestFallbackAuthenticationIsSent(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	fb := newFakeFallback(t, "sekrit", deletedTrackID)
	worker, _ := newChainedWorker(t, env, fake, fb, "sekrit")
	user := env.NewUser("authuser")

	seedListensFor(t, env, user.ID, deletedTrackID)

	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("run tracks: %v", err)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE id = $1 AND metadata_state = 'resolved'`,
		deletedTrackID); got != 1 {
		t.Fatal("the authenticated fallback did not serve the track")
	}
}

// TestAFallbackOutageLeavesSpotifyInCharge: a mirror that is down must not stop
// enrichment, and must not stop Spotify's verdict being recorded.
func TestAFallbackOutageLeavesSpotifyInCharge(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	fb := newFakeFallback(t, "", deletedTrackID)
	worker, _ := newChainedWorker(t, env, fake, fb, "")
	user := env.NewUser("outageuser")

	fb.server.Close() // the mirror is gone before a single request

	live := "aaaaaaaaaaaaaaaaaaaaa1"
	seedListensFor(t, env, user.ID, live, deletedTrackID)

	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("a fallback outage failed the batch: %v", err)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE id = $1 AND metadata_state = 'resolved'`, live); got != 1 {
		t.Fatal("Spotify's answer was lost because the fallback was unreachable")
	}
	// Spotify declined this one, and Spotify is authoritative, so the verdict
	// stands exactly as it would on an instance with no fallback at all.
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE id = $1 AND metadata_state = 'unavailable'`,
		deletedTrackID); got != 1 {
		t.Fatal("an id Spotify declined was not recorded as unavailable")
	}
}
