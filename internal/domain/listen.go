package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Source records where a listening event came from. The value is persisted, so
// the numbers are part of the on-disk format and must not be reordered.
type Source int16

const (
	// SourceSync is the recently-played polling endpoint.
	SourceSync Source = 0
	// SourceAccountData is a StreamingHistory*.json file from the standard
	// "Account data" export.
	SourceAccountData Source = 1
	// SourceExtended is a Streaming_History_Audio_*.json / endsong_*.json file
	// from the "Extended streaming history" export.
	SourceExtended Source = 2
)

func (s Source) String() string {
	switch s {
	case SourceSync:
		return "sync"
	case SourceAccountData:
		return "account_data"
	case SourceExtended:
		return "extended"
	default:
		return fmt.Sprintf("source(%d)", int16(s))
	}
}

// Valid reports whether s is a known source.
func (s Source) Valid() bool { return s >= SourceSync && s <= SourceExtended }

// Precision describes how exactly a source pins down the moment of playback.
// It drives the width of the cross-source duplicate window.
type Precision int16

const (
	// PrecisionMillisecond: the recently-played API returns an ISO-8601 instant
	// with milliseconds.
	PrecisionMillisecond Precision = 0
	// PrecisionSecond: extended streaming history records `ts` to the second.
	PrecisionSecond Precision = 1
	// PrecisionMinute: account-data `endTime` is "YYYY-MM-DD HH:MM".
	PrecisionMinute Precision = 2
)

func (p Precision) String() string {
	switch p {
	case PrecisionMillisecond:
		return "ms"
	case PrecisionSecond:
		return "s"
	case PrecisionMinute:
		return "min"
	default:
		return fmt.Sprintf("precision(%d)", int16(p))
	}
}

// Tolerance is how far a timestamp from this source may drift from the same
// event observed through another source.
func (p Precision) Tolerance() time.Duration {
	switch p {
	case PrecisionMinute:
		return 60 * time.Second
	default:
		// Sub-minute sources still disagree by a few seconds because one reports
		// the start of playback and the other the end minus ms_played, and the
		// client clock is not the server clock.
		return 10 * time.Second
	}
}

// FuzzyWindow returns the half-width of the window used to suppress a listen that
// has already been recorded from a *different* source.
//
// The window is deliberately not applied within a single source: there, the exact
// dedupe key is authoritative, and a non-zero window would discard genuine rapid
// repeats (play, skip after three seconds, play again).
func FuzzyWindow(a, b Precision) time.Duration {
	if ta, tb := a.Tolerance(), b.Tolerance(); ta > tb {
		return ta
	} else {
		return tb
	}
}

// MaxFuzzyWindow is the widest window FuzzyWindow can return. Range scans use it
// as a bound so the probe can be satisfied by an index.
const MaxFuzzyWindow = 60 * time.Second

// Listen is a single playback event attributed to a user.
//
// PlayedAt is always normalised to the *start* of playback in UTC, whatever the
// source reported, because that is the only anchor all three sources can agree on.
type Listen struct {
	UserID    uuid.UUID
	PlayedAt  time.Time
	Precision Precision
	Identity  TrackIdentity
	MsPlayed  int32
	Source    Source

	// TrackName, ArtistName and AlbumName are what the source called this,
	// exactly as it spelled it. They are kept even when Identity is anchored to a
	// Spotify id, because both export formats carry the names beside the URI and
	// throwing them away leaves a freshly imported history displaying nothing at
	// all until the catalogue queue drains — which on a rate-limited application
	// can be never.
	//
	// The artist and the album become local catalogue rows keyed by their
	// normalised names, since the exports identify neither. Nothing about
	// identity or duplicate detection reads any of the three.
	TrackName  string
	ArtistName string
	AlbumName  string

	// Rich context, present only in extended streaming history. Empty strings and
	// nil pointers mean "not reported", which is distinct from false.
	Platform    string
	ConnCountry string
	ReasonStart string
	ReasonEnd   string
	Shuffle     *bool
	Skipped     *bool
	Offline     *bool
	Incognito   *bool
}

// IdentityKey returns the 32-byte identity hash.
func (l Listen) IdentityKey() []byte { return l.Identity.Key() }

// DedupeKey returns the 32-byte exact-duplicate key.
func (l Listen) DedupeKey() []byte { return DedupeKey(l.UserID, l.Identity, l.PlayedAt) }

// Bounds on plausible values. Spotify launched in October 2008; a little slack
// absorbs client clocks that are wrong rather than rejecting real history.
var (
	// EarliestPlausibleListen rejects epoch-zero and obviously corrupt dates.
	EarliestPlausibleListen = time.Date(2006, time.January, 1, 0, 0, 0, 0, time.UTC)
	// MaxMsPlayed is 24h. Longer values indicate a corrupt record, not a track.
	MaxMsPlayed int32 = 24 * 60 * 60 * 1000
	// FutureSkew is how far ahead of the ingesting server a timestamp may be.
	FutureSkew = 48 * time.Hour
)

// Validate checks the invariants that must hold before a listen may be persisted.
// It returns a *RejectError so the importer can record a precise reason.
func (l Listen) Validate(now time.Time) error {
	if l.UserID == uuid.Nil {
		return &RejectError{Reason: RejectMalformedRecord, Detail: "listen has no user"}
	}
	if l.PlayedAt.IsZero() {
		return &RejectError{Reason: RejectMissingTimestamp, Detail: "no playback timestamp"}
	}
	if l.PlayedAt.Before(EarliestPlausibleListen) {
		return &RejectError{
			Reason: RejectTimestampOutOfRange,
			Detail: fmt.Sprintf("played_at %s is before %s", l.PlayedAt.UTC().Format(time.RFC3339), EarliestPlausibleListen.Format("2006-01-02")),
		}
	}
	if l.PlayedAt.After(now.Add(FutureSkew)) {
		return &RejectError{
			Reason: RejectTimestampOutOfRange,
			Detail: fmt.Sprintf("played_at %s is more than %s in the future", l.PlayedAt.UTC().Format(time.RFC3339), FutureSkew),
		}
	}
	if l.MsPlayed < 0 {
		return &RejectError{Reason: RejectInvalidMsPlayed, Detail: fmt.Sprintf("ms_played %d is negative", l.MsPlayed)}
	}
	if l.MsPlayed > MaxMsPlayed {
		return &RejectError{Reason: RejectInvalidMsPlayed, Detail: fmt.Sprintf("ms_played %d exceeds 24h", l.MsPlayed)}
	}
	if l.Identity.IsZero() {
		return &RejectError{Reason: RejectMissingTrack, Detail: "neither a track URI nor an artist/title pair"}
	}
	if !l.Identity.IsResolved() && (l.Identity.Artist == "" || l.Identity.Title == "") {
		return &RejectError{
			Reason: RejectMissingTrack,
			Detail: "unresolved listen needs both an artist and a title after normalisation",
		}
	}
	if !l.Source.Valid() {
		return &RejectError{Reason: RejectMalformedRecord, Detail: fmt.Sprintf("unknown source %d", int16(l.Source))}
	}
	return nil
}

// EndedAt is when playback stopped, which is what the export formats report.
func (l Listen) EndedAt() time.Time {
	return l.PlayedAt.Add(time.Duration(l.MsPlayed) * time.Millisecond)
}

// StartFromEnd converts a reported stream *end* time into the normalised start.
// Both export formats timestamp the end of the stream; the sync API does not.
func StartFromEnd(endedAt time.Time, msPlayed int32) time.Time {
	if msPlayed < 0 {
		msPlayed = 0
	}
	return endedAt.Add(-time.Duration(msPlayed) * time.Millisecond).UTC()
}

// BoolPtr is a small helper for the optional context flags.
func BoolPtr(b bool) *bool { return &b }
