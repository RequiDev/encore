package httpapi

import (
	"net/http"
	"strings"
)

// blacklistRequest is the body of POST /api/blacklist.
type blacklistRequest struct {
	ArtistID string `json:"artistId"`
}

// handleListBlacklist answers GET /api/blacklist.
//
// The full artist rows are returned rather than bare identifiers, so the
// settings screen can show names and pictures. An artist blacklisted before
// enrichment reached them comes back with an empty name, which is honest: the
// catalogue genuinely does not know it yet.
func (s *Server) handleListBlacklist(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	artists, err := s.catalog.ListBlacklisted(r.Context(), s.querier, user.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]Artist, 0, len(artists))
	for _, a := range artists {
		out = append(out, toArtist(a))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// handleAddBlacklist answers POST /api/blacklist.
//
// Nothing is deleted: the listens stay and the exclusion is applied at query
// time, so removing an artist from the blacklist restores every statistic
// exactly as it was.
func (s *Server) handleAddBlacklist(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var body blacklistRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	artistID := strings.TrimSpace(body.ArtistID)
	if artistID == "" || len(artistID) > 64 || !isBase62(artistID) {
		writeError(w, r, ErrFieldInvalid("artistId", `"artistId" must be a Spotify artist id.`))
		return
	}

	if err := s.catalog.Blacklist(r.Context(), s.querier, user.ID, artistID); err != nil {
		writeError(w, r, err)
		return
	}
	writeNoContent(w)
}

// handleRemoveBlacklist answers DELETE /api/blacklist/{artistId}.
//
// Like adding, it is idempotent: the caller is expressing a desired end state
// rather than editing a record, so removing an artist who was never excluded
// succeeds.
func (s *Server) handleRemoveBlacklist(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	artistID, err := parseSpotifyIDPath(r, "artistId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.catalog.Unblacklist(r.Context(), s.querier, user.ID, artistID); err != nil {
		writeError(w, r, err)
		return
	}
	writeNoContent(w)
}
