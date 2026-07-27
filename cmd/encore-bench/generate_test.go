package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/importer/formats"
)

// generated streams a dataset into memory and reports what it contains. It is
// the fixture every test below shares, because the only thing worth asserting
// about a generator is what the real parser makes of its output.
func generated(t *testing.T, opts generateOptions) ([]byte, datasetStats) {
	t.Helper()
	var buf bytes.Buffer
	stats, err := generateExport(&buf, opts)
	if err != nil {
		t.Fatalf("generateExport: %v", err)
	}
	if stats.Bytes != int64(buf.Len()) {
		t.Fatalf("reported %d bytes, wrote %d", stats.Bytes, buf.Len())
	}
	return buf.Bytes(), stats
}

func TestGenerateIsReproducible(t *testing.T) {
	opts := generateOptions{Records: 500, Format: domain.FormatExtended, Seed: 7, Now: fixedNow()}

	first, _ := generated(t, opts)
	second, _ := generated(t, opts)
	if !bytes.Equal(first, second) {
		t.Fatal("the same seed produced two different exports; a benchmark run must be reproducible")
	}

	opts.Seed = 8
	third, _ := generated(t, opts)
	if bytes.Equal(first, third) {
		t.Fatal("two different seeds produced the same export")
	}
}

func TestGeneratedExportIsValidJSONArray(t *testing.T) {
	body, stats := generated(t, generateOptions{
		Records: 200, Format: domain.FormatExtended, Seed: 1, Now: fixedNow(),
	})

	var records []json.RawMessage
	if err := json.Unmarshal(body, &records); err != nil {
		t.Fatalf("the export is not a JSON array: %v", err)
	}
	if int64(len(records)) != stats.Records {
		t.Fatalf("array holds %d records, stats claim %d", len(records), stats.Records)
	}
}

func TestGeneratedExportsAreDetectedAsTheirFormat(t *testing.T) {
	for _, format := range []domain.ImportFormat{domain.FormatExtended, domain.FormatAccountData} {
		body, _ := generated(t, generateOptions{
			Records: 50, Format: format, Seed: 3, Now: fixedNow(),
		})
		head := body
		if len(head) > formats.SniffBytes {
			head = head[:formats.SniffBytes]
		}
		// Detection has to work from the content alone: the name a generated file
		// is given is a convenience, not the contract.
		if got := formats.DetectByContent(head); got != format {
			t.Fatalf("content detection reported %q for a %q export", got, format)
		}
		if got := formats.Detect(datasetName(format), head); got != format {
			t.Fatalf("detection reported %q for %s", got, datasetName(format))
		}
	}
}

// outcome is what the importer's own parser made of a generated dataset.
type outcome struct {
	records   int
	listens   int
	skips     map[domain.SkipReason]int
	rejects   []string
	firstPlay time.Time
	lastPlay  time.Time
}

// parseGenerated runs a dataset through exactly the reader and parser the
// importer uses, which is the only assertion that means anything: a generator
// whose output the real parser rejects would silently measure the reject path.
func parseGenerated(t *testing.T, body []byte, format domain.ImportFormat) outcome {
	t.Helper()

	parser, ok := formats.ParserFor(format)
	if !ok {
		t.Fatalf("no parser for %q", format)
	}
	reader, err := formats.NewArrayReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}

	user := uuid.New()
	seen := map[string]int64{}
	res := outcome{skips: map[domain.SkipReason]int{}}

	for {
		var raw json.RawMessage
		more, err := reader.Next(&raw)
		if err != nil {
			t.Fatalf("record %d: %v", res.records, err)
		}
		if !more {
			break
		}
		res.records++

		rec, err := parser.Parse(raw, minMsPlayed)
		if err != nil {
			res.rejects = append(res.rejects, err.Error())
			continue
		}
		if rec.Skip != nil {
			res.skips[rec.Skip.Reason]++
			continue
		}

		listen := rec.Listen
		listen.UserID = user
		if err := listen.Validate(time.Now()); err != nil {
			t.Fatalf("record %d does not validate: %v", res.records, err)
		}

		// The generator promises that no two records collide in Encore's exact
		// duplicate key, so that a benchmark measures insertion rather than
		// duplicate suppression. This is that promise being checked.
		key := hex.EncodeToString(listen.DedupeKey())
		if prev, dup := seen[key]; dup {
			t.Fatalf("records %d and %d share a dedupe key (played_at %s)",
				prev, res.records, listen.PlayedAt.UTC())
		}
		seen[key] = int64(res.records)

		if res.listens == 0 {
			res.firstPlay = listen.PlayedAt
		}
		res.lastPlay = listen.PlayedAt
		res.listens++
	}
	return res
}

func TestGeneratedExtendedExportImportsCleanly(t *testing.T) {
	const records = 4000
	body, stats := generated(t, generateOptions{
		Records: records, Format: domain.FormatExtended, Seed: 11, Now: fixedNow(),
	})
	res := parseGenerated(t, body, domain.FormatExtended)

	if res.records != records {
		t.Fatalf("read %d records, generated %d", res.records, records)
	}
	if len(res.rejects) > 0 {
		t.Fatalf("the parser rejected %d records, first: %s", len(res.rejects), res.rejects[0])
	}
	// Every skip path the extended format has must be exercised, or the benchmark
	// would be measuring a pipeline the real one only resembles.
	for _, reason := range []domain.SkipReason{
		domain.SkipNotMusic, domain.SkipLocalFile, domain.SkipBelowMinimum,
	} {
		if res.skips[reason] == 0 {
			t.Errorf("no record was skipped as %q", reason)
		}
	}
	if got, want := res.skips[domain.SkipNotMusic], int(stats.Podcasts); got != want {
		t.Errorf("skipped %d podcasts, generated %d", got, want)
	}
	if got, want := res.skips[domain.SkipLocalFile], int(stats.LocalFiles); got != want {
		t.Errorf("skipped %d local files, generated %d", got, want)
	}
	if res.listens < records/2 {
		t.Fatalf("only %d of %d records became listens; the mix is too far from music",
			res.listens, records)
	}
}

func TestGeneratedAccountDataExportImportsCleanly(t *testing.T) {
	const records = 4000
	body, stats := generated(t, generateOptions{
		Records: records, Format: domain.FormatAccountData, Seed: 12, Now: fixedNow(),
	})
	res := parseGenerated(t, body, domain.FormatAccountData)

	if res.records != records {
		t.Fatalf("read %d records, generated %d", res.records, records)
	}
	if len(res.rejects) > 0 {
		t.Fatalf("the parser rejected %d records, first: %s", len(res.rejects), res.rejects[0])
	}
	if stats.LocalFiles != 0 {
		t.Errorf("the account-data export has no concept of a local file, generated %d", stats.LocalFiles)
	}
	for _, reason := range []domain.SkipReason{domain.SkipNotMusic, domain.SkipBelowMinimum} {
		if res.skips[reason] == 0 {
			t.Errorf("no record was skipped as %q", reason)
		}
	}
}

func TestGeneratedHistorySpansAboutTenYears(t *testing.T) {
	now := fixedNow()
	body, stats := generated(t, generateOptions{
		Records: 3000, Format: domain.FormatExtended, Seed: 5, Now: now,
	})

	if !stats.FirstPlay.Before(stats.LastPlay) {
		t.Fatalf("history runs from %s to %s", stats.FirstPlay, stats.LastPlay)
	}
	span := stats.LastPlay.Sub(stats.FirstPlay)
	if span < 9*365*24*time.Hour || span > 11*365*24*time.Hour {
		t.Fatalf("history spans %s, expected about ten years", span)
	}
	if stats.LastPlay.After(now) {
		t.Fatalf("history ends at %s, after the moment it was generated (%s)", stats.LastPlay, now)
	}
	if stats.FirstPlay.Before(domain.EarliestPlausibleListen) {
		t.Fatalf("history starts at %s, before the earliest plausible listen", stats.FirstPlay)
	}

	// The importer must agree about the window, since it is the one that would
	// reject an implausible timestamp.
	res := parseGenerated(t, body, domain.FormatExtended)
	if res.firstPlay.Before(domain.EarliestPlausibleListen) {
		t.Fatalf("the first parsed listen is at %s", res.firstPlay)
	}
}

func TestGeneratorRefusesAnImpossiblyDenseHistory(t *testing.T) {
	// Far more records than the plausible window holds at the minimum spacing.
	// Refusing is the correct answer: silently packing them closer would make
	// every second record a duplicate and quietly change what is measured.
	_, err := generateExport(bytes.NewBuffer(nil), generateOptions{
		Records: 500_000_000, Format: domain.FormatExtended, Seed: 1, Now: fixedNow(),
	})
	if err == nil {
		t.Fatal("expected a refusal, got a dataset")
	}
}

func TestGeneratedCatalogueIsSkewedAndBounded(t *testing.T) {
	body, stats := generated(t, generateOptions{
		Records: 5000, Format: domain.FormatExtended, Seed: 2, Now: fixedNow(),
	})
	_ = body

	if stats.Tracks > catalogTracks {
		t.Fatalf("used %d tracks, catalogue holds %d", stats.Tracks, catalogTracks)
	}
	if stats.Artists > catalogArtists {
		t.Fatalf("used %d artists, catalogue holds %d", stats.Artists, catalogArtists)
	}
	// A Zipf-like draw over five thousand tracks must not behave like a uniform
	// one: five thousand plays should touch far fewer than five thousand tracks.
	if stats.Tracks > stats.MusicPlays/2 {
		t.Fatalf("%d plays touched %d distinct tracks, which is not a skewed distribution",
			stats.MusicPlays, stats.Tracks)
	}
	if stats.Tracks == 0 || stats.Artists == 0 {
		t.Fatal("the dataset references no catalogue at all")
	}
}

func TestParseFormat(t *testing.T) {
	for input, want := range map[string]domain.ImportFormat{
		"extended":     domain.FormatExtended,
		"account_data": domain.FormatAccountData,
	} {
		got, err := parseFormat(input)
		if err != nil || got != want {
			t.Fatalf("parseFormat(%q) = %q, %v", input, got, err)
		}
	}
	if _, err := parseFormat("csv"); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}

// fixedNow anchors generated histories so that a test asserting on dates does
// not drift with the calendar.
func fixedNow() time.Time {
	return time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
}
