//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store/listens"
	"github.com/RequiDev/encore/test/harness"
)

// TestEntityFirstListenIgnoresTheSelectedRange is the regression test for a
// figure that changed meaning depending on what the viewer was looking at.
//
// "First listen" is a fact about the music. Derived from the selected range, a
// track somebody has loved for a decade claims to have been discovered last
// month — and the album and artist pages said exactly that, because the API
// computed the all-time answer and then dropped it before the response.
func TestEntityFirstListenIgnoresTheSelectedRange(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("entitydates")
	track := "ent0000000000000000aa"

	// Played once long ago, then again recently.
	long := time.Date(2019, time.March, 4, 20, 0, 0, 0, time.UTC)
	recent := time.Date(2026, time.June, 15, 21, 0, 0, 0, time.UTC)

	seeds := []listens.TrackSeed{{ID: track, Name: "Old favourite"}}
	if err := env.Listens.EnsureTracks(env.Ctx(), env.Store.DB(), seeds); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	batch := make([]listens.StagedListen, 0, 2)
	for _, at := range []time.Time{long, recent} {
		batch = append(batch, listens.Stage(domain.Listen{
			UserID: user.ID, PlayedAt: at, Precision: domain.PrecisionSecond,
			Identity: domain.TrackIdentityFromID(track),
			MsPlayed: 200_000, Source: domain.SourceExtended,
		}, nil))
	}
	if _, err := env.Listens.InsertListens(env.Ctx(), env.Store.DB(), batch, "UTC"); err != nil {
		t.Fatalf("insert listens: %v", err)
	}

	// A range covering only the recent play, which is what somebody looking at
	// "this year" has selected.
	rng := domain.TimeRange{
		From: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	got, err := stats.New(env.Store).TrackStats(env.Ctx(), env.Store.DB(), user.ID, rng, "UTC", track)
	if err != nil {
		t.Fatalf("track stats: %v", err)
	}

	// Scoped to the range: one play, and the range's own first and last.
	if got.Plays != 1 {
		t.Fatalf("plays = %d, want the 1 inside the range", got.Plays)
	}
	if got.FirstListenAt == nil || !got.FirstListenAt.Equal(recent) {
		t.Fatalf("first-in-range = %v, want %s", got.FirstListenAt, recent)
	}

	// Not scoped: the answer a page labelled "first listen" must show.
	if got.DiscoveredAt == nil {
		t.Fatal("no all-time first listen was returned")
	}
	if !got.DiscoveredAt.Equal(long) {
		t.Fatalf("all-time first listen = %s, want %s — narrowing the range must not "+
			"move when somebody first heard a track", got.DiscoveredAt, long)
	}
	if got.LastPlayedAt == nil || !got.LastPlayedAt.Equal(recent) {
		t.Fatalf("all-time last listen = %v, want %s", got.LastPlayedAt, recent)
	}
}

// TestEntityAllTimeDatesSurviveARangeWithNoPlays: select a period the track was
// silent in and the range figures are empty, but when it was first heard is
// still known.
func TestEntityAllTimeDatesSurviveARangeWithNoPlays(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("silentrange")
	track := "ent0000000000000000bb"
	at := time.Date(2019, time.March, 4, 20, 0, 0, 0, time.UTC)

	seeds := []listens.TrackSeed{{ID: track, Name: "Only once"}}
	if err := env.Listens.EnsureTracks(env.Ctx(), env.Store.DB(), seeds); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	if _, err := env.Listens.InsertListens(env.Ctx(), env.Store.DB(), []listens.StagedListen{
		listens.Stage(domain.Listen{
			UserID: user.ID, PlayedAt: at, Precision: domain.PrecisionSecond,
			Identity: domain.TrackIdentityFromID(track),
			MsPlayed: 200_000, Source: domain.SourceExtended,
		}, nil),
	}, "UTC"); err != nil {
		t.Fatalf("insert listens: %v", err)
	}

	rng := domain.TimeRange{
		From: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	got, err := stats.New(env.Store).TrackStats(env.Ctx(), env.Store.DB(), user.ID, rng, "UTC", track)
	if err != nil {
		t.Fatalf("track stats: %v", err)
	}

	if got.Plays != 0 || got.FirstListenAt != nil {
		t.Fatalf("plays = %d and first-in-range = %v, want a silent range",
			got.Plays, got.FirstListenAt)
	}
	if got.DiscoveredAt == nil || !got.DiscoveredAt.Equal(at) {
		t.Fatalf("all-time first listen = %v, want %s even though the range is empty",
			got.DiscoveredAt, at)
	}
}
