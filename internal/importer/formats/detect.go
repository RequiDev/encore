package formats

import (
	"bytes"
	"strings"

	"github.com/requi/encore/internal/domain"
)

// SniffBytes is how much of a file's head Encore reads to identify its format.
// Four kilobytes covers the first record of every export Spotify has shipped and
// bounds the cost of classifying the hundred-odd entries of a full archive.
const SniffBytes = 4 << 10

// DetectByName classifies a file from its name.
//
// Matching is case-insensitive and tolerates a container path in front of the
// name, because entries arrive as "my_spotify_data/Streaming_History_Audio_0.json"
// when a whole export archive is uploaded. A trailing ".gz" is ignored, since
// compression says nothing about the format underneath.
//
// Everything else in a Spotify export -- Playlist1.json, SearchQueries.json,
// Userdata.json, YourLibrary.json, Marquee.json, the "Read Me First" PDF, the
// stray .txt and .csv files -- is domain.FormatUnknown, which tells the importer
// to skip the file cleanly instead of failing the job over it.
func DetectByName(name string) domain.ImportFormat {
	base := strings.ToLower(strings.TrimSpace(baseName(name)))
	base = strings.TrimSuffix(base, ".gz")
	if !strings.HasSuffix(base, ".json") {
		return domain.FormatUnknown
	}
	switch {
	// Only the *audio* file is named. Streaming_History_Video_*.json has the same
	// record shape but contains nothing but shows, so leaving it unrecognised
	// skips it for free; if it is passed anyway every record is skipped as
	// not-music, which is equally correct and merely slower.
	case strings.HasPrefix(base, "streaming_history_audio"), strings.HasPrefix(base, "endsong"):
		return domain.FormatExtended
	case strings.HasPrefix(base, "streaminghistory"):
		return domain.FormatAccountData
	}
	return domain.FormatUnknown
}

// DetectByContent classifies a file from the first SniffBytes of its content by
// looking for keys that only one export uses.
//
// It insists on a key *and* a colon so that a value cannot be mistaken for a
// field name: Marquee.json is a list of objects with an "artistName" value in
// them, and a looser test would happily import it as account data.
func DetectByContent(head []byte) domain.ImportFormat {
	body := bytes.TrimSpace(bytes.TrimPrefix(head, utf8BOM))
	if len(body) == 0 || (body[0] != '[' && body[0] != '{') {
		return domain.FormatUnknown
	}
	switch {
	case hasJSONKey(body, "ms_played"), hasJSONKey(body, "master_metadata_track_name"),
		hasJSONKey(body, "spotify_track_uri"), hasJSONKey(body, "conn_country"):
		return domain.FormatExtended
	case hasJSONKey(body, "msPlayed") && (hasJSONKey(body, "endTime") ||
		hasJSONKey(body, "trackName") || hasJSONKey(body, "podcastName")):
		return domain.FormatAccountData
	}
	return domain.FormatUnknown
}

// Detect classifies a file from both its name and its content.
//
// Content wins when the two disagree. People rename their exports, unzip them
// into folders that rename them, and send each other single files with helpful
// names that no longer say anything true about what is inside.
func Detect(name string, head []byte) domain.ImportFormat {
	if format := DetectByContent(head); format != domain.FormatUnknown {
		return format
	}
	return DetectByName(name)
}

// hasJSONKey reports whether b uses key as an object key.
func hasJSONKey(b []byte, key string) bool {
	needle := []byte(`"` + key + `"`)
	from := 0
	for {
		i := bytes.Index(b[from:], needle)
		if i < 0 {
			return false
		}
		rest := b[from+i+len(needle):]
		for _, c := range rest {
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				continue
			}
			if c == ':' {
				return true
			}
			break
		}
		from += i + len(needle)
	}
}

// baseName strips any container path, accepting both separators because archive
// entries always use "/" while an uploaded name may carry whatever the listener's
// operating system produced.
func baseName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}
