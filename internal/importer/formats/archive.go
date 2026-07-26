package formats

import (
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/requi/encore/internal/domain"
)

// MaxEntryBytes bounds the uncompressed size of an archive entry Encore is
// willing to read. A zip declares that size in its central directory, so an entry
// that claims to inflate to more than this is refused before a single byte of it
// is decompressed.
const MaxEntryBytes int64 = 8 << 30

// Entry is one importable file found inside an uploaded archive.
type Entry struct {
	// Path is the entry's path within the archive, always with "/" separators.
	Path string
	// Size is the uncompressed size in bytes as declared by the archive.
	Size int64
	// Format is what the entry was detected as; only real streaming-history
	// formats are ever returned.
	Format domain.ImportFormat
}

// ListArchiveEntries opens a .zip and returns the entries that are streaming
// history, in a stable order.
//
// Directories, the __MACOSX sidecar tree, AppleDouble resource forks, entries
// that are not JSON at all and entries larger than MaxEntryBytes are all left
// out, so a user can upload the archive Spotify sent them without first having to
// work out which files matter. Only the first SniffBytes of an entry are
// decompressed to classify it.
func ListArchiveEntries(path string) ([]Entry, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open archive %s: %w", filepath.Base(path), err)
	}
	defer zr.Close()

	var entries []Entry
	for _, f := range zr.File {
		name := normaliseEntryPath(f.Name)
		if name == "" || strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir() {
			continue
		}
		if isArchiveNoise(name) {
			continue
		}
		size := int64(f.UncompressedSize64)
		if f.UncompressedSize64 > uint64(MaxEntryBytes) {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(strings.TrimSuffix(baseName(name), ".gz")), ".json") {
			continue
		}

		var format domain.ImportFormat
		head, err := readEntryHead(f)
		if err != nil {
			// A damaged entry is still listed when its name is unambiguous. The
			// per-file import then reports the read failure, which is far more
			// useful to the user than silently dropping history they believe they
			// uploaded.
			format = DetectByName(name)
		} else {
			format = Detect(name, head)
		}
		if format == domain.FormatUnknown {
			continue
		}
		entries = append(entries, Entry{Path: name, Size: size, Format: format})
	}

	// Ordering decides the import ordinals, so it must not depend on how the
	// archive happened to be written.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// OpenArchiveEntry opens one entry of a .zip for reading.
//
// The returned stream is not seekable, which is what tells the importer to resume
// by replaying and discarding records rather than by seeking to a byte offset.
// Closing it releases the archive as well.
func OpenArchiveEntry(path, entryPath string) (io.ReadCloser, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open archive %s: %w", filepath.Base(path), err)
	}
	want := normaliseEntryPath(entryPath)
	for _, f := range zr.File {
		if normaliseEntryPath(f.Name) != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			_ = zr.Close()
			return nil, fmt.Errorf("open archive entry %q: %w", want, err)
		}
		return &multiCloser{Reader: rc, closers: []io.Closer{rc, zr}}, nil
	}
	_ = zr.Close()
	return nil, fmt.Errorf("archive entry %q: %w", want, domain.ErrNotFound)
}

// IsZip reports whether a file begins with a zip local file header, so that an
// upload can be routed to the archive path without trusting its extension.
func IsZip(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()

	var magic [4]byte
	n, err := io.ReadFull(f, magic[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if n < 4 || magic[0] != 'P' || magic[1] != 'K' {
		return false, nil
	}
	// 03 04 is a local file header, 05 06 an archive with no entries at all, and
	// 07 08 the marker a spanned archive starts with.
	switch {
	case magic[2] == 3 && magic[3] == 4,
		magic[2] == 5 && magic[3] == 6,
		magic[2] == 7 && magic[3] == 8:
		return true, nil
	}
	return false, nil
}

// OpenMaybeCompressed opens a spooled upload, transparently unwrapping a plain
// .gz.
//
// The seekable result is the important one: it is false for a compressed stream,
// which is how the importer knows it must resume by replaying and discarding
// record_offset records instead of seeking to byte_offset. When it is true the
// returned value is the underlying *os.File and can be type-asserted to
// io.ReadSeeker for NewArrayReaderAt.
//
// A .zip is returned as an opaque byte stream; callers test for that with IsZip
// first and go through ListArchiveEntries instead.
func OpenMaybeCompressed(path string) (rc io.ReadCloser, seekable bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}

	var magic [2]byte
	n, err := io.ReadFull(f, magic[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		_ = f.Close()
		return nil, false, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("rewind %s: %w", filepath.Base(path), err)
	}

	if n == 2 && magic[0] == 0x1F && magic[1] == 0x8B {
		gz, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, false, fmt.Errorf("open gzip %s: %w", filepath.Base(path), err)
		}
		return &multiCloser{Reader: gz, closers: []io.Closer{gz, f}}, false, nil
	}
	return f, true, nil
}

// readEntryHead decompresses at most SniffBytes of an entry for classification.
func readEntryHead(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	head := make([]byte, SniffBytes)
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return head[:n], nil
}

// isArchiveNoise reports whether an entry is macOS packaging rather than data.
func isArchiveNoise(name string) bool {
	if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/__MACOSX/") {
		return true
	}
	return strings.HasPrefix(baseName(name), "._")
}

// normaliseEntryPath puts an entry path into the one form comparisons use.
func normaliseEntryPath(name string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	for strings.HasPrefix(name, "./") {
		name = name[2:]
	}
	return strings.TrimPrefix(name, "/")
}

// multiCloser reads from one stream and closes several, so that unwrapping a
// gzip or a zip entry does not leak the file handle underneath it.
type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m *multiCloser) Close() error {
	var first error
	for _, c := range m.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
