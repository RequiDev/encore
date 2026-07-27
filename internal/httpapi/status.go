package httpapi

import (
	"net/http"

	"github.com/RequiDev/encore/internal/logging"
)

// handleStatus answers GET /api/status.
//
// It exists so that "why are the artists blank, and is anything happening?" has
// an answer inside the application rather than only in a terminal on the server.
// Everything here is instance-wide operational state — how much of the shared
// catalogue has been fetched, and whether Spotify is currently holding Encore
// back — and none of it is anyone's listening data, so any signed-in user may
// read it.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if _, err := requireUser(r); err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	progress, err := s.catalog.CatalogueProgress(ctx, s.querier)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := StatusResponse{
		Catalogue: progress,
		Metadata: MetadataStatus{
			Outstanding: progress.Outstanding(),
			Complete:    progress.Complete(),
		},
	}

	// A recorded pause is the single most useful fact when metadata has stopped
	// arriving, and the only one a user cannot infer from the counts alone.
	//
	// A failure to read it is logged rather than returned: the counts above are
	// the bulk of the answer and are already in hand, and losing the whole panel
	// over one optional setting would leave the user with less than they had.
	pausedUntil, err := s.settings.SpotifyPausedUntil(ctx, s.querier)
	switch {
	case err != nil:
		s.log.Warn("could not read the Spotify pause for the status endpoint", logging.Err(err))
	case !pausedUntil.IsZero() && pausedUntil.After(s.now()):
		until := pausedUntil.UTC()
		out.Metadata.PausedUntil = &until
		out.Metadata.Paused = true
	}

	writeJSON(w, r, http.StatusOK, out)
}
