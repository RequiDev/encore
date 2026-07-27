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

// statsFixture seeds a small, entirely deterministic listening history so every
// figure below can be asserted exactly rather than approximately.
//
// The shape, in the user's own timezone (Europe/Berlin, UTC+1 in January):
//
//	2024-01-01  three plays of track A, one of track B      (all artist X)
//	2024-01-02  two plays of track C                        (artist Y)
//	2024-01-03  one play of track A
//	2024-01-05  one play of track D                         (artist Z)
//
// 2024-01-04 is deliberately silent, so the streak and timeline gap behaviour is
// exercised rather than assumed.
type statsFixture struct {
	env  *harness.Env
	svc  *stats.Service
	user domain.User
	tz   string
	loc  *time.Location
}

const berlin = "Europe/Berlin"

func seedStats(t *testing.T) *statsFixture {
	t.Helper()
	env := harness.New(t)
	svc := stats.New(env.Store)

	user := env.NewUser("statsuser")
	if _, err := env.Accounts.Users.SetTimezone(env.Ctx(), env.Store.DB(), user.ID, berlin); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	user.Timezone = berlin
	loc, err := time.LoadLocation(berlin)
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}

	seedCatalog(t, env)

	type play struct {
		track string
		local string // local wall-clock time in Berlin
		ms    int32
	}
	plays := []play{
		{"trk-a", "2024-01-01 09:00", 200_000},
		{"trk-a", "2024-01-01 11:00", 200_000},
		{"trk-a", "2024-01-01 21:00", 200_000},
		{"trk-b", "2024-01-01 22:00", 100_000},
		{"trk-c", "2024-01-02 09:00", 300_000},
		{"trk-c", "2024-01-02 18:00", 300_000},
		{"trk-a", "2024-01-03 09:00", 200_000},
		{"trk-d", "2024-01-05 09:00", 150_000},
	}

	batch := make([]listens.StagedListen, 0, len(plays))
	for _, p := range plays {
		at, err := time.ParseInLocation("2006-01-02 15:04", p.local, loc)
		if err != nil {
			t.Fatalf("parse %q: %v", p.local, err)
		}
		batch = append(batch, listens.Stage(domain.Listen{
			UserID:    user.ID,
			PlayedAt:  at.UTC(),
			Precision: domain.PrecisionSecond,
			Identity:  domain.TrackIdentityFromID(p.track),
			MsPlayed:  p.ms,
			Source:    domain.SourceExtended,
		}, nil))
	}
	n, err := env.Listens.InsertListens(env.Ctx(), env.Store.DB(), batch, berlin)
	if err != nil {
		t.Fatalf("seed listens: %v", err)
	}
	if int(n) != len(plays) {
		t.Fatalf("seeded %d of %d plays; the fixture has an accidental duplicate", n, len(plays))
	}

	return &statsFixture{env: env, svc: svc, user: user, tz: berlin, loc: loc}
}

// seedCatalog inserts the tracks, albums and artists the fixture refers to, in
// the resolved state, so the joins the statistics rely on have something to find.
func seedCatalog(t *testing.T, env *harness.Env) {
	t.Helper()
	ctx, db := env.Ctx(), env.Store.DB()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := env.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed catalogue (%s): %v", sql, err)
		}
	}
	_ = db

	exec(`INSERT INTO artists (id, name, name_norm, metadata_state, genres) VALUES
        ('art-x', 'Artist X', 'artist x', 'resolved', ARRAY['rock']),
        ('art-y', 'Artist Y', 'artist y', 'resolved', ARRAY['jazz']),
        ('art-z', 'Artist Z', 'artist z', 'resolved', ARRAY['folk'])`)

	exec(`INSERT INTO albums (id, name, name_norm, album_type, release_date, release_precision, metadata_state) VALUES
        ('alb-1', 'Album One',   'album one',   'album',  DATE '2010-05-01', 'day', 'resolved'),
        ('alb-2', 'Album Two',   'album two',   'album',  DATE '2020-01-01', 'day', 'resolved'),
        ('alb-3', 'Album Three', 'album three', 'single', DATE '2000-01-01', 'day', 'resolved')`)

	exec(`INSERT INTO album_artists (album_id, artist_id, position) VALUES
        ('alb-1','art-x',0), ('alb-2','art-y',0), ('alb-3','art-z',0)`)

	exec(`INSERT INTO tracks (id, name, name_norm, album_id, duration_ms, metadata_state) VALUES
        ('trk-a', 'Track A', 'track a', 'alb-1', 210000, 'resolved'),
        ('trk-b', 'Track B', 'track b', 'alb-1', 110000, 'resolved'),
        ('trk-c', 'Track C', 'track c', 'alb-2', 310000, 'resolved'),
        ('trk-d', 'Track D', 'track d', 'alb-3', 160000, 'resolved')`)

	// Track A has two credited artists, so "average artists per track" has
	// something other than 1.0 to compute.
	exec(`INSERT INTO track_artists (track_id, artist_id, position) VALUES
        ('trk-a','art-x',0), ('trk-a','art-y',1),
        ('trk-b','art-x',0),
        ('trk-c','art-y',0),
        ('trk-d','art-z',0)`)
}

// fullRange covers every seeded play, in local terms.
func (f *statsFixture) fullRange() domain.TimeRange {
	from := time.Date(2024, time.January, 1, 0, 0, 0, 0, f.loc)
	return domain.TimeRange{From: from, To: from.AddDate(0, 0, 10)}
}

func TestSummaryCountsExactly(t *testing.T) {
	f := seedStats(t)
	got, err := f.svc.Summary(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.Listens != 8 {
		t.Errorf("listens = %d, want 8", got.Listens)
	}
	if got.DistinctTracks != 4 {
		t.Errorf("distinct tracks = %d, want 4", got.DistinctTracks)
	}
	if got.DistinctArtists != 3 {
		t.Errorf("distinct artists = %d, want 3", got.DistinctArtists)
	}
	if got.DistinctAlbums != 3 {
		t.Errorf("distinct albums = %d, want 3", got.DistinctAlbums)
	}
	const wantMs = 200_000*4 + 100_000 + 300_000*2 + 150_000
	if got.MsPlayed != wantMs {
		t.Errorf("ms played = %d, want %d", got.MsPlayed, wantMs)
	}
	if got.ActiveDays != 4 {
		t.Errorf("active days = %d, want 4 (the 4th is silent)", got.ActiveDays)
	}
}

func TestSummaryRespectsTheDateRange(t *testing.T) {
	f := seedStats(t)
	// Only 2024-01-01, in Berlin local time. The 21:00 and 22:00 plays are the
	// interesting ones: in UTC they are still on the 1st, but a naive UTC range
	// would be wrong for a user east of Greenwich, and this asserts it is not.
	from := time.Date(2024, time.January, 1, 0, 0, 0, 0, f.loc)
	r := domain.TimeRange{From: from, To: from.AddDate(0, 0, 1)}

	got, err := f.svc.Summary(f.env.Ctx(), f.env.Store.DB(), f.user.ID, r, f.tz)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.Listens != 4 {
		t.Fatalf("listens on 2024-01-01 = %d, want 4", got.Listens)
	}
	if got.ActiveDays != 1 {
		t.Fatalf("active days = %d, want 1", got.ActiveDays)
	}
}

func TestTopListsRankAndPaginate(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	tracks, err := f.svc.TopTracks(ctx, db, f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top tracks: %v", err)
	}
	if tracks.Total != 4 {
		t.Fatalf("total tracks = %d, want 4", tracks.Total)
	}
	if len(tracks.Entries) != 4 {
		t.Fatalf("returned %d tracks, want 4", len(tracks.Entries))
	}
	if tracks.Entries[0].ID != "trk-a" || tracks.Entries[0].Plays != 4 {
		t.Fatalf("top track = %q with %d plays, want trk-a with 4",
			tracks.Entries[0].ID, tracks.Entries[0].Plays)
	}
	if tracks.Entries[0].Rank != 1 {
		t.Fatalf("top track rank = %d, want 1", tracks.Entries[0].Rank)
	}

	// Second page must continue the ranking rather than restart it.
	page2, err := f.svc.TopTracks(ctx, db, f.user.ID, f.fullRange(), f.tz, 2, 2)
	if err != nil {
		t.Fatalf("top tracks page 2: %v", err)
	}
	if len(page2.Entries) != 2 {
		t.Fatalf("page 2 returned %d tracks, want 2", len(page2.Entries))
	}
	if page2.Entries[0].Rank != 3 {
		t.Fatalf("first rank on page 2 = %d, want 3", page2.Entries[0].Rank)
	}

	artists, err := f.svc.TopArtists(ctx, db, f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top artists: %v", err)
	}
	// Artist X is credited on A (4 plays) and B (1), so 5. Artist Y is credited
	// on A (4) and C (2), so 6 — which makes Y first and proves the join goes
	// through track_artists rather than assuming one artist per track.
	if artists.Entries[0].ID != "art-y" || artists.Entries[0].Plays != 6 {
		t.Fatalf("top artist = %q with %d plays, want art-y with 6",
			artists.Entries[0].ID, artists.Entries[0].Plays)
	}

	albums, err := f.svc.TopAlbums(ctx, db, f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top albums: %v", err)
	}
	if albums.Entries[0].ID != "alb-1" || albums.Entries[0].Plays != 5 {
		t.Fatalf("top album = %q with %d plays, want alb-1 with 5",
			albums.Entries[0].ID, albums.Entries[0].Plays)
	}
}

func TestTimelineFillsEmptyBuckets(t *testing.T) {
	f := seedStats(t)
	from := time.Date(2024, time.January, 1, 0, 0, 0, 0, f.loc)
	r := domain.TimeRange{From: from, To: from.AddDate(0, 0, 6)}

	points, err := f.svc.Timeline(f.env.Ctx(), f.env.Store.DB(), f.user.ID, r, f.tz, domain.IntervalDay)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(points) != 6 {
		t.Fatalf("returned %d daily buckets for a six-day range, want 6", len(points))
	}
	want := []int64{4, 2, 1, 0, 1, 0}
	for i, p := range points {
		if p.Plays != want[i] {
			t.Errorf("bucket %d (%s) has %d plays, want %d",
				i, p.Bucket.Format("2006-01-02"), p.Plays, want[i])
		}
	}
	if points[3].Plays != 0 {
		t.Error("the silent day must appear as a zero, not as a gap")
	}
}

func TestTimelineRejectsTooManyBuckets(t *testing.T) {
	f := seedStats(t)
	from := time.Date(2000, time.January, 1, 0, 0, 0, 0, f.loc)
	r := domain.TimeRange{From: from, To: from.AddDate(30, 0, 0)}

	_, err := f.svc.Timeline(f.env.Ctx(), f.env.Store.DB(), f.user.ID, r, f.tz, domain.IntervalHour)
	if err == nil {
		t.Fatal("an hourly timeline over thirty years must be refused, not attempted")
	}
}

func TestRepartitionUsesLocalTime(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	hours, err := f.svc.HourRepartition(ctx, db, f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("hour repartition: %v", err)
	}
	if len(hours) != 24 {
		t.Fatalf("returned %d hour buckets, want 24", len(hours))
	}
	byHour := map[int]int64{}
	for _, h := range hours {
		byHour[h.Hour] = h.Plays
	}
	// Four plays start at 09:00 local. In UTC that is 08:00, so a query that
	// forgot to convert would report hour 8.
	if byHour[9] != 4 {
		t.Fatalf("local hour 09 has %d plays, want 4 (got hour 08 = %d, which would mean UTC bucketing)",
			byHour[9], byHour[8])
	}
	if byHour[21] != 1 || byHour[22] != 1 {
		t.Fatalf("evening plays landed in hours 21=%d 22=%d, want 1 each", byHour[21], byHour[22])
	}

	weekdays, err := f.svc.WeekdayRepartition(ctx, db, f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("weekday repartition: %v", err)
	}
	if len(weekdays) != 7 {
		t.Fatalf("returned %d weekday buckets, want 7", len(weekdays))
	}
	// 2024-01-01 was a Monday, which is index 0 with a Monday-first week.
	if weekdays[0].Plays != 4 {
		t.Fatalf("Monday has %d plays, want 4 — the week must start on Monday", weekdays[0].Plays)
	}

	cells, err := f.svc.HourWeekdayHeatmap(ctx, db, f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	if len(cells) != 7*24 {
		t.Fatalf("heatmap has %d cells, want 168", len(cells))
	}
}

func TestBlacklistedArtistsAreExcludedEverywhere(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	before, err := f.svc.Summary(ctx, db, f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	if err := f.env.Catalog.Blacklist(ctx, db, f.user.ID, "art-z"); err != nil {
		t.Fatalf("blacklist: %v", err)
	}

	after, err := f.svc.Summary(ctx, db, f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("summary after blacklist: %v", err)
	}
	// Artist Z is credited only on track D, which was played once.
	if after.Listens != before.Listens-1 {
		t.Fatalf("listens went from %d to %d after blacklisting an artist with one play",
			before.Listens, after.Listens)
	}

	tracks, err := f.svc.TopTracks(ctx, db, f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top tracks: %v", err)
	}
	for _, item := range tracks.Entries {
		if item.ID == "trk-d" {
			t.Fatal("a blacklisted artist's track still appears in the top tracks")
		}
	}
}

func TestHistoryIsKeysetPaginated(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := f.svc.History(ctx, db, f.user.ID, f.fullRange(), f.tz, cursor, 3)
		if err != nil {
			t.Fatalf("history page %d: %v", pages, err)
		}
		for _, item := range page.Entries {
			key := item.PlayedAt.Format(time.RFC3339Nano) + "/" + item.TrackID
			if seen[key] {
				t.Fatalf("history returned the same listen twice across pages: %s", key)
			}
			seen[key] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("history pagination did not terminate")
		}
	}
	if len(seen) != 8 {
		t.Fatalf("paged through %d listens, want 8", len(seen))
	}
	if pages < 3 {
		t.Fatalf("eight listens at three per page took %d pages, want at least 3", pages)
	}
}

func TestLongestSessionsGroupsConsecutivePlays(t *testing.T) {
	f := seedStats(t)
	sessions, err := f.svc.LongestSessions(f.env.Ctx(), f.env.Store.DB(), f.user.ID,
		f.fullRange(), domain.SessionGap, 10)
	if err != nil {
		t.Fatalf("longest sessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("no sessions were found in a history with eight plays")
	}
	// Every play in the fixture is at least an hour from its neighbours, so with
	// a thirty-minute gap each is its own session.
	if len(sessions) != 8 {
		t.Fatalf("found %d sessions, want 8: with plays an hour apart and a %s gap, "+
			"none of them should be grouped", len(sessions), domain.SessionGap)
	}
	for _, s := range sessions {
		if s.TrackCount < 1 {
			t.Fatal("a session was reported with no tracks")
		}
	}
}

func TestStreaksFindGapsAndIslands(t *testing.T) {
	f := seedStats(t)
	got, err := f.svc.Streaks(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.tz)
	if err != nil {
		t.Fatalf("streaks: %v", err)
	}
	// 1st, 2nd and 3rd of January are consecutive; the 5th stands alone.
	if got.Longest.Days != 3 {
		t.Fatalf("longest streak = %d days, want 3", got.Longest.Days)
	}
}

func TestDiscoveryCountsFirstListensOnly(t *testing.T) {
	f := seedStats(t)
	from := time.Date(2024, time.January, 1, 0, 0, 0, 0, f.loc)
	r := domain.TimeRange{From: from, To: from.AddDate(0, 0, 6)}

	points, err := f.svc.Discovery(f.env.Ctx(), f.env.Store.DB(), f.user.ID, r, f.tz, domain.IntervalDay)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	var newTracks int64
	for _, p := range points {
		newTracks += p.NewTracks
	}
	if newTracks != 4 {
		t.Fatalf("discovered %d new tracks over the whole history, want 4", newTracks)
	}
	if points[0].NewTracks != 2 {
		t.Fatalf("day one discovered %d tracks, want 2 (A and B); "+
			"a repeat play must not count as a discovery", points[0].NewTracks)
	}
	if points[2].NewTracks != 0 {
		t.Fatalf("day three discovered %d tracks, want 0: track A was already known",
			points[2].NewTracks)
	}
}

func TestEntityStatsAndComparison(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	track, err := f.svc.TrackStats(ctx, db, f.user.ID, f.fullRange(), f.tz, "trk-a")
	if err != nil {
		t.Fatalf("track stats: %v", err)
	}
	if track.Plays != 4 {
		t.Fatalf("track A has %d plays, want 4", track.Plays)
	}
	if track.FirstListenAt == nil || track.LastListenAt == nil {
		t.Fatal("track stats must report first and last listen")
	}
	if !track.FirstListenAt.Before(*track.LastListenAt) {
		t.Fatal("first listen is not before last listen")
	}

	artist, err := f.svc.ArtistStats(ctx, db, f.user.ID, f.fullRange(), f.tz, "art-x", 5)
	if err != nil {
		t.Fatalf("artist stats: %v", err)
	}
	if artist.Plays != 5 {
		t.Fatalf("artist X has %d plays, want 5", artist.Plays)
	}
	if artist.MsShare <= 0 || artist.MsShare > 1 {
		t.Fatalf("artist share = %v, want a fraction between 0 and 1", artist.MsShare)
	}

	album, err := f.svc.AlbumStats(ctx, db, f.user.ID, f.fullRange(), f.tz, "alb-1", 5)
	if err != nil {
		t.Fatalf("album stats: %v", err)
	}
	if album.Plays != 5 {
		t.Fatalf("album one has %d plays, want 5", album.Plays)
	}

	day1 := domain.TimeRange{
		From: time.Date(2024, time.January, 1, 0, 0, 0, 0, f.loc),
		To:   time.Date(2024, time.January, 2, 0, 0, 0, 0, f.loc),
	}
	day2 := domain.TimeRange{From: day1.To, To: day1.To.AddDate(0, 0, 1)}
	cmp, err := f.svc.Compare(ctx, db, f.user.ID, day1, day2, f.tz)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if cmp.A.Listens != 4 || cmp.B.Listens != 2 {
		t.Fatalf("comparison summaries = %d and %d, want 4 and 2",
			cmp.A.Listens, cmp.B.Listens)
	}
	if cmp.Delta.Listens != -2 {
		t.Fatalf("listens delta = %d, want -2", cmp.Delta.Listens)
	}
}

func TestDashboardExtras(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	release, err := f.svc.AverageAlbumReleaseYear(ctx, db, f.user.ID, f.fullRange())
	if err != nil {
		t.Fatalf("average release year: %v", err)
	}
	// Five plays from a 2010 album, two from 2020, one from 2000.
	want := (2010.0*5 + 2020*2 + 2000) / 8
	if diff := release.AverageYear - want; diff > 0.51 || diff < -0.51 {
		t.Fatalf("average release year = %.2f, want about %.2f", release.AverageYear, want)
	}

	perTrack, err := f.svc.AverageArtistsPerTrack(ctx, db, f.user.ID, f.fullRange())
	if err != nil {
		t.Fatalf("average artists per track: %v", err)
	}
	if perTrack.Average <= 1.0 {
		t.Fatalf("average artists per track = %.3f, want above 1: track A has two credited artists",
			perTrack.Average)
	}

	year, err := f.svc.YearInReview(ctx, db, f.user.ID, 2024, f.tz)
	if err != nil {
		t.Fatalf("year in review: %v", err)
	}
	if year.Summary.Listens != 8 {
		t.Fatalf("year in review counted %d listens, want 8", year.Summary.Listens)
	}
	if year.BusiestDay.Plays != 4 {
		t.Fatalf("busiest day had %d plays, want 4", year.BusiestDay.Plays)
	}
}

func TestAffinityBetweenTwoUsers(t *testing.T) {
	f := seedStats(t)
	other := f.env.NewUser("otheruser")

	// The second user shares track A and adds one of their own.
	at := time.Date(2024, time.January, 6, 12, 0, 0, 0, time.UTC)
	batch := []listens.StagedListen{
		stageAt(other.ID, "trk-a", at),
		stageAt(other.ID, "trk-a", at.Add(time.Hour)),
		stageAt(other.ID, "trk-d", at.Add(2*time.Hour)),
	}
	if _, err := f.env.Listens.InsertListens(f.env.Ctx(), f.env.Store.DB(), batch, "UTC"); err != nil {
		t.Fatalf("seed second user: %v", err)
	}

	r := domain.TimeRange{
		From: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2024, time.January, 10, 0, 0, 0, 0, time.UTC),
	}
	aff, err := f.svc.Affinity(f.env.Ctx(), f.env.Store.DB(), f.user.ID, other.ID, r, 10)
	if err != nil {
		t.Fatalf("affinity: %v", err)
	}
	if len(aff.SharedTracks) == 0 {
		t.Fatal("two users who both played track A share no tracks")
	}
	if aff.Score <= 0 {
		t.Fatalf("similarity score = %v, want above zero", aff.Score)
	}
}

func stageAt(userID uuid.UUID, trackID string, at time.Time) listens.StagedListen {
	return listens.Stage(domain.Listen{
		UserID:    userID,
		PlayedAt:  at,
		Precision: domain.PrecisionSecond,
		Identity:  domain.TrackIdentityFromID(trackID),
		MsPlayed:  200_000,
		Source:    domain.SourceExtended,
	}, nil)
}

func TestRollupRefreshMatchesTheFactTable(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	// Ingestion marked the affected local days dirty, so the statistics layer
	// knows the rollup cannot be trusted yet.
	dirty, err := f.svc.HasDirtyDays(ctx, db, f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("has dirty days: %v", err)
	}
	if !dirty {
		t.Fatal("seeding listens should have marked their days dirty")
	}

	if err := f.svc.RefreshDirtyDays(ctx, 100); err != nil {
		t.Fatalf("refresh rollups: %v", err)
	}

	dirty, err = f.svc.HasDirtyDays(ctx, db, f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("has dirty days after refresh: %v", err)
	}
	if dirty {
		t.Fatal("days are still marked dirty after a refresh")
	}

	// The rollup must agree with the fact table exactly, or the fast path would
	// silently answer differently from the slow one.
	rollupPlays := f.env.ScalarInt(
		`SELECT COALESCE(sum(plays), 0) FROM listen_daily_rollup WHERE user_id = $1`, f.user.ID.String())
	if rollupPlays != 8 {
		t.Fatalf("rollup totals %d plays, want the 8 in the fact table", rollupPlays)
	}
	rollupMs := f.env.ScalarInt(
		`SELECT COALESCE(sum(ms), 0) FROM listen_daily_rollup WHERE user_id = $1`, f.user.ID.String())
	factMs := f.env.ScalarInt(
		`SELECT COALESCE(sum(ms_played), 0) FROM listens WHERE user_id = $1`, f.user.ID.String())
	if rollupMs != factMs {
		t.Fatalf("rollup totals %d ms but the fact table has %d", rollupMs, factMs)
	}

	// And the answers must not change now that the fast path is available.
	after, err := f.svc.TopTracks(ctx, db, f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top tracks after refresh: %v", err)
	}
	if after.Entries[0].ID != "trk-a" || after.Entries[0].Plays != 4 {
		t.Fatalf("after the rollup refresh the top track is %q with %d plays, want trk-a with 4",
			after.Entries[0].ID, after.Entries[0].Plays)
	}
}
