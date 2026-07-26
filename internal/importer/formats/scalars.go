package formats

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/requi/encore/internal/domain"
)

// The three types below exist because Spotify's exports are not type-stable.
// Across the years the same field has arrived as a number, as a decimal string,
// as a boolean, as the string "true", and as null. Failing a record over that
// would throw away a real listening event for a cosmetic reason, so each decoder
// accepts every shape it has been seen in and *never* returns an error: a value
// it cannot interpret is reported as absent or invalid, and the parser decides
// whether that matters for the field in question.

// flexString decodes any JSON scalar into a string. Numbers and booleans keep
// their literal text, null and structured values become empty.
type flexString string

// UnmarshalJSON implements json.Unmarshaler.
func (s *flexString) UnmarshalJSON(data []byte) error {
	*s = ""
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	switch data[0] {
	case '"':
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			// A broken escape sequence is not worth losing the whole record over.
			return nil
		}
		*s = flexString(v)
	case '{', '[':
		// A structured value where a name was expected carries no usable text.
	default:
		*s = flexString(data)
	}
	return nil
}

// String returns the trimmed text. Exports have been seen padding names and
// timestamps with stray whitespace.
func (s flexString) String() string { return strings.TrimSpace(string(s)) }

// flexInt decodes an integer that may arrive as a number, as a decimal or
// floating-point string, or not at all. It distinguishes "absent" from "zero",
// which matters because offline_timestamp legitimately arrives as null, as 0 and
// as a string in different generations of the export.
type flexInt struct {
	// Value is the decoded integer, valid only when Present is true and Invalid
	// is false.
	Value int64
	// Present is true when the field carried something other than null or an
	// empty string.
	Present bool
	// Invalid is true when a value was supplied but is not a number.
	Invalid bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (n *flexInt) UnmarshalJSON(data []byte) error {
	*n = flexInt{}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil
	}

	text := string(data)
	switch data[0] {
	case '"':
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			n.Present, n.Invalid = true, true
			return nil
		}
		text = strings.TrimSpace(v)
		if text == "" {
			// An empty string means "not reported", not zero.
			return nil
		}
	case '{', '[':
		n.Present, n.Invalid = true, true
		return nil
	}

	n.Present = true
	if v, err := strconv.ParseInt(text, 10, 64); err == nil {
		n.Value = v
		return nil
	}
	// Some exports write whole numbers in floating-point notation.
	if f, err := strconv.ParseFloat(text, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		n.Value = int64(f)
		return nil
	}
	n.Invalid = true
	return nil
}

// flexBool decodes an optional boolean that may arrive as a JSON boolean, as the
// string "true"/"false", as 0/1, or as null.
//
// Unknown stays unknown: very old exports wrote a reason string such as
// "trackdone" into `skipped`, and recording that as false would invent a fact
// about the listener's behaviour that the export never stated.
type flexBool struct{ value *bool }

// UnmarshalJSON implements json.Unmarshaler.
func (b *flexBool) UnmarshalJSON(data []byte) error {
	b.value = nil
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}

	text := string(data)
	if data[0] == '"' {
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			return nil
		}
		text = strings.ToLower(strings.TrimSpace(v))
	}
	switch text {
	case "true", "1":
		b.value = domain.BoolPtr(true)
	case "false", "0":
		b.value = domain.BoolPtr(false)
	}
	return nil
}

// Ptr returns the decoded value, or nil when the export did not state one.
func (b flexBool) Ptr() *bool { return b.value }

// msPlayed converts a decoded duration into the int32 the domain uses, rejecting
// anything outside the plausible range rather than storing a nonsense figure that
// would distort every "time listened" statistic derived from it.
func msPlayed(n flexInt, field string) (int32, *domain.RejectError) {
	if !n.Present {
		return 0, nil
	}
	if n.Invalid {
		return 0, reject(domain.RejectInvalidMsPlayed, "%s is not a number", field)
	}
	if n.Value < 0 || n.Value > int64(domain.MaxMsPlayed) {
		return 0, reject(domain.RejectInvalidMsPlayed,
			"%s %d is outside the plausible range 0..%d", field, n.Value, domain.MaxMsPlayed)
	}
	return int32(n.Value), nil
}

// parseTimestamp interprets an export timestamp against an ordered list of
// layouts, always returning UTC. Layouts without a zone are documented by Spotify
// as UTC, which is also how time.Parse reads them.
func parseTimestamp(value string, layouts []string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
