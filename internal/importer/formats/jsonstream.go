package formats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// readBufferSize is the window the reader keeps over the file. It is large enough
// that a record is almost always satisfied without a syscall and small enough
// that peak memory stays a property of the batch size rather than of the file.
const readBufferSize = 64 << 10

// utf8BOM is the byte-order mark some editors, and some Spotify exports, put in
// front of a UTF-8 JSON document. It is not JSON and must be discarded before the
// decoder sees it.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// SyntaxError reports damage to the stream itself: a truncated file, a stray
// byte between records, an array that is never closed.
//
// It exists to keep two very different failures apart. A record that is
// well-formed JSON but nonsense as a listening event is a *domain.RejectError and
// costs one row; a SyntaxError means nothing further can be read at all and the
// file has to be failed. Elements are decoded into json.RawMessage precisely so
// that the shape of an individual record can never be mistaken for the second.
type SyntaxError struct {
	// Offset is the byte position in the file at which the damage was found.
	Offset int64
	Err    error
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("malformed JSON stream at byte %d: %v", e.Offset, e.Err)
}

func (e *SyntaxError) Unwrap() error { return e.Err }

// ArrayReader streams the elements of a JSON document one at a time.
//
// It reads a top-level array (the shape of every Spotify export), and also
// accepts NDJSON or plainly concatenated objects with no enclosing array, a
// leading UTF-8 BOM, an empty array, and a completely empty file. Memory is
// O(one record) whatever the size of the input.
//
// The reader is not safe for concurrent use.
type ArrayReader struct {
	dec *json.Decoder
	// base is added to the decoder's own input offset to give a file offset. It
	// absorbs the bytes consumed before the decoder was created, including the
	// synthetic opening bracket used when resuming mid-array.
	base   int64
	index  int64
	offset int64
	array  bool
	done   bool
	err    error
}

// NewArrayReader opens a stream at the beginning of r.
//
// It returns an error only when r cannot be read at all; a document that is not
// JSON surfaces as a *SyntaxError from the first call to Next, with the offset of
// the offending byte.
func NewArrayReader(r io.Reader) (*ArrayReader, error) {
	return openArrayReader(r, 0, 0)
}

// NewArrayReaderAt reopens a stream at a checkpoint taken from a previous
// reader's InputOffset and Index.
//
// This is the whole of crash resume: byteOffset always points just past a fully
// decoded element, so the reader skips whitespace, consumes the one optional
// comma that separates it from the next element, and carries on until the closing
// bracket as though it had never stopped. Index continues from the supplied
// value, so record numbers stay stable across a restart and line up with the
// checkpoint the importer committed.
//
// Passing byteOffset 0 restarts from the beginning of the file, BOM and opening
// bracket included.
func NewArrayReaderAt(rs io.ReadSeeker, byteOffset int64, index int64) (*ArrayReader, error) {
	if byteOffset < 0 {
		byteOffset = 0
	}
	if index < 0 {
		index = 0
	}
	if _, err := rs.Seek(byteOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to checkpoint at byte %d: %w", byteOffset, err)
	}
	return openArrayReader(rs, byteOffset, index)
}

func openArrayReader(r io.Reader, start, index int64) (*ArrayReader, error) {
	br := bufio.NewReaderSize(r, readBufferSize)
	pos := start

	// A BOM is only possible at the very start of the file; anywhere else those
	// three bytes are content.
	if start == 0 {
		if b, _ := br.Peek(len(utf8BOM)); len(b) == len(utf8BOM) && string(b) == string(utf8BOM) {
			if _, err := br.Discard(len(utf8BOM)); err != nil {
				return nil, &SyntaxError{Offset: pos, Err: err}
			}
			pos += int64(len(utf8BOM))
		}
	}

	a := &ArrayReader{index: index, offset: start}

	c, skipped, err := skipSpace(br)
	pos += skipped
	switch {
	case errors.Is(err, io.EOF):
		// An empty file, or a checkpoint that already reached the end. Zero
		// records and no error: an export with nothing in it is not a failure.
		// The offset stays where the reader was opened, so that reopening a
		// finished stream reports the same checkpoint rather than drifting
		// forward over trailing whitespace on every attempt.
		a.done = true
		return a, nil
	case err != nil:
		return nil, &SyntaxError{Offset: pos, Err: err}
	}

	switch c {
	case '[':
		// Beginning of the document. Let the decoder consume the bracket so that
		// it tracks array state, commas and the terminator itself.
		a.dec = json.NewDecoder(br)
		a.base = pos
		tok, err := a.dec.Token()
		if err != nil {
			return nil, &SyntaxError{Offset: pos, Err: err}
		}
		if d, ok := tok.(json.Delim); !ok || d != '[' {
			return nil, &SyntaxError{Offset: pos, Err: fmt.Errorf("expected '[', got %v", tok)}
		}
		a.array = true

	case ',':
		// Resuming immediately after an element. Consume the separator and hand
		// the decoder a synthetic opening bracket, which puts it back into array
		// state so that it stops cleanly at the real closing bracket.
		if _, err := br.ReadByte(); err != nil {
			return nil, &SyntaxError{Offset: pos, Err: err}
		}
		pos++
		a.dec = json.NewDecoder(io.MultiReader(strings.NewReader("["), br))
		a.base = pos - 1 // the synthetic bracket is one byte the file does not have
		if _, err := a.dec.Token(); err != nil {
			return nil, &SyntaxError{Offset: pos, Err: err}
		}
		a.array = true

	case ']':
		// Resuming after the last element of an array.
		if _, err := br.ReadByte(); err != nil {
			return nil, &SyntaxError{Offset: pos, Err: err}
		}
		a.done = true
		a.offset = pos + 1

	default:
		// No enclosing array: NDJSON, or objects simply written one after another.
		a.dec = json.NewDecoder(br)
		a.base = pos
	}
	return a, nil
}

// skipSpace consumes JSON whitespace and reports the next byte without consuming
// it, together with how many bytes were skipped.
func skipSpace(br *bufio.Reader) (byte, int64, error) {
	var skipped int64
	for {
		c, err := br.ReadByte()
		if err != nil {
			return 0, skipped, err
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			skipped++
			continue
		}
		if err := br.UnreadByte(); err != nil {
			return 0, skipped, err
		}
		return c, skipped, nil
	}
}

// Next decodes the next element into v and reports whether one was consumed.
//
// It returns (false, nil) at the closing bracket or at the end of the input,
// which is the normal way a loop terminates. A *SyntaxError always comes with
// false and is permanent: every later call returns the same error. A non-nil
// error with true means only that the element did not fit v; the stream is still
// positioned after it and the reader may be used again. Passing a
// *json.RawMessage, which is what the importer does, makes that case impossible.
func (a *ArrayReader) Next(v any) (bool, error) {
	if a.err != nil {
		return false, a.err
	}
	if a.done {
		return false, nil
	}
	if !a.dec.More() {
		return false, a.finish()
	}
	if err := a.dec.Decode(v); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			// The element was well-formed JSON that did not match the caller's
			// target. Decode has already stepped over it, so this is a per-record
			// problem and must not poison the reader.
			a.index++
			a.offset = a.base + a.dec.InputOffset()
			return true, err
		}
		return false, a.fail(err)
	}
	a.index++
	a.offset = a.base + a.dec.InputOffset()
	return true, nil
}

// finish consumes whatever must follow the last element and decides whether the
// stream ended cleanly.
func (a *ArrayReader) finish() error {
	a.done = true
	tok, err := a.dec.Token()

	if a.array {
		if err != nil {
			return a.fail(fmt.Errorf("array is never closed: %w", err))
		}
		if d, ok := tok.(json.Delim); !ok || d != ']' {
			return a.fail(fmt.Errorf("expected ']' after the last record, got %v", tok))
		}
		a.offset = a.base + a.dec.InputOffset()
		return nil
	}

	if err == nil {
		return a.fail(fmt.Errorf("unexpected %v after the last record", tok))
	}
	if !errors.Is(err, io.EOF) {
		return a.fail(err)
	}
	return nil
}

// fail records permanent stream damage at the most precise offset available.
func (a *ArrayReader) fail(err error) error {
	offset := a.base + a.dec.InputOffset()
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		offset = a.base + syntaxErr.Offset
	}
	a.done = true
	a.err = &SyntaxError{Offset: offset, Err: err}
	return a.err
}

// InputOffset is the byte offset in the file just after the last decoded element.
//
// It is always a position NewArrayReaderAt can resume from, including before the
// first element has been read, and it is what the importer stores in
// import_files.byte_offset inside the same transaction as the batch it describes.
func (a *ArrayReader) InputOffset() int64 { return a.offset }

// Index is the number of elements decoded so far, continuing from the value
// passed to NewArrayReaderAt.
func (a *ArrayReader) Index() int64 { return a.index }
