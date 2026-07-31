package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store"
)

// maxPlaylistDescription is Spotify's ceiling, minus the three bytes
// store.Truncate appends when it cuts.
//
// domain.Describe is already bounded well under this by its own test, so the
// clamp is a guard rather than a working part: it exists so that a future
// clause added to a description without re-running that test is truncated
// rather than silently rejected by Spotify, which would fail a rename for a
// reason nobody could see.
const maxPlaylistDescription = 297

// playlistDescription is what Spotify shows under the name.
func playlistDescription(def domain.PlaylistDefinition, builtAt time.Time) string {
	return store.Truncate(def.Describe(builtAt), maxPlaylistDescription)
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

	sel, err := s.selectPlaylistTracks(ctx, user, def)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ids := sel.IDs()
	if len(ids) == 0 {
		writeError(w, r, ErrInvalidRequest(
			"Nothing matches that description, so there is no playlist to make. "+
				"Try a wider period or a lower minimum.", nil))
		return
	}

	created, err := s.spotify.CreatePlaylist(ctx, token, user.SpotifyUserID, name,
		playlistDescription(def, s.now()))
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
	out.Matched = sel.Matched
	writeJSON(w, r, http.StatusCreated, out)
}

// handleRenamePlaylist answers PATCH /api/playlists/{id}.
//
// The project's first write to a listener's real Spotify account that is not
// the creation of a new object, and the ordering below is the whole of its
// safety story:
//
//  1. Spotify first. PUT /v1/playlists/{id} sets the name and the description
//     in one request, so there is no half-renamed state to reconcile.
//  2. Encore second, and only on a 2xx. The listener's Spotify account is the
//     authority on what their playlist is called.
//  3. Every message names what is true of the playlist right now. A refusal
//     says the old name is still in place, because it is. A transport failure
//     says Encore cannot tell, because it cannot — flattening that into
//     "nothing has changed" would be a claim about somebody else's account
//     that this process is in no position to make. And the one case where
//     Spotify accepted and the local write did not says exactly that, rather
//     than reporting a failure that would send somebody to rename it again.
//
// The Spotify playlist id comes from the stored row, never from the request
// body: Get is scoped by user, so a caller cannot address a playlist that is
// not theirs and no field can widen what this writes to.
func (s *Server) handleRenamePlaylist(w http.ResponseWriter, r *http.Request) {
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

	var body RenamePlaylistRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Name == nil {
		writeError(w, r, ErrFieldInvalid("name", `"name" is required.`))
		return
	}
	name := strings.TrimSpace(*body.Name)
	if err := domain.ValidatePlaylistName(name); err != nil {
		writeError(w, r, err)
		return
	}

	ctx := r.Context()
	// Before anything is sent. A playlist that is not the caller's is not found
	// here, on the same sentence a missing one gets, and Spotify is never asked
	// about an id the caller could not otherwise name.
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

	// The description is rewritten alongside the name, because it is derived
	// from the definition and the last build rather than from the name, and
	// because one request that sets both is one fewer state to be in.
	description := playlistDescription(stored.Definition, stored.BuiltAt)
	if err := s.spotify.UpdatePlaylistDetails(ctx, token, stored.SpotifyID, name, description); err != nil {
		refusal, unknown := renameError(err)
		if unknown {
			// The one outcome nobody can reconstruct afterwards. The listener is
			// told Encore cannot tell what happened; if this were not logged, an
			// operator asked "why is my playlist called something I did not
			// choose" would have no record that Encore ever tried.
			logging.FromContext(ctx).Warn("could not tell whether a rename reached spotify",
				"playlist", stored.SpotifyID, logging.Err(err))
		}
		writeError(w, r, refusal)
		return
	}

	updated, err := s.playlists.Rename(ctx, s.querier, user.ID, stored.ID, name)
	if err != nil {
		logging.FromContext(ctx).Error("spotify accepted a rename that could not be recorded",
			"playlist", stored.SpotifyID, logging.Err(err))
		writeError(w, r, ErrConflictf(
			"Spotify has the new name, but Encore could not record it. "+
				"The playlist itself is correct; reload this page to see the current state."))
		return
	}
	writeJSON(w, r, http.StatusOK, toPlaylist(updated))
}

// renameError turns a Spotify refusal into something a person can act on, and
// every branch states what is true of the playlist afterwards. The second
// return value is whether this is the outcome Encore cannot see through, which
// is the one — and the only one — worth a log line: the caller is being told
// that nobody knows what happened to their playlist.
//
// The last branch is the one that matters most and is the easiest to get
// wrong. A transport error means the request may have reached Spotify and the
// answer may have been lost; "the playlist has not been renamed" reads like
// the cautious thing to say and is in fact an unverified claim about somebody
// else's account. Encore says what it knows, which is nothing, and says that
// trying again is safe — which is true, because a rename is idempotent.
//
// The symmetry matters as much as the caution, which is what the answered-4xx
// branch is for. "Encore did not get an answer" is a positive assertion about
// Encore's own state, and for a status it read and branched on it is simply
// false — it would also send somebody to check a playlist Encore already knows
// was not renamed, and invite a retry that will fail identically for ever.
// Admitting ignorance is only the safe answer while it is the true one.
func renameError(err error) (error, bool) {
	var paused *spotify.PausedError
	if errors.As(err, &paused) {
		return ErrConflictf(
			"Spotify is rate limiting this instance until %s, so it would not accept the "+
				"rename. Your listening data is unaffected and the playlist still has the "+
				"name it had before; try again after that.",
			paused.Until.UTC().Format(time.RFC3339)), false
	}
	if apiErr, ok := spotify.AsAPIError(err); ok {
		switch {
		case apiErr.IsForbidden():
			return ErrForbiddenf(
				"Spotify refused the rename. The permission may have been revoked; granting " +
					"it again from Settings restores it. The playlist still has the name it had before."), false
		case apiErr.StatusCode == http.StatusNotFound:
			return ErrNotFoundf(
				"Spotify no longer has that playlist — it may have been deleted from your " +
					"account. Encore still has the definition, so you can build it again."), false
		case apiErr.StatusCode < http.StatusInternalServerError:
			// Spotify answered and refused. Which status it chose is nothing a
			// listener can act on, but "Encore did not get an answer" would be a
			// false account of what happened — and a 4xx never applied the write,
			// so the old name is a fact here rather than a guess.
			return ErrConflictf(
				"Spotify would not accept the rename and did not say why. The playlist still " +
					"has the name it had before. If it keeps happening, signing in again from " +
					"Settings is the usual fix.").WithCause(err), false
		}
	}
	// A 5xx that outlived the retry budget, or no answer at all. The cause is
	// attached rather than dropped, and only here: this is the branch where
	// nobody knows what happened, so it is the one where an operator has nothing
	// else to go on. It never reaches the response — writeError sends Message
	// and logs the chain — and it keeps a caller that simply hung up
	// recognisable as context.Canceled, which writeError answers by writing
	// nothing at all rather than a 409 nobody is left to read.
	return ErrConflictf(
		"Encore did not get an answer from Spotify, so it cannot tell whether the rename " +
			"went through. Open the playlist in Spotify to check — renaming it again is safe " +
			"either way.").WithCause(err), true
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
	sel, err := s.selectPlaylistTracks(ctx, user, stored.Definition)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ids := sel.IDs()
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

	// The description names the date of the last build, so a rebuild has just
	// made the stored one false. Refreshed best-effort: the tracks are already
	// replaced and recorded, and failing a rebuild that succeeded — over a
	// sentence — would be a worse outcome than a description that is one build
	// behind.
	//
	// The description and nothing else. Nobody pressing "rebuild" asked for
	// anything about the name, and a listener who renamed this playlist in the
	// Spotify app has an edit Encore never recorded and could not restore. The
	// description is different in kind: it is Encore's own sentence, whose only
	// factual claim this rebuild has just invalidated. Somebody who rewrote
	// that in Spotify does lose it here, which is the narrower cost of keeping
	// the sentence true.
	if err := s.spotify.UpdatePlaylistDescription(ctx, token, stored.SpotifyID,
		playlistDescription(stored.Definition, now)); err != nil {
		logging.FromContext(ctx).Warn("could not refresh a rebuilt playlist's description",
			"playlist", stored.SpotifyID, logging.Err(err))
	}

	out := toPlaylist(stored)
	out.Matched = sel.Matched
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

// handlePreviewPlaylist answers POST /api/playlists/preview.
//
// Runs a definition and returns what it would put in a playlist, without
// touching Spotify at all.
//
// Deliberately not behind the playlist scope. It writes nothing, and being able
// to see what a definition selects *before* deciding whether to grant Encore
// write access to a Spotify account is the right order for that decision. It is
// also the only way to use the modes whose size cannot be guessed: a minimum
// play count returns however many tracks clear the bar, which is the question
// the mode exists to ask.
func (s *Server) handlePreviewPlaylist(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()
	sel, err := s.selectPlaylistTracks(ctx, user, def)
	if err != nil {
		writeError(w, r, err)
		return
	}

	ids := sel.IDs()
	refs, err := s.resolveRefs(ctx, ids, nil, nil)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := PlaylistPreview{
		Matched: sel.Matched,
		Limit:   def.Limit,
		Tracks:  make([]PlaylistTrack, 0, len(sel.Tracks)),
	}
	for i, entry := range sel.Tracks {
		out.Tracks = append(out.Tracks, PlaylistTrack{
			Rank:     i + 1,
			Track:    refs.trackEntity(entry.TrackID),
			Plays:    entry.Plays,
			MsPlayed: entry.MsPlayed,
		})
	}
	writeJSON(w, r, http.StatusOK, out)
}

// selectPlaylistTracks resolves a definition against the caller's history.
func (s *Server) selectPlaylistTracks(
	ctx context.Context, user domain.User, def domain.PlaylistDefinition,
) (stats.PlaylistSelection, error) {
	first, _, err := s.listens.Bounds(ctx, s.querier, user.ID)
	if err != nil {
		return stats.PlaylistSelection{}, err
	}
	var firstListen time.Time
	if first != nil {
		firstListen = *first
	}
	return s.stats.SelectPlaylistTracks(ctx, s.querier, user.ID, def, def.Range(s.now(), firstListen))
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
	// One sentence for both writes this scope covers. It is now reached by a
	// rename as well as a creation, and "create playlists" shown to somebody
	// renaming one describes a permission they are not being asked for.
	//
	// Deliberately not markNeedsReauth and never retried (see
	// internal/sync/account.go): the listener simply never granted this, so
	// parking their account over it would stop the synchronisation they did
	// grant.
	if !spotify.HasScope(creds.Scopes, spotify.ScopePlaylistPrivate) {
		return "", ErrForbiddenf(
			"Encore needs permission to create and change playlists on your Spotify account. " +
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
