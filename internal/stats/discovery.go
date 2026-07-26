package stats

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// MaxStreaks is how many of the longest streaks Streaks reports alongside the
// current one.
const MaxStreaks = 5

// DiscoveryPoint counts first encounters in one bucket: artists and tracks whose
// very first listen in the user's whole history falls inside it.
type DiscoveryPoint struct {
	Bucket     time.Time
	NewArtists int64
	NewTracks  int64
}

// Streaks describes a user's listening consistency in local days.
type Streaks struct {
	// Current is the run of days ending today or yesterday. Days is zero when the
	// user has not listened to anything since the day before yesterday; yesterday
	// still counts because a streak should not appear broken at breakfast.
	Current domain.Streak
	// Longest is the longest run ever recorded.
	Longest domain.Streak
	// Top holds the longest runs, longest first, Longest included.
	Top []domain.Streak
}

// discoverySQL derives each entity's first listen from the *whole* history and
// then buckets only those firsts that fall inside the range. Restricting the
// firsts to the range would count every artist heard in a month as newly
// discovered that month, which is the one thing this statistic must not do.
//
// It is deliberately the most expensive query in the package: the firsts are a
// full aggregate over the user's history, which no index can shortcut.
var discoverySQL = fmt.Sprintf(`
WITH bounds AS (
    SELECT date_trunc($5::text, ($2::timestamptz AT TIME ZONE $4::text)) AS lo,
           ($3::timestamptz AT TIME ZONE $4::text) AS hi
),
buckets AS (
    SELECT generate_series(b.lo, b.hi - interval '1 microsecond', ('1 ' || $5::text)::interval) AS bucket
    FROM bounds b
),
track_firsts AS (
    SELECT l.track_id AS id, min(l.played_at) AS first_at
    FROM listens l
    WHERE l.user_id = $1 AND l.track_id IS NOT NULL AND %[1]s
    GROUP BY 1
),
artist_firsts AS (
    SELECT ta.artist_id AS id, min(l.played_at) AS first_at
    FROM listens l
    JOIN track_artists ta ON ta.track_id = l.track_id
    WHERE l.user_id = $1 AND %[1]s
    GROUP BY 1
),
new_tracks AS (
    SELECT date_trunc($5::text, (first_at AT TIME ZONE $4::text)) AS bucket, count(*)::bigint AS n
    FROM track_firsts
    WHERE first_at >= $2::timestamptz AND first_at < $3::timestamptz
    GROUP BY 1
),
new_artists AS (
    SELECT date_trunc($5::text, (first_at AT TIME ZONE $4::text)) AS bucket, count(*)::bigint AS n
    FROM artist_firsts
    WHERE first_at >= $2::timestamptz AND first_at < $3::timestamptz
    GROUP BY 1
)
SELECT b.bucket, coalesce(na.n, 0)::bigint, coalesce(nt.n, 0)::bigint
FROM buckets b
LEFT JOIN new_artists na ON na.bucket = b.bucket
LEFT JOIN new_tracks nt ON nt.bucket = b.bucket
ORDER BY b.bucket`, blacklistFilter("l"))

// Discovery counts newly encountered artists and tracks per bucket.
func (s *Service) Discovery(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, interval domain.Interval) ([]DiscoveryPoint, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return nil, err
	}
	if err := checkInterval(r, interval); err != nil {
		return nil, err
	}

	rows, err := q.Query(ctx, discoverySQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz), string(interval))
	if err != nil {
		return nil, postgres.Classify("discovery timeline", err)
	}
	defer rows.Close()

	var out []DiscoveryPoint
	for rows.Next() {
		var p DiscoveryPoint
		if err := rows.Scan(&p.Bucket, &p.NewArtists, &p.NewTracks); err != nil {
			return nil, postgres.Classify("scan discovery bucket", err)
		}
		p.Bucket = inLocation(p.Bucket, loc)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("discovery timeline", err)
	}
	return out, nil
}

// streaksSQL is gaps-and-islands over the distinct local days with a listen:
// subtracting the row number from the day gives every day in a consecutive run
// the same constant, which is then the group key.
//
// The most recent island is unioned in explicitly because the current streak is
// interesting however short it is, and a two-day streak would never survive an
// ordering by length.
var streaksSQL = fmt.Sprintf(`
WITH days AS (
    SELECT DISTINCT (l.played_at AT TIME ZONE $2::text)::date AS day
    FROM listens l
    WHERE l.user_id = $1 AND %s
),
numbered AS (
    SELECT day, day - (row_number() OVER (ORDER BY day))::int AS grp
    FROM days
),
islands AS (
    SELECT min(day) AS start_day, max(day) AS end_day, count(*)::int AS days
    FROM numbered
    GROUP BY grp
),
top AS (SELECT * FROM islands ORDER BY days DESC, end_day DESC LIMIT $3),
latest AS (SELECT * FROM islands ORDER BY end_day DESC LIMIT 1)
SELECT start_day, end_day, days
FROM (SELECT * FROM top UNION SELECT * FROM latest) u
ORDER BY days DESC, end_day DESC`, blacklistFilter("l"))

// Streaks reports the user's current and longest runs of consecutive local days
// with at least one listen, over their whole history.
func (s *Service) Streaks(ctx context.Context, q store.Querier, userID uuid.UUID, tz string) (Streaks, error) {
	if userID == uuid.Nil {
		return Streaks{}, fmt.Errorf("%w: a user is required", domain.ErrValidation)
	}
	loc, err := location(tz)
	if err != nil {
		return Streaks{}, err
	}

	rows, err := q.Query(ctx, streaksSQL, store.UUIDArg(userID), tzArg(tz), MaxStreaks)
	if err != nil {
		return Streaks{}, postgres.Classify("listening streaks", err)
	}
	defer rows.Close()

	var all []domain.Streak
	for rows.Next() {
		var st domain.Streak
		if err := rows.Scan(&st.StartDay, &st.EndDay, &st.Days); err != nil {
			return Streaks{}, postgres.Classify("scan streak", err)
		}
		st.StartDay = inLocation(st.StartDay, loc)
		st.EndDay = inLocation(st.EndDay, loc)
		all = append(all, st)
	}
	if err := rows.Err(); err != nil {
		return Streaks{}, postgres.Classify("listening streaks", err)
	}
	if len(all) == 0 {
		return Streaks{}, nil
	}

	out := Streaks{Longest: all[0]}
	if len(all) > MaxStreaks {
		out.Top = all[:MaxStreaks]
	} else {
		out.Top = all
	}

	// The rows are ordered by length, so the most recent island has to be found
	// rather than assumed to be first.
	latest := all[0]
	for _, st := range all[1:] {
		if st.EndDay.After(latest.EndDay) {
			latest = st
		}
	}
	if isCurrentStreak(latest, time.Now(), loc) {
		out.Current = latest
	}
	return out, nil
}

// isCurrentStreak reports whether a run reaches up to today or yesterday. A
// streak that ended yesterday is still alive: the user simply has not listened to
// anything yet today.
func isCurrentStreak(st domain.Streak, now time.Time, loc *time.Location) bool {
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return !st.EndDay.Before(today.AddDate(0, 0, -1))
}
