package formats

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/requi/encore/internal/domain"
)

// buildArchive writes a zip that looks like the one Spotify sends: history files
// mixed in with everything else the export contains.
func buildArchive(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "my_spotify_data.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create entry %s: %v", name, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return path
}

func exportArchive(t *testing.T) (path string, extended, accountData []byte) {
	t.Helper()
	extended = readFixture(t, "extended_modern.json")
	accountData = readFixture(t, "accountdata.json")
	path = buildArchive(t, map[string][]byte{
		"my_spotify_data/": nil,
		"my_spotify_data/Streaming_History_Audio_2024_0.json":   extended,
		"my_spotify_data/StreamingHistory0.json":                accountData,
		"my_spotify_data/renamed-by-the-user.json":              extended,
		"my_spotify_data/Playlist1.json":                        []byte(`{"playlists":[{"name":"Chill"}]}`),
		"my_spotify_data/Marquee.json":                          []byte(`[{"artistName":"A","segment":"S"}]`),
		"my_spotify_data/SearchQueries.json":                    []byte(`[{"searchQuery":"frahm"}]`),
		"my_spotify_data/YourLibrary.json":                      []byte(`{"tracks":[]}`),
		"my_spotify_data/Userdata.json":                         []byte(`{"username":"listener42"}`),
		"my_spotify_data/Read Me First.pdf":                     []byte("%PDF-1.7 not json"),
		"my_spotify_data/notes.txt":                             []byte("hello"),
		"__MACOSX/my_spotify_data/._StreamingHistory0.json":     []byte("\x00\x05\x16\x07"),
		"my_spotify_data/._Streaming_History_Audio_2024_1.json": []byte("\x00\x05\x16\x07"),
	})
	return path, extended, accountData
}

func TestListArchiveEntries(t *testing.T) {
	path, _, _ := exportArchive(t)

	entries, err := ListArchiveEntries(path)
	if err != nil {
		t.Fatalf("ListArchiveEntries: %v", err)
	}

	want := []Entry{
		{Path: "my_spotify_data/StreamingHistory0.json", Format: domain.FormatAccountData},
		{Path: "my_spotify_data/Streaming_History_Audio_2024_0.json", Format: domain.FormatExtended},
		// Detected by content despite a name that says nothing.
		{Path: "my_spotify_data/renamed-by-the-user.json", Format: domain.FormatExtended},
	}
	if len(entries) != len(want) {
		t.Fatalf("listed %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, got := range entries {
		if got.Path != want[i].Path || got.Format != want[i].Format {
			t.Errorf("entry %d = %+v, want %+v", i, got, want[i])
		}
		if got.Size <= 0 {
			t.Errorf("entry %d has size %d", i, got.Size)
		}
	}
}

func TestOpenArchiveEntry(t *testing.T) {
	path, extended, _ := exportArchive(t)

	rc, err := OpenArchiveEntry(path, "my_spotify_data/Streaming_History_Audio_2024_0.json")
	if err != nil {
		t.Fatalf("OpenArchiveEntry: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close entry: %v", err)
	}
	if !bytes.Equal(got, extended) {
		t.Errorf("entry content differs from the fixture (%d vs %d bytes)", len(got), len(extended))
	}

	// The entry streams into the reader without a seek, which is the resume mode
	// a compressed source forces.
	a, err := NewArrayReader(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("NewArrayReader: %v", err)
	}
	if n := len(drain(t, a)); n != 2 {
		t.Errorf("read %d records from the entry, want 2", n)
	}

	if _, err := OpenArchiveEntry(path, "my_spotify_data/Nope.json"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing entry error = %v, want domain.ErrNotFound", err)
	}
}

func TestIsZip(t *testing.T) {
	path, _, _ := exportArchive(t)
	if ok, err := IsZip(path); err != nil || !ok {
		t.Errorf("IsZip(archive) = (%v, %v), want (true, nil)", ok, err)
	}

	dir := t.TempDir()
	plain := filepath.Join(dir, "endsong_0.json")
	if err := os.WriteFile(plain, readFixture(t, "extended_modern.json"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if ok, err := IsZip(plain); err != nil || ok {
		t.Errorf("IsZip(json) = (%v, %v), want (false, nil)", ok, err)
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if ok, err := IsZip(empty); err != nil || ok {
		t.Errorf("IsZip(empty) = (%v, %v), want (false, nil)", ok, err)
	}

	if _, err := IsZip(filepath.Join(dir, "absent")); err == nil {
		t.Error("IsZip on a missing file returned no error")
	}
}

func TestOpenMaybeCompressed(t *testing.T) {
	dir := t.TempDir()
	content := readFixture(t, "extended_modern.json")

	plain := filepath.Join(dir, "Streaming_History_Audio_2024_0.json")
	if err := os.WriteFile(plain, content, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	compressed := filepath.Join(dir, "Streaming_History_Audio_2024_0.json.gz")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(content); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(compressed, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	t.Run("plain file is seekable", func(t *testing.T) {
		rc, seekable, err := OpenMaybeCompressed(plain)
		if err != nil {
			t.Fatalf("OpenMaybeCompressed: %v", err)
		}
		defer rc.Close()
		if !seekable {
			t.Fatal("a plain file must be reported as seekable")
		}
		rs, ok := rc.(io.ReadSeeker)
		if !ok {
			t.Fatal("a seekable result must satisfy io.ReadSeeker so that the importer can resume by seeking")
		}
		// Resume mid-file straight from the returned handle.
		want, offsets := scan(t, content)
		a, err := NewArrayReaderAt(rs, offsets[0], 1)
		if err != nil {
			t.Fatalf("NewArrayReaderAt: %v", err)
		}
		var raw json.RawMessage
		ok, err = a.Next(&raw)
		if err != nil || !ok {
			t.Fatalf("Next() = (%v, %v)", ok, err)
		}
		if string(raw) != want[1] {
			t.Errorf("resumed record = %s, want %s", raw, want[1])
		}
	})

	t.Run("gzip is not seekable", func(t *testing.T) {
		rc, seekable, err := OpenMaybeCompressed(compressed)
		if err != nil {
			t.Fatalf("OpenMaybeCompressed: %v", err)
		}
		defer rc.Close()
		if seekable {
			t.Fatal("a gzip stream must not be reported as seekable")
		}
		a, err := NewArrayReader(rc)
		if err != nil {
			t.Fatalf("NewArrayReader: %v", err)
		}
		if n := len(drain(t, a)); n != 2 {
			t.Errorf("read %d records through gzip, want 2", n)
		}
	})

	if _, _, err := OpenMaybeCompressed(filepath.Join(dir, "absent")); err == nil {
		t.Error("OpenMaybeCompressed on a missing file returned no error")
	}

	t.Run("sniffing sees through the wrapper", func(t *testing.T) {
		for _, path := range []string{plain, compressed} {
			head, err := SniffFile(path)
			if err != nil {
				t.Fatalf("SniffFile(%s): %v", filepath.Base(path), err)
			}
			if len(head) > SniffBytes {
				t.Errorf("SniffFile read %d bytes, want at most %d", len(head), SniffBytes)
			}
			if got := Detect(filepath.Base(path), head); got != domain.FormatExtended {
				t.Errorf("Detect(%s) = %s, want %s", filepath.Base(path), got, domain.FormatExtended)
			}
		}
		if _, err := SniffFile(filepath.Join(dir, "absent")); err == nil {
			t.Error("SniffFile on a missing file returned no error")
		}
	})
}
