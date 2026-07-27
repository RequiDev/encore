package sync

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/catalog"
	"github.com/RequiDev/encore/internal/store/listens"
)

// now is the fixed clock every test runs on, so nothing here depends on when it
// is executed.
var now = time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)

// fakeSpotify satisfies SpotifyAPI without a network. The poller's database work
// is exercised by the integration suite; everything here is the pure logic.
type fakeSpotify struct {
	plays []spotify.PlayHistory
	err   error
}

func (f *fakeSpotify) RecentlyPlayed(context.Context, string, time.Time, int, int) ([]spotify.PlayHistory, error) {
	return f.plays, f.err
}

func (f *fakeSpotify) RefreshToken(context.Context, string) (*spotify.Token, error) {
	return nil, f.err
}

// testDeps builds the minimum a Poller needs to be constructed. The repositories
// are never called by the tests in this file.
func testDeps() Deps {
	db := &store.Store{}
	return Deps{
		Store:    db,
		Accounts: &accounts.Repo{Users: &accounts.Users{}, Credentials: &accounts.Credentials{}},
		Listens:  &listens.Repo{},
		Catalog:  &catalog.Repo{},
		Spotify:  &fakeSpotify{},
		Now:      func() time.Time { return now },
	}
}

func testPoller(t *testing.T, cfg config.Sync) *Poller {
	t.Helper()
	p, err := NewPoller(cfg, testDeps())
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return p
}

// track builds a play-history entry for a catalogue track.
func track(id, name string, durationMs int, playedAt time.Time) spotify.PlayHistory {
	return spotify.PlayHistory{
		PlayedAt: playedAt,
		Track: spotify.Track{
			ID:          id,
			Name:        name,
			Type:        "track",
			DurationMs:  durationMs,
			Artists:     []spotify.Artist{{ID: "artist-" + id, Name: "Artist " + id}},
			ExternalIDs: spotify.ExternalIDs{ISRC: "GBAYE0601498"},
			Album: spotify.Album{
				ID:                   "album-" + id,
				Name:                 "Album " + name,
				AlbumType:            "album",
				ReleaseDate:          "2011-03-04",
				ReleaseDatePrecision: "day",
				TotalTracks:          12,
				Artists:              []spotify.Artist{{ID: "artist-" + id, Name: "Artist " + id}},
			},
		},
	}
}

func TestNewPollerRequiresItsCollaborators(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Deps)
	}{
		{"no store", func(d *Deps) { d.Store = nil }},
		{"no accounts", func(d *Deps) { d.Accounts = nil }},
		{"no credentials repository", func(d *Deps) { d.Accounts = &accounts.Repo{Users: &accounts.Users{}} }},
		{"no users repository", func(d *Deps) { d.Accounts = &accounts.Repo{Credentials: &accounts.Credentials{}} }},
		{"no listens", func(d *Deps) { d.Listens = nil }},
		{"no catalog", func(d *Deps) { d.Catalog = nil }},
		{"no spotify client", func(d *Deps) { d.Spotify = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps()
			tc.mut(&deps)
			if _, err := NewPoller(config.Sync{}, deps); err == nil {
				t.Fatal("expected an error when a required dependency is missing")
			}
		})
	}
}

func TestNewPollerFillsInDefaults(t *testing.T) {
	p := testPoller(t, config.Sync{})
	if p.cfg.Interval != defaultInterval {
		t.Errorf("interval = %s, want %s", p.cfg.Interval, defaultInterval)
	}
	if p.cfg.Concurrency != 1 {
		t.Errorf("concurrency = %d, want 1", p.cfg.Concurrency)
	}
	if p.cfg.InitialLookback != defaultInitialLookback {
		t.Errorf("initial lookback = %s, want %s", p.cfg.InitialLookback, defaultInitialLookback)
	}
	if p.stat == nil || p.log == nil || p.rnd == nil || p.running == nil {
		t.Error("poller was built with a nil collaborator")
	}
}

func TestPrepareConvertsAPlay(t *testing.T) {
	userID := uuid.New()
	playedAt := now.Add(-5 * time.Minute)

	b := prepare(userID, []spotify.PlayHistory{track("t1", "Song", 214000, playedAt)}, now)

	if len(b.staged) != 1 {
		t.Fatalf("staged %d listens, want 1", len(b.staged))
	}
	got := b.staged[0]
	if !got.PlayedAt.Equal(playedAt) {
		// The recently-played feed is the one source that timestamps the start
		// of playback, so nothing may be derived from the duration.
		t.Errorf("played_at = %s, want %s", got.PlayedAt, playedAt)
	}
	if got.Precision != domain.PrecisionMillisecond {
		t.Errorf("precision = %v, want millisecond", got.Precision)
	}
	if got.Source != domain.SourceSync {
		t.Errorf("source = %v, want sync", got.Source)
	}
	if got.TrackID != "t1" {
		t.Errorf("track id = %q, want %q", got.TrackID, "t1")
	}
	if got.MsPlayed != 214000 {
		t.Errorf("ms_played = %d, want the track duration 214000", got.MsPlayed)
	}
	if got.ImportFileID != nil {
		t.Error("a synced listen belongs to no import file")
	}
	if len(got.IdentityKey) == 0 || len(got.DedupeKey) == 0 {
		t.Error("staging must compute both keys")
	}
	if !b.newest.Equal(playedAt) {
		t.Errorf("newest = %s, want %s", b.newest, playedAt)
	}
}

func TestPrepareCarriesTheCatalogueDetail(t *testing.T) {
	userID := uuid.New()
	plays := []spotify.PlayHistory{
		track("t1", "Song", 200000, now.Add(-time.Minute)),
		// The same track again: the catalogue must be collected once even when a
		// listener has it on repeat.
		track("t1", "Song", 200000, now.Add(-10*time.Minute)),
		track("t2", "Other", 180000, now.Add(-20*time.Minute)),
	}

	b := prepare(userID, plays, now)

	if len(b.staged) != 3 {
		t.Fatalf("staged %d listens, want 3", len(b.staged))
	}
	if len(b.trackSeeds) != 2 || len(b.tracks) != 2 || len(b.albums) != 2 {
		t.Fatalf("catalogue = %d ids, %d tracks, %d albums; want 2 of each",
			len(b.trackSeeds), len(b.tracks), len(b.albums))
	}
	if b.tracks[0].ID != "t1" || b.tracks[0].Name != "Song" {
		t.Errorf("first track = %+v, want the detail the feed carried", b.tracks[0])
	}
	if b.tracks[0].AlbumID != "album-t1" {
		t.Errorf("track album = %q, want album-t1", b.tracks[0].AlbumID)
	}
	if len(b.tracks[0].ArtistIDs) != 1 || b.tracks[0].ArtistIDs[0] != "artist-t1" {
		t.Errorf("track credits = %v, want [artist-t1]", b.tracks[0].ArtistIDs)
	}
	if b.albums[0].ReleaseDate == nil || b.albums[0].ReleaseDate.Year() != 2011 {
		t.Errorf("album release date = %v, want 2011", b.albums[0].ReleaseDate)
	}
}

func TestPrepareWithoutTrackDetailStillStagesTheListen(t *testing.T) {
	// An entry that is a catalogue track but carries no name is not the full
	// object; the row must stay pending rather than be recorded as resolved.
	play := track("t1", "", 200000, now.Add(-time.Minute))

	b := prepare(uuid.New(), []spotify.PlayHistory{play}, now)

	if len(b.staged) != 1 {
		t.Fatalf("staged %d listens, want 1", len(b.staged))
	}
	if len(b.trackSeeds) != 1 {
		t.Fatalf("track ids = %v, want the id so the foreign key holds", b.trackSeeds)
	}
	if len(b.tracks) != 0 || len(b.albums) != 0 {
		t.Errorf("upserted %d tracks and %d albums, want none", len(b.tracks), len(b.albums))
	}
}

func TestPrepareCountsWhatItCannotStore(t *testing.T) {
	userID := uuid.New()
	podcast := spotify.PlayHistory{
		PlayedAt: now.Add(-time.Minute),
		Track:    spotify.Track{ID: "ep1", Name: "Episode", Type: "episode", DurationMs: 3600000},
	}
	local := spotify.PlayHistory{
		PlayedAt: now.Add(-2 * time.Minute),
		Track:    spotify.Track{ID: "loc", Name: "Demo", IsLocal: true, DurationMs: 1000},
	}
	future := track("t9", "Impossible", 1000, now.Add(domain.FutureSkew+time.Hour))
	ancient := track("t8", "Too old", 1000, time.Date(1998, time.January, 1, 0, 0, 0, 0, time.UTC))
	good := track("t1", "Song", 200000, now.Add(-30*time.Minute))

	b := prepare(userID, []spotify.PlayHistory{podcast, local, future, ancient, good}, now)

	if len(b.staged) != 1 {
		t.Fatalf("staged %d listens, want 1", len(b.staged))
	}
	if b.skipped != 2 {
		t.Errorf("skipped = %d, want 2 (a podcast and a local file)", b.skipped)
	}
	if b.invalid != 2 {
		t.Errorf("invalid = %d, want 2 (an impossible date each way)", b.invalid)
	}
	// The podcast moves the watermark, because Encore will never store it and
	// re-fetching it every minute would achieve nothing. The play from the
	// future must not, or it would hide every real play behind it.
	if !b.newest.Equal(podcast.PlayedAt.UTC()) {
		t.Errorf("newest = %s, want the podcast at %s", b.newest, podcast.PlayedAt.UTC())
	}
}

func TestPrepareAccountsForEveryEntry(t *testing.T) {
	// Imported + Duplicates + Skipped + Invalid == Fetched is the promise
	// SyncResult makes, and staged listens are what become the first two.
	plays := []spotify.PlayHistory{
		track("t1", "Song", 200000, now.Add(-time.Minute)),
		track("t2", "Other", 200000, now.Add(-2*time.Minute)),
		{PlayedAt: now.Add(-3 * time.Minute), Track: spotify.Track{ID: "ep", Type: "episode"}},
		track("t3", "Broken", 1000, time.Time{}),
	}

	b := prepare(uuid.New(), plays, now)

	if got := len(b.staged) + b.skipped + b.invalid; got != len(plays) {
		t.Errorf("accounted for %d of %d fetched plays", got, len(plays))
	}
}

func TestPrepareEmptyPage(t *testing.T) {
	b := prepare(uuid.New(), nil, now)
	if len(b.staged) != 0 || b.skipped != 0 || b.invalid != 0 {
		t.Errorf("an empty page produced work: %+v", b)
	}
	if !b.newest.IsZero() {
		t.Error("an empty page must not move the watermark")
	}
	if cursorPtr(b.newest) != nil {
		t.Error("a zero watermark must reach the store as nil")
	}
}

func TestMsPlayedClamps(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"ordinary track", 214000, 214000},
		{"absent duration", 0, 0},
		{"negative duration", -1, 0},
		{"absurd duration", int(domain.MaxMsPlayed) + 1, domain.MaxMsPlayed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := msPlayed(spotify.Track{DurationMs: tc.in}); got != tc.want {
				t.Errorf("msPlayed(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestListenFromValidates(t *testing.T) {
	l := listenFrom(uuid.New(), track("t1", "Song", 200000, now.Add(-time.Minute)))
	if err := l.Validate(now); err != nil {
		t.Fatalf("a converted play must be storable: %v", err)
	}
	if !l.Identity.IsResolved() {
		t.Error("a play from the feed always knows its track id")
	}
}

func TestNotAfter(t *testing.T) {
	past := now.Add(-time.Hour)
	if got := notAfter(past, now); !got.Equal(past) {
		t.Errorf("notAfter(past) = %s, want %s", got, past)
	}
	if got := notAfter(now.Add(time.Hour), now); !got.Equal(now) {
		t.Errorf("a cursor from the future must be clamped to now, got %s", got)
	}
}

func TestPlausible(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"recent", now.Add(-time.Minute), true},
		{"zero", time.Time{}, false},
		{"before Spotify existed", time.Date(1999, time.January, 1, 0, 0, 0, 0, time.UTC), false},
		{"just inside the future skew", now.Add(domain.FutureSkew - time.Minute), true},
		{"beyond the future skew", now.Add(domain.FutureSkew + time.Minute), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := plausible(tc.at, now); got != tc.want {
				t.Errorf("plausible(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func TestLater(t *testing.T) {
	earlier := now.Add(-time.Hour)
	if got := later(earlier, now); !got.Equal(now) {
		t.Errorf("later = %s, want %s", got, now)
	}
	if got := later(now, earlier); !got.Equal(now) {
		t.Errorf("later = %s, want %s", got, now)
	}
}

func TestCursorPtr(t *testing.T) {
	if cursorPtr(time.Time{}) != nil {
		t.Error("a zero watermark must not move the cursor")
	}
	got := cursorPtr(now.In(time.FixedZone("CEST", 2*60*60)))
	if got == nil || !got.Equal(now) || got.Location() != time.UTC {
		t.Errorf("cursorPtr = %v, want %s in UTC", got, now)
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name            string
		err             error
		unauth, forbade bool
	}{
		{"nil", nil, false, false},
		{"unauthorised", &spotify.APIError{StatusCode: http.StatusUnauthorized}, true, false},
		{"forbidden", &spotify.APIError{StatusCode: http.StatusForbidden}, false, true},
		{"server error", &spotify.APIError{StatusCode: http.StatusBadGateway}, false, false},
		{"wrapped", errors.Join(errors.New("fetch"), &spotify.APIError{StatusCode: http.StatusUnauthorized}), true, false},
		{"not from spotify", errors.New("database is down"), false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := unauthorised(tc.err); got != tc.unauth {
				t.Errorf("unauthorised = %v, want %v", got, tc.unauth)
			}
			if got := forbidden(tc.err); got != tc.forbade {
				t.Errorf("forbidden = %v, want %v", got, tc.forbade)
			}
		})
	}
}

func TestClaimIsExclusive(t *testing.T) {
	p := testPoller(t, config.Sync{})
	userID := uuid.New()
	other := uuid.New()

	if !p.claim(userID) {
		t.Fatal("the first claim must succeed")
	}
	if p.claim(userID) {
		t.Error("a second poll of the same account must be refused")
	}
	if !p.claim(other) {
		t.Error("a different account must not be blocked")
	}
	p.release(userID)
	if !p.claim(userID) {
		t.Error("a released account must be claimable again")
	}
}

func TestSyncUserRefusesAConcurrentPoll(t *testing.T) {
	p := testPoller(t, config.Sync{})
	userID := uuid.New()
	if !p.claim(userID) {
		t.Fatal("could not take the claim the test needs")
	}
	defer p.release(userID)

	// No repository is reached, because the claim is checked first: this is what
	// POST /api/sync/now turns into a 409.
	if _, err := p.SyncUser(context.Background(), userID); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("err = %v, want ErrAlreadyRunning", err)
	}
}

func TestSyncUserRejectsTheNilUser(t *testing.T) {
	p := testPoller(t, config.Sync{})
	if _, err := p.SyncUser(context.Background(), uuid.Nil); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want a validation error", err)
	}
}

func TestRunReturnsImmediatelyWhenDisabled(t *testing.T) {
	p := testPoller(t, config.Sync{Enabled: false, Interval: time.Hour})
	if err := p.Run(context.Background()); err != nil {
		// A disabled poller must not block the worker supervisor, and must not
		// need its context cancelled to come back.
		t.Errorf("Run = %v, want nil", err)
	}
}

func TestRunStopsWithTheContext(t *testing.T) {
	p := testPoller(t, config.Sync{Enabled: true, Interval: time.Hour, Concurrency: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Run(ctx); err != nil {
		t.Errorf("Run = %v, want nil on a cancelled context", err)
	}
}

func TestDelayJitter(t *testing.T) {
	const interval = 60 * time.Second
	p := testPoller(t, config.Sync{Enabled: true, Interval: interval, Concurrency: 4})

	// The first delay spreads a freshly started fleet across a whole interval.
	for _, draw := range []float64{0, 0.5, 0.999} {
		p.rnd = func() float64 { return draw }
		got := p.firstDelay()
		if got < 0 || got >= interval {
			t.Errorf("firstDelay with rnd=%v = %s, want [0, %s)", draw, got, interval)
		}
	}

	// Subsequent delays stay within the jitter band, centred on the interval so
	// the long-run polling rate is the configured one.
	spread := time.Duration(float64(interval) * tickJitter)
	lo, hi := interval-spread/2, interval+spread/2
	p.rnd = func() float64 { return 0 }
	if got := p.nextDelay(); got != lo {
		t.Errorf("nextDelay at the bottom of the band = %s, want %s", got, lo)
	}
	p.rnd = func() float64 { return 1 }
	if got := p.nextDelay(); got != hi {
		t.Errorf("nextDelay at the top of the band = %s, want %s", got, hi)
	}
	p.rnd = func() float64 { return 0.5 }
	if got := p.nextDelay(); got != interval {
		t.Errorf("nextDelay at the middle of the band = %s, want %s", got, interval)
	}
}

func TestNextDelayNeverBusyLoops(t *testing.T) {
	p := testPoller(t, config.Sync{Enabled: true, Interval: time.Millisecond})
	p.rnd = func() float64 { return 0 }
	if got := p.nextDelay(); got < time.Second {
		t.Errorf("nextDelay = %s, want at least a second whatever the interval", got)
	}
}

func TestBatchLimit(t *testing.T) {
	tests := []struct {
		concurrency int
		want        int
	}{
		{1, accountsPerWorker},
		{4, 4 * accountsPerWorker},
		{64, maxAccountsPerTick},
	}
	for _, tc := range tests {
		p := testPoller(t, config.Sync{Concurrency: tc.concurrency})
		if got := p.batchLimit(); got != tc.want {
			t.Errorf("batchLimit at concurrency %d = %d, want %d", tc.concurrency, got, tc.want)
		}
	}
}

func TestNopMetricsSatisfiesTheInterface(t *testing.T) {
	var m Metrics = NopMetrics{}
	m.SyncRun(resultSuccess)
	m.SyncListens(1)
	m.SyncLastSuccess(now)
}

// spotifyClientSatisfiesTheInterface fails to compile if the real client ever
// stops matching what the poller asks of it.
var _ SpotifyAPI = (*spotify.Client)(nil)
