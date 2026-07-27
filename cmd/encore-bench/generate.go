package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// Shape of a generated history.
const (
	// defaultSpanDays is roughly ten years, which is the history length the
	// benchmark is meant to represent: long enough that the statistics rollups
	// see thousands of distinct local days.
	defaultSpanDays = 3653

	// minPlayGapSeconds is the smallest interval the generator puts between two
	// consecutive plays.
	//
	// It exists to keep the dataset free of accidental duplicates. Encore's exact
	// duplicate key buckets played_at to the minute, and the account-data export
	// truncates its end time to the minute as well, which can move a play's
	// computed start back by up to 59 seconds. Two starts at least 121 seconds
	// apart therefore always land in different buckets whatever the truncation
	// does, so every record the generator emits is a genuinely new listen and the
	// benchmark measures insertion rather than duplicate suppression.
	minPlayGapSeconds = 121

	// minMsPlayed mirrors the default of ENCORE_IMPORT_MIN_MS. The generator uses
	// it only to report how many plays it deliberately put below the importer's
	// floor; the importer applies whatever value it is configured with.
	minMsPlayed = 1000
)

// Mix of record kinds. The non-music shares are small but non-zero on purpose:
// they are what drives the importer's skip paths, which would otherwise never
// run in a benchmark and would therefore never be measured.
const (
	podcastShare      = 0.025
	localFileShare    = 0.015
	belowMinimumShare = 0.03
	skippedEarlyShare = 0.15
	partialPlayShare  = 0.07
)

// generateOptions describe one synthetic export.
type generateOptions struct {
	Records int
	Format  domain.ImportFormat
	Seed    uint64
	// Now anchors the end of the generated history. It is injectable so that a
	// test can assert on exact timestamps.
	Now time.Time
}

// datasetStats describe what was generated. Every figure is counted while
// streaming; nothing here comes from a second pass over the file.
type datasetStats struct {
	Path       string    `json:"path,omitempty"`
	Format     string    `json:"format"`
	Records    int64     `json:"records"`
	Bytes      int64     `json:"bytes"`
	MusicPlays int64     `json:"music_plays"`
	Podcasts   int64     `json:"podcast_plays"`
	LocalFiles int64     `json:"local_file_plays"`
	ShortPlays int64     `json:"below_minimum_plays"`
	Tracks     int64     `json:"distinct_tracks"`
	Artists    int64     `json:"distinct_artists"`
	FirstPlay  time.Time `json:"first_played_at"`
	LastPlay   time.Time `json:"last_played_at"`
}

// extendedRecord is one element of a generated extended streaming history.
//
// Every field the real export carries is emitted, including the ones Encore
// deliberately discards, so that the parser's tolerance of them is exercised
// rather than assumed. A nil pointer is written as JSON null, which is what a
// real export does for a podcast's music columns and for playback flags Spotify
// did not record.
type extendedRecord struct {
	TS          string  `json:"ts"`
	Platform    string  `json:"platform"`
	MsPlayed    int32   `json:"ms_played"`
	ConnCountry string  `json:"conn_country"`
	IPAddr      *string `json:"ip_addr"`

	TrackName  *string `json:"master_metadata_track_name"`
	ArtistName *string `json:"master_metadata_album_artist_name"`
	AlbumName  *string `json:"master_metadata_album_album_name"`
	TrackURI   *string `json:"spotify_track_uri"`

	EpisodeName     *string `json:"episode_name"`
	EpisodeShowName *string `json:"episode_show_name"`
	EpisodeURI      *string `json:"spotify_episode_uri"`

	ReasonStart      string `json:"reason_start"`
	ReasonEnd        string `json:"reason_end"`
	Shuffle          *bool  `json:"shuffle"`
	Skipped          *bool  `json:"skipped"`
	Offline          *bool  `json:"offline"`
	OfflineTimestamp *int64 `json:"offline_timestamp"`
	IncognitoMode    *bool  `json:"incognito_mode"`
}

// accountDataRecord is one element of a generated account-data history. The
// podcast variant of that export swaps the music columns for a show and an
// episode while keeping the same envelope.
type accountDataRecord struct {
	EndTime     string  `json:"endTime"`
	ArtistName  *string `json:"artistName"`
	TrackName   *string `json:"trackName"`
	MsPlayed    int32   `json:"msPlayed"`
	PodcastName *string `json:"podcastName,omitempty"`
	EpisodeName *string `json:"episodeName,omitempty"`
}

var (
	platforms    = []string{"android", "ios", "windows", "osx", "web_player", "linux", "cast"}
	countries    = []string{"GB", "DE", "US", "SE", "NL", "FR", "IE", "AU"}
	reasonStarts = []string{"trackdone", "clickrow", "fwdbtn", "playbtn", "backbtn", "appload", "remote"}
	reasonEnds   = []string{"trackdone", "fwdbtn", "endplay", "logout", "backbtn", "unexpected-exit"}
)

// generateExport streams a synthetic export to w and reports what it wrote.
//
// The array is written element by element through an encoding/json Encoder and
// is never assembled in memory: generating a million records costs the same
// memory as generating ten, which is the least a tool whose job is to prove the
// importer does the same can do.
func generateExport(w io.Writer, opts generateOptions) (datasetStats, error) {
	if opts.Records <= 0 {
		return datasetStats{}, errors.New("--records must be at least 1")
	}
	if opts.Format != domain.FormatExtended && opts.Format != domain.FormatAccountData {
		return datasetStats{}, fmt.Errorf("cannot generate the %q format", opts.Format)
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	// Two independent streams from one seed. The catalogue must not shift when
	// the record count changes, so that a hundred-record smoke test and a
	// million-record benchmark still describe the same listener.
	catalogue := newSynthCatalog(rand.New(rand.NewPCG(opts.Seed, 0x9E3779B97F4A7C15)))
	rng := rand.New(rand.NewPCG(opts.Seed, 0xBF58476D1CE4E5B9))

	clock, err := newPlayClock(opts.Records, opts.Now, rng)
	if err != nil {
		return datasetStats{}, err
	}

	counted := &countingWriter{w: w}
	buffered := bufio.NewWriterSize(counted, 1<<20)
	enc := json.NewEncoder(buffered)
	// Spotify does not escape HTML in its exports, and neither should a file that
	// claims to look like one.
	enc.SetEscapeHTML(false)

	stats := datasetStats{Format: string(opts.Format), Records: int64(opts.Records)}
	usedTracks := make(map[int]struct{}, catalogTracks)
	usedArtists := make(map[string]struct{}, catalogArtists)

	if _, err := buffered.WriteString("[\n"); err != nil {
		return stats, fmt.Errorf("write export: %w", err)
	}

	for i := range opts.Records {
		if i > 0 {
			if _, err := buffered.WriteString(","); err != nil {
				return stats, fmt.Errorf("write export: %w", err)
			}
		}

		started := clock.next()
		if i == 0 {
			stats.FirstPlay = started
		}
		stats.LastPlay = started

		var (
			ms      int32
			payload any
		)
		switch kind := rng.Float64(); {
		case kind < podcastShare:
			ms, payload = podcastPlay(rng, catalogue, opts.Format, started)
			stats.Podcasts++
		case kind < podcastShare+localFileShare && opts.Format == domain.FormatExtended:
			ms, payload = localFilePlay(rng, catalogue, started)
			stats.LocalFiles++
		default:
			index := catalogue.pickTrack()
			track := catalogue.tracks[index]
			ms, payload = musicPlay(rng, track, opts.Format, started)
			stats.MusicPlays++
			usedTracks[index] = struct{}{}
			usedArtists[track.Artist] = struct{}{}
		}
		if ms < minMsPlayed {
			stats.ShortPlays++
		}

		if err := enc.Encode(payload); err != nil {
			return stats, fmt.Errorf("encode record %d: %w", i, err)
		}
	}

	if _, err := buffered.WriteString("]\n"); err != nil {
		return stats, fmt.Errorf("write export: %w", err)
	}
	if err := buffered.Flush(); err != nil {
		return stats, fmt.Errorf("flush export: %w", err)
	}

	stats.Tracks = int64(len(usedTracks))
	stats.Artists = int64(len(usedArtists))
	stats.Bytes = counted.n
	return stats, nil
}

// playClock produces the strictly increasing start times of a generated history.
//
// The timeline is a grid of equal slots with a jittered position inside each
// slot. That is a deliberate choice over the exponential inter-arrival process a
// real listener produces, and it buys two properties a benchmark needs more than
// it needs burstiness:
//
//   - consecutive starts are always at least minPlayGapSeconds apart, so no
//     record can collide with its neighbour in Encore's minute-wide duplicate
//     bucket and every emitted record is a genuine insert;
//   - the last play is guaranteed to fall inside the history's window rather
//     than merely expected to, so no run can fail because a long tail of random
//     gaps wandered past the present.
//
// Both are correctness properties of the dataset. A more faithful hour-of-day
// curve, by contrast, would not change a single line of the code being measured.
type playClock struct {
	rng    *rand.Rand
	origin time.Time
	slot   time.Duration
	jitter time.Duration
	index  int
}

func newPlayClock(records int, now time.Time, rng *rand.Rand) (*playClock, error) {
	minGap := time.Duration(minPlayGapSeconds) * time.Second

	// Ending a day before "now" keeps every timestamp comfortably inside
	// domain.FutureSkew even when the machine generating the file and the
	// database importing it disagree about the time.
	end := now.UTC().Add(-24 * time.Hour).Truncate(time.Second)
	// A listen from before domain.EarliestPlausibleListen is rejected as corrupt,
	// so the history can never start earlier than that however many records are
	// asked for. A year of margin keeps the dataset clear of the boundary itself.
	earliest := domain.EarliestPlausibleListen.AddDate(1, 0, 0)

	// The plausible window is finite, so the record count is too. Refusing is the
	// only honest answer: packing plays closer together than minGap would make a
	// share of them duplicates of their neighbours and quietly change what the
	// benchmark measures. Checking this first also keeps the arithmetic below out
	// of the range where a duration overflows.
	available := end.Sub(earliest)
	if maxRecords := int64(available / minGap); int64(records) > maxRecords {
		return nil, fmt.Errorf(
			"%s records cannot be spread over %s while keeping plays %s apart; generate at most %s",
			humanInt(int64(records)), humanDuration(available), minGap, humanInt(maxRecords))
	}

	span := time.Duration(defaultSpanDays) * 24 * time.Hour
	if want := time.Duration(records) * 2 * minGap; span < want {
		// More records than ten years holds at a comfortable spacing: stretch the
		// history rather than crowd the plays.
		span = want
	}
	if span > available {
		span = available
	}
	slot := span / time.Duration(records)

	return &playClock{
		rng:    rng,
		origin: end.Add(-span),
		slot:   slot,
		jitter: slot - minGap,
	}, nil
}

// next returns the start of the next play.
//
// The offset within a slot is squared, which biases it towards the beginning of
// the slot and gives the gap sequence some variety instead of a metronome.
func (c *playClock) next() time.Time {
	r := c.rng.Float64()
	offset := time.Duration(float64(c.jitter) * r * r)
	t := c.origin.Add(time.Duration(c.index)*c.slot + offset)
	c.index++
	return t.Truncate(time.Second)
}

// musicPlay builds one ordinary listen in the requested format.
func musicPlay(rng *rand.Rand, track synthTrack, format domain.ImportFormat, started time.Time) (int32, any) {
	ms := playDuration(rng, track.DurationMs)
	if format == domain.FormatAccountData {
		return ms, &accountDataRecord{
			EndTime:    accountDataTime(started, ms),
			ArtistName: &track.Artist,
			TrackName:  &track.Title,
			MsPlayed:   ms,
		}
	}
	uri := "spotify:track:" + track.ID
	album := albumTitleFor(rng, track.Album)
	rec := newExtendedRecord(rng, started, ms)
	rec.TrackName = &track.Title
	rec.ArtistName = &track.Artist
	rec.AlbumName = &album
	rec.TrackURI = &uri
	return ms, rec
}

// localFilePlay builds a play of a file from the listener's own disk, which the
// importer must skip rather than reject: it has no catalogue identity and never
// will. Only the extended export records these.
func localFilePlay(rng *rand.Rand, cat *synthCatalog, started time.Time) (int32, any) {
	track := cat.tracks[cat.pickTrack()]
	ms := playDuration(rng, track.DurationMs)
	uri := fmt.Sprintf("spotify:local:%s:%s:%s:%d",
		uriToken(track.Artist), uriToken(track.Album), uriToken(track.Title), track.DurationMs/1000)
	rec := newExtendedRecord(rng, started, ms)
	rec.TrackName = &track.Title
	rec.ArtistName = &track.Artist
	rec.AlbumName = &track.Album
	rec.TrackURI = &uri
	return ms, rec
}

// podcastPlay builds a podcast episode, which is valid data that is simply not
// music: the importer counts it as skipped rather than rejecting it.
func podcastPlay(rng *rand.Rand, cat *synthCatalog, format domain.ImportFormat, started time.Time) (int32, any) {
	show, episode := cat.pickEpisode()
	// Episodes run far longer than tracks, and are abandoned far more often.
	ms := int32(rng.IntN(45 * 60 * 1000))
	if format == domain.FormatAccountData {
		return ms, &accountDataRecord{
			EndTime:     accountDataTime(started, ms),
			MsPlayed:    ms,
			PodcastName: &show.Name,
			EpisodeName: &episode.Title,
		}
	}
	uri := "spotify:episode:" + episode.ID
	rec := newExtendedRecord(rng, started, ms)
	rec.EpisodeName = &episode.Title
	rec.EpisodeShowName = &show.Name
	rec.EpisodeURI = &uri
	return ms, rec
}

// newExtendedRecord fills in the playback context every extended record carries.
func newExtendedRecord(rng *rand.Rand, started time.Time, ms int32) *extendedRecord {
	rec := &extendedRecord{
		TS:            started.Add(time.Duration(ms) * time.Millisecond).UTC().Format("2006-01-02T15:04:05Z"),
		Platform:      platforms[rng.IntN(len(platforms))],
		MsPlayed:      ms,
		ConnCountry:   countries[rng.IntN(len(countries))],
		ReasonStart:   reasonStarts[rng.IntN(len(reasonStarts))],
		ReasonEnd:     reasonEnds[rng.IntN(len(reasonEnds))],
		Shuffle:       maybeBool(rng, 0.35),
		Skipped:       maybeBool(rng, 0.2),
		Offline:       maybeBool(rng, 0.08),
		IncognitoMode: maybeBool(rng, 0.01),
	}
	if rng.IntN(4) == 0 {
		// RFC 5737's documentation range. A benchmark must never write anything
		// that could be mistaken for a real address into a file someone keeps.
		addr := fmt.Sprintf("203.0.113.%d", rng.IntN(254)+1)
		rec.IPAddr = &addr
	}
	if rec.Offline != nil && *rec.Offline {
		ts := started.Unix()
		rec.OfflineTimestamp = &ts
	}
	return rec
}

// maybeBool returns a pointer to a boolean that is true with probability p, and
// nil one time in ten. A null flag is not the same fact as a false one and the
// importer stores the difference, so the generator has to produce both.
func maybeBool(rng *rand.Rand, p float64) *bool {
	if rng.Float64() < 0.1 {
		return nil
	}
	v := rng.Float64() < p
	return &v
}

// playDuration draws a plausible ms_played for a track of the given length.
func playDuration(rng *rand.Rand, trackMs int32) int32 {
	switch r := rng.Float64(); {
	case r < belowMinimumShare:
		// A stray tap on the wrong row: below any sensible minimum, and therefore
		// skipped by the importer.
		return int32(rng.IntN(900) + 50)
	case r < belowMinimumShare+skippedEarlyShare:
		// Heard the intro, moved on.
		return int32(rng.IntN(44_000) + 1_000)
	case r < belowMinimumShare+skippedEarlyShare+partialPlayShare:
		return trackMs * int32(30+rng.IntN(60)) / 100
	default:
		// Played out, give or take the couple of seconds Spotify's own figures
		// wobble by.
		jitter := int32(rng.IntN(4000)) - 2000
		if trackMs+jitter < minMsPlayed {
			return trackMs
		}
		return trackMs + jitter
	}
}

// accountDataTime renders a stream end time the way the account-data export
// does: minute precision, UTC, no zone marker.
func accountDataTime(started time.Time, ms int32) string {
	return started.Add(time.Duration(ms) * time.Millisecond).UTC().Format("2006-01-02 15:04")
}

// uriToken makes a name safe to embed in a spotify:local: URI, whose fields are
// colon separated and plus escaped.
func uriToken(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r == ':' || r == ' ' {
			out[i] = '+'
		}
	}
	return string(out)
}

// countingWriter counts the bytes written through it, which is how the report
// knows the dataset size without stat-ing anything.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// --- the generate command --------------------------------------------------

// runGenerate implements `encore-bench generate`.
func runGenerate(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	records := fs.Int("records", 1_000_000, "number of records to generate")
	format := fs.String("format", string(domain.FormatExtended), "extended | account_data")
	out := fs.String("out", "", "path to write the export to")
	seed := fs.Uint64("seed", 1, "seed for the deterministic generator")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	parsed, err := parseFormat(*format)
	if err != nil {
		return err
	}

	started := time.Now()
	stats, err := writeExportFile(*out, generateOptions{Records: *records, Format: parsed, Seed: *seed})
	if err != nil {
		return err
	}
	elapsed := time.Since(started)

	fmt.Printf("wrote %s %s records (%s) to %s in %s (%s records/s)\n",
		humanInt(stats.Records), stats.Format, humanBytes(stats.Bytes), stats.Path,
		humanDuration(elapsed), humanInt(int64(perSecond(stats.Records, elapsed))))
	fmt.Printf("covering %s to %s: %s music, %s podcast, %s local-file plays over %s tracks by %s artists\n",
		stats.FirstPlay.Format(time.DateOnly), stats.LastPlay.Format(time.DateOnly),
		humanInt(stats.MusicPlays), humanInt(stats.Podcasts), humanInt(stats.LocalFiles),
		humanInt(stats.Tracks), humanInt(stats.Artists))
	return nil
}

// writeExportFile generates straight to a file, creating its directory.
func writeExportFile(path string, opts generateOptions) (datasetStats, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return datasetStats{}, fmt.Errorf("create output directory: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return datasetStats{}, fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	stats, err := generateExport(f, opts)
	if err != nil {
		return stats, err
	}
	if err := f.Sync(); err != nil {
		return stats, fmt.Errorf("flush %s: %w", path, err)
	}
	stats.Path = path
	return stats, nil
}

// parseFormat maps the command line onto a domain format.
func parseFormat(s string) (domain.ImportFormat, error) {
	switch domain.ImportFormat(s) {
	case domain.FormatExtended:
		return domain.FormatExtended, nil
	case domain.FormatAccountData:
		return domain.FormatAccountData, nil
	default:
		return domain.FormatUnknown, fmt.Errorf("--format must be extended or account_data, got %q", s)
	}
}
