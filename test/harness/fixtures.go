package harness

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FixtureStart is where every generated history begins. It is well inside
// domain.EarliestPlausibleListen and far enough in the past that even a
// million-record fixture ends before the present, so nothing is rejected for
// being in the future.
var FixtureStart = time.Date(2015, time.January, 1, 8, 0, 0, 0, time.UTC)

// GenOptions controls a synthetic export.
type GenOptions struct {
	// Records is how many entries to write.
	Records int
	// Artists and Tracks bound the catalogue the plays are drawn from. Real
	// histories are heavily skewed, so the generator uses a Zipf-like draw over
	// this many tracks rather than a uniform one.
	Artists int
	Tracks  int
	// Seed makes a run reproducible. Two runs with the same seed produce
	// byte-identical files, which is what lets a re-import test assert zero.
	Seed uint64
	// PodcastEvery, if non-zero, makes every Nth record a podcast episode so the
	// skip path is exercised.
	PodcastEvery int
	// LocalFileEvery, if non-zero, makes every Nth record a spotify:local: URI.
	LocalFileEvery int
	// ShortPlayEvery, if non-zero, makes every Nth record shorter than the
	// default minimum play length.
	ShortPlayEvery int
	// MalformedEvery, if non-zero, makes every Nth record unparseable.
	MalformedEvery int
	// Start overrides FixtureStart.
	Start time.Time
}

func (o GenOptions) withDefaults() GenOptions {
	if o.Records <= 0 {
		o.Records = 100
	}
	if o.Artists <= 0 {
		o.Artists = max(1, o.Records/40)
	}
	if o.Tracks <= 0 {
		o.Tracks = max(1, o.Records/8)
	}
	if o.Start.IsZero() {
		o.Start = FixtureStart
	}
	return o
}

// TrackID renders the deterministic Spotify-shaped id for track n. Spotify ids
// are base62 and 22 characters; these are valid by that shape so they exercise
// the same URI parsing a real export would.
func TrackID(n int) string { return fmt.Sprintf("t%021d", n) }

// ArtistName and TrackName give the names an account-data export would carry for
// the same play, so the two formats can be generated to overlap exactly.
func ArtistName(n int) string { return fmt.Sprintf("Artist %04d", n) }
func TrackName(n int) string  { return fmt.Sprintf("Track %06d", n) }
func AlbumName(n int) string  { return fmt.Sprintf("Album %05d", n) }

// play is one generated listening event, before it is rendered into either
// export format.
type play struct {
	index    int
	at       time.Time // stream END time, which is what both formats record
	trackNum int
	msPlayed int32
	podcast  bool
	local    bool
	bad      bool
}

// generate walks the synthetic history, calling fn for each play. It never holds
// more than one at a time, so a million-record fixture costs no more memory than
// a hundred-record one.
func generate(o GenOptions, fn func(play)) {
	o = o.withDefaults()
	rng := rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b9))
	at := o.Start

	for i := range o.Records {
		// Plays are two to eight minutes apart, so consecutive events land in
		// distinct minute buckets and are not mistaken for duplicates.
		at = at.Add(time.Duration(120+rng.IntN(360)) * time.Second)

		// A skewed draw: the low-numbered tracks are played far more often, the
		// way a real listening history looks.
		t := int(float64(o.Tracks) * rng.Float64() * rng.Float64())
		if t >= o.Tracks {
			t = o.Tracks - 1
		}

		p := play{index: i, at: at, trackNum: t, msPlayed: int32(90_000 + rng.IntN(210_000))}
		if o.PodcastEvery > 0 && i%o.PodcastEvery == 0 {
			p.podcast = true
		}
		if o.LocalFileEvery > 0 && i%o.LocalFileEvery == 0 {
			p.local = true
		}
		if o.ShortPlayEvery > 0 && i%o.ShortPlayEvery == 0 {
			p.msPlayed = int32(rng.IntN(900))
		}
		if o.MalformedEvery > 0 && i%o.MalformedEvery == 0 {
			p.bad = true
		}
		fn(p)
	}
}

// artistFor maps a track onto its artist, deterministically.
func artistFor(trackNum, artists int) int {
	if artists <= 0 {
		return 0
	}
	return trackNum % artists
}

// WriteExtendedExport writes a Streaming_History_Audio-style file and returns
// how many records it contains.
//
// The records are written straight to disk through a streaming encoder, so
// generating a large fixture does not need a large amount of memory either.
func WriteExtendedExport(t *testing.T, path string, o GenOptions) int {
	t.Helper()
	o = o.withDefaults()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture %s: %v", path, err)
	}
	defer f.Close()

	w := newArrayWriter(t, f)
	generate(o, func(p play) {
		if p.bad {
			// Valid JSON, wrong shape: ms_played is not a number and the
			// timestamp is nonsense. The importer must reject the record and
			// carry on, not fail the file.
			w.write(map[string]any{
				"ts":        "not-a-timestamp",
				"ms_played": []any{1, 2, 3},
			})
			return
		}
		rec := map[string]any{
			"ts":                p.at.Format(time.RFC3339),
			"platform":          "android",
			"ms_played":         p.msPlayed,
			"conn_country":      "DE",
			"ip_addr":           "10.0.0.1",
			"reason_start":      "trackdone",
			"reason_end":        "trackdone",
			"shuffle":           false,
			"skipped":           false,
			"offline":           false,
			"offline_timestamp": nil,
			"incognito_mode":    false,
		}
		switch {
		case p.podcast:
			rec["episode_name"] = fmt.Sprintf("Episode %d", p.index)
			rec["episode_show_name"] = "A Podcast"
			rec["spotify_episode_uri"] = fmt.Sprintf("spotify:episode:e%020d", p.index)
			rec["master_metadata_track_name"] = nil
			rec["master_metadata_album_artist_name"] = nil
			rec["master_metadata_album_album_name"] = nil
			rec["spotify_track_uri"] = nil
		case p.local:
			rec["master_metadata_track_name"] = TrackName(p.trackNum)
			rec["master_metadata_album_artist_name"] = ArtistName(artistFor(p.trackNum, o.Artists))
			rec["master_metadata_album_album_name"] = AlbumName(p.trackNum / 10)
			rec["spotify_track_uri"] = fmt.Sprintf("spotify:local:%s:%s:%s:%d",
				ArtistName(artistFor(p.trackNum, o.Artists)), AlbumName(p.trackNum/10), TrackName(p.trackNum), p.msPlayed/1000)
		default:
			rec["master_metadata_track_name"] = TrackName(p.trackNum)
			rec["master_metadata_album_artist_name"] = ArtistName(artistFor(p.trackNum, o.Artists))
			rec["master_metadata_album_album_name"] = AlbumName(p.trackNum / 10)
			rec["spotify_track_uri"] = "spotify:track:" + TrackID(p.trackNum)
		}
		w.write(rec)
	})
	w.close()
	return o.Records
}

// WriteAccountDataExport writes a StreamingHistory-style file covering the same
// plays as WriteExtendedExport would for the same options, so the two can be
// imported together to exercise cross-format duplicate suppression.
func WriteAccountDataExport(t *testing.T, path string, o GenOptions) int {
	t.Helper()
	o = o.withDefaults()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture %s: %v", path, err)
	}
	defer f.Close()

	w := newArrayWriter(t, f)
	generate(o, func(p play) {
		if p.bad {
			w.write(map[string]any{"endTime": "", "msPlayed": "lots"})
			return
		}
		if p.podcast {
			w.write(map[string]any{
				"endTime":     p.at.Format("2006-01-02 15:04"),
				"podcastName": "A Podcast",
				"episodeName": fmt.Sprintf("Episode %d", p.index),
				"msPlayed":    p.msPlayed,
			})
			return
		}
		// The account-data format truncates the stream end time to the minute,
		// which is precisely why cross-source duplicate suppression needs a
		// window rather than an exact key.
		w.write(map[string]any{
			"endTime":    p.at.Format("2006-01-02 15:04"),
			"artistName": ArtistName(artistFor(p.trackNum, o.Artists)),
			"trackName":  TrackName(p.trackNum),
			"msPlayed":   p.msPlayed,
		})
	})
	w.close()
	return o.Records
}

// WriteZipExport packages several generated files into one archive, the way
// Spotify delivers an export, including a few files Encore must recognise and
// skip rather than choke on.
func WriteZipExport(t *testing.T, path string, o GenOptions, parts int) int {
	t.Helper()
	o = o.withDefaults()
	if parts < 1 {
		parts = 1
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip fixture: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	perPart := o.Records / parts
	written := 0

	for i := range parts {
		n := perPart
		if i == parts-1 {
			n = o.Records - written
		}
		name := fmt.Sprintf("Spotify Extended Streaming History/Streaming_History_Audio_2015-2017_%d.json", i)
		entry, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		part := o
		part.Records = n
		part.Seed = o.Seed + uint64(i)*1000
		part.Start = o.Start.AddDate(i, 0, 0)

		w := newArrayWriter(t, entry)
		generate(part, func(p play) {
			w.write(map[string]any{
				"ts":                                p.at.Format(time.RFC3339),
				"platform":                          "osx",
				"ms_played":                         p.msPlayed,
				"conn_country":                      "GB",
				"master_metadata_track_name":        TrackName(p.trackNum),
				"master_metadata_album_artist_name": ArtistName(artistFor(p.trackNum, part.Artists)),
				"master_metadata_album_album_name":  AlbumName(p.trackNum / 10),
				"spotify_track_uri":                 "spotify:track:" + TrackID(p.trackNum),
				"reason_start":                      "clickrow",
				"reason_end":                        "trackdone",
				"shuffle":                           true,
				"skipped":                           nil,
				"offline":                           false,
				"incognito_mode":                    false,
			})
		})
		w.close()
		written += n
	}

	// The rest of a real export: Encore must skip these, not fail on them.
	for name, body := range map[string]string{
		"Spotify Account Data/Playlist1.json":     `{"playlists":[{"name":"Chill"}]}`,
		"Spotify Account Data/SearchQueries.json": `[{"searchQuery":"radiohead"}]`,
		"Read Me First.txt":                       "Thanks for downloading your data.",
	} {
		entry, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip noise entry: %v", err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatalf("write zip noise entry: %v", err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return written
}

// WriteRawJSON writes an arbitrary body, for the tests that need a specific
// malformed or edge-case file.
func WriteRawJSON(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// arrayWriter streams a JSON array element by element.
type arrayWriter struct {
	t     *testing.T
	w     interface{ Write([]byte) (int, error) }
	enc   *json.Encoder
	first bool
}

func newArrayWriter(t *testing.T, w interface{ Write([]byte) (int, error) }) *arrayWriter {
	t.Helper()
	if _, err := w.Write([]byte("[")); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return &arrayWriter{t: t, w: w, enc: json.NewEncoder(w), first: true}
}

func (a *arrayWriter) write(v any) {
	if !a.first {
		if _, err := a.w.Write([]byte(",")); err != nil {
			a.t.Fatalf("write fixture: %v", err)
		}
	}
	a.first = false
	if err := a.enc.Encode(v); err != nil {
		a.t.Fatalf("encode fixture record: %v", err)
	}
}

func (a *arrayWriter) close() {
	if _, err := a.w.Write([]byte("]")); err != nil {
		a.t.Fatalf("write fixture: %v", err)
	}
}
