package imports

import (
	"errors"
	"strings"
	"testing"

	"github.com/RequiDev/encore/internal/domain"
)

func TestClampPage(t *testing.T) {
	tests := []struct {
		name                  string
		limit, offset         int
		wantLimit, wantOffset int
	}{
		{name: "defaults", limit: 0, offset: 0, wantLimit: DefaultPageSize, wantOffset: 0},
		{name: "negative limit", limit: -5, offset: 10, wantLimit: DefaultPageSize, wantOffset: 10},
		{name: "negative offset", limit: 5, offset: -1, wantLimit: 5, wantOffset: 0},
		{name: "kept", limit: 50, offset: 100, wantLimit: 50, wantOffset: 100},
		{name: "capped", limit: MaxPageSize + 1, offset: 0, wantLimit: MaxPageSize, wantOffset: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit, offset := clampPage(tt.limit, tt.offset)
			if limit != tt.wantLimit || offset != tt.wantOffset {
				t.Fatalf("clampPage(%d, %d) = (%d, %d), want (%d, %d)",
					tt.limit, tt.offset, limit, offset, tt.wantLimit, tt.wantOffset)
			}
		})
	}
}

func TestSumCounters(t *testing.T) {
	files := []domain.ImportFile{
		{Counters: domain.Counters{Imported: 10, Duplicates: 2, Skipped: 1, Rejected: 3}},
		{Counters: domain.Counters{Imported: 5, Duplicates: 0, Skipped: 4, Rejected: 0}},
		{},
	}
	got := sumCounters(files)
	want := domain.Counters{Imported: 15, Duplicates: 2, Skipped: 5, Rejected: 3}
	if got != want {
		t.Fatalf("sumCounters() = %+v, want %+v", got, want)
	}
	if got.Processed() != 25 {
		t.Fatalf("Processed() = %d, want 25", got.Processed())
	}
	if empty := sumCounters(nil); empty != (domain.Counters{}) {
		t.Fatalf("sumCounters(nil) = %+v, want zero", empty)
	}
}

func TestNewFileNormalise(t *testing.T) {
	base := NewFile{
		Name:        "  Streaming_History_Audio_2019.json  ",
		Format:      "",
		SizeBytes:   1024,
		SHA256:      make([]byte, 32),
		StoragePath: "/data/imports/abc.json",
	}

	got, err := base.normalise()
	if err != nil {
		t.Fatalf("normalise() unexpected error: %v", err)
	}
	if got.Name != "Streaming_History_Audio_2019.json" {
		t.Fatalf("name = %q, want it trimmed", got.Name)
	}
	// An unstated format must not become an invalid enum value.
	if got.Format != domain.FormatUnknown {
		t.Fatalf("format = %q, want %q", got.Format, domain.FormatUnknown)
	}

	bad := []struct {
		name string
		file NewFile
	}{
		{name: "no name", file: NewFile{Name: "   ", StoragePath: "/data/x"}},
		{name: "unknown format", file: NewFile{Name: "x.json", Format: "csv", StoragePath: "/data/x"}},
		{name: "negative size", file: NewFile{Name: "x.json", SizeBytes: -1, StoragePath: "/data/x"}},
		{name: "short digest", file: NewFile{Name: "x.json", SHA256: []byte{1, 2, 3}, StoragePath: "/data/x"}},
		{name: "no storage path", file: NewFile{Name: "x.json"}},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.file.normalise(); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("normalise() error = %v, want domain.ErrValidation", err)
			}
		})
	}
}

func TestNormaliseFiles(t *testing.T) {
	if _, err := normaliseFiles(nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("normaliseFiles(nil) error = %v, want domain.ErrValidation", err)
	}

	in := []NewFile{
		{Name: "a.json", Format: domain.FormatExtended, StoragePath: "/data/a"},
		{Name: "b.json", Format: domain.FormatAccountData, StoragePath: "/data/b"},
	}
	out, err := normaliseFiles(in)
	if err != nil {
		t.Fatalf("normaliseFiles() unexpected error: %v", err)
	}
	if len(out) != 2 || out[0].Name != "a.json" || out[1].Name != "b.json" {
		t.Fatalf("normaliseFiles() = %+v, want the input order preserved", out)
	}

	// One bad file rejects the whole upload: a job must never be created half
	// described.
	in = append(in, NewFile{Name: "c.json"})
	if _, err := normaliseFiles(in); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("normaliseFiles() error = %v, want domain.ErrValidation", err)
	}
}

// TestRetryJobSQLPreservesCheckpoints guards the property that makes a retry a
// resume: the statement may only reset a file's status, never its position or
// its counters.
func TestRetryJobSQLPreservesCheckpoints(t *testing.T) {
	for _, forbidden := range []string{"record_offset =", "byte_offset =", "imported =", "duplicates =", "rejected ="} {
		if strings.Contains(retryJobSQL, forbidden) {
			t.Fatalf("retryJobSQL assigns %q; a retry must resume from the checkpoint", forbidden)
		}
	}
	if !strings.Contains(retryJobSQL, "cancel_requested = false") {
		t.Fatal("retryJobSQL must clear cancel_requested, or the worker stops again immediately")
	}
}
