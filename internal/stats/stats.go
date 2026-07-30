// Package stats answers every analytic question Encore's dashboard asks of the
// listening fact table, and maintains the daily rollup those answers may fall
// back on.
//
// Three rules hold throughout the package.
//
// Local time. The fact table stores UTC instants, but a listener thinks in local
// days: every bucket boundary is therefore computed as
// (played_at AT TIME ZONE $tz) with the owning user's IANA timezone, so "today",
// "this week" and "3pm" mean what the listener means rather than what the
// server's clock means.
//
// The blacklist. A listen whose track has an artist the user has blacklisted is
// invisible to every statistic here. The rule is written once, in
// blacklistFilter, and composed into every query, so an artist cannot leak back
// into one chart after being excluded from another. The one documented
// exception is TopDiff (topdiff.go): it can still show Spotify's own captured
// ranking of a blacklisted artist, because that ranking is a snapshot Spotify
// computed independently and is not a listen this package has any authority
// over. Encore's own numbers for that artist - its rank and play count on
// Encore's side of the same comparison - are still unconditionally zero, which
// is the guarantee this rule actually makes: a blacklisted artist's listens
// never resurface in Encore's own play-derived numbers, anywhere.
//
// Correctness before speed. Wide top-N queries may read listen_daily_rollup, but
// only when the requested range is provably clean and aligned to local midnight.
// In every other case they scan the fact table, which is slower and always right.
package stats

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/store"
)

// Service is the statistics repository. Like every repository in Encore it holds
// only the store; each method takes an explicit Querier so exactly the same code
// runs inside and outside a transaction.
//
// Almost nothing here calls time.Now directly - every range-scoped statistic is
// handed a domain.TimeRange by its caller instead, which is what makes the
// package testable without waiting for real time to pass. Now is the one
// exception, injectable for exactly the same reason the rest of the package
// avoids needing it: TopDiff must derive its own window from the current
// instant (see topdiff.go) rather than from a caller-supplied range, and a
// clock is the only way to make that derivation reproducible in a test.
type Service struct {
	db  *store.Store
	Now func() time.Time
}

// New builds the service. The clock defaults to time.Now; tests that need a
// fixed "now" - TopDiff's window in particular - override Service.Now
// directly after construction.
func New(db *store.Store) *Service { return &Service{db: db, Now: time.Now} }

// Page-size bounds shared by every paginated statistic. The maximum exists
// because a page is materialised in memory on both sides of the wire.
const (
	DefaultPageSize = 50
	MaxPageSize     = 500
)

// blacklistFilter is the single definition of Encore's blacklist rule: as soon as
// one of a track's artists is on the user's blacklist, every listen of that track
// disappears from every statistic, retroactively and without deleting anything.
//
// The join aliases are deliberately unusual (bta, bl) so the fragment can be
// pasted into a query that already joins track_artists without shadowing it.
// alias is always a literal chosen inside this package, never caller input, so
// composing it into SQL is not an injection vector.
func blacklistFilter(alias string) string {
	return fmt.Sprintf(`NOT EXISTS (
            SELECT 1 FROM track_artists bta
            JOIN user_blacklisted_artists bl
              ON bl.artist_id = bta.artist_id AND bl.user_id = %[1]s.user_id
            WHERE bta.track_id = %[1]s.track_id)`, alias)
}

// rangeFilter is the standard scope of a statistic: one user, the half-open
// instant range [from, to), and the blacklist. The argument placeholders are
// passed in because the same fragment serves the requested period, the preceding
// period and the second user of an affinity comparison.
func rangeFilter(alias, userArg, fromArg, toArg string) string {
	return fmt.Sprintf(
		"%[1]s.user_id = %[2]s AND %[1]s.played_at >= %[3]s::timestamptz AND %[1]s.played_at < %[4]s::timestamptz AND %[5]s",
		alias, userArg, fromArg, toArg, blacklistFilter(alias))
}

// checkScope validates the arguments every statistic shares.
func checkScope(userID uuid.UUID, r domain.TimeRange) error {
	if userID == uuid.Nil {
		return fmt.Errorf("%w: a user is required", domain.ErrValidation)
	}
	return r.Validate()
}

// scope validates the shared arguments and resolves the timezone.
//
// An unknown timezone is a caller error rather than a silent fallback to UTC:
// bucketing in the wrong zone produces numbers that look plausible and are wrong,
// which is worse than a 400.
func scope(userID uuid.UUID, r domain.TimeRange, tz string) (*time.Location, error) {
	if err := checkScope(userID, r); err != nil {
		return nil, err
	}
	return location(tz)
}

// location resolves an IANA timezone name. An empty name means UTC, which is what
// a user row carries before the owner ever visits the settings page.
func location(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown timezone %q", domain.ErrValidation, tz)
	}
	return loc, nil
}

// tzArg is the timezone name as SQL sees it.
func tzArg(tz string) string {
	if tz == "" {
		return "UTC"
	}
	return tz
}

// inLocation reattaches the user's timezone to a bucket boundary.
//
// A boundary comes back from Postgres as `timestamp without time zone`, which pgx
// labels UTC, but the value is a local wall-clock reading: 2026-03-01T00:00 in
// Berlin, not in UTC. Only after the location is reattached does the instant
// agree with the data the bucket contains.
func inLocation(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
}

// toLocation converts a real instant for display. Unlike a bucket boundary, a
// timestamptz column already identifies the moment correctly; only its offset
// changes.
func toLocation(t *time.Time, loc *time.Location) *time.Time {
	if t == nil {
		return nil
	}
	v := t.In(loc)
	return &v
}

// clampLimit bounds a caller-supplied page size.
func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPageSize
	case limit > MaxPageSize:
		return MaxPageSize
	default:
		return limit
	}
}

// clampOffset rejects a negative offset rather than letting it reach SQL.
func clampOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}

// share is a fraction of a total, guarding the empty-range case where the total
// is zero and every share is meaningless rather than infinite.
func share(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total)
}
