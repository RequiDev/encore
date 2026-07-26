package formats

import (
	"encoding/json"
	"time"

	"github.com/requi/encore/internal/domain"
)

// accountDataTimeLayouts are the shapes `endTime` has been observed in. The
// documented form is "2006-01-02 15:04" in UTC, but exports produced for accounts
// in some regions carry a full RFC 3339 instant instead.
var accountDataTimeLayouts = []string{
	"2006-01-02 15:04",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04",
}

// AccountDataParser reads the standard "Account data" export:
// StreamingHistory*.json and StreamingHistory_music_*.json.
type AccountDataParser struct{}

// NewAccountDataParser builds the parser. It holds no state, so one instance may
// be shared by every file in a job.
func NewAccountDataParser() *AccountDataParser { return &AccountDataParser{} }

// Format identifies the export this parser reads.
func (p *AccountDataParser) Format() domain.ImportFormat { return domain.FormatAccountData }

// accountDataRecord is one element of an account-data history file. The podcast
// variant of the export replaces the artist and track names with a show and an
// episode while keeping the same envelope.
type accountDataRecord struct {
	EndTime    flexString `json:"endTime"`
	ArtistName flexString `json:"artistName"`
	TrackName  flexString `json:"trackName"`
	MsPlayed   flexInt    `json:"msPlayed"`

	PodcastName flexString `json:"podcastName"`
	EpisodeName flexString `json:"episodeName"`
}

// Parse converts one raw record into a listening event.
//
// This export carries no track URI at all, so every listen it produces starts out
// unresolved and is matched to the catalogue later by the alias resolver. Its
// timestamps are only accurate to the minute, which is why the precision recorded
// here widens the cross-source duplicate window: it is what lets the same play,
// imported once from account data and once from an extended export, be recognised
// as one event instead of two.
func (p *AccountDataParser) Parse(raw json.RawMessage, minMsPlayed int32) (Record, error) {
	var rec accountDataRecord
	if rej := decodeObject(raw, &rec); rej != nil {
		return Record{}, rej
	}
	if rec.empty() {
		return Record{}, reject(domain.RejectUnknownShape,
			"object carries no account data streaming history fields")
	}
	if show, episode := rec.PodcastName.String(), rec.EpisodeName.String(); show != "" || episode != "" {
		return Record{Skip: skipf(domain.SkipNotMusic,
			"podcast episode %s", firstNonEmpty(episode, show))}, nil
	}

	ms, rej := msPlayed(rec.MsPlayed, "msPlayed")
	if rej != nil {
		return Record{}, rej
	}

	endTime := rec.EndTime.String()
	if endTime == "" {
		return Record{}, reject(domain.RejectMissingTimestamp, "endTime is absent")
	}
	endedAt, ok := parseTimestamp(endTime, accountDataTimeLayouts)
	if !ok {
		return Record{}, reject(domain.RejectInvalidTimestamp,
			"endTime %q is not a recognised timestamp", endTime)
	}

	if ms < minMsPlayed {
		return Record{Skip: skipf(domain.SkipBelowMinimum,
			"msPlayed %d is below the %d ms minimum", ms, minMsPlayed)}, nil
	}

	identity, rej := namesIdentity(rec.ArtistName.String(), rec.TrackName.String())
	if rej != nil {
		return Record{}, rej
	}

	return Record{Listen: domain.Listen{
		PlayedAt:  domain.StartFromEnd(endedAt, ms),
		Precision: domain.PrecisionMinute,
		Identity:  identity,
		MsPlayed:  ms,
		Source:    domain.SourceAccountData,
	}}, nil
}

// empty reports whether the object has none of the format's fields at all, which
// means the file is not an account-data streaming history however it was labelled.
func (r accountDataRecord) empty() bool {
	return r.EndTime.String() == "" && !r.MsPlayed.Present &&
		r.ArtistName.String() == "" && r.TrackName.String() == "" &&
		r.PodcastName.String() == "" && r.EpisodeName.String() == ""
}
