//go:build integration

package integration

import (
	"testing"

	"github.com/RequiDev/encore/internal/stats"
)

// seedContext adds playback detail to the shared fixture's listens.
//
// The fixture's eight plays are all source 0 (sync) by default, which carry no
// context at all. This marks five of them as source 2 with detail, and leaves
// one of those five with a NULL shuffle so the per-column denominator rule has
// something to prove.
func seedContext(t *testing.T, f *statsFixture) {
	t.Helper()
	f.env.Exec(`
        WITH ordered AS (
            SELECT id, row_number() OVER (ORDER BY played_at) AS n
            FROM listens WHERE user_id = $1
        )
        UPDATE listens l SET
            source       = 2,
            reason_end   = CASE o.n WHEN 1 THEN 'trackdone' WHEN 2 THEN 'fwdbtn'
                                    WHEN 3 THEN 'fwdbtn'    WHEN 4 THEN 'backbtn'
                                    ELSE 'trackdone' END,
            shuffle      = CASE o.n WHEN 5 THEN NULL ELSE (o.n % 2 = 0) END,
            platform     = CASE o.n WHEN 1 THEN 'Android OS 10 API 29 (samsung, SM-G970F)'
                                    WHEN 2 THEN 'Android OS 11 API 30 (google, Pixel 5)'
                                    ELSE 'Windows 10 (10.0.19042; x64)' END,
            conn_country = CASE o.n WHEN 1 THEN 'DE' ELSE 'GB' END,
            offline      = false,
            incognito    = false
        FROM ordered o
        WHERE l.id = o.id AND o.n <= 5`, f.user.ID)
}

// TestContextDenominatorsArePerColumn is the rule the whole file rests on. An
// extended export can omit an individual field, so keying the denominator on
// source = 2 would overstate it for whichever field the export did not write.
func TestContextDenominatorsArePerColumn(t *testing.T) {
	f := seedStats(t)
	seedContext(t, f)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	// Five rows carry reason_end; only four of those carry shuffle.
	if got.EndReasonCoverage.Covered != 5 || got.EndReasonCoverage.Total != 8 {
		t.Errorf("reason_end coverage = %+v, want 5/8", got.EndReasonCoverage)
	}
	if got.ShuffleCoverage.Covered != 4 || got.ShuffleCoverage.Total != 8 {
		t.Errorf("shuffle coverage = %+v, want 4/8 — the denominator keyed on source, not on the column", got.ShuffleCoverage)
	}
}

// TestSkipRateCountsForwardOnly pins the definition. Going back is not the same
// gesture as skipping forward, and only fwdbtn counts.
func TestSkipRateCountsForwardOnly(t *testing.T) {
	f := seedStats(t)
	seedContext(t, f)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	// Of five rows with reason_end: two fwdbtn, one backbtn, two trackdone.
	if diff := got.SkipRate - 0.4; diff > 0.001 || diff < -0.001 {
		t.Errorf("skip rate = %v, want 0.4 (2 of 5) — backbtn may have been counted", got.SkipRate)
	}
	if got.SkipCoverage.Covered != 5 {
		t.Errorf("skip coverage = %+v, want 5 covered", got.SkipCoverage)
	}
}

// TestPlatformsAreGroupedByFamily checks the classifier is actually applied and
// that two different Android strings collapse to one slice.
func TestPlatformsAreGroupedByFamily(t *testing.T) {
	f := seedStats(t)
	seedContext(t, f)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	by := map[string]int64{}
	for _, p := range got.Platforms {
		by[p.Key] = p.Plays
	}
	if by[stats.PlatformAndroid] != 2 {
		t.Errorf("android = %d, want 2 — two distinct Android strings should collapse into one family", by[stats.PlatformAndroid])
	}
	if by[stats.PlatformWindows] != 3 {
		t.Errorf("windows = %d, want 3", by[stats.PlatformWindows])
	}
	if len(got.Platforms) != 2 {
		t.Errorf("got %d platform families, want 2: %+v", len(got.Platforms), got.Platforms)
	}
}

// TestContextWithNoExtendedRowsIsZeroNotAnError is the state of an instance whose
// history came entirely from live sync or an account-data export.
func TestContextWithNoExtendedRowsIsZeroNotAnError(t *testing.T) {
	f := seedStats(t)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context with no extended rows: %v", err)
	}
	if got.SkipCoverage.Covered != 0 {
		t.Errorf("skip coverage covered = %d, want 0", got.SkipCoverage.Covered)
	}
	if got.SkipCoverage.Total != 8 {
		t.Errorf("skip coverage total = %d, want 8 — the total is all in-range listens", got.SkipCoverage.Total)
	}
	if got.SkipRate != 0 {
		t.Errorf("skip rate = %v, want 0", got.SkipRate)
	}
	if len(got.Platforms) != 0 {
		t.Errorf("expected no platforms, got %+v", got.Platforms)
	}
}
