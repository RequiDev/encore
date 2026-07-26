// Package formats reads the streaming-history files Spotify hands out.
//
// Two exports exist and both have changed shape several times: the "Account
// data" export (StreamingHistory*.json), which carries only names and a
// minute-resolution end time, and the "Extended streaming history" export
// (Streaming_History_Audio_*.json, historically endsong_*.json), which carries a
// track URI, playback context and second-resolution timestamps.
//
// Everything here streams. A reader holds one record at a time whatever the size
// of the file, and ArrayReader reports a byte offset after every record which can
// be handed straight back to NewArrayReaderAt, so an import that died halfway
// through a two-gigabyte export continues from exactly where it stopped.
//
// The parsers are deliberately forgiving about the *type* of a field and strict
// about its *meaning*: a number that arrives as a string is accepted, because
// Spotify has shipped exports that do exactly that, while a record that cannot be
// turned into a listening event is rejected on its own with a reason, so one bad
// record never fails an import.
package formats

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/requi/encore/internal/domain"
)

// Record is the result of parsing one raw export record.
//
// Listen carries no UserID. The parsers are pure functions of the file's bytes
// and know nothing about who is importing it, so the caller stamps the owner on
// before validating and staging the row.
//
// Skip is non-nil when the record is well-formed but must not be stored, for
// instance a podcast episode or a play shorter than the configured minimum. When
// Skip is set, Listen is not populated and must not be used.
type Record struct {
	Listen domain.Listen
	Skip   *domain.SkipError
}

// Parser turns one raw record of a known export format into a Record.
//
// Parse returns a *domain.RejectError and nothing else. Every failure is
// therefore permanent and per-record: the importer writes it to import_rejects
// with a reason and carries on, which is what stops a single corrupt element in
// the middle of a large export from costing the user the rest of their history.
type Parser interface {
	Format() domain.ImportFormat
	Parse(raw json.RawMessage, minMsPlayed int32) (Record, error)
}

// ParserFor returns the parser for a detected format. The second result is false
// for domain.FormatUnknown, which is the signal to skip the file rather than to
// fail the job.
func ParserFor(format domain.ImportFormat) (Parser, bool) {
	switch format {
	case domain.FormatExtended:
		return NewExtendedParser(), true
	case domain.FormatAccountData:
		return NewAccountDataParser(), true
	default:
		return nil, false
	}
}

// reject builds the only error type a parser is allowed to return.
func reject(reason domain.RejectReason, format string, args ...any) *domain.RejectError {
	return &domain.RejectError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// skipf builds a skip decision with a human-readable detail.
func skipf(reason domain.SkipReason, format string, args ...any) *domain.SkipError {
	return &domain.SkipError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// decodeObject unmarshals one raw record, insisting that it is a JSON object.
//
// Anything else -- an array, a bare string, a stray null left behind by an
// export bug -- is a per-record reject rather than a stream error, because the
// surrounding stream is still perfectly readable.
func decodeObject(raw json.RawMessage, v any) *domain.RejectError {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return reject(domain.RejectMalformedRecord, "record is empty")
	}
	if trimmed[0] != '{' {
		return reject(domain.RejectMalformedRecord, "record is not a JSON object")
	}
	if err := json.Unmarshal(trimmed, v); err != nil {
		return reject(domain.RejectMalformedRecord, "record could not be decoded: %v", err)
	}
	return nil
}

// namesIdentity builds the names-only identity used when no track URI is
// available, rejecting the record when normalisation leaves nothing to match on.
func namesIdentity(artist, title string) (domain.TrackIdentity, *domain.RejectError) {
	id := domain.TrackIdentityFromNames(artist, title)
	if id.Artist == "" || id.Title == "" {
		return domain.TrackIdentity{}, reject(domain.RejectMissingTrack,
			"artist %q and track %q do not give a usable identity after normalisation", artist, title)
	}
	return id, nil
}

// firstNonEmpty returns the first value that is not the empty string, which keeps
// diagnostic messages informative when only one of several related fields is set.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
