package metadata

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/spotify"
)

// fakeSource answers from a fixed table and records what it was asked.
type fakeSource struct {
	tracks map[string]spotify.Track
	err    error
	// asked accumulates every id this source was given, so a test can assert
	// that a source was skipped entirely rather than merely unhelpful.
	asked []string
	calls int
	// extra is returned regardless of what was requested, for the hostile-source
	// case.
	extra []spotify.Track
}

func newFake(ids ...string) *fakeSource {
	f := &fakeSource{tracks: map[string]spotify.Track{}}
	for _, id := range ids {
		f.tracks[id] = spotify.Track{ID: id, Name: "Track " + id}
	}
	return f
}

func (f *fakeSource) GetTracks(_ context.Context, ids []string) ([]spotify.Track, error) {
	f.calls++
	f.asked = append(f.asked, ids...)
	if f.err != nil {
		return nil, f.err
	}
	out := append([]spotify.Track(nil), f.extra...)
	for _, id := range ids {
		if t, ok := f.tracks[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeSource) GetArtists(context.Context, []string) ([]spotify.Artist, error) {
	return nil, nil
}
func (f *fakeSource) GetAlbums(context.Context, []string) ([]spotify.Album, error) { return nil, nil }

func ids(tracks []spotify.Track) []string {
	out := make([]string, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, t.ID)
	}
	slices.Sort(out)
	return out
}

func paused(until time.Time) ChainOption {
	return WithPauseCheck(func() time.Time { return until })
}

// TestChainWithoutAFallbackBehavesAsBefore: the pass-through case must be
// exactly what the enrichment worker used to do inline, including treating
// everything the primary did not return as declined.
func TestChainWithoutAFallbackBehavesAsBefore(t *testing.T) {
	primary := newFake("aaaaaaaaaa", "bbbbbbbbbb")
	chain := NewChain(primary, nil)

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "cccccccccc"})
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"aaaaaaaaaa"}) {
		t.Fatalf("items = %v, want [aaaaaaaaaa]", got)
	}
	if !slices.Equal(batch.Declined, []string{"cccccccccc"}) {
		t.Fatalf("declined = %v, want [cccccccccc]", batch.Declined)
	}
	if chain.HasFallback() {
		t.Fatal("a chain built with a nil fallback reports having one")
	}
}

// TestFallbackServesTheBatchWhileThePrimaryIsPaused is the case the feature
// exists for.
//
// The primary must not merely be tried and found wanting — it must not be called
// at all. Its limiter blocks rather than erroring, and for an exhausted daily
// quota it would block for most of a day before anything else got a turn.
func TestFallbackServesTheBatchWhileThePrimaryIsPaused(t *testing.T) {
	primary := newFake("aaaaaaaaaa", "bbbbbbbbbb")
	fallback := newFake("aaaaaaaaaa", "bbbbbbbbbb")
	chain := NewChain(primary, fallback, paused(time.Now().Add(6*time.Hour)))

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "bbbbbbbbbb"})
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if primary.calls != 0 {
		t.Fatalf("the paused primary was called %d times; it would have blocked for hours",
			primary.calls)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"aaaaaaaaaa", "bbbbbbbbbb"}) {
		t.Fatalf("items = %v, want both tracks", got)
	}
}

// TestAPausedPrimaryDeclinesNothing is the destructive case.
//
// While the primary is paused it has not spoken, so an id the fallback happens
// not to know must stay pending. Declining it would mark it unavailable, which
// is terminal and which the repair pass deliberately never revisits — so a track
// Spotify could have described tomorrow would stay blank for the life of the
// instance.
func TestAPausedPrimaryDeclinesNothing(t *testing.T) {
	primary := newFake("aaaaaaaaaa", "bbbbbbbbbb")
	fallback := newFake("aaaaaaaaaa") // knows one of the two
	chain := NewChain(primary, fallback, paused(time.Now().Add(time.Hour)))

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "bbbbbbbbbb"})
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if len(batch.Declined) != 0 {
		t.Fatalf("declined %v while the primary was paused; those ids would be marked "+
			"permanently unavailable even though Spotify has them", batch.Declined)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"aaaaaaaaaa"}) {
		t.Fatalf("items = %v, want the one the fallback knew", got)
	}
}

// TestAnExpiredPauseGoesBackToThePrimary: the pause is a window, not a switch.
func TestAnExpiredPauseGoesBackToThePrimary(t *testing.T) {
	primary := newFake("aaaaaaaaaa")
	fallback := newFake("aaaaaaaaaa")
	chain := NewChain(primary, fallback, paused(time.Now().Add(-time.Minute)))

	if _, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa"}); err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if primary.calls != 1 {
		t.Fatalf("the primary was called %d times after the pause expired, want 1", primary.calls)
	}
	if fallback.calls != 0 {
		t.Fatalf("the fallback was consulted %d times for an id the primary served", fallback.calls)
	}
}

// TestTheFallbackFillsWhatSpotifyWillNotServe covers the second, quieter case:
// ids Spotify answers with null. Those are terminal today, so a fallback is the
// only thing that can ever fill them.
func TestTheFallbackFillsWhatSpotifyWillNotServe(t *testing.T) {
	primary := newFake("aaaaaaaaaa")
	fallback := newFake("bbbbbbbbbb")
	chain := NewChain(primary, fallback)

	batch, err := chain.Tracks(context.Background(),
		[]string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"})
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"aaaaaaaaaa", "bbbbbbbbbb"}) {
		t.Fatalf("items = %v, want the primary's and the fallback's", got)
	}
	// Only what neither source has may be written off.
	if !slices.Equal(batch.Declined, []string{"cccccccccc"}) {
		t.Fatalf("declined = %v, want only [cccccccccc]", batch.Declined)
	}
	// And the fallback was asked only about what the primary could not serve.
	if !slices.Equal(fallback.asked, []string{"bbbbbbbbbb", "cccccccccc"}) {
		t.Fatalf("the fallback was asked for %v, want only the ids the primary missed",
			fallback.asked)
	}
}

// TestAFallbackOutageLeavesThePrimaryAnswerIntact: an instance whose mirror is
// down must behave like an instance that never had one, not like a broken one.
func TestAFallbackOutageLeavesThePrimaryAnswerIntact(t *testing.T) {
	primary := newFake("aaaaaaaaaa")
	fallback := &fakeSource{err: errors.New("connection refused")}
	chain := NewChain(primary, fallback)

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "bbbbbbbbbb"})
	if err != nil {
		t.Fatalf("a fallback outage failed the whole batch: %v", err)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"aaaaaaaaaa"}) {
		t.Fatalf("items = %v, want the primary's answer", got)
	}
	// The primary spoke, so its verdict stands.
	if !slices.Equal(batch.Declined, []string{"bbbbbbbbbb"}) {
		t.Fatalf("declined = %v, want [bbbbbbbbbb]", batch.Declined)
	}
}

// TestAPrimaryFailureIsNotSwallowed: with the primary broken and no pause
// declared, the batch must fail so the enrichment worker advances its backoff.
// Returning a partial answer would let un-fetched ids look like declined ones.
func TestAPrimaryFailureIsNotSwallowed(t *testing.T) {
	primary := &fakeSource{err: errors.New("spotify is having a bad minute")}
	fallback := newFake("aaaaaaaaaa")
	chain := NewChain(primary, fallback)

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa"})
	if err == nil {
		t.Fatal("a broken primary produced no error")
	}
	if len(batch.Items) != 0 || len(batch.Declined) != 0 {
		t.Fatalf("a failed batch reported items %v and declined %v, want neither",
			batch.Items, batch.Declined)
	}
}

// TestUnrequestedEntitiesAreDropped: a fallback is somebody's own server, and a
// buggy or hostile one must not be able to write rows into the catalogue by
// volunteering ids nobody asked about.
func TestUnrequestedEntitiesAreDropped(t *testing.T) {
	primary := newFake()
	fallback := newFake("bbbbbbbbbb")
	fallback.extra = []spotify.Track{{ID: "zzzzzzzzzz", Name: "Never requested"}}
	chain := NewChain(primary, fallback)

	batch, err := chain.Tracks(context.Background(), []string{"bbbbbbbbbb"})
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"bbbbbbbbbb"}) {
		t.Fatalf("items = %v; the fallback smuggled in an id nobody asked for", got)
	}
}

// TestEmptyRequestTouchesNothing keeps an idle queue from generating traffic.
func TestEmptyRequestTouchesNothing(t *testing.T) {
	primary := newFake("aaaaaaaaaa")
	fallback := newFake("aaaaaaaaaa")
	chain := NewChain(primary, fallback)

	batch, err := chain.Tracks(context.Background(), nil)
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if len(batch.Items) != 0 || len(batch.Declined) != 0 {
		t.Fatal("an empty request produced a non-empty batch")
	}
	if primary.calls != 0 || fallback.calls != 0 {
		t.Fatal("an empty request reached a source")
	}
}

// --- preferred fallback ------------------------------------------------------

// TestPreferredFallbackAnswersFirst is the point of the setting: the Spotify
// quota is spent only on what the mirror lacks, so a rate limit never arises.
func TestPreferredFallbackAnswersFirst(t *testing.T) {
	primary := newFake("aaaaaaaaaa", "bbbbbbbbbb")
	fallback := newFake("aaaaaaaaaa", "bbbbbbbbbb")
	chain := NewChain(primary, fallback, WithPreferredFallback(true))

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "bbbbbbbbbb"})
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if primary.calls != 0 {
		t.Fatalf("the primary was called %d times for ids the fallback had; the quota is "+
			"exactly what this setting exists to save", primary.calls)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"aaaaaaaaaa", "bbbbbbbbbb"}) {
		t.Fatalf("items = %v, want both from the fallback", got)
	}
}

// TestPreferredFallbackStillAsksTheAuthorityForWhatItLacks: a mirror is a
// point-in-time copy, so anything released since the scrape has to come from
// Spotify or it never arrives.
func TestPreferredFallbackStillAsksTheAuthorityForWhatItLacks(t *testing.T) {
	primary := newFake("newrelease")
	fallback := newFake("aaaaaaaaaa")
	chain := NewChain(primary, fallback, WithPreferredFallback(true))

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "newrelease"})
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"aaaaaaaaaa", "newrelease"}) {
		t.Fatalf("items = %v, want both sources' answers", got)
	}
	// And the primary was asked only about what the fallback could not serve.
	if !slices.Equal(primary.asked, []string{"newrelease"}) {
		t.Fatalf("the primary was asked for %v, want only the id the fallback lacked",
			primary.asked)
	}
	if len(batch.Declined) != 0 {
		t.Fatalf("declined %v when both ids were served", batch.Declined)
	}
}

// TestPreferredFallbackDeclinesOnlyWhatTheAuthorityRefuses keeps the terminal
// state in the authority's hands, whichever source is asked first.
func TestPreferredFallbackDeclinesOnlyWhatTheAuthorityRefuses(t *testing.T) {
	primary := newFake()  // Spotify has nothing for either
	fallback := newFake() // and neither does the mirror
	chain := NewChain(primary, fallback, WithPreferredFallback(true))

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "bbbbbbbbbb"})
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	// Spotify was asked and refused, so these are genuinely unavailable.
	if !slices.Equal(batch.Declined, []string{"aaaaaaaaaa", "bbbbbbbbbb"}) {
		t.Fatalf("declined = %v, want both", batch.Declined)
	}
}

// TestAPreferredFallbackNeverWritesOffWhatSpotifyDidNotSee is the destructive
// case, in the reversed order.
func TestAPreferredFallbackNeverWritesOffWhatSpotifyDidNotSee(t *testing.T) {
	primary := &fakeSource{err: errors.New("spotify is having a bad minute")}
	fallback := newFake("aaaaaaaaaa")
	chain := NewChain(primary, fallback, WithPreferredFallback(true))

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "bbbbbbbbbb"})
	if err != nil {
		t.Fatalf("a failing primary lost the fallback's answer too: %v", err)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"aaaaaaaaaa"}) {
		t.Fatalf("items = %v, want what the fallback served", got)
	}
	if len(batch.Declined) != 0 {
		t.Fatal("an id was written off although Spotify never answered for it; " +
			"unavailable is terminal and nothing revisits it")
	}
}

// TestAPreferredFallbackOutageFallsBackToTheAuthority: the preference is an
// order, not a dependency.
func TestAPreferredFallbackOutageFallsBackToTheAuthority(t *testing.T) {
	primary := newFake("aaaaaaaaaa", "bbbbbbbbbb")
	fallback := &fakeSource{err: errors.New("connection refused")}
	chain := NewChain(primary, fallback, WithPreferredFallback(true))

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "bbbbbbbbbb"})
	if err != nil {
		t.Fatalf("a fallback outage failed the batch: %v", err)
	}
	if got := ids(batch.Items); !slices.Equal(got, []string{"aaaaaaaaaa", "bbbbbbbbbb"}) {
		t.Fatalf("items = %v, want the primary's answer", got)
	}
}

// TestAPreferredFallbackCarriesAPausedInstanceAlone: with Spotify rate limited
// there is nowhere else to ask, so nothing may be concluded.
func TestAPreferredFallbackCarriesAPausedInstanceAlone(t *testing.T) {
	primary := newFake("aaaaaaaaaa", "bbbbbbbbbb")
	fallback := newFake("aaaaaaaaaa")
	chain := NewChain(primary, fallback,
		WithPreferredFallback(true), paused(time.Now().Add(6*time.Hour)))

	batch, err := chain.Tracks(context.Background(), []string{"aaaaaaaaaa", "bbbbbbbbbb"})
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if primary.calls != 0 {
		t.Fatalf("the paused primary was called %d times", primary.calls)
	}
	if len(batch.Declined) != 0 {
		t.Fatalf("declined %v while Spotify was paused", batch.Declined)
	}
}
