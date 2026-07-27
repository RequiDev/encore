package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// testUser is the caller the parameter tests parse on behalf of. The timezone
// matters: the default range is aligned to local midnight, not to UTC midnight.
func testUser(t *testing.T, timezone string) domain.User {
	t.Helper()
	if _, err := time.LoadLocation(timezone); err != nil {
		t.Skipf("the runtime has no tzdata for %s", timezone)
	}
	return domain.User{Timezone: timezone}
}

func request(query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/api/stats/summary?"+query, nil)
}

// apiErrorOf extracts the API error a parser returned, failing when it returned
// something else.
func apiErrorOf(t *testing.T, err error) *APIError {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	return apiErr
}

// TestParseRangeDefaultsToTheTrailingMonth checks the contract's default: both
// bounds omitted gives the trailing thirty local days.
func TestParseRangeDefaultsToTheTrailingMonth(t *testing.T) {
	user := testUser(t, "Europe/Berlin")
	now := time.Date(2026, time.July, 26, 9, 30, 0, 0, time.UTC)

	tr, err := parseRange(request(""), user, now)
	if err != nil {
		t.Fatalf("parseRange with no bounds: %v", err)
	}
	want := domain.DefaultRange(now, user.Location())
	if !tr.From.Equal(want.From) || !tr.To.Equal(want.To) {
		t.Fatalf("parseRange = [%s, %s), want [%s, %s)", tr.From, tr.To, want.From, want.To)
	}
	if hours := tr.Duration().Hours(); hours < 30*24-2 || hours > 30*24+2 {
		t.Fatalf("the default range spans %.1f hours, want about %d", hours, 30*24)
	}
}

// TestParseRangeAcceptsAnExplicitWindow checks that a well-formed pair is taken
// as given and normalised to UTC.
func TestParseRangeAcceptsAnExplicitWindow(t *testing.T) {
	user := testUser(t, "UTC")
	now := time.Date(2026, time.July, 26, 9, 30, 0, 0, time.UTC)

	tr, err := parseRange(request("from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00%2B01:00"), user, now)
	if err != nil {
		t.Fatalf("parseRange: %v", err)
	}
	if got := tr.From.Format(time.RFC3339); got != "2026-01-01T00:00:00Z" {
		t.Fatalf("from = %s", got)
	}
	if got := tr.To.Format(time.RFC3339); got != "2026-01-31T23:00:00Z" {
		t.Fatalf("to = %s, want the offset normalised to UTC", got)
	}
}

// TestParseRangeRejections checks that every bad window is a 400 that names the
// parameter at fault.
func TestParseRangeRejections(t *testing.T) {
	user := testUser(t, "UTC")
	now := time.Date(2026, time.July, 26, 9, 30, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
		field string
	}{
		{"only from", "from=2026-01-01T00:00:00Z", "to"},
		{"only to", "to=2026-01-01T00:00:00Z", "from"},
		{"from is not a timestamp", "from=yesterday&to=2026-01-01T00:00:00Z", "from"},
		{"to is a bare date", "from=2026-01-01T00:00:00Z&to=2026-02-01", "to"},
		{"from equals to", "from=2026-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", "from"},
		{"from is after to", "from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z", "from"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseRange(request(c.query), user, now)
			apiErr := apiErrorOf(t, err)
			if apiErr.Status != http.StatusBadRequest || apiErr.Code != CodeInvalidRequest {
				t.Fatalf("status/code = %d/%s, want 400/%s", apiErr.Status, apiErr.Code, CodeInvalidRequest)
			}
			if apiErr.Details["field"] != c.field {
				t.Fatalf("details named %q, want %q", apiErr.Details["field"], c.field)
			}
		})
	}
}

// TestParseIntervalDefaultsToTheFinestThatFits checks the suggestion rule: a
// short range is bucketed by hour, a decade by something much coarser.
func TestParseIntervalDefaultsToTheFinestThatFits(t *testing.T) {
	cases := []struct {
		name string
		span time.Duration
		want domain.Interval
	}{
		{"a week", 7 * 24 * time.Hour, domain.IntervalHour},
		{"a year", 365 * 24 * time.Hour, domain.IntervalDay},
		{"a decade", 10 * 365 * 24 * time.Hour, domain.IntervalWeek},
		{"a century", 100 * 365 * 24 * time.Hour, domain.IntervalMonth},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			from := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			tr := domain.TimeRange{From: from, To: from.Add(c.span)}

			got, err := parseInterval(request(""), tr)
			if err != nil {
				t.Fatalf("parseInterval: %v", err)
			}
			if got != c.want {
				t.Fatalf("interval = %q, want %q", got, c.want)
			}
			if n := bucketCount(tr, got); n > domain.MaxTimelineBuckets {
				t.Fatalf("the suggested interval would produce %d buckets, over the cap of %d", n, domain.MaxTimelineBuckets)
			}
		})
	}
}

// TestParseIntervalRejectsTooManyBuckets checks that an interval which would
// blow the cap is refused with advice rather than served slowly.
func TestParseIntervalRejectsTooManyBuckets(t *testing.T) {
	from := time.Date(2016, time.January, 1, 0, 0, 0, 0, time.UTC)
	tr := domain.TimeRange{From: from, To: from.Add(10 * 365 * 24 * time.Hour)}

	_, err := parseInterval(request("interval=hour"), tr)
	apiErr := apiErrorOf(t, err)
	if apiErr.Status != http.StatusBadRequest || apiErr.Details["field"] != "interval" {
		t.Fatalf("status/field = %d/%s, want 400/interval", apiErr.Status, apiErr.Details["field"])
	}
	if want := string(domain.SuggestInterval(tr)); !strings.Contains(apiErr.Message, want) {
		t.Fatalf("message %q does not suggest the coarser interval %q", apiErr.Message, want)
	}
}

// TestParseIntervalRejectsUnknownNames checks that only the five documented
// widths are accepted.
func TestParseIntervalRejectsUnknownNames(t *testing.T) {
	from := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	tr := domain.TimeRange{From: from, To: from.AddDate(0, 1, 0)}

	if _, err := parseInterval(request("interval=fortnight"), tr); err == nil {
		t.Fatal("an unknown interval was accepted")
	}
	if got, err := parseInterval(request("interval=DAY"), tr); err != nil || got != domain.IntervalDay {
		t.Fatalf("parseInterval(DAY) = %q, %v; want day and no error", got, err)
	}
}

// TestParsePage checks the bounds every paginated endpoint shares.
func TestParsePage(t *testing.T) {
	limit, offset, err := parsePage(request(""))
	if err != nil || limit != defaultPageLimit || offset != 0 {
		t.Fatalf("parsePage with no parameters = %d, %d, %v", limit, offset, err)
	}
	if limit, offset, err = parsePage(request("limit=25&offset=100")); err != nil || limit != 25 || offset != 100 {
		t.Fatalf("parsePage = %d, %d, %v", limit, offset, err)
	}
	for _, query := range []string{"limit=0", "limit=-1", "limit=abc", "limit=100000", "offset=-1", "offset=x"} {
		if _, _, err := parsePage(request(query)); err == nil {
			t.Errorf("parsePage(%q) was accepted", query)
		}
	}
}

// TestParseYear keeps a retrospective inside the years Spotify has existed for.
func TestParseYear(t *testing.T) {
	now := time.Date(2026, time.July, 26, 0, 0, 0, 0, time.UTC)

	if year, err := parseYear(request(""), now); err != nil || year != 2026 {
		t.Fatalf("parseYear with no parameter = %d, %v", year, err)
	}
	if year, err := parseYear(request("year=2019"), now); err != nil || year != 2019 {
		t.Fatalf("parseYear(2019) = %d, %v", year, err)
	}
	for _, query := range []string{"year=1999", "year=2100", "year=soon"} {
		if _, err := parseYear(request(query), now); err == nil {
			t.Errorf("parseYear(%q) was accepted", query)
		}
	}
}

// TestValidateRedirect is the open-redirect guard.
//
// Everything the login flow will follow has to resolve inside ENCORE_WEB_URL;
// everything else is discarded so that a crafted ?redirect_to= cannot turn a
// trusted host into a springboard.
func TestValidateRedirect(t *testing.T) {
	const webURL = "https://encore.example.com/app"

	accepted := map[string]string{
		"/dashboard":                              "https://encore.example.com/app/dashboard",
		"/dashboard?tab=imports":                  "https://encore.example.com/app/dashboard?tab=imports",
		"https://encore.example.com/app":          "https://encore.example.com/app",
		"https://encore.example.com/app/settings": "https://encore.example.com/app/settings",
		"https://ENCORE.example.com/app/settings": "https://ENCORE.example.com/app/settings",
	}
	for in, want := range accepted {
		got, ok := validateRedirect(webURL, in)
		if !ok || got != want {
			t.Errorf("validateRedirect(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}

	rejected := []string{
		"",
		"//evil.example.com/steal",
		"https://evil.example.com/app",
		"http://encore.example.com/app",
		"https://encore.example.com.evil/app",
		"https://encore.example.com/apphijack",
		"https://encore.example.com/other",
		"javascript:alert(1)",
		"/dashboard\r\nSet-Cookie: a=b",
		"dashboard",
	}
	for _, in := range rejected {
		if got, ok := validateRedirect(webURL, in); ok {
			t.Errorf("validateRedirect(%q) = %q, true; want it refused", in, got)
		}
	}
}

// TestParseExportFormat checks the two formats the export offers.
func TestParseExportFormat(t *testing.T) {
	for query, want := range map[string]string{"": "json", "format=json": "json", "format=CSV": "csv"} {
		got, err := parseExportFormat(request(query))
		if err != nil || got != want {
			t.Errorf("parseExportFormat(%q) = %q, %v; want %q", query, got, err, want)
		}
	}
	if _, err := parseExportFormat(request("format=xml")); err == nil {
		t.Fatal("an unsupported export format was accepted")
	}
}
