package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/requi/encore/internal/crypto"
	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/logging"
)

// handleMe answers GET /api/me, the bootstrap call the client makes on load.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	auth, ok := authFrom(r.Context())
	if !ok {
		writeError(w, r, ErrUnauthorized())
		return
	}
	ctx := r.Context()

	connection, err := s.spotifyConnection(r, auth.user)
	if err != nil {
		writeError(w, r, err)
		return
	}
	registrations, err := s.settings.RegistrationsEnabled(ctx, s.querier)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, MeResponse{
		User:      toUser(auth.user),
		Spotify:   connection,
		CSRFToken: auth.session.CSRFToken,
		Instance:  InstanceInfo{RegistrationsEnabled: registrations, Version: s.version},
	})
}

// spotifyConnection describes a user's grant without ever touching the tokens
// it holds.
func (s *Server) spotifyConnection(r *http.Request, user domain.User) (SpotifyConnection, error) {
	creds, err := s.credentials.Get(r.Context(), s.querier, user.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// An account with no grant is one that has to authorise before it can
			// sync, which is exactly what needs_reauth means.
			return SpotifyConnection{
				Connected: false,
				SyncState: string(domain.SyncStateNeedsReauth),
				Scopes:    []string{},
			}, nil
		}
		return SpotifyConnection{}, err
	}
	return SpotifyConnection{
		Connected:     true,
		SyncState:     string(creds.SyncState),
		LastSyncAt:    tsPtr(creds.LastSyncAt),
		LastSyncError: creds.LastSyncError,
		Scopes:        nonNil(creds.Scopes),
	}, nil
}

// updateMeRequest is the body of PATCH /api/me.
type updateMeRequest struct {
	Timezone *string `json:"timezone"`
}

// handleUpdateMe answers PATCH /api/me.
func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var body updateMeRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Timezone == nil {
		writeError(w, r, ErrFieldInvalid("timezone", `"timezone" is required.`))
		return
	}

	updated, err := s.users.SetTimezone(r.Context(), s.querier, user.ID, strings.TrimSpace(*body.Timezone))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toUser(updated))
}

// deleteMeRequest is the body of DELETE /api/me.
type deleteMeRequest struct {
	Confirm string `json:"confirm"`
}

// handleDeleteMe answers DELETE /api/me.
//
// This is a hard delete: the user, their listens, sessions, credentials and
// import bookkeeping all go, by foreign-key cascade. The shared music catalogue
// is untouched, because it holds nothing personal. Confirmation is the caller's
// own Spotify username, which is a thing they have to look up rather than a
// button they can hit by accident.
func (s *Server) handleDeleteMe(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var body deleteMeRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !crypto.EqualTokens(strings.TrimSpace(body.Confirm), user.SpotifyUserID) {
		writeError(w, r, ErrFieldInvalid("confirm",
			"To delete your account, confirm with your Spotify username."))
		return
	}
	ctx := r.Context()

	// The uploaded exports live on disk and are not reachable from the database
	// once the rows cascade away, so they are removed first. A failure here is
	// logged rather than fatal: leaving a file behind is far better than
	// refusing to delete an account somebody asked to be rid of.
	s.removeAllImportFiles(r, user)

	if err := s.users.DeleteUser(ctx, s.querier, user.ID); err != nil {
		writeError(w, r, err)
		return
	}
	s.touched.forget(user.ID)
	s.clearAuthCookies(w)
	writeNoContent(w)
}

// removeAllImportFiles deletes every upload a user still has spooled.
func (s *Server) removeAllImportFiles(r *http.Request, user domain.User) {
	ctx := r.Context()
	lg := logging.FromContext(ctx)

	for offset := 0; ; offset += maxPageLimit {
		jobs, total, err := s.imports.ListJobsForUser(ctx, s.querier, user.ID, maxPageLimit, offset)
		if err != nil {
			lg.Warn("could not list import jobs before deleting the account", logging.Err(err))
			return
		}
		for _, job := range jobs {
			if err := s.intake.RemoveJobFiles(ctx, job.ID); err != nil {
				lg.Warn("could not remove the uploads of an import job", logging.Err(err))
			}
		}
		if len(jobs) == 0 || int64(offset+len(jobs)) >= total {
			return
		}
	}
}

// handleSyncNow answers POST /api/sync/now.
func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// A poll already in flight is a conflict rather than a second poll: two
	// concurrent runs would spend the same rate-limit budget fetching the same
	// page twice.
	if !s.syncing.acquire(user.ID) {
		writeError(w, r, ErrConflictf("A synchronisation is already running for your account."))
		return
	}
	defer s.syncing.release(user.ID)

	if err := s.syncNow(r.Context(), user.ID); err != nil {
		writeError(w, r, err)
		return
	}
	writeNoContent(w)
}

// --- request bodies --------------------------------------------------------

// decodeJSON reads a request body into v.
//
// The body is already capped by the body-limit middleware, so this only has to
// turn the failures into the contract's errors: an oversized body is a 413 and
// anything malformed is a 400 that says so without echoing what was sent.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return ErrInvalidRequest("A JSON body is required.", nil)
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return ErrTooLarge(tooLarge.Limit)
		}
		if errors.Is(err, io.EOF) {
			return ErrInvalidRequest("A JSON body is required.", nil)
		}
		return ErrInvalidRequest("The request body is not valid JSON.", nil).WithCause(err)
	}
	// A second document in the same body is a sign the caller is confused about
	// the endpoint, and accepting it would mean silently ignoring half the request.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return ErrInvalidRequest("The request body must hold exactly one JSON document.", nil)
	}
	return nil
}

// jsonString renders a Go string as a JSON string literal, for the streaming
// writers that build a document by hand.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string fails only on invalid UTF-8, which the
		// database columns cannot hold.
		return `""`
	}
	return string(b)
}

// contentDisposition builds the attachment header for a download.
func contentDisposition(name string) string {
	return fmt.Sprintf("attachment; filename=%q", name)
}
