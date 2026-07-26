package formats

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// streamFixtures are the same eight records written in every shape the reader is
// required to accept.
var streamFixtures = []string{
	"stream_array.json",
	"stream_compact.json",
	"stream_ndjson.json",
	"stream_concat.json",
	"stream_bom.json",
}

const streamRecordCount = 8

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// drain reads every remaining element as raw text.
func drain(t *testing.T, a *ArrayReader) []string {
	t.Helper()
	var out []string
	for {
		var raw json.RawMessage
		ok, err := a.Next(&raw)
		if err != nil {
			t.Fatalf("Next after %d records: %v", len(out), err)
		}
		if !ok {
			return out
		}
		out = append(out, string(raw))
	}
}

// scan reads a whole stream in one pass, returning every element and the
// checkpoint that follows it. offsets[i] is InputOffset() after element i, and
// the final entry is the checkpoint reported at the end of the stream.
func scan(t *testing.T, data []byte) (records []string, offsets []int64) {
	t.Helper()
	a, err := NewArrayReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewArrayReader: %v", err)
	}
	for {
		var raw json.RawMessage
		ok, err := a.Next(&raw)
		if err != nil {
			t.Fatalf("Next after %d records: %v", len(records), err)
		}
		offsets = append(offsets, a.InputOffset())
		if !ok {
			return records, offsets
		}
		records = append(records, string(raw))
	}
}

// readPrefix reads n records and returns them with the checkpoint that follows.
func readPrefix(t *testing.T, data []byte, n int) (records []string, offset, index int64) {
	t.Helper()
	a, err := NewArrayReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewArrayReader: %v", err)
	}
	for range n {
		var raw json.RawMessage
		ok, err := a.Next(&raw)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			t.Fatalf("stream ended after %d of %d records", len(records), n)
		}
		records = append(records, string(raw))
	}
	return records, a.InputOffset(), a.Index()
}

func TestArrayReaderAcceptsEveryInputShape(t *testing.T) {
	for _, name := range streamFixtures {
		t.Run(name, func(t *testing.T) {
			a, err := NewArrayReader(bytes.NewReader(readFixture(t, name)))
			if err != nil {
				t.Fatalf("NewArrayReader: %v", err)
			}
			got := drain(t, a)
			if len(got) != streamRecordCount {
				t.Fatalf("read %d records, want %d", len(got), streamRecordCount)
			}
			if a.Index() != streamRecordCount {
				t.Errorf("Index() = %d, want %d", a.Index(), streamRecordCount)
			}
			for i, raw := range got {
				var rec struct {
					I    int    `json:"i"`
					Name string `json:"name"`
				}
				if err := json.Unmarshal([]byte(raw), &rec); err != nil {
					t.Fatalf("record %d is not decodable: %v", i, err)
				}
				if rec.I != i || rec.Name != fmt.Sprintf("record-%d", i) {
					t.Errorf("record %d = %+v, want i=%d", i, rec, i)
				}
			}
		})
	}
}

func TestArrayReaderEmptyInputs(t *testing.T) {
	cases := map[string]io.Reader{
		"completely empty file": strings.NewReader(""),
		"whitespace only":       strings.NewReader("   \n\t\r\n "),
		"bom only":              bytes.NewReader(utf8BOM),
		"empty array":           bytes.NewReader(readFixture(t, "stream_empty_array.json")),
		"empty array with bom":  bytes.NewReader(append(append([]byte{}, utf8BOM...), []byte("[]")...)),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			a, err := NewArrayReader(r)
			if err != nil {
				t.Fatalf("NewArrayReader: %v", err)
			}
			var raw json.RawMessage
			ok, err := a.Next(&raw)
			if ok || err != nil {
				t.Fatalf("Next() = (%v, %v), want (false, nil)", ok, err)
			}
			if a.Index() != 0 {
				t.Errorf("Index() = %d, want 0", a.Index())
			}
		})
	}
}

func TestArrayReaderReportsStreamDamage(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"truncated array", readFixture(t, "stream_truncated.json")},
		{"not json at all", readFixture(t, "stream_garbage.txt")},
		{"array never closed", []byte(`[{"i":0}`)},
		{"missing separator", []byte(`[{"i":0} {"i":1}]`)},
		{"trailing garbage after array element", []byte(`[{"i":0},}]`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := NewArrayReader(bytes.NewReader(tc.data))
			if err != nil {
				var syntaxErr *SyntaxError
				if !errors.As(err, &syntaxErr) {
					t.Fatalf("NewArrayReader error = %v, want *SyntaxError", err)
				}
				return
			}
			var last error
			for {
				var raw json.RawMessage
				ok, err := a.Next(&raw)
				if err != nil {
					last = err
					break
				}
				if !ok {
					break
				}
			}
			var syntaxErr *SyntaxError
			if !errors.As(last, &syntaxErr) {
				t.Fatalf("error = %v, want *SyntaxError", last)
			}
			if syntaxErr.Offset < 0 || syntaxErr.Offset > int64(len(tc.data)) {
				t.Errorf("offset %d is outside the file (%d bytes)", syntaxErr.Offset, len(tc.data))
			}
			// The failure must be sticky: an importer that keeps calling Next
			// must never see the stream appear to recover.
			var raw json.RawMessage
			if ok, err := a.Next(&raw); ok || !errors.Is(err, syntaxErr) {
				t.Errorf("Next() after damage = (%v, %v), want (false, the same error)", ok, err)
			}
		})
	}
}

// TestArrayReaderResumeEquivalence is the proof that crash resume is correct: for
// every prefix length, stopping after N records and reopening at the reported
// checkpoint must produce exactly the same records *and the same checkpoints* as
// one uninterrupted read. Comparing the checkpoints matters as much as comparing
// the records, because a checkpoint that drifts by a byte survives one resume and
// corrupts the next.
func TestArrayReaderResumeEquivalence(t *testing.T) {
	for _, name := range streamFixtures {
		t.Run(name, func(t *testing.T) {
			data := readFixture(t, name)
			want, offsets := scan(t, data)
			if len(want) != streamRecordCount {
				t.Fatalf("fixture yielded %d records, want %d", len(want), streamRecordCount)
			}

			// Every N, including none at all and every record consumed.
			for n := 0; n <= len(want); n++ {
				got, offset, index := readPrefix(t, data, n)
				if index != int64(n) {
					t.Fatalf("Index() after %d records = %d", n, index)
				}

				resumed, err := NewArrayReaderAt(bytes.NewReader(data), offset, index)
				if err != nil {
					t.Fatalf("NewArrayReaderAt(%d, %d): %v", offset, index, err)
				}
				for i := n; i < len(want); i++ {
					var raw json.RawMessage
					ok, err := resumed.Next(&raw)
					if err != nil || !ok {
						t.Fatalf("resume after %d: Next() for record %d = (%v, %v)", n, i, ok, err)
					}
					got = append(got, string(raw))
					if resumed.InputOffset() != offsets[i] {
						t.Fatalf("resume after %d: checkpoint following record %d = %d, want %d",
							n, i, resumed.InputOffset(), offsets[i])
					}
					if resumed.Index() != int64(i+1) {
						t.Fatalf("resume after %d: Index() = %d, want %d", n, resumed.Index(), i+1)
					}
				}
				var raw json.RawMessage
				if ok, err := resumed.Next(&raw); ok || err != nil {
					t.Fatalf("resume after %d: Next() past the end = (%v, %v)", n, ok, err)
				}
				if resumed.InputOffset() != offsets[len(want)] {
					t.Errorf("resume after %d: final checkpoint = %d, want %d",
						n, resumed.InputOffset(), offsets[len(want)])
				}
				if !slices.Equal(got, want) {
					t.Fatalf("resume after %d records gave %d records:\n got %q\nwant %q",
						n, len(got), got, want)
				}
			}
		})
	}
}

// TestArrayReaderResumeAfterEveryRecord simulates a worker that dies immediately
// after committing each batch of exactly one record, so that every checkpoint is
// taken by a reader that was itself resumed from the previous one.
func TestArrayReaderResumeAfterEveryRecord(t *testing.T) {
	for _, name := range streamFixtures {
		t.Run(name, func(t *testing.T) {
			data := readFixture(t, name)
			want, offsets := scan(t, data)

			var got []string
			var offset, index int64
			for {
				a, err := NewArrayReaderAt(bytes.NewReader(data), offset, index)
				if err != nil {
					t.Fatalf("NewArrayReaderAt(%d, %d): %v", offset, index, err)
				}
				var raw json.RawMessage
				ok, err := a.Next(&raw)
				if err != nil {
					t.Fatalf("Next at offset %d: %v", offset, err)
				}
				if !ok {
					if a.InputOffset() != offsets[len(want)] {
						t.Errorf("final checkpoint = %d, want %d", a.InputOffset(), offsets[len(want)])
					}
					break
				}
				got = append(got, string(raw))
				offset, index = a.InputOffset(), a.Index()
				if index != int64(len(got)) {
					t.Fatalf("Index() = %d after %d records", index, len(got))
				}
				if offset != offsets[len(got)-1] {
					t.Fatalf("checkpoint after record %d = %d, want %d",
						len(got)-1, offset, offsets[len(got)-1])
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("record-by-record resume gave %q, want %q", got, want)
			}
		})
	}
}

func TestArrayReaderCheckpointIsAlwaysReopenable(t *testing.T) {
	data := readFixture(t, "stream_array.json")

	// The checkpoint taken once the reader has run to completion must reopen as
	// an empty stream rather than as damage.
	a, err := NewArrayReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewArrayReader: %v", err)
	}
	drain(t, a)

	reopened, err := NewArrayReaderAt(bytes.NewReader(data), a.InputOffset(), a.Index())
	if err != nil {
		t.Fatalf("NewArrayReaderAt: %v", err)
	}
	var raw json.RawMessage
	if ok, err := reopened.Next(&raw); ok || err != nil {
		t.Fatalf("Next() = (%v, %v), want (false, nil)", ok, err)
	}
	if reopened.Index() != streamRecordCount {
		t.Errorf("Index() = %d, want %d", reopened.Index(), streamRecordCount)
	}
}

func TestArrayReaderAtToleratesOutOfRangeCheckpoint(t *testing.T) {
	data := readFixture(t, "stream_array.json")
	a, err := NewArrayReaderAt(bytes.NewReader(data), -5, -1)
	if err != nil {
		t.Fatalf("NewArrayReaderAt: %v", err)
	}
	if got := drain(t, a); len(got) != streamRecordCount {
		t.Fatalf("read %d records from a negative offset, want %d", len(got), streamRecordCount)
	}
}

// TestArrayReaderElementTypeErrorIsNotFatal covers the case a caller hits by
// decoding into something narrower than json.RawMessage: the element is consumed,
// the mismatch is reported, and the stream carries on.
func TestArrayReaderElementTypeErrorIsNotFatal(t *testing.T) {
	a, err := NewArrayReader(strings.NewReader(`[{"i":0},{"i":"seven"},{"i":2}]`))
	if err != nil {
		t.Fatalf("NewArrayReader: %v", err)
	}
	var seen []int
	var typeErrors int
	for {
		var rec struct {
			I int `json:"i"`
		}
		ok, err := a.Next(&rec)
		if err != nil {
			var syntaxErr *SyntaxError
			if errors.As(err, &syntaxErr) {
				t.Fatalf("stream reported as damaged: %v", err)
			}
			typeErrors++
			if !ok {
				t.Fatalf("element was not consumed: %v", err)
			}
			continue
		}
		if !ok {
			break
		}
		seen = append(seen, rec.I)
	}
	if typeErrors != 1 {
		t.Errorf("saw %d type errors, want 1", typeErrors)
	}
	if !slices.Equal(seen, []int{0, 2}) {
		t.Errorf("decoded %v, want [0 2]", seen)
	}
	if a.Index() != 3 {
		t.Errorf("Index() = %d, want 3", a.Index())
	}
}

func TestArrayReaderInputOffsetTracksTheFile(t *testing.T) {
	data := readFixture(t, "stream_compact.json")
	a, err := NewArrayReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewArrayReader: %v", err)
	}
	if a.InputOffset() != 0 {
		t.Errorf("InputOffset() before the first record = %d, want 0", a.InputOffset())
	}
	var raw json.RawMessage
	if _, err := a.Next(&raw); err != nil {
		t.Fatalf("Next: %v", err)
	}
	// The compact fixture starts "[" immediately followed by the first record.
	if want := int64(1 + len(raw)); a.InputOffset() != want {
		t.Errorf("InputOffset() = %d, want %d", a.InputOffset(), want)
	}
	if got := data[a.InputOffset()]; got != ',' {
		t.Errorf("byte at the checkpoint is %q, want a separator", got)
	}
}
