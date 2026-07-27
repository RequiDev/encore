package formats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// rawRecords reads a fixture through the streaming reader, which is how the
// importer sees it: the parsers are only ever handed elements the reader produced.
func rawRecords(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	a, err := NewArrayReader(bytes.NewReader(readFixture(t, name)))
	if err != nil {
		t.Fatalf("NewArrayReader: %v", err)
	}
	var out []json.RawMessage
	for {
		var raw json.RawMessage
		ok, err := a.Next(&raw)
		if err != nil {
			t.Fatalf("Next after %d records: %v", len(out), err)
		}
		if !ok {
			return out
		}
		out = append(out, raw)
	}
}

func mustParse(t *testing.T, p Parser, raw json.RawMessage, minMs int32) Record {
	t.Helper()
	rec, err := p.Parse(raw, minMs)
	if err != nil {
		t.Fatalf("Parse(%s): %v", raw, err)
	}
	return rec
}

// assertRejected checks that a record was rejected for a specific reason, and
// that the error is the *domain.RejectError the importer records diagnostics from.
func assertRejected(t *testing.T, p Parser, raw json.RawMessage, minMs int32, want domain.RejectReason) {
	t.Helper()
	_, err := p.Parse(raw, minMs)
	if err == nil {
		t.Fatalf("Parse(%s) succeeded, want reject %s", raw, want)
	}
	rej, ok := domain.AsReject(err)
	if !ok {
		t.Fatalf("Parse(%s) error = %T, want *domain.RejectError", raw, err)
	}
	if rej.Reason != want {
		t.Errorf("Parse(%s) reason = %s, want %s (detail: %s)", raw, rej.Reason, want, rej.Detail)
	}
	if rej.Detail == "" {
		t.Errorf("Parse(%s) gave no detail; the user has to be told what was wrong", raw)
	}
}

func assertSkipped(t *testing.T, rec Record, want domain.SkipReason) {
	t.Helper()
	if rec.Skip == nil {
		t.Fatalf("record was not skipped, want %s", want)
	}
	if rec.Skip.Reason != want {
		t.Errorf("skip reason = %s, want %s", rec.Skip.Reason, want)
	}
}

// assertListen compares a parsed listen field by field. Times are compared with
// Equal rather than by value because two identical instants may still differ in
// their internal representation.
func assertListen(t *testing.T, got, want domain.Listen) {
	t.Helper()
	if !got.PlayedAt.Equal(want.PlayedAt) {
		t.Errorf("played_at = %s, want %s",
			got.PlayedAt.Format(time.RFC3339Nano), want.PlayedAt.Format(time.RFC3339Nano))
	}
	if got.PlayedAt.Location() != time.UTC {
		t.Errorf("played_at location = %s, want UTC", got.PlayedAt.Location())
	}
	got.PlayedAt, want.PlayedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listen = %+v\n          want %+v", got, want)
	}
}

func TestExtendedParserModernRecords(t *testing.T) {
	p := NewExtendedParser()
	if p.Format() != domain.FormatExtended {
		t.Fatalf("Format() = %s", p.Format())
	}
	records := rawRecords(t, "extended_modern.json")
	if len(records) != 2 {
		t.Fatalf("fixture has %d records, want 2", len(records))
	}

	assertListen(t, mustParse(t, p, records[0], 1000).Listen, domain.Listen{
		// ts is the end of the stream, so the start is ts - ms_played.
		PlayedAt:    time.Date(2024, time.March, 11, 20, 10, 32, 907_000_000, time.UTC),
		Precision:   domain.PrecisionSecond,
		Identity:    domain.TrackIdentityFromID("0Svkvt5I79wficMFgaqEQJ"),
		TrackName:   "Weightless",
		ArtistName:  "Marconi Union",
		MsPlayed:    214093,
		Source:      domain.SourceExtended,
		Platform:    "android",
		ConnCountry: "DE",
		ReasonStart: "clickrow",
		ReasonEnd:   "trackdone",
		Shuffle:     domain.BoolPtr(false),
		Skipped:     domain.BoolPtr(false),
		Offline:     domain.BoolPtr(false),
		Incognito:   domain.BoolPtr(false),
	})

	// A null track URI falls back to the names, with the edition suffix stripped.
	assertListen(t, mustParse(t, p, records[1], 1000).Listen, domain.Listen{
		PlayedAt:    time.Date(2024, time.March, 11, 20, 17, 56, 0, time.UTC),
		Precision:   domain.PrecisionSecond,
		Identity:    domain.TrackIdentityFromNames("Secession", "Nocturne - Remastered 2011"),
		TrackName:   "Nocturne - Remastered 2011",
		ArtistName:  "Secession",
		MsPlayed:    4000,
		Source:      domain.SourceExtended,
		Platform:    "android",
		ConnCountry: "DE",
		ReasonStart: "fwdbtn",
		ReasonEnd:   "fwdbtn",
		Shuffle:     domain.BoolPtr(true),
		Skipped:     domain.BoolPtr(true),
		Offline:     domain.BoolPtr(false),
		Incognito:   domain.BoolPtr(false),
	})
	if id := mustParse(t, p, records[1], 1000).Listen.Identity; id.Artist != "secession" || id.Title != "nocturne" {
		t.Errorf("names identity = %+v, want normalised artist/title", id)
	}
}

// TestExtendedParser2018Record covers the endsong-era export: a username and a
// decrypted user agent that Encore does not store, and booleans that are either
// null or, in the case of `skipped`, a reason string.
func TestExtendedParser2018Record(t *testing.T) {
	p := NewExtendedParser()
	records := rawRecords(t, "extended_2018.json")
	rec := mustParse(t, p, records[0], 1000)

	assertListen(t, rec.Listen, domain.Listen{
		PlayedAt:    time.Date(2018, time.November, 2, 9, 9, 37, 0, time.UTC),
		Precision:   domain.PrecisionSecond,
		Identity:    domain.TrackIdentityFromID("67Hna13dNDkZvBpTXRIaOJ"),
		TrackName:   "Teardrop",
		ArtistName:  "Massive Attack",
		MsPlayed:    187000,
		Source:      domain.SourceExtended,
		Platform:    "OS X 10.13.6 [x86 8]",
		ConnCountry: "GB",
		ReasonStart: "trackdone",
		ReasonEnd:   "trackdone",
	})
	// Every nullable boolean must stay unknown; false would be a claim the export
	// never made.
	for name, got := range map[string]*bool{
		"shuffle":   rec.Listen.Shuffle,
		"skipped":   rec.Listen.Skipped,
		"offline":   rec.Listen.Offline,
		"incognito": rec.Listen.Incognito,
	} {
		if got != nil {
			t.Errorf("%s = %v, want nil", name, *got)
		}
	}
}

func TestExtendedParser2024Records(t *testing.T) {
	p := NewExtendedParser()
	records := rawRecords(t, "extended_2024.json")
	if len(records) != 3 {
		t.Fatalf("fixture has %d records, want 3", len(records))
	}

	assertSkipped(t, mustParse(t, p, records[0], 1000), domain.SkipNotMusic)
	assertSkipped(t, mustParse(t, p, records[1], 1000), domain.SkipNotMusic)

	assertListen(t, mustParse(t, p, records[2], 1000).Listen, domain.Listen{
		PlayedAt:    time.Date(2024, time.November, 2, 19, 58, 45, 600_000_000, time.UTC),
		Precision:   domain.PrecisionSecond,
		Identity:    domain.TrackIdentityFromID("6Ha4aHVHmMlyTLTZgQtXhE"),
		TrackName:   "Svefn-g-englar",
		ArtistName:  "Sigur Rós",
		MsPlayed:    253400,
		Source:      domain.SourceExtended,
		Platform:    "web_player chrome",
		ConnCountry: "NL",
		ReasonStart: "playbtn",
		ReasonEnd:   "trackdone",
		Shuffle:     domain.BoolPtr(true),
		Skipped:     domain.BoolPtr(false),
		Offline:     domain.BoolPtr(false),
		Incognito:   domain.BoolPtr(true),
	})
}

// TestExtendedParserTolerance walks the fixture of everything real exports have
// been caught doing to the types of their own fields.
func TestExtendedParserTolerance(t *testing.T) {
	p := NewExtendedParser()
	records := rawRecords(t, "extended_tolerant.json")
	if len(records) != 8 {
		t.Fatalf("fixture has %d records, want 8", len(records))
	}

	t.Run("string ms_played and string booleans", func(t *testing.T) {
		assertListen(t, mustParse(t, p, records[0], 1000).Listen, domain.Listen{
			PlayedAt:    time.Date(2021, time.June, 1, 9, 57, 56, 544_000_000, time.UTC),
			Precision:   domain.PrecisionSecond,
			Identity:    domain.TrackIdentityFromID("6Ha4aHVHmMlyTLTZgQtXhE"),
			TrackName:   "Hoppipolla",
			ArtistName:  "Sigur Rós",
			MsPlayed:    123456,
			Source:      domain.SourceExtended,
			Platform:    "windows",
			ConnCountry: "IS",
			ReasonStart: "fwdbtn",
			ReasonEnd:   "endplay",
			Shuffle:     domain.BoolPtr(true),
			Skipped:     domain.BoolPtr(false),
			Offline:     domain.BoolPtr(true),
			Incognito:   domain.BoolPtr(false),
		})
	})

	t.Run("local file", func(t *testing.T) {
		assertSkipped(t, mustParse(t, p, records[1], 1000), domain.SkipLocalFile)
	})

	t.Run("names only fallback", func(t *testing.T) {
		rec := mustParse(t, p, records[2], 1000)
		if rec.Skip != nil {
			t.Fatalf("record was skipped: %v", rec.Skip)
		}
		if rec.Listen.Identity.IsResolved() {
			t.Fatalf("identity = %+v, want names only", rec.Listen.Identity)
		}
		want := domain.TrackIdentityFromNames("Nameless Band", "Unresolved Song")
		if rec.Listen.Identity != want {
			t.Errorf("identity = %+v, want %+v", rec.Listen.Identity, want)
		}
	})

	t.Run("below minimum", func(t *testing.T) {
		assertSkipped(t, mustParse(t, p, records[3], 1000), domain.SkipBelowMinimum)
		// The same record is a real listen when the instance keeps everything.
		if rec := mustParse(t, p, records[3], 0); rec.Skip != nil {
			t.Errorf("with no minimum the record was still skipped: %v", rec.Skip)
		}
	})

	t.Run("timestamps", func(t *testing.T) {
		assertRejected(t, p, records[4], 1000, domain.RejectMissingTimestamp)
		assertRejected(t, p, records[5], 1000, domain.RejectInvalidTimestamp)
	})

	t.Run("no usable identity", func(t *testing.T) {
		assertRejected(t, p, records[6], 1000, domain.RejectMissingTrack)
	})

	t.Run("open.spotify.com link and zoneless timestamp", func(t *testing.T) {
		rec := mustParse(t, p, records[7], 1000)
		if got := rec.Listen.Identity.TrackID; got != "1301WleyT98MSxVHPZCA6M" {
			t.Errorf("track id = %q", got)
		}
		want := time.Date(2021, time.June, 1, 12, 29, 0, 0, time.UTC)
		if !rec.Listen.PlayedAt.Equal(want) {
			t.Errorf("played_at = %s, want %s", rec.Listen.PlayedAt, want)
		}
		if rec.Listen.MsPlayed != 60000 {
			t.Errorf("ms_played = %d, want 60000", rec.Listen.MsPlayed)
		}
	})
}

// TestExtendedParserMalformedRecords is the "one bad record must not cost the
// rest of the file" case: the stream reads to the end and the good records on
// either side of the damage still parse.
func TestExtendedParserMalformedRecords(t *testing.T) {
	p := NewExtendedParser()
	records := rawRecords(t, "extended_malformed.json")
	if len(records) != 7 {
		t.Fatalf("fixture has %d records, want 7", len(records))
	}

	if rec := mustParse(t, p, records[0], 1000); rec.Listen.Identity.TrackID != "1301WleyT98MSxVHPZCA6M" {
		t.Errorf("first record = %+v", rec.Listen)
	}
	assertRejected(t, p, records[1], 1000, domain.RejectMalformedRecord)
	assertRejected(t, p, records[2], 1000, domain.RejectMalformedRecord)
	assertRejected(t, p, records[3], 1000, domain.RejectMalformedRecord)
	assertRejected(t, p, records[4], 1000, domain.RejectUnknownShape)
	assertRejected(t, p, records[5], 1000, domain.RejectInvalidMsPlayed)

	last := mustParse(t, p, records[6], 1000)
	if last.Skip != nil || last.Listen.Identity.Artist != "band" {
		t.Errorf("last record = %+v (skip %v)", last.Listen, last.Skip)
	}
}

func TestExtendedParserRejectsImplausibleDurations(t *testing.T) {
	p := NewExtendedParser()
	base := `{"ts":"2023-01-01T00:00:00Z","master_metadata_track_name":"T",` +
		`"master_metadata_album_artist_name":"A","ms_played":%s}`
	for _, ms := range []string{"-1", "86400001", `"nonsense"`, `{"value":1}`} {
		assertRejected(t, p, json.RawMessage(fmt.Sprintf(base, ms)), 1000, domain.RejectInvalidMsPlayed)
	}
}

func TestParserFor(t *testing.T) {
	for format, want := range map[domain.ImportFormat]bool{
		domain.FormatExtended:    true,
		domain.FormatAccountData: true,
		domain.FormatUnknown:     false,
	} {
		p, ok := ParserFor(format)
		if ok != want {
			t.Fatalf("ParserFor(%s) ok = %v, want %v", format, ok, want)
		}
		if ok && p.Format() != format {
			t.Errorf("ParserFor(%s).Format() = %s", format, p.Format())
		}
	}
}
