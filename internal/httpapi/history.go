package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/requi/encore/internal/stats"
)

// historyPageSize is the default page of the listening feed. It is smaller than
// the general page limit because each row carries a whole resolved track.
const historyPageSize = 50

// handleHistory answers GET /api/history.
//
// The feed is keyset paginated on (playedAt, id), never by OFFSET: a user may
// legitimately hold millions of rows, and OFFSET makes the database walk and
// discard every skipped one. The cursor is opaque and is passed back verbatim.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	limit, err := parseLimit(r, historyPageSize, maxPageLimit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	ctx := r.Context()

	page, err := s.stats.History(ctx, s.querier, user.ID, tr, user.Timezone, cursor, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	items, err := s.renderHistory(ctx, page.Entries)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, HistoryResponse{
		Items:      items,
		NextCursor: strPtr(page.NextCursor),
		HasMore:    page.NextCursor != "",
	})
}

// renderHistory resolves a page of listens into the contract's shape, batching
// the catalogue lookups for the whole page.
func (s *Server) renderHistory(ctx context.Context, entries []stats.HistoryEntry) ([]HistoryItem, error) {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.TrackID != "" {
			ids = append(ids, e.TrackID)
		}
	}
	refs, err := s.resolveRefs(ctx, ids, nil, nil)
	if err != nil {
		return nil, err
	}

	items := make([]HistoryItem, 0, len(entries))
	for _, e := range entries {
		item := HistoryItem{
			ID:       idString(e.ID),
			PlayedAt: ts(e.PlayedAt),
			MsPlayed: e.MsPlayed,
			Source:   e.Source.String(),
			// A names-only listen has no catalogue identity yet, so the client is
			// told so explicitly and shown the names the export carried instead.
			AliasArtist: strPtr(e.AliasArtist),
			AliasTitle:  strPtr(e.AliasTitle),
		}
		if e.TrackID != "" {
			item.Track = refs.trackRef(e.TrackID)
		}
		items = append(items, item)
	}
	return items, nil
}
