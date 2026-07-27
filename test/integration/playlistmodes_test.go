//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store/listens"
	"github.com/RequiDev/encore/test/harness"
)

// The four playlist modes, checked against a history built to tell them apart.
//
// They are four different questions, and it is easy to write three of them
// correctly and have the fourth quietly answer one of the others.

type play struct {
	track string
	when  time.Time
}

func seedPlaylistHistory(t *testing.T, env *harness.Env, userID uuid.UUID, plays []play) {
	t.Helper()
	seen := map[string]bool{}
	seeds := make([]listens.TrackSeed, 0, len(plays))
	batch := make([]listens.StagedListen, 0, len(plays))
	for _, p := range plays {
		if !seen[p.track] {
			seen[p.track] = true
			seeds = append(seeds, listens.TrackSeed{ID: p.track, Name: "Track " + p.track})
		}
		batch = append(batch, listens.Stage(domain.Listen{
			UserID: userID, PlayedAt: p.when, Precision: domain.PrecisionSecond,
			Identity: domain.TrackIdentityFromID(p.track),
			MsPlayed: 200_000, Source: domain.SourceExtended,
		}, nil))
	}
	ctx, db := env.Ctx(), env.Store.DB()
	if err := env.Listens.EnsureTracks(ctx, db, seeds); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	if _, err := env.Listens.InsertListens(ctx, db, batch, "UTC"); err != nil {
		t.Fatalf("insert listens: %v", err)
	}
}

// Track ids for the four shapes that tell the modes apart.
const (
	// trackOld was played a lot before the window and never inside it.
	trackOld = "old0000000000000000aa"
	// trackSteady was played before the window and again inside it.
	trackSteady = "steady0000000000000bb"
	// trackFresh was first heard inside the window.
	trackFresh = "fresh00000000000000cc"
	// trackRare was heard once inside the window.
	trackRare = "rare000000000000000dd"
)

func playlistFixture(t *testing.T, env *harness.Env) (uuid.UUID, domain.TimeRange) {
	t.Helper()
	user := env.NewUser("playlists")
	before := time.Date(2024, time.January, 10, 12, 0, 0, 0, time.UTC)
	inside := time.Date(2024, time.June, 10, 12, 0, 0, 0, time.UTC)

	plays := make([]play, 0, 32)
	for i := range 8 {
		plays = append(plays, play{trackOld, before.Add(time.Duration(i) * time.Hour)})
	}
	for i := range 3 {
		plays = append(plays, play{trackSteady, before.Add(time.Duration(i) * time.Hour)})
	}
	for i := range 6 {
		plays = append(plays, play{trackSteady, inside.Add(time.Duration(i) * time.Hour)})
	}
	for i := range 4 {
		plays = append(plays, play{trackFresh, inside.Add(time.Duration(i) * time.Hour)})
	}
	plays = append(plays, play{trackRare, inside.Add(9 * time.Hour)})

	seedPlaylistHistory(t, env, user.ID, plays)
	return user.ID, domain.TimeRange{
		From: time.Date(2024, time.May, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2024, time.July, 1, 0, 0, 0, 0, time.UTC),
	}
}

func selectTracks(
	t *testing.T, env *harness.Env, userID uuid.UUID,
	def domain.PlaylistDefinition, r domain.TimeRange,
) stats.PlaylistSelection {
	t.Helper()
	sel, err := stats.New(env.Store).SelectPlaylistTracks(env.Ctx(), env.Store.DB(), userID, def, r)
	if err != nil {
		t.Fatalf("select tracks: %v", err)
	}
	return sel
}

func TestPlaylistModeTopRanksWithinTheRange(t *testing.T) {
	env := harness.New(t)
	userID, rng := playlistFixture(t, env)

	sel := selectTracks(t, env, userID, domain.PlaylistDefinition{
		Mode: domain.PlaylistModeTop, Sort: domain.SortByPlays, Limit: 10,
	}, rng)

	// Only what was played inside the window, best first. The track played
	// heavily before it does not appear at all.
	want := []string{trackSteady, trackFresh, trackRare}
	if len(sel.IDs()) != len(want) {
		t.Fatalf("selected %v, want %v", sel.IDs(), want)
	}
	for i := range want {
		if sel.IDs()[i] != want[i] {
			t.Fatalf("selected %v, want %v", sel.IDs(), want)
		}
	}
	if sel.Matched != 3 {
		t.Fatalf("matched = %d, want 3", sel.Matched)
	}
}

// TestPlaylistModeTopReportsWhatItLeftOut: the limit is a cut, and the caller is
// told how much was on the other side of it.
func TestPlaylistModeTopReportsWhatItLeftOut(t *testing.T) {
	env := harness.New(t)
	userID, rng := playlistFixture(t, env)

	sel := selectTracks(t, env, userID, domain.PlaylistDefinition{
		Mode: domain.PlaylistModeTop, Sort: domain.SortByPlays, Limit: 1,
	}, rng)

	if len(sel.IDs()) != 1 {
		t.Fatalf("selected %d tracks, want the limit of 1", len(sel.IDs()))
	}
	if sel.Matched != 3 {
		t.Fatalf("matched = %d, want 3 — the count must be of what qualified, "+
			"not of what survived the limit", sel.Matched)
	}
}

func TestPlaylistModeMinPlaysIsNotATopN(t *testing.T) {
	env := harness.New(t)
	userID, rng := playlistFixture(t, env)

	sel := selectTracks(t, env, userID, domain.PlaylistDefinition{
		Mode: domain.PlaylistModeMinPlays, Sort: domain.SortByPlays, Limit: 100, MinPlays: 4,
	}, rng)

	// Everything that cleared the bar, and nothing that did not — the single
	// play is out, even though a top-N would have included it.
	if len(sel.IDs()) != 2 {
		t.Fatalf("selected %v, want the two tracks played at least four times", sel.IDs())
	}
	for _, id := range sel.IDs() {
		if id == trackRare {
			t.Fatal("a track played once cleared a minimum of four")
		}
	}
}

// TestPlaylistModeDiscoveriesMeansFirstEver is the one that is easy to get
// wrong: "first heard in the range" has to mean first heard at all, or every
// track qualifies whenever the range reaches back to the start of the history.
func TestPlaylistModeDiscoveriesMeansFirstEver(t *testing.T) {
	env := harness.New(t)
	userID, rng := playlistFixture(t, env)

	sel := selectTracks(t, env, userID, domain.PlaylistDefinition{
		Mode: domain.PlaylistModeDiscoveries, Sort: domain.SortByPlays, Limit: 100,
	}, rng)

	got := map[string]bool{}
	for _, id := range sel.IDs() {
		got[id] = true
	}
	if !got[trackFresh] || !got[trackRare] {
		t.Fatalf("selected %v, want both tracks first heard inside the window", sel.IDs())
	}
	if got[trackSteady] {
		t.Fatal("a track already heard before the window counted as a discovery")
	}
}

func TestPlaylistModeForgottenLooksBackwards(t *testing.T) {
	env := harness.New(t)
	userID, rng := playlistFixture(t, env)

	sel := selectTracks(t, env, userID, domain.PlaylistDefinition{
		Mode: domain.PlaylistModeForgotten, Sort: domain.SortByPlays, Limit: 100,
		From: rng.From, To: rng.To,
	}, rng)

	if len(sel.IDs()) != 1 || sel.IDs()[0] != trackOld {
		t.Fatalf("selected %v, want only the track that dropped out of rotation", sel.IDs())
	}
}

// TestPlaylistsExcludeHiddenArtists: the blacklist is meant to apply to every
// statistic, and a playlist is a statistic that ends up in somebody's library.
func TestPlaylistsExcludeHiddenArtists(t *testing.T) {
	env := harness.New(t)
	userID, rng := playlistFixture(t, env)

	env.Exec(`INSERT INTO artists (id, name, name_norm, metadata_state)
              VALUES ('hidden0000000000000000', 'Hidden', 'hidden', 'resolved')`)
	env.Exec(`INSERT INTO track_artists (track_id, artist_id, position) VALUES ($1, $2, 0)`,
		trackSteady, "hidden0000000000000000")
	env.Exec(`INSERT INTO user_blacklisted_artists (user_id, artist_id) VALUES ($1, $2)`,
		userID, "hidden0000000000000000")

	for _, mode := range []domain.PlaylistMode{
		domain.PlaylistModeTop, domain.PlaylistModeMinPlays, domain.PlaylistModeDiscoveries,
	} {
		sel := selectTracks(t, env, userID, domain.PlaylistDefinition{
			Mode: mode, Sort: domain.SortByPlays, Limit: 100, MinPlays: 1,
		}, rng)
		for _, id := range sel.IDs() {
			if id == trackSteady {
				t.Fatalf("mode %q selected a hidden artist's track", mode)
			}
		}
	}
}

// TestSortByTimeRanksDifferentlyFromPlays: the two orderings must genuinely be
// two orderings, or the option is decoration.
func TestSortByTimeRanksDifferentlyFromPlays(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("sorting")
	at := time.Date(2024, time.June, 1, 12, 0, 0, 0, time.UTC)

	// One track played twice for a long time, another four times briefly.
	long, short := "long00000000000000aa", "short0000000000000bb"
	seeds := []listens.TrackSeed{{ID: long, Name: "Long"}, {ID: short, Name: "Short"}}
	if err := env.Listens.EnsureTracks(env.Ctx(), env.Store.DB(), seeds); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	batch := []listens.StagedListen{}
	for i := range 2 {
		batch = append(batch, listens.Stage(domain.Listen{
			UserID: user.ID, PlayedAt: at.Add(time.Duration(i) * time.Hour),
			Precision: domain.PrecisionSecond, Identity: domain.TrackIdentityFromID(long),
			MsPlayed: 600_000, Source: domain.SourceExtended,
		}, nil))
	}
	for i := range 4 {
		batch = append(batch, listens.Stage(domain.Listen{
			UserID: user.ID, PlayedAt: at.Add(time.Duration(10+i) * time.Hour),
			Precision: domain.PrecisionSecond, Identity: domain.TrackIdentityFromID(short),
			MsPlayed: 60_000, Source: domain.SourceExtended,
		}, nil))
	}
	if _, err := env.Listens.InsertListens(env.Ctx(), env.Store.DB(), batch, "UTC"); err != nil {
		t.Fatalf("insert listens: %v", err)
	}

	rng := domain.TimeRange{From: at.Add(-time.Hour), To: at.Add(48 * time.Hour)}

	byPlays := selectTracks(t, env, user.ID, domain.PlaylistDefinition{
		Mode: domain.PlaylistModeTop, Sort: domain.SortByPlays, Limit: 10,
	}, rng)
	if byPlays.IDs()[0] != short {
		t.Fatalf("ranked by plays, first is %q, want the one played more often", byPlays.IDs()[0])
	}

	byTime := selectTracks(t, env, user.ID, domain.PlaylistDefinition{
		Mode: domain.PlaylistModeTop, Sort: domain.SortByTime, Limit: 10,
	}, rng)
	if byTime.IDs()[0] != long {
		t.Fatalf("ranked by time, first is %q, want the one listened to longer", byTime.IDs()[0])
	}
}
