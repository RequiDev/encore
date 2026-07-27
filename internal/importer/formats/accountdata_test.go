package formats

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

func TestAccountDataParserRecords(t *testing.T) {
	p := NewAccountDataParser()
	if p.Format() != domain.FormatAccountData {
		t.Fatalf("Format() = %s", p.Format())
	}
	records := rawRecords(t, "accountdata.json")
	if len(records) != 5 {
		t.Fatalf("fixture has %d records, want 5", len(records))
	}

	// endTime is the end of the stream and is only accurate to the minute, which
	// is what PrecisionMinute records for the cross-source duplicate window.
	assertListen(t, mustParse(t, p, records[0], 1000).Listen, domain.Listen{
		PlayedAt:   time.Date(2020, time.February, 14, 21, 28, 12, 0, time.UTC),
		Precision:  domain.PrecisionMinute,
		Identity:   domain.TrackIdentityFromNames("Nils Frahm", "Says"),
		TrackName:  "Says",
		ArtistName: "Nils Frahm",
		MsPlayed:   528000,
		Source:     domain.SourceAccountData,
	})

	assertSkipped(t, mustParse(t, p, records[1], 1000), domain.SkipBelowMinimum)

	// An RFC 3339 endTime and a string msPlayed, both seen in real exports.
	assertListen(t, mustParse(t, p, records[2], 1000).Listen, domain.Listen{
		PlayedAt:   time.Date(2020, time.February, 15, 7, 57, 59, 0, time.UTC),
		Precision:  domain.PrecisionMinute,
		Identity:   domain.TrackIdentityFromNames("Kiasmos", "Blurred - Remastered 2019"),
		TrackName:  "Blurred - Remastered 2019",
		ArtistName: "Kiasmos",
		MsPlayed:   312000,
		Source:     domain.SourceAccountData,
	})

	assertRejected(t, p, records[3], 1000, domain.RejectMissingTrack)
	assertRejected(t, p, records[4], 1000, domain.RejectInvalidTimestamp)
}

func TestAccountDataParserPodcastVariant(t *testing.T) {
	p := NewAccountDataParser()
	for i, raw := range rawRecords(t, "accountdata_podcast.json") {
		rec := mustParse(t, p, raw, 1000)
		if rec.Skip == nil || rec.Skip.Reason != domain.SkipNotMusic {
			t.Errorf("record %d skip = %v, want %s", i, rec.Skip, domain.SkipNotMusic)
		}
	}
}

func TestAccountDataParserRejects(t *testing.T) {
	p := NewAccountDataParser()
	cases := []struct {
		name string
		raw  string
		want domain.RejectReason
	}{
		{"not an object", `"a string"`, domain.RejectMalformedRecord},
		{"nothing recognisable", `{"searchQuery":"nils frahm"}`, domain.RejectUnknownShape},
		{"missing endTime", `{"artistName":"A","trackName":"T","msPlayed":60000}`, domain.RejectMissingTimestamp},
		{"unparseable msPlayed", `{"endTime":"2020-01-01 10:00","artistName":"A","trackName":"T","msPlayed":"soon"}`, domain.RejectInvalidMsPlayed},
		{"artist only", `{"endTime":"2020-01-01 10:00","artistName":"A","trackName":"","msPlayed":60000}`, domain.RejectMissingTrack},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertRejected(t, p, json.RawMessage(tc.raw), 1000, tc.want)
		})
	}
}

// TestAccountDataParserCarriesNoPlaybackContext documents that this export has
// none of the extended fields, so those columns stay NULL rather than being
// invented.
func TestAccountDataParserCarriesNoPlaybackContext(t *testing.T) {
	p := NewAccountDataParser()
	rec := mustParse(t, p, rawRecords(t, "accountdata.json")[0], 1000)
	l := rec.Listen
	if l.Platform != "" || l.ConnCountry != "" || l.ReasonStart != "" || l.ReasonEnd != "" {
		t.Errorf("context fields = %q/%q/%q/%q, want empty",
			l.Platform, l.ConnCountry, l.ReasonStart, l.ReasonEnd)
	}
	if l.Shuffle != nil || l.Skipped != nil || l.Offline != nil || l.Incognito != nil {
		t.Error("nullable booleans should be nil for an account data listen")
	}
}
