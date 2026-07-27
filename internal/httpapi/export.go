package httpapi

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/stats"
)

// exportPageSize is how many listens one keyset page of the export carries. It
// is the largest page the statistics layer allows, because the export is one
// long sequential read and every page costs a round trip.
const exportPageSize = stats.MaxPageSize

// exportBufferBytes is the size of the writer between the encoder and the
// socket. It bounds how much of the export is ever in memory at once, whatever
// the size of the history behind it.
const exportBufferBytes = 64 << 10

// handleExport answers GET /api/me/export.
//
// The response is written straight to the wire in keyset pages and is never
// assembled in memory: a listener with a decade of history has millions of rows,
// and buffering them would cost the API process as much memory as the file it
// produced.
//
// Blacklisted artists are excluded, as they are from every other read: the
// export mirrors what Encore shows, so a listener who has hidden an artist does
// not find them again in the download.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	format, err := parseExportFormat(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	tr, err := s.exportRange(r, user)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	// The first page is fetched before a single header goes out, so the usual
	// failures — an unparseable range, a database that is not answering — still
	// become a proper error document rather than a truncated download.
	first, err := s.stats.History(ctx, s.querier, user.ID, tr, user.Timezone, "", exportPageSize)
	if err != nil {
		writeError(w, r, err)
		return
	}

	filename := fmt.Sprintf("encore-listening-history-%s.%s", s.now().UTC().Format("20060102"), format)
	w.Header().Set("Content-Disposition", contentDisposition(filename))
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)

	buf := bufio.NewWriterSize(w, exportBufferBytes)
	writer := s.newExportWriter(format, buf, user, tr)

	if err := s.streamExport(r, writer, user, tr, first); err != nil {
		// Headers have gone; the only honest signal left is an incomplete body.
		// ErrAbortHandler is net/http's way of saying exactly that, and the
		// recoverer lets it through untouched.
		logging.FromContext(ctx).Error("listening history export failed part-way", logging.Err(err))
		panic(http.ErrAbortHandler)
	}
	if err := buf.Flush(); err != nil {
		logging.FromContext(ctx).Debug("could not flush the export", logging.Err(err))
	}
}

// exportRange is the window an export covers.
//
// Unlike a statistic, an export defaults to everything rather than to the last
// thirty days: somebody asking for their data wants all of it.
func (s *Server) exportRange(r *http.Request, user domain.User) (domain.TimeRange, error) {
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("from")) == "" && strings.TrimSpace(q.Get("to")) == "" {
		return domain.TimeRange{
			From: domain.EarliestPlausibleListen,
			To:   s.now().UTC().Add(domain.FutureSkew),
		}, nil
	}
	return parseRange(r, user, s.now())
}

// streamExport walks the feed from the page already in hand to the end.
func (s *Server) streamExport(r *http.Request, writer exportWriter, user domain.User, tr domain.TimeRange, page stats.HistoryPage) error {
	ctx := r.Context()
	if err := writer.begin(); err != nil {
		return err
	}
	for {
		items, err := s.renderHistory(ctx, page.Entries)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := writer.write(item); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return writer.end()
		}
		// A client that has gone away should not make the database keep reading.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writer.flush(); err != nil {
			return err
		}
		if page, err = s.stats.History(ctx, s.querier, user.ID, tr, user.Timezone, page.NextCursor, exportPageSize); err != nil {
			return err
		}
	}
}

// exportWriter is one serialisation of the feed.
type exportWriter interface {
	begin() error
	write(item HistoryItem) error
	flush() error
	end() error
}

// newExportWriter builds the writer for a format.
func (s *Server) newExportWriter(format string, buf *bufio.Writer, user domain.User, tr domain.TimeRange) exportWriter {
	if format == "csv" {
		return &csvExport{buf: buf, w: csv.NewWriter(buf)}
	}
	return &jsonExport{
		buf:        buf,
		enc:        json.NewEncoder(buf),
		user:       user,
		tr:         tr,
		exportedAt: s.now(),
	}
}

// jsonExport writes one JSON document with the items as a streamed array.
type jsonExport struct {
	buf        *bufio.Writer
	enc        *json.Encoder
	user       domain.User
	tr         domain.TimeRange
	exportedAt time.Time
	wrote      bool
}

func (e *jsonExport) begin() error {
	_, err := fmt.Fprintf(e.buf,
		`{"exportedAt":%s,"user":{"id":%s,"spotifyUserId":%s},"from":%s,"to":%s,"items":[`,
		jsonString(ts(e.exportedAt)),
		jsonString(e.user.ID.String()),
		jsonString(e.user.SpotifyUserID),
		jsonString(ts(e.tr.From)),
		jsonString(ts(e.tr.To)))
	return err
}

func (e *jsonExport) write(item HistoryItem) error {
	if e.wrote {
		if _, err := e.buf.WriteString(","); err != nil {
			return err
		}
	}
	e.wrote = true
	// Encode appends a newline, which keeps the document readable and costs one
	// byte per listen.
	return e.enc.Encode(item)
}

func (e *jsonExport) flush() error { return e.buf.Flush() }

func (e *jsonExport) end() error {
	if _, err := e.buf.WriteString("]}\n"); err != nil {
		return err
	}
	return e.buf.Flush()
}

// csvExport writes the feed as a spreadsheet-friendly table.
type csvExport struct {
	buf *bufio.Writer
	w   *csv.Writer
}

func (e *csvExport) begin() error {
	return e.w.Write([]string{
		"played_at", "ms_played", "source",
		"track_id", "track_name", "artist_names",
		"album_id", "album_name",
		"alias_artist", "alias_title",
	})
}

func (e *csvExport) write(item HistoryItem) error {
	var trackID, trackName, artistNames, albumID, albumName string
	if item.Track != nil {
		trackID, trackName = item.Track.ID, item.Track.Name
		names := make([]string, 0, len(item.Track.Artists))
		for _, a := range item.Track.Artists {
			names = append(names, a.Name)
		}
		artistNames = strings.Join(names, "; ")
		if item.Track.Album != nil {
			albumID, albumName = item.Track.Album.ID, item.Track.Album.Name
		}
	}
	return e.w.Write([]string{
		item.PlayedAt,
		strconv.FormatInt(int64(item.MsPlayed), 10),
		item.Source,
		trackID, trackName, artistNames,
		albumID, albumName,
		deref(item.AliasArtist), deref(item.AliasTitle),
	})
}

func (e *csvExport) flush() error {
	e.w.Flush()
	if err := e.w.Error(); err != nil {
		return err
	}
	return e.buf.Flush()
}

func (e *csvExport) end() error { return e.flush() }

// deref renders an optional string as itself or as empty.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
