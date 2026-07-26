package domain

import (
	"fmt"
	"time"
)

// Interval is the bucket width for a timeline query.
type Interval string

const (
	IntervalHour  Interval = "hour"
	IntervalDay   Interval = "day"
	IntervalWeek  Interval = "week"
	IntervalMonth Interval = "month"
	IntervalYear  Interval = "year"
)

func (i Interval) Valid() bool {
	switch i {
	case IntervalHour, IntervalDay, IntervalWeek, IntervalMonth, IntervalYear:
		return true
	}
	return false
}

// Approx is the nominal length of the interval, used only to estimate how many
// buckets a range will produce.
func (i Interval) Approx() time.Duration {
	switch i {
	case IntervalHour:
		return time.Hour
	case IntervalDay:
		return 24 * time.Hour
	case IntervalWeek:
		return 7 * 24 * time.Hour
	case IntervalMonth:
		return 30 * 24 * time.Hour
	case IntervalYear:
		return 365 * 24 * time.Hour
	}
	return 24 * time.Hour
}

// MaxTimelineBuckets caps how many points a single timeline response may contain,
// which is what keeps the endpoint responsive on a decade of history.
const MaxTimelineBuckets = 1500

// SuggestInterval picks the finest interval whose bucket count stays under the
// cap, so "all time" degrades to months or years instead of timing out.
func SuggestInterval(r TimeRange) Interval {
	d := r.Duration()
	for _, i := range []Interval{IntervalHour, IntervalDay, IntervalWeek, IntervalMonth, IntervalYear} {
		if d/i.Approx() <= MaxTimelineBuckets {
			return i
		}
	}
	return IntervalYear
}

// TimeRange is a half-open interval [From, To).
type TimeRange struct {
	From time.Time
	To   time.Time
}

// Duration is the length of the range.
func (r TimeRange) Duration() time.Duration { return r.To.Sub(r.From) }

// IsZero reports whether neither bound is set.
func (r TimeRange) IsZero() bool { return r.From.IsZero() && r.To.IsZero() }

// Validate checks that the range is usable for a query.
func (r TimeRange) Validate() error {
	if r.From.IsZero() || r.To.IsZero() {
		return fmt.Errorf("%w: both 'from' and 'to' are required", ErrValidation)
	}
	if !r.From.Before(r.To) {
		return fmt.Errorf("%w: 'from' must be strictly before 'to'", ErrValidation)
	}
	return nil
}

// Previous returns the equal-length range immediately preceding r. It is what
// powers "up 3 places since last month" style rank deltas and period comparison.
func (r TimeRange) Previous() TimeRange {
	d := r.Duration()
	return TimeRange{From: r.From.Add(-d), To: r.From}
}

// Contains reports whether t falls inside the half-open range.
func (r TimeRange) Contains(t time.Time) bool {
	return !t.Before(r.From) && t.Before(r.To)
}

// DefaultRange is the range used when a request omits both bounds: the trailing
// 30 days in the user's own timezone, aligned to local midnight so "today" means
// what the user expects.
func DefaultRange(now time.Time, loc *time.Location) TimeRange {
	local := now.In(loc)
	end := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	return TimeRange{From: end.AddDate(0, 0, -30), To: end}
}

// SessionGap is how long a silence must be before Encore treats the next listen as
// a new listening session. Thirty minutes is long enough to survive an advert
// break or a short interruption and short enough that leaving Spotify paused
// overnight does not merge two days into one session.
const SessionGap = 30 * time.Minute

// ListeningSession is a run of listens with no gap longer than SessionGap.
type ListeningSession struct {
	StartedAt  time.Time
	EndedAt    time.Time
	TrackCount int
	MsPlayed   int64
	TrackIDs   []string
}

// Duration is the wall-clock span of the session.
func (s ListeningSession) Duration() time.Duration { return s.EndedAt.Sub(s.StartedAt) }

// SessionInput is the minimum a listen must expose to be grouped into sessions.
type SessionInput struct {
	PlayedAt time.Time
	MsPlayed int32
	TrackID  string
}

// BuildSessions groups chronologically ordered listens into listening sessions.
// Input must be sorted ascending by PlayedAt; the caller gets that ordering from
// the index on (user_id, played_at).
func BuildSessions(listens []SessionInput, gap time.Duration) []ListeningSession {
	if len(listens) == 0 {
		return nil
	}
	if gap <= 0 {
		gap = SessionGap
	}
	var out []ListeningSession
	cur := ListeningSession{StartedAt: listens[0].PlayedAt}
	var prevEnd time.Time

	flush := func() {
		cur.EndedAt = prevEnd
		out = append(out, cur)
	}

	for i, l := range listens {
		end := l.PlayedAt.Add(time.Duration(l.MsPlayed) * time.Millisecond)
		if i > 0 && l.PlayedAt.Sub(prevEnd) > gap {
			flush()
			cur = ListeningSession{StartedAt: l.PlayedAt}
		}
		cur.TrackCount++
		cur.MsPlayed += int64(l.MsPlayed)
		if l.TrackID != "" {
			cur.TrackIDs = append(cur.TrackIDs, l.TrackID)
		}
		if end.After(prevEnd) || i == 0 {
			prevEnd = end
		}
	}
	flush()
	return out
}

// Streak is a run of consecutive local days with at least one listen.
type Streak struct {
	StartDay time.Time // local midnight
	EndDay   time.Time // local midnight of the last active day
	Days     int
}

// BuildStreaks turns a sorted, de-duplicated list of active local days into
// streaks, longest-relevant ordering left to the caller.
func BuildStreaks(days []time.Time) []Streak {
	if len(days) == 0 {
		return nil
	}
	var out []Streak
	cur := Streak{StartDay: days[0], EndDay: days[0], Days: 1}
	for _, d := range days[1:] {
		if d.Equal(cur.EndDay) {
			continue
		}
		if d.Equal(cur.EndDay.AddDate(0, 0, 1)) {
			cur.EndDay = d
			cur.Days++
			continue
		}
		out = append(out, cur)
		cur = Streak{StartDay: d, EndDay: d, Days: 1}
	}
	out = append(out, cur)
	return out
}
