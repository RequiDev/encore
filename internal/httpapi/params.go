package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/stats"
)

// Page-size bounds for the endpoints that take ?limit=&offset=. They mirror the
// repositories' own clamps so that an out-of-range request is answered with a
// 400 that names the parameter, rather than being silently rounded.
const (
	defaultPageLimit = 50
	maxPageLimit     = 500
)

// parseRange reads ?from= and ?to= as a half-open [from, to) instant range.
//
// Omitting both gives domain.DefaultRange in the user's own timezone, which is
// the trailing thirty local days. Supplying only one is an error rather than an
// open-ended scan, because "everything since January" over a decade of history
// is a very different query from the one the caller probably meant.
func parseRange(r *http.Request, u domain.User, now time.Time) (domain.TimeRange, error) {
	return parseNamedRange(r, u, now, "from", "to")
}

// parseNamedRange is parseRange with the parameter names supplied, for
// /api/stats/compare, which carries two ranges in one query string.
func parseNamedRange(r *http.Request, u domain.User, now time.Time, fromKey, toKey string) (domain.TimeRange, error) {
	q := r.URL.Query()
	fromRaw := strings.TrimSpace(q.Get(fromKey))
	toRaw := strings.TrimSpace(q.Get(toKey))

	if fromRaw == "" && toRaw == "" {
		return domain.DefaultRange(now, u.Location()), nil
	}
	if fromRaw == "" {
		return domain.TimeRange{}, ErrFieldInvalid(fromKey,
			fmt.Sprintf("%q is required when %q is given.", fromKey, toKey))
	}
	if toRaw == "" {
		return domain.TimeRange{}, ErrFieldInvalid(toKey,
			fmt.Sprintf("%q is required when %q is given.", toKey, fromKey))
	}

	from, err := parseTimestamp(fromKey, fromRaw)
	if err != nil {
		return domain.TimeRange{}, err
	}
	to, err := parseTimestamp(toKey, toRaw)
	if err != nil {
		return domain.TimeRange{}, err
	}

	tr := domain.TimeRange{From: from, To: to}
	if err := tr.Validate(); err != nil {
		return domain.TimeRange{}, ErrFieldInvalid(fromKey,
			fmt.Sprintf("%q must be strictly before %q.", fromKey, toKey)).WithCause(err)
	}
	return tr, nil
}

// parseTimestamp reads one RFC 3339 instant.
func parseTimestamp(field, raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, ErrFieldInvalid(field,
			fmt.Sprintf("%q must be an RFC 3339 timestamp such as 2026-01-31T00:00:00Z.", field)).WithCause(err)
	}
	return t.UTC(), nil
}

// parseInterval reads ?interval=, defaulting to the finest width that keeps the
// response under domain.MaxTimelineBuckets.
//
// An interval that would exceed the cap is refused with the coarser one that
// would work, so the client can retry without having to know the arithmetic.
func parseInterval(r *http.Request, tr domain.TimeRange) (domain.Interval, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("interval"))
	if raw == "" {
		// domain.SuggestInterval counts buckets without the boundary one that the
		// statistics layer's own check includes, so at the exact limit its answer
		// would be refused one level down. Stepping to the next width closes that
		// gap rather than turning an omitted parameter into a 400.
		suggested := domain.SuggestInterval(tr)
		for bucketCount(tr, suggested) > domain.MaxTimelineBuckets {
			next, ok := coarser(suggested)
			if !ok {
				break
			}
			suggested = next
		}
		return suggested, nil
	}
	interval := domain.Interval(strings.ToLower(raw))
	if !interval.Valid() {
		return "", ErrFieldInvalid("interval",
			`"interval" must be one of hour, day, week, month or year.`)
	}
	if buckets := bucketCount(tr, interval); buckets > domain.MaxTimelineBuckets {
		return "", ErrFieldInvalid("interval", fmt.Sprintf(
			"%q buckets would produce about %d points over this range, more than the maximum of %d; try %q or a shorter range.",
			interval, buckets, domain.MaxTimelineBuckets, domain.SuggestInterval(tr)))
	}
	return interval, nil
}

// bucketCount estimates how many points an interval yields over a range. It is
// the same estimate the statistics layer makes, so a request this accepts is
// never refused one level down.
func bucketCount(tr domain.TimeRange, interval domain.Interval) int64 {
	return int64(tr.Duration()/interval.Approx()) + 1
}

// intervalLadder is the widths in order, coarsening left to right.
var intervalLadder = []domain.Interval{
	domain.IntervalHour, domain.IntervalDay, domain.IntervalWeek,
	domain.IntervalMonth, domain.IntervalYear,
}

// coarser returns the next wider interval, or false at the top of the ladder.
func coarser(interval domain.Interval) (domain.Interval, bool) {
	for i, candidate := range intervalLadder {
		if candidate == interval && i+1 < len(intervalLadder) {
			return intervalLadder[i+1], true
		}
	}
	return interval, false
}

// parseLimit reads ?limit=, falling back to def and refusing anything outside
// [1, max].
func parseLimit(r *http.Request, def, max int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		return 0, ErrFieldInvalid("limit",
			fmt.Sprintf(`"limit" must be a whole number between 1 and %d.`, max)).WithCause(err)
	}
	return n, nil
}

// parseOffset reads ?offset=, which must be zero or more.
func parseOffset(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("offset"))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, ErrFieldInvalid("offset", `"offset" must be zero or a positive whole number.`).WithCause(err)
	}
	return n, nil
}

// parsePage reads the ?limit=&offset= pair every paginated endpoint accepts.
func parsePage(r *http.Request) (limit, offset int, err error) {
	if limit, err = parseLimit(r, defaultPageLimit, maxPageLimit); err != nil {
		return 0, 0, err
	}
	if offset, err = parseOffset(r); err != nil {
		return 0, 0, err
	}
	return limit, offset, nil
}

// parseUUIDPath reads a {name} path segment as a UUID.
//
// A malformed id is a 404 rather than a 400: the caller followed a link that
// does not address anything, and saying "that is not a valid identifier" only
// helps somebody probing the shape of the id space.
func parseUUIDPath(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, ErrNotFoundf("That does not exist.").WithCause(err)
	}
	return id, nil
}

// parseSpotifyIDPath reads a {name} path segment as a Spotify catalogue id.
func parseSpotifyIDPath(r *http.Request, name string) (string, error) {
	id := strings.TrimSpace(r.PathValue(name))
	if id == "" || len(id) > 64 || !isBase62(id) {
		return "", ErrNotFoundf("That does not exist.")
	}
	return id, nil
}

// isBase62 checks the shape of a Spotify id without asserting a fixed length,
// since Spotify has never formally guaranteed twenty-two characters.
func isBase62(s string) bool {
	for i := range s {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

// parseYear reads ?year=, which must be a plausible year of listening.
func parseYear(r *http.Request, now time.Time) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("year"))
	if raw == "" {
		return now.Year(), nil
	}
	year, err := strconv.Atoi(raw)
	if err != nil || year < stats.EarliestYear || year > now.Year()+1 {
		return 0, ErrFieldInvalid("year", fmt.Sprintf(
			`"year" must be a whole number between %d and %d.`, stats.EarliestYear, now.Year()+1)).WithCause(err)
	}
	return year, nil
}

// parseExportFormat reads ?format=, which is json or csv.
func parseExportFormat(r *http.Request) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	switch raw {
	case "", "json":
		return "json", nil
	case "csv":
		return "csv", nil
	default:
		return "", ErrFieldInvalid("format", `"format" must be json or csv.`)
	}
}

// parseTopDiffKind reads ?kind= for GET /api/stats/top-diff: one of the two
// entity kinds Spotify's own top-items endpoint knows. Spotify has no
// top-albums endpoint (see stats.topDiffKind), so "album" is refused here, at
// the edge, rather than reaching stats.TopDiff's own validation and coming
// back as the same domain.ErrValidation a genuine typo would produce.
func parseTopDiffKind(r *http.Request) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	switch kind {
	case "track", "artist":
		return kind, nil
	default:
		return "", ErrFieldInvalid("kind", `"kind" must be track or artist.`)
	}
}

// parseTopDiffRange reads ?range= for GET /api/stats/top-diff: one of
// Spotify's own three top-items time ranges (see stats.topDiffWindow).
// Deliberately not the ?from=&to= pair every other statistic takes - this
// endpoint's window comes from Spotify's own time range rather than a caller
// picked one, and giving it a different parameter name keeps that from being
// an easy mistake to make silently.
func parseTopDiffRange(r *http.Request) (string, error) {
	timeRange := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("range")))
	switch timeRange {
	case "short_term", "medium_term", "long_term":
		return timeRange, nil
	default:
		return "", ErrFieldInvalid("range", `"range" must be short_term, medium_term or long_term.`)
	}
}

// validateRedirect decides whether a caller-supplied redirect target may be used.
//
// This is the guard that stops the login journey being turned into an open
// redirect. A relative path is resolved against ENCORE_WEB_URL; an absolute URL
// is accepted only when its scheme, host and path prefix all match that base.
// Anything else — a different host, a protocol-relative "//evil.example",
// a javascript: URL, a control character — is discarded and the caller lands on
// the web client's root instead.
func validateRedirect(webURL, raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || webURL == "" {
		return "", false
	}
	// A newline or NUL in a Location header is a response-splitting attempt.
	if strings.ContainsAny(raw, "\r\n\x00") {
		return "", false
	}
	base, err := url.Parse(strings.TrimRight(webURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", false
	}

	// "//host/path" is protocol-relative and therefore off-site, even though
	// url.Parse happily reports it as having no scheme.
	if strings.HasPrefix(raw, "//") {
		return "", false
	}

	target, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if !target.IsAbs() {
		if !strings.HasPrefix(target.Path, "/") {
			return "", false
		}
		resolved := *base
		resolved.Path = joinPath(base.Path, target.Path)
		resolved.RawQuery = target.RawQuery
		resolved.Fragment = target.Fragment
		return resolved.String(), true
	}

	if !strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Host, base.Host) {
		return "", false
	}
	if !withinPath(base.Path, target.Path) {
		return "", false
	}
	return target.String(), true
}

// joinPath appends a rooted path onto the web client's base path.
func joinPath(base, path string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return path
	}
	return base + path
}

// withinPath reports whether target lies under base, comparing whole segments so
// that "/app" does not admit "/application".
func withinPath(base, target string) bool {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return true
	}
	return target == base || strings.HasPrefix(target, base+"/")
}
