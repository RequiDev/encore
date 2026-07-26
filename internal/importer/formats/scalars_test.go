package formats

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFlexStringAcceptsAnyScalar(t *testing.T) {
	cases := map[string]string{
		`"android"`:    "android",
		`"  padded  "`: "padded",
		`null`:         "",
		`""`:           "",
		`1234`:         "1234",
		`true`:         "true",
		`{"a":1}`:      "",
		`[1,2]`:        "",
		`"Sigur Rós"`:  "Sigur Rós",
	}
	for input, want := range cases {
		var s flexString
		if err := json.Unmarshal([]byte(input), &s); err != nil {
			t.Fatalf("Unmarshal(%s) errored: %v", input, err)
		}
		if got := s.String(); got != want {
			t.Errorf("flexString(%s) = %q, want %q", input, got, want)
		}
	}
}

func TestFlexIntAcceptsNumbersAndStrings(t *testing.T) {
	cases := []struct {
		input   string
		value   int64
		present bool
		invalid bool
	}{
		{input: `123456`, value: 123456, present: true},
		{input: `"123456"`, value: 123456, present: true},
		{input: `" 42 "`, value: 42, present: true},
		{input: `0`, present: true},
		{input: `-1`, value: -1, present: true},
		{input: `60000.0`, value: 60000, present: true},
		{input: `1622541600`, value: 1622541600, present: true},
		{input: `null`},
		{input: `""`},
		{input: `"soon"`, present: true, invalid: true},
		{input: `true`, present: true, invalid: true},
		{input: `{"v":1}`, present: true, invalid: true},
	}
	for _, tc := range cases {
		var n flexInt
		if err := json.Unmarshal([]byte(tc.input), &n); err != nil {
			t.Fatalf("Unmarshal(%s) errored: %v", tc.input, err)
		}
		if n.Value != tc.value || n.Present != tc.present || n.Invalid != tc.invalid {
			t.Errorf("flexInt(%s) = %+v, want {Value:%d Present:%v Invalid:%v}",
				tc.input, n, tc.value, tc.present, tc.invalid)
		}
	}
}

// TestFlexBoolKeepsUnknownUnknown is the rule that matters: a value the export
// did not state, or stated in a way that is not a boolean, must never become
// false, because false is a claim about what the listener did.
func TestFlexBoolKeepsUnknownUnknown(t *testing.T) {
	cases := map[string]*bool{
		`true`:        boolp(true),
		`false`:       boolp(false),
		`"true"`:      boolp(true),
		`"TRUE"`:      boolp(true),
		`" false "`:   boolp(false),
		`1`:           boolp(true),
		`0`:           boolp(false),
		`null`:        nil,
		`"trackdone"`: nil,
		`""`:          nil,
		`"maybe"`:     nil,
		`{"v":true}`:  nil,
	}
	for input, want := range cases {
		var b flexBool
		if err := json.Unmarshal([]byte(input), &b); err != nil {
			t.Fatalf("Unmarshal(%s) errored: %v", input, err)
		}
		got := b.Ptr()
		switch {
		case want == nil && got != nil:
			t.Errorf("flexBool(%s) = %v, want nil", input, *got)
		case want != nil && got == nil:
			t.Errorf("flexBool(%s) = nil, want %v", input, *want)
		case want != nil && got != nil && *got != *want:
			t.Errorf("flexBool(%s) = %v, want %v", input, *got, *want)
		}
	}
}

func boolp(v bool) *bool { return &v }

// TestFlexDecodersNeverError guards the property the parsers rely on: a scalar of
// an unexpected type is a value-level oddity, never a decode failure that would
// take the whole record with it.
func TestFlexDecodersNeverError(t *testing.T) {
	inputs := []string{`null`, `""`, `"x"`, `0`, `1.5`, `true`, `[]`, `{}`, `"\ud800"`}
	for _, in := range inputs {
		var target struct {
			S flexString `json:"s"`
			N flexInt    `json:"n"`
			B flexBool   `json:"b"`
		}
		doc := `{"s":` + in + `,"n":` + in + `,"b":` + in + `}`
		if err := json.Unmarshal([]byte(doc), &target); err != nil {
			t.Errorf("Unmarshal(%s) errored: %v", doc, err)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		layouts []string
		value   string
		want    time.Time
		ok      bool
	}{
		{extendedTimeLayouts, "2024-03-11T20:14:07Z", time.Date(2024, 3, 11, 20, 14, 7, 0, time.UTC), true},
		{extendedTimeLayouts, "2024-03-11T20:14:07.512Z", time.Date(2024, 3, 11, 20, 14, 7, 512_000_000, time.UTC), true},
		{extendedTimeLayouts, "2024-03-11T21:14:07+01:00", time.Date(2024, 3, 11, 20, 14, 7, 0, time.UTC), true},
		{extendedTimeLayouts, "2024-03-11 20:14:07", time.Date(2024, 3, 11, 20, 14, 7, 0, time.UTC), true},
		{extendedTimeLayouts, " 2024-03-11T20:14:07Z ", time.Date(2024, 3, 11, 20, 14, 7, 0, time.UTC), true},
		{accountDataTimeLayouts, "2020-02-14 21:37", time.Date(2020, 2, 14, 21, 37, 0, 0, time.UTC), true},
		{accountDataTimeLayouts, "2020-02-15T08:03:11Z", time.Date(2020, 2, 15, 8, 3, 11, 0, time.UTC), true},
		{extendedTimeLayouts, "", time.Time{}, false},
		{extendedTimeLayouts, "yesterday", time.Time{}, false},
		{accountDataTimeLayouts, "14/02/2020 21:37", time.Time{}, false},
	}
	for _, tc := range cases {
		got, ok := parseTimestamp(tc.value, tc.layouts)
		if ok != tc.ok {
			t.Errorf("parseTimestamp(%q) ok = %v, want %v", tc.value, ok, tc.ok)
			continue
		}
		if ok && !got.Equal(tc.want) {
			t.Errorf("parseTimestamp(%q) = %s, want %s", tc.value, got, tc.want)
		}
		if ok && got.Location() != time.UTC {
			t.Errorf("parseTimestamp(%q) location = %s, want UTC", tc.value, got.Location())
		}
	}
}
