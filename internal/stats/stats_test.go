package stats

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
)

// statement describes one composed statement well enough to check it without a
// database.
type statement struct {
	name   string
	sql    string
	params int
	// rawListens marks the one statement that is allowed to read the fact table
	// without the blacklist, because it maintains the raw rollup and the
	// blacklist is applied when the rollup is read.
	rawListens bool
}

func statements() []statement {
	return []statement{
		{name: "summary", sql: summarySQL, params: 4},
		{name: "topTracksFact", sql: topTracksFactSQL, params: 7},
		{name: "topTracksRollup", sql: topTracksRollupSQL, params: 8},
		{name: "topArtistsFact", sql: topArtistsFactSQL, params: 7},
		{name: "topArtistsRollup", sql: topArtistsRollupSQL, params: 8},
		{name: "topAlbumsFact", sql: topAlbumsFactSQL, params: 7},
		{name: "topAlbumsRollup", sql: topAlbumsRollupSQL, params: 8},
		{name: "libraryTimeline", sql: libraryTimelineSQL, params: 5},
		{name: "trackTimeline", sql: trackTimelineSQL, params: 6},
		{name: "artistTimeline", sql: artistTimelineSQL, params: 6},
		{name: "albumTimeline", sql: albumTimelineSQL, params: 6},
		{name: "trackStats", sql: trackStatsSQL, params: 4},
		{name: "artistStats", sql: artistStatsSQL, params: 4},
		{name: "albumStats", sql: albumStatsSQL, params: 4},
		{name: "artistTopTracks", sql: artistTopTracksSQL, params: 5},
		{name: "artistTopAlbums", sql: artistTopAlbumsSQL, params: 5},
		{name: "albumTopTracks", sql: albumTopTracksSQL, params: 5},
		{name: "hourRepartition", sql: hourRepartitionSQL, params: 4},
		{name: "weekdayRepartition", sql: weekdayRepartitionSQL, params: 4},
		{name: "heatmap", sql: heatmapSQL, params: 4},
		{name: "historyFirstPage", sql: historyFirstPageSQL, params: 4},
		{name: "historyNextPage", sql: historyNextPageSQL, params: 6},
		{name: "longestSessions", sql: longestSessionsSQL, params: 5},
		{name: "discovery", sql: discoverySQL, params: 5},
		{name: "streaks", sql: streaksSQL, params: 3},
		{name: "busiestDay", sql: busiestDaySQL, params: 4},
		{name: "newArtists", sql: newArtistsSQL, params: 3},
		{name: "differentArtists", sql: differentArtistsSQL, params: 5},
		{name: "releaseYear", sql: releaseYearSQL, params: 3},
		{name: "artistsPerTrack", sql: artistsPerTrackSQL, params: 3},
		{name: "hasDirtyDays", sql: hasDirtyDaysSQL, params: 4},
		{name: "refreshDirtyDays", sql: refreshDirtyDaysSQL, params: 1, rawListens: true},
		{name: "sharedArtists", sql: sharedArtistsSQL, params: 5},
		{name: "sharedAlbums", sql: sharedAlbumsSQL, params: 5},
		{name: "sharedTracks", sql: sharedTracksSQL, params: 5},
		{name: "affinityScore", sql: affinityScoreSQL, params: 4},
	}
}

var paramPattern = regexp.MustCompile(`\$(\d+)`)

// TestParameterNumberingIsContiguous guards the one mistake SQL composition makes
// easily and Postgres refuses at parse time: a statement that uses $1, $2 and $4
// but never $3 cannot be prepared at all, and no unit test that avoids the
// database would otherwise catch it.
func TestParameterNumberingIsContiguous(t *testing.T) {
	for _, st := range statements() {
		t.Run(st.name, func(t *testing.T) {
			seen := map[int]bool{}
			high := 0
			for _, m := range paramPattern.FindAllStringSubmatch(st.sql, -1) {
				n, err := strconv.Atoi(m[1])
				if err != nil {
					t.Fatalf("unparsable placeholder %q", m[0])
				}
				seen[n] = true
				if n > high {
					high = n
				}
			}
			if high != st.params {
				t.Fatalf("highest placeholder is $%d, want $%d", high, st.params)
			}
			for i := 1; i <= high; i++ {
				if !seen[i] {
					t.Errorf("placeholder $%d is never used", i)
				}
			}
		})
	}
}

// TestBlacklistIsAppliedEverywhere is the whole point of having one fragment: a
// statistic that reads the fact table without it would quietly show an artist the
// user has excluded.
func TestBlacklistIsAppliedEverywhere(t *testing.T) {
	for _, st := range statements() {
		if !strings.Contains(st.sql, "FROM listens") && !strings.Contains(st.sql, "JOIN listens") {
			continue
		}
		if st.rawListens {
			continue
		}
		if !strings.Contains(st.sql, "user_blacklisted_artists") {
			t.Errorf("%s reads the fact table without the blacklist filter", st.name)
		}
	}
}

func TestBlacklistFilterBindsToItsAlias(t *testing.T) {
	got := blacklistFilter("r")
	for _, want := range []string{
		"NOT EXISTS",
		"bl.user_id = r.user_id",
		"bta.track_id = r.track_id",
		"bl.artist_id = bta.artist_id",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("blacklist filter is missing %q:\n%s", want, got)
		}
	}
}

func TestRangeFilterComposition(t *testing.T) {
	got := rangeFilter("l", "$1", "$6", "$7")
	for _, want := range []string{
		"l.user_id = $1",
		"l.played_at >= $6::timestamptz",
		"l.played_at < $7::timestamptz",
		"user_blacklisted_artists",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("range filter is missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "(") != strings.Count(got, ")") {
		t.Errorf("unbalanced parentheses:\n%s", got)
	}
}

// TestTopSourceSelection checks that each variant reads the table it claims to.
func TestTopSourceSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sql    string
		rollup bool
	}{
		{"tracks fact", topTracksFactSQL, false},
		{"tracks rollup", topTracksRollupSQL, true},
		{"artists fact", topArtistsFactSQL, false},
		{"artists rollup", topArtistsRollupSQL, true},
		{"albums fact", topAlbumsFactSQL, false},
		{"albums rollup", topAlbumsRollupSQL, true},
	} {
		usesRollup := strings.Contains(tc.sql, "listen_daily_rollup")
		usesFacts := strings.Contains(tc.sql, "FROM listens")
		if usesRollup != tc.rollup || usesFacts == tc.rollup {
			t.Errorf("%s: reads rollup=%v facts=%v", tc.name, usesRollup, usesFacts)
		}
	}
	if topStatement(topArtists, true) != topArtistsRollupSQL {
		t.Error("topStatement picked the wrong statement for rollup artists")
	}
	if topStatement(topTracks, false) != topTracksFactSQL {
		t.Error("topStatement picked the wrong statement for fact tracks")
	}
}

func TestCheckInterval(t *testing.T) {
	day := 24 * time.Hour
	from := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	rangeOf := func(d time.Duration) domain.TimeRange {
		return domain.TimeRange{From: from, To: from.Add(d)}
	}

	for _, tc := range []struct {
		name     string
		r        domain.TimeRange
		interval domain.Interval
		wantErr  bool
	}{
		{"hours over a month", rangeOf(30 * day), domain.IntervalHour, false},
		{"hours over a year", rangeOf(365 * day), domain.IntervalHour, true},
		{"days over a year", rangeOf(365 * day), domain.IntervalDay, false},
		{"days over a decade", rangeOf(3650 * day), domain.IntervalDay, true},
		{"months over a decade", rangeOf(3650 * day), domain.IntervalMonth, false},
		{"unknown interval", rangeOf(day), domain.Interval("fortnight"), true},
	} {
		err := checkInterval(tc.r, tc.interval)
		if tc.wantErr && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
		if err != nil && !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error is not a validation error: %v", tc.name, err)
		}
	}

	// Whatever the range, the suggested interval must pass the check; that is the
	// contract that lets a caller always produce a timeline.
	for _, d := range []time.Duration{time.Hour, 30 * day, 365 * day, 20 * 365 * day} {
		r := rangeOf(d)
		if err := checkInterval(r, domain.SuggestInterval(r)); err != nil {
			t.Errorf("suggested interval for %s was rejected: %v", d, err)
		}
	}
}

func TestDailyIntervalDegradesInsteadOfFailing(t *testing.T) {
	from := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
	short := domain.TimeRange{From: from, To: from.AddDate(1, 0, 0)}
	if got := dailyInterval(short); got != domain.IntervalDay {
		t.Errorf("a one-year detail timeline should be daily, got %q", got)
	}
	long := domain.TimeRange{From: from, To: from.AddDate(16, 0, 0)}
	if got := dailyInterval(long); got == domain.IntervalDay {
		t.Error("a sixteen-year detail timeline should not stay daily")
	} else if err := checkInterval(long, got); err != nil {
		t.Errorf("degraded interval %q is still too fine: %v", got, err)
	}
}

func TestPreviousPeriodMaths(t *testing.T) {
	from := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	r := domain.TimeRange{From: from, To: from.AddDate(0, 1, 0)}
	prev := r.Previous()

	if !prev.To.Equal(r.From) {
		t.Errorf("previous period must end where the range begins: %s vs %s", prev.To, r.From)
	}
	if prev.Duration() != r.Duration() {
		t.Errorf("previous period must be the same length: %s vs %s", prev.Duration(), r.Duration())
	}

	check := rollupCheckRange(r)
	if !check.From.Equal(prev.From) || !check.To.Equal(r.To) {
		t.Errorf("the dirty-day check must span both periods, got %s..%s", check.From, check.To)
	}
}

func TestRollupDecision(t *testing.T) {
	loc := time.FixedZone("Test", 2*60*60)
	midnight := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	wide := domain.TimeRange{From: midnight(2025, time.January, 1), To: midnight(2025, time.July, 1)}
	narrow := domain.TimeRange{From: midnight(2025, time.January, 1), To: midnight(2025, time.February, 1)}
	unaligned := domain.TimeRange{From: wide.From.Add(6 * time.Hour), To: wide.To}

	for _, tc := range []struct {
		name  string
		r     domain.TimeRange
		dirty bool
		want  bool
	}{
		{"wide and clean", wide, false, true},
		{"wide but dirty", wide, true, false},
		{"narrow and clean", narrow, false, false},
		{"unaligned and clean", unaligned, false, false},
		{"exactly the threshold", domain.TimeRange{From: wide.From, To: wide.From.Add(RollupMinRange)}, false, false},
	} {
		if got := useRollup(tc.r, loc, tc.dirty); got != tc.want {
			t.Errorf("%s: useRollup = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsLocalMidnight(t *testing.T) {
	loc := time.FixedZone("Test", -5*60*60)
	local := time.Date(2026, time.June, 1, 0, 0, 0, 0, loc)
	if !isLocalMidnight(local, loc) {
		t.Error("local midnight was not recognised")
	}
	if isLocalMidnight(local, time.UTC) {
		t.Error("05:00 UTC is not UTC midnight")
	}
	if isLocalMidnight(local.Add(time.Nanosecond), loc) {
		t.Error("a nanosecond past midnight is not midnight")
	}
}

func TestInLocationReattachesTheZone(t *testing.T) {
	loc := time.FixedZone("Test", 3*60*60)
	// What Postgres returns for date_trunc('day', ...) in that zone: a wall clock
	// reading that pgx labels UTC.
	raw := time.Date(2026, time.May, 4, 0, 0, 0, 0, time.UTC)
	got := inLocation(raw, loc)

	if got.Location() != loc {
		t.Errorf("location is %s, want %s", got.Location(), loc)
	}
	if got.Year() != 2026 || got.Month() != time.May || got.Day() != 4 || got.Hour() != 0 {
		t.Errorf("wall clock changed: %s", got)
	}
	if !got.Equal(raw.Add(-3 * time.Hour)) {
		t.Errorf("instant is %s, want %s", got.UTC(), raw.Add(-3*time.Hour))
	}
}

func TestLocationRejectsUnknownTimezone(t *testing.T) {
	if _, err := location("Mars/Olympus_Mons"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("unknown timezone should be a validation error, got %v", err)
	}
	loc, err := location("")
	if err != nil || loc != time.UTC {
		t.Errorf("an empty timezone should mean UTC, got %v, %v", loc, err)
	}
	if tzArg("") != "UTC" {
		t.Errorf("tzArg(\"\") = %q, want UTC", tzArg(""))
	}
	if got, err := location("Europe/Berlin"); err != nil {
		t.Skipf("tzdata is unavailable in this environment: %v", err)
	} else if got.String() != "Europe/Berlin" {
		t.Errorf("loaded %q, want Europe/Berlin", got)
	}
}

func TestScopeValidation(t *testing.T) {
	valid := domain.TimeRange{
		From: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := scope(uuid.Nil, valid, "UTC"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a nil user should be a validation error, got %v", err)
	}
	backwards := domain.TimeRange{From: valid.To, To: valid.From}
	if _, err := scope(uuid.New(), backwards, "UTC"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("an inverted range should be a validation error, got %v", err)
	}
	if _, err := scope(uuid.New(), valid, "UTC"); err != nil {
		t.Errorf("a valid scope was rejected: %v", err)
	}
}

func TestClampLimitAndOffset(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, DefaultPageSize},
		{-1, DefaultPageSize},
		{10, 10},
		{MaxPageSize, MaxPageSize},
		{MaxPageSize + 1, MaxPageSize},
	} {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if got := clampOffset(-5); got != 0 {
		t.Errorf("clampOffset(-5) = %d, want 0", got)
	}
	if got := clampOffset(7); got != 7 {
		t.Errorf("clampOffset(7) = %d, want 7", got)
	}
}

func TestShareAndPctChange(t *testing.T) {
	if got := share(3, 12); got != 0.25 {
		t.Errorf("share(3, 12) = %v, want 0.25", got)
	}
	if got := share(3, 0); got != 0 {
		t.Errorf("share of an empty total must be zero, got %v", got)
	}
	if got := pctChange(200, 300); got != 50 {
		t.Errorf("pctChange(200, 300) = %v, want 50", got)
	}
	if got := pctChange(200, 100); got != -50 {
		t.Errorf("pctChange(200, 100) = %v, want -50", got)
	}
	if got := pctChange(0, 100); got != 0 {
		t.Errorf("a change from nothing has no percentage, got %v", got)
	}
}

func TestTopEntryMovement(t *testing.T) {
	risen := TopEntry{Rank: 2, PreviousRank: 7}
	if risen.IsNew() || risen.Movement() != 5 {
		t.Errorf("expected a rise of 5, got new=%v movement=%d", risen.IsNew(), risen.Movement())
	}
	fallen := TopEntry{Rank: 9, PreviousRank: 4}
	if fallen.Movement() != -5 {
		t.Errorf("expected a fall of 5, got %d", fallen.Movement())
	}
	fresh := TopEntry{Rank: 3}
	if !fresh.IsNew() || fresh.Movement() != 0 {
		t.Errorf("a new entry has nothing to move from, got new=%v movement=%d", fresh.IsNew(), fresh.Movement())
	}
}

func TestIsCurrentStreak(t *testing.T) {
	loc := time.FixedZone("Test", 60*60)
	now := time.Date(2026, time.July, 26, 9, 30, 0, 0, loc)
	day := func(d int) time.Time { return time.Date(2026, time.July, d, 0, 0, 0, 0, loc) }

	for _, tc := range []struct {
		name string
		end  time.Time
		want bool
	}{
		{"ends today", day(26), true},
		{"ends yesterday", day(25), true},
		{"ends the day before yesterday", day(24), false},
		{"ended last month", time.Date(2026, time.June, 30, 0, 0, 0, 0, loc), false},
	} {
		got := isCurrentStreak(domain.Streak{StartDay: day(1), EndDay: tc.end, Days: 3}, now, loc)
		if got != tc.want {
			t.Errorf("%s: isCurrentStreak = %v, want %v", tc.name, got, tc.want)
		}
	}
}
