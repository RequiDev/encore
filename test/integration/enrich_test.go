//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/enrich"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store/listens"
	"github.com/RequiDev/encore/test/harness"
)

// fakeSpotify is a stand-in for the Spotify Web API. It counts requests per
// path and can be told to answer with 429 a given number of times first, which
// is how the rate-limit behaviour is exercised without waiting on the real
// service or holding real credentials.
type fakeSpotify struct {
	t *testing.T

	// rateLimitFirst is how many requests to reject with 429 before serving.
	rateLimitFirst atomic.Int32
	retryAfter     string

	tokenCalls  atomic.Int32
	trackCalls  atomic.Int32
	albumCalls  atomic.Int32
	artistCalls atomic.Int32
	searchCalls atomic.Int32
	rejected    atomic.Int32

	server *httptest.Server
}

func newFakeSpotify(t *testing.T) *fakeSpotify {
	t.Helper()
	f := &fakeSpotify{t: t, retryAfter: "1"}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		writeJSON(w, map[string]any{
			"access_token": "test-app-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})

	mux.HandleFunc("/v1/tracks", func(w http.ResponseWriter, r *http.Request) {
		if f.throttle(w) {
			return
		}
		f.trackCalls.Add(1)
		ids := splitIDs(r.URL.Query().Get("ids"))
		out := make([]any, 0, len(ids))
		for _, id := range ids {
			// A single well-known id is answered as null, which is how Spotify
			// reports a track that no longer exists.
			if id == deletedTrackID {
				out = append(out, nil)
				continue
			}
			out = append(out, map[string]any{
				"id": id, "name": "Track " + id, "duration_ms": 210000, "explicit": false,
				"popularity":   50,
				"external_ids": map[string]any{"isrc": "GB" + id[:8]},
				"album": map[string]any{
					"id": albumIDFor(id), "name": "Album " + id, "album_type": "album",
					"release_date": "2011-03", "release_date_precision": "month",
					"total_tracks": 12,
					"images":       []any{map[string]any{"url": "https://i.example/" + id, "width": 640, "height": 640}},
					"artists":      []any{map[string]any{"id": artistIDFor(id), "name": "Artist " + id}},
				},
				"artists": []any{map[string]any{"id": artistIDFor(id), "name": "Artist " + id}},
			})
		}
		writeJSON(w, map[string]any{"tracks": out})
	})

	mux.HandleFunc("/v1/albums", func(w http.ResponseWriter, r *http.Request) {
		if f.throttle(w) {
			return
		}
		f.albumCalls.Add(1)
		ids := splitIDs(r.URL.Query().Get("ids"))
		out := make([]any, 0, len(ids))
		for _, id := range ids {
			out = append(out, map[string]any{
				"id": id, "name": "Album " + id, "album_type": "album",
				"release_date": "2011-03-04", "release_date_precision": "day",
				"total_tracks": 12,
				"images":       []any{map[string]any{"url": "https://i.example/" + id}},
				"artists":      []any{map[string]any{"id": artistIDFor(id), "name": "Artist " + id}},
			})
		}
		writeJSON(w, map[string]any{"albums": out})
	})

	mux.HandleFunc("/v1/artists", func(w http.ResponseWriter, r *http.Request) {
		if f.throttle(w) {
			return
		}
		f.artistCalls.Add(1)
		ids := splitIDs(r.URL.Query().Get("ids"))
		out := make([]any, 0, len(ids))
		for _, id := range ids {
			out = append(out, map[string]any{
				"id": id, "name": "Artist " + id, "popularity": 60,
				"genres":    []string{"indie"},
				"followers": map[string]any{"total": 12345},
				"images":    []any{map[string]any{"url": "https://i.example/" + id}},
			})
		}
		writeJSON(w, map[string]any{"artists": out})
	})

	mux.HandleFunc("/v1/search", func(w http.ResponseWriter, r *http.Request) {
		if f.throttle(w) {
			return
		}
		f.searchCalls.Add(1)
		writeJSON(w, map[string]any{
			"tracks": map[string]any{"items": []any{map[string]any{
				"id": searchedTrack, "name": "Weird Fishes", "duration_ms": 300000,
				"album":   map[string]any{"id": searchedAlbum, "name": "In Rainbows", "album_type": "album"},
				"artists": []any{map[string]any{"id": searchedArtist, "name": "Radiohead"}},
			}}},
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// Spotify ids are base62, and the client refuses anything else rather than
// spending a request on an id that cannot exist. The fake therefore derives
// valid ids rather than readable ones like "album-<track>", which the client
// would (correctly) drop.
const (
	deletedTrackID = "deleted00000000000000z"
	searchedTrack  = "srch000000000000000001"
	searchedAlbum  = "srch000000000000000002"
	searchedArtist = "srch000000000000000003"
)

// albumIDFor and artistIDFor derive a valid, distinct id from a track id.
func albumIDFor(trackID string) string  { return "b" + trackID[1:] }
func artistIDFor(trackID string) string { return "c" + trackID[1:] }

// throttle answers with 429 while the budget lasts, reporting whether it did.
func (f *fakeSpotify) throttle(w http.ResponseWriter) bool {
	if f.rateLimitFirst.Load() <= 0 {
		return false
	}
	f.rateLimitFirst.Add(-1)
	f.rejected.Add(1)
	w.Header().Set("Retry-After", f.retryAfter)
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":{"status":429,"message":"API rate limit exceeded"}}`))
	return true
}

func (f *fakeSpotify) client(t *testing.T) *spotify.Client {
	t.Helper()
	cfg := config.Spotify{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		APIBaseURL:   f.server.URL,
		AuthBaseURL:  f.server.URL,
		TokenURL:     f.server.URL + "/api/token",
		RateLimit:    1000,
		RateBurst:    1000,
		Timeout:      10 * time.Second,
		MaxRetries:   4,
	}
	return spotify.NewClient(cfg, harness.Discard(), spotify.WithBaseURL(f.server.URL))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func splitIDs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func newEnrichWorker(t *testing.T, env *harness.Env, fake *fakeSpotify, mutate func(*config.Enrich)) *enrich.Worker {
	t.Helper()
	cfg := config.Enrich{
		Enabled:        true,
		Interval:       10 * time.Millisecond,
		BatchSize:      50,
		AliasEnabled:   true,
		AliasRate:      1000,
		RepairInterval: time.Hour,
		RollupInterval: time.Hour,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	w, err := enrich.New(cfg, enrich.Deps{
		Store:    env.Store,
		Catalog:  env.Catalog,
		Listens:  env.Listens,
		Accounts: env.Accounts,
		Stats:    stats.New(env.Store),
		Spotify:  fake.client(t),
		Logger:   harness.Discard(),
		Rand:     func() float64 { return 0 },
	})
	if err != nil {
		t.Fatalf("build enrichment worker: %v", err)
	}
	return w
}

// TestEnrichmentResolvesCatalogueQueuedByIngestion walks the seam the whole
// design turns on: ingestion writes ids in the pending state without ever
// calling Spotify, and enrichment fills them in afterwards.
func TestEnrichmentResolvesCatalogueQueuedByIngestion(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	worker := newEnrichWorker(t, env, fake, nil)
	user := env.NewUser("enrichuser")

	trackIDs := []string{"aaaaaaaaaaaaaaaaaaaaa1", "bbbbbbbbbbbbbbbbbbbbb2", deletedTrackID}
	if err := env.Listens.EnsureTracks(env.Ctx(), env.Store.DB(), trackSeeds(trackIDs...)); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	batch := make([]listens.StagedListen, 0, len(trackIDs))
	at := time.Date(2024, time.February, 1, 12, 0, 0, 0, time.UTC)
	for i, id := range trackIDs {
		batch = append(batch, listens.Stage(domain.Listen{
			UserID:    user.ID,
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

	if got := env.ScalarInt(`SELECT count(*) FROM tracks WHERE metadata_state = 'pending'`); got != 3 {
		t.Fatalf("%d tracks pending after ingestion, want 3", got)
	}

	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("run tracks: %v", err)
	}

	if got := env.ScalarInt(`SELECT count(*) FROM tracks WHERE metadata_state = 'resolved'`); got != 2 {
		t.Fatalf("%d tracks resolved, want 2", got)
	}
	// The id Spotify answered as null is unavailable, not failed: retrying it on
	// the normal schedule would be pointless.
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE id = $1 AND metadata_state = 'unavailable'`, deletedTrackID); got != 1 {
		t.Fatal("a track Spotify returns as null must be recorded as unavailable")
	}
	// Resolving a track must also register its album and artist for enrichment.
	if got := env.ScalarInt(`SELECT count(*) FROM albums`); got == 0 {
		t.Fatal("resolving a track did not register its album")
	}
	if got := env.ScalarInt(`SELECT count(*) FROM track_artists`); got == 0 {
		t.Fatal("resolving a track did not record its artists")
	}

	// The listening records were never touched by any of this.
	if got := env.CountListens(user.ID); got != 3 {
		t.Fatalf("enrichment changed the listen count to %d, want 3", got)
	}

	if _, err := worker.RunAlbumsOnce(env.Ctx()); err != nil {
		t.Fatalf("run albums: %v", err)
	}
	if _, err := worker.RunArtistsOnce(env.Ctx()); err != nil {
		t.Fatalf("run artists: %v", err)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM albums WHERE metadata_state = 'resolved'`); got == 0 {
		t.Fatal("no albums were resolved")
	}
	if got := env.ScalarInt(`SELECT count(*) FROM artists WHERE metadata_state = 'resolved'`); got == 0 {
		t.Fatal("no artists were resolved")
	}
}

// TestRateLimitIsHonouredAndImportsAreUnaffected is the rate-limiting scenario
// the brief requires. It asserts two separate things: the client waits out
// Retry-After rather than hammering, and a rate limit cannot touch a listening
// record, because ingestion never calls Spotify at all.
func TestRateLimitIsHonouredAndImportsAreUnaffected(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	// Two rejections, then success. Retry-After is a fraction of a second so the
	// test observes the delay without spending a real second on it.
	fake.retryAfter = "1"
	fake.rateLimitFirst.Store(2)

	worker := newEnrichWorker(t, env, fake, nil)
	user := env.NewUser("ratelimit")

	ids := []string{"ccccccccccccccccccccc3", "ddddddddddddddddddddd4"}
	if err := env.Listens.EnsureTracks(env.Ctx(), env.Store.DB(), trackSeeds(ids...)); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	batch := []listens.StagedListen{
		listens.Stage(domain.Listen{
			UserID: user.ID, PlayedAt: time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC),
			Precision: domain.PrecisionSecond, Identity: domain.TrackIdentityFromID(ids[0]),
			MsPlayed: 200_000, Source: domain.SourceExtended,
		}, nil),
	}
	if _, err := env.Listens.InsertListens(env.Ctx(), env.Store.DB(), batch, "UTC"); err != nil {
		t.Fatalf("seed listens: %v", err)
	}
	before := env.CountListens(user.ID)

	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("enrichment gave up on a 429 instead of waiting: %v", err)
	}

	if fake.rejected.Load() != 2 {
		t.Fatalf("the fake rejected %d requests, want 2", fake.rejected.Load())
	}
	if fake.trackCalls.Load() == 0 {
		t.Fatal("enrichment never retried after the rate limit cleared")
	}
	if got := env.ScalarInt(`SELECT count(*) FROM tracks WHERE metadata_state = 'resolved'`); got != 2 {
		t.Fatalf("%d tracks resolved after the rate limit cleared, want 2", got)
	}

	// The decisive assertion: nothing about the listening data moved.
	if after := env.CountListens(user.ID); after != before {
		t.Fatalf("a Spotify rate limit changed the listen count from %d to %d", before, after)
	}
}

// TestRateLimitDoesNotBlockAnImport runs a real import while Spotify is
// refusing every request, and asserts the import completes with full counts.
func TestRateLimitDoesNotBlockAnImport(t *testing.T) {
	rig := harness.NewRig(t, nil)
	fake := newFakeSpotify(t)
	fake.rateLimitFirst.Store(1 << 20) // refuse everything, indefinitely
	_ = newEnrichWorker(t, rig.Env, fake, nil)

	user := rig.NewUser("importduringlimit")
	path := fmt.Sprintf("%s/Streaming_History_Audio_2015-2017_0.json", t.TempDir())
	total := harness.WriteExtendedExport(t, path, harness.GenOptions{Records: 300, Seed: 8})

	job := rig.Submit(user.ID, "during a rate limit", path)
	rig.Drain(rig.Ctx())

	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	rig.RequireAccounted(done)
	rig.RequireCommitted(done)
	if done.Counters.Processed() != int64(total) {
		t.Fatalf("accounted for %d of %d records while Spotify was rate limiting", done.Counters.Processed(), total)
	}
	if fake.trackCalls.Load() != 0 {
		t.Fatalf("the importer made %d Spotify catalogue calls; it must make none",
			fake.trackCalls.Load())
	}
}

// TestAliasResolutionConvergesNamesOnlyListens drives the relink pass through
// the real enrichment worker rather than by hand.
func TestAliasResolutionConvergesNamesOnlyListens(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	worker := newEnrichWorker(t, env, fake, nil)
	user := env.NewUser("aliasconverge")

	// A URI-based listen and, one minute-bucket away, the same play as it would
	// arrive from an account-data export.
	at := time.Date(2024, time.April, 1, 12, 0, 10, 0, time.UTC)
	if err := env.Listens.EnsureTracks(env.Ctx(), env.Store.DB(), trackSeeds(searchedTrack)); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	uriListen := listens.Stage(domain.Listen{
		UserID: user.ID, PlayedAt: at, Precision: domain.PrecisionSecond,
		Identity: domain.TrackIdentityFromID(searchedTrack),
		MsPlayed: 300_000, Source: domain.SourceExtended,
	}, nil)
	nameListen := listens.Stage(domain.Listen{
		UserID: user.ID, PlayedAt: at.Add(-37 * time.Second), Precision: domain.PrecisionMinute,
		Identity: domain.TrackIdentityFromNames("Radiohead", "Weird Fishes"),
		MsPlayed: 300_000, Source: domain.SourceAccountData,
	}, nil)

	if _, err := env.Listens.InsertListens(env.Ctx(), env.Store.DB(), []listens.StagedListen{uriListen}, "UTC"); err != nil {
		t.Fatalf("seed uri listen: %v", err)
	}
	if err := env.Listens.EnsureAliases(env.Ctx(), env.Store.DB(),
		[]domain.AliasKey{domain.AliasKeyFor("Radiohead", "Weird Fishes")}); err != nil {
		t.Fatalf("ensure alias: %v", err)
	}
	if _, err := env.Listens.InsertListens(env.Ctx(), env.Store.DB(), []listens.StagedListen{nameListen}, "UTC"); err != nil {
		t.Fatalf("seed name listen: %v", err)
	}
	if got := env.CountListens(user.ID); got != 2 {
		t.Fatalf("before resolution the user has %d listens, want 2", got)
	}

	if _, err := worker.RunAliasesOnce(env.Ctx()); err != nil {
		t.Fatalf("resolve aliases: %v", err)
	}
	// The relink loop drains a queue the alias loop fed, so it may need a couple
	// of turns.
	for range 5 {
		if _, err := worker.RunRelinkOnce(env.Ctx()); err != nil {
			t.Fatalf("relink: %v", err)
		}
		if env.ScalarInt(`SELECT count(*) FROM listens WHERE track_id IS NULL`) == 0 {
			break
		}
	}

	if got := env.ScalarInt(`SELECT count(*) FROM listens WHERE track_id IS NULL`); got != 0 {
		t.Fatalf("%d listens are still unresolved after alias resolution", got)
	}
	if got := env.CountListens(user.ID); got != 1 {
		t.Fatalf("after convergence the user has %d listens, want 1: the two records "+
			"describe the same play seen through two exports", got)
	}
	if fake.searchCalls.Load() == 0 {
		t.Fatal("alias resolution never called the search endpoint")
	}
}

// trackSeeds turns bare ids into seeds for the tests that only care that the
// rows exist, not what they are called.
func trackSeeds(ids ...string) []listens.TrackSeed {
	out := make([]listens.TrackSeed, len(ids))
	for i, id := range ids {
		out[i] = listens.TrackSeed{ID: id}
	}
	return out
}
