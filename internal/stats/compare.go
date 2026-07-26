package stats

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// Comparison is two periods and the movement between them. Delta is always
// B minus A, so a positive number means the later period was busier when the
// caller passes the periods in chronological order.
type Comparison struct {
	A     Summary
	B     Summary
	Delta Delta
}

// Delta is the change from the first period to the second.
type Delta struct {
	Listens         int64
	DistinctTracks  int64
	DistinctArtists int64
	DistinctAlbums  int64
	MsPlayed        int64
	ActiveDays      int64
	// ListensPct and MsPlayedPct are percentage changes relative to the first
	// period. Both are zero when the first period is empty, because a change from
	// nothing has no meaningful percentage and the UI should say "new" instead.
	ListensPct  float64
	MsPlayedPct float64
}

// pctChange is the change from a to b as a percentage of a.
func pctChange(a, b int64) float64 {
	if a == 0 {
		return 0
	}
	return float64(b-a) / float64(a) * 100
}

// Compare summarises two ranges and reports the movement between them. It is what
// backs "this month vs last month"; pass r.Previous() as a to get exactly that.
func (s *Service) Compare(ctx context.Context, q store.Querier, userID uuid.UUID, a, b domain.TimeRange, tz string) (Comparison, error) {
	sumA, err := s.Summary(ctx, q, userID, a, tz)
	if err != nil {
		return Comparison{}, err
	}
	sumB, err := s.Summary(ctx, q, userID, b, tz)
	if err != nil {
		return Comparison{}, err
	}
	return Comparison{
		A: sumA,
		B: sumB,
		Delta: Delta{
			Listens:         sumB.Listens - sumA.Listens,
			DistinctTracks:  sumB.DistinctTracks - sumA.DistinctTracks,
			DistinctArtists: sumB.DistinctArtists - sumA.DistinctArtists,
			DistinctAlbums:  sumB.DistinctAlbums - sumA.DistinctAlbums,
			MsPlayed:        sumB.MsPlayed - sumA.MsPlayed,
			ActiveDays:      sumB.ActiveDays - sumA.ActiveDays,
			ListensPct:      pctChange(sumA.Listens, sumB.Listens),
			MsPlayedPct:     pctChange(sumA.MsPlayed, sumB.MsPlayed),
		},
	}, nil
}

// DayCount is one local day's listening.
type DayCount struct {
	Day      time.Time
	Plays    int64
	MsPlayed int64
}

// YearInReview is the end-of-year retrospective for one calendar year, measured
// in the user's own timezone.
type YearInReview struct {
	Year            int
	Range           domain.TimeRange
	Summary         Summary
	MinutesListened int64
	TopTracks       []TopEntry
	TopArtists      []TopEntry
	TopAlbums       []TopEntry
	// BusiestDay is zero-valued when the year holds no listens at all.
	BusiestDay DayCount
	// LongestSession is nil when the year holds no listens at all.
	LongestSession *domain.ListeningSession
	NewArtists     int64
}

// YearInReviewTopN is how many entries each of the year's lists carries.
const YearInReviewTopN = 10

// EarliestYear is the first year Encore will summarise. Spotify launched in
// October 2008; anything earlier is a typo or a corrupt export.
const EarliestYear = 2008

var busiestDaySQL = fmt.Sprintf(`
SELECT (l.played_at AT TIME ZONE $4::text)::date AS day,
       count(*)::bigint AS plays,
       coalesce(sum(l.ms_played), 0)::bigint AS ms
FROM listens l
WHERE %s
GROUP BY 1
ORDER BY plays DESC, ms DESC, day
LIMIT 1`, rangeFilter("l", "$1", "$2", "$3"))

// newArtistsSQL counts artists whose first listen ever falls inside the range.
var newArtistsSQL = fmt.Sprintf(`
WITH artist_firsts AS (
    SELECT ta.artist_id AS id, min(l.played_at) AS first_at
    FROM listens l
    JOIN track_artists ta ON ta.track_id = l.track_id
    WHERE l.user_id = $1 AND %s
    GROUP BY 1
)
SELECT count(*)::bigint
FROM artist_firsts
WHERE first_at >= $2::timestamptz AND first_at < $3::timestamptz`, blacklistFilter("l"))

// YearInReview assembles the retrospective for a calendar year.
//
// The year runs from local midnight on 1 January to local midnight on the
// following 1 January, so a listener in Auckland and a listener in Los Angeles
// each get their own year rather than the server's.
func (s *Service) YearInReview(ctx context.Context, q store.Querier, userID uuid.UUID, year int, tz string) (YearInReview, error) {
	loc, err := location(tz)
	if err != nil {
		return YearInReview{}, err
	}
	if year < EarliestYear || year > time.Now().In(loc).Year() {
		return YearInReview{}, fmt.Errorf("%w: year %d is outside the range Encore can summarise", domain.ErrValidation, year)
	}
	from := time.Date(year, time.January, 1, 0, 0, 0, 0, loc)
	r := domain.TimeRange{From: from, To: from.AddDate(1, 0, 0)}

	out := YearInReview{Year: year, Range: r}
	if out.Summary, err = s.Summary(ctx, q, userID, r, tz); err != nil {
		return YearInReview{}, err
	}
	out.MinutesListened = out.Summary.Minutes()

	tracks, err := s.TopTracks(ctx, q, userID, r, tz, YearInReviewTopN, 0)
	if err != nil {
		return YearInReview{}, err
	}
	out.TopTracks = tracks.Entries

	artists, err := s.TopArtists(ctx, q, userID, r, tz, YearInReviewTopN, 0)
	if err != nil {
		return YearInReview{}, err
	}
	out.TopArtists = artists.Entries

	albums, err := s.TopAlbums(ctx, q, userID, r, tz, YearInReviewTopN, 0)
	if err != nil {
		return YearInReview{}, err
	}
	out.TopAlbums = albums.Entries

	if out.BusiestDay, err = s.busiestDay(ctx, q, userID, r, tz, loc); err != nil {
		return YearInReview{}, err
	}

	sessions, err := s.LongestSessions(ctx, q, userID, r, domain.SessionGap, 1)
	if err != nil {
		return YearInReview{}, err
	}
	if len(sessions) > 0 {
		out.LongestSession = &sessions[0]
	}

	if out.NewArtists, err = s.newArtists(ctx, q, userID, r); err != nil {
		return YearInReview{}, err
	}
	return out, nil
}

// busiestDay is the local day with the most plays in the range.
func (s *Service) busiestDay(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, loc *time.Location) (DayCount, error) {
	var d DayCount
	err := q.QueryRow(ctx, busiestDaySQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz)).
		Scan(&d.Day, &d.Plays, &d.MsPlayed)
	if err != nil {
		// A range with no listening has no busiest day, which is an empty answer
		// rather than a missing resource.
		classified := postgres.Classify("busiest day", err)
		if errors.Is(classified, domain.ErrNotFound) {
			return DayCount{}, nil
		}
		return DayCount{}, classified
	}
	d.Day = inLocation(d.Day, loc)
	return d, nil
}

// newArtists counts the artists first heard during the range.
func (s *Service) newArtists(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, newArtistsSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC()).Scan(&n)
	if err != nil {
		return 0, postgres.Classify("new artists", err)
	}
	return n, nil
}
