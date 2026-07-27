package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
)

// playlistDescription is what Spotify shows under the name. It says where the
// playlist came from, because a listener scrolling their library months later
// should not have to guess which application made it.
func playlistDescription(def domain.PlaylistDefinition) string {
	switch def.Mode {
	case domain.PlaylistModeMinPlays:
		return fmt.Sprintf("Built by Encore: everything played at least %d times.", def.MinPlays)
	case domain.PlaylistModeDiscoveries:
		return "Built by Encore: tracks heard for the first time in this period."
	case domain.PlaylistModeForgotten:
		return "Built by Encore: played heavily before this period, and not during it."
	default:
		return "Built by Encore: most played in this period."
	}
}

// handleCreatePlaylist answers POST /api/playlists.
//
// It creates the playlist on Spotify and fills it in one request, because a
// half-made playlist is worse than none: the listener would be left with an
// empty one in their library and no way to tell whether Encore intended it.
func (s *Server) handleCreatePlaylist(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var body CreatePlaylistRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	def, err := body.definition()
	if err != nil {
		writeError(w, r, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if err := domain.ValidatePlaylistName(name); err != nil {
		writeError(w, r, err)
		return
	}

	ctx := r.Context()
	token, err := s.playlistToken(ctx, user)
	if err != nil {
		writeError(w, r, err)
		return
	}

	ids, matched, err := s.selectPlaylistTracks(ctx, user, def)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if len(ids) == 0 {
		writeError(w, r, ErrInvalidRequest(
			"Nothing matches that description, so there is no playlist to make. "+
				"Try a wider period or a lower minimum.", nil))
		return
	}

	created, err := s.spotify.CreatePlaylist(ctx, token, user.SpotifyUserID, name, playlistDescription(def))
	if err != nil {
		writeError(w, r, playlistError(err))
		return
	}
	if err := s.spotify.ReplacePlaylistItems(ctx, token, created.ID, ids); err != nil {
		// The playlist exists but is empty. Say so rather than reporting a
		// failure that leaves the listener wondering what is in their library.
		logging.FromContext(ctx).Error("could not fill a new playlist",
			"playlist", created.ID, logging.Err(err))
		writeError(w, r, playlistError(err))
		return
	}

	stored, err := s.playlists.Create(ctx, s.querier, domain.Playlist{
		UserID:     user.ID,
		Name:       name,
		SpotifyID:  created.ID,
		SpotifyURL: created.URL(),
		Definition: def,
		TrackCount: len(ids),
		BuiltAt:    s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := toPlaylist(stored)
	out.Matched = matched
	writeJSON(w, r, http.StatusCreated, out)
}

// handleListPlaylists answers GET /api/playlists.
func (s *Server) handleListPlaylists(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	rows, err := s.playlists.ListForUser(r.Context(), s.querier, user.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]Playlist, 0, len(rows))
	for _, p := range rows {
		out = append(out, toPlaylist(p))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// handleRebuildPlaylist answers POST /api/playlists/{id}/rebuild.
//
// Re-runs the stored definition and replaces the contents in place, so the
// playlist keeps its identity: anyone who saved or followed it keeps the same
// one rather than being left on a stale copy.
func (s *Server) handleRebuildPlaylist(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, ErrInvalidRequest("That is not a valid playlist id.", nil))
		return
	}

	ctx := r.Context()
	stored, err := s.playlists.Get(ctx, s.querier, user.ID, id)
	if err != nil {
		writeError(w, r, err)
		return
	}

	token, err := s.playlistToken(ctx, user)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ids, matched, err := s.selectPlaylistTracks(ctx, user, stored.Definition)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.spotify.ReplacePlaylistItems(ctx, token, stored.SpotifyID, ids); err != nil {
		writeError(w, r, playlistError(err))
		return
	}

	now := s.now()
	if err := s.playlists.RecordBuild(ctx, s.querier, stored.ID, len(ids), now); err != nil {
		writeError(w, r, err)
		return
	}
	stored.TrackCount = len(ids)
	stored.BuiltAt = now

	out := toPlaylist(stored)
	out.Matched = matched
	writeJSON(w, r, http.StatusOK, out)
}

// handleForgetPlaylist answers DELETE /api/playlists/{id}.
//
// Encore stops managing it. The playlist itself stays in the listener's Spotify
// library, because deleting things from somebody's account is well beyond what
// "stop managing this" can be taken to mean.
func (s *Server) handleForgetPlaylist(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, ErrInvalidRequest("That is not a valid playlist id.", nil))
		return
	}
	if err := s.playlists.Forget(r.Context(), s.querier, user.ID, id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// selectPlaylistTracks resolves a definition against the caller's history.
func (s *Server) selectPlaylistTracks(
	ctx context.Context, user domain.User, def domain.PlaylistDefinition,
) ([]string, int64, error) {
	first, _, err := s.listens.Bounds(ctx, s.querier, user.ID)
	if err != nil {
		return nil, 0, err
	}
	var firstListen time.Time
	if first != nil {
		firstListen = *first
	}
	sel, err := s.stats.SelectPlaylistTracks(ctx, s.querier, user.ID, def, def.Range(s.now(), firstListen))
	if err != nil {
		return nil, 0, err
	}
	return sel.TrackIDs, sel.Matched, nil
}

// playlistToken returns an access token that may write playlists, or an error
// the interface can turn into "authorise playlists".
//
// The scope is asked for only when somebody uses the feature. Encore's default
// grant is read-only, and forcing every user of every instance to re-authorise
// with write access for a feature they may never touch would be a poor trade.
func (s *Server) playlistToken(ctx context.Context, user domain.User) (string, error) {
	if s.userToken == nil {
		return "", ErrConflictf("This instance cannot create playlists.")
	}
	creds, err := s.credentials.Get(ctx, s.querier, user.ID)
	if err != nil {
		return "", err
	}
	if !spotify.HasScope(creds.Scopes, spotify.ScopePlaylistPrivate) {
		return "", ErrForbiddenf(
			"Encore needs permission to create playlists on your Spotify account. " +
				"Grant it from Settings — nothing else changes, and you can revoke it in Spotify.")
	}
	token, err := s.userToken(ctx, user.ID)
	if err != nil {
		return "", err
	}
	return token, nil
}

// playlistError turns a Spotify refusal into something a person can act on.
func playlistError(err error) error {
	var paused *spotify.PausedError
	if errors.As(err, &paused) {
		return ErrConflictf(
			"Spotify is rate limiting this instance until %s, so it would not accept the "+
				"playlist. Your listening data is unaffected; try again after that.",
			paused.Until.UTC().Format(time.RFC3339))
	}
	if apiErr, ok := spotify.AsAPIError(err); ok && apiErr.IsForbidden() {
		return ErrForbiddenf(
			"Spotify refused the change. The permission may have been revoked; " +
				"granting it again from Settings restores it.")
	}
	return err
}
