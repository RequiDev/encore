package httpapi

import (
	"errors"
	"net/http"

	"github.com/RequiDev/encore/internal/domain"
)

// scopeReadPlaybackState is the grant the now-playing poller needs. Declared
// here rather than imported so this package keeps depending only on domain and
// config; it is the same literal config.DefaultScopes() lists.
const scopeReadPlaybackState = "user-read-playback-state"

// handleNowPlaying answers GET /api/nowplaying.
//
// It reads the stored observation and makes no Spotify request of its own. That
// is deliberate and load bearing: the dashboard polls this endpoint from every
// open tab, and a handler that fetched would multiply the feature's cost by the
// number of tabs somebody happens to have open — and would put that traffic on
// whichever budget the handler used, which is exactly the coupling this phase
// exists to avoid.
//
// Never reachable without a session, and never composed into a share link. Real
// time presence is precisely the concern internal/domain/share.go was written
// around: a share exposes what somebody listens to, never when they are awake.
func (s *Server) handleNowPlaying(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	out := NowPlayingResponse{Enabled: s.cfg.NowPlaying.Enabled()}
	if out.Enabled {
		out.IntervalSeconds = int(s.cfg.NowPlaying.Interval.Seconds())
	}

	// The scope is read from the stored grant rather than inferred from whether
	// a row exists. An account that simply has not been reached yet must not be
	// told to reconnect Spotify: that is a prompt to fix something that is not
	// broken.
	creds, err := s.credentials.Get(ctx, s.querier, user.ID)
	switch {
	case err == nil:
		out.ScopeGranted = creds.HasScope(scopeReadPlaybackState)
	case errors.Is(err, domain.ErrNotFound):
		// No grant at all. Not connected is a state /api/me already reports and
		// the shell already renders; here it simply means no scope.
	default:
		writeError(w, r, err)
		return
	}

	row, err := s.nowPlaying.Get(ctx, s.querier, user.ID)
	if errors.Is(err, domain.ErrNotFound) {
		// Never checked. Everything stays zero, and Observation stays nil,
		// which is the payload's way of saying so.
		writeJSON(w, r, http.StatusOK, out)
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}

	checked := row.CheckedAt.UTC()
	out.CheckedAt = &checked
	out.Failed = row.Failed
	out.Observation = toNowPlayingObservation(row)

	writeJSON(w, r, http.StatusOK, out)
}

// toNowPlayingObservation maps a stored row to the payload, or nil when there
// has never been a successful look.
//
// The nil is the point. domain.PlaybackUnknown is not a kind of silence, and
// returning an observation carrying it would hand the client a state value to
// misread as one.
func toNowPlayingObservation(n domain.NowPlaying) *NowPlayingObservation {
	if !n.Observed() {
		return nil
	}
	out := &NowPlayingObservation{
		ObservedAt: n.ObservedAt.UTC(),
		State:      string(n.State),
		Kind:       string(n.Kind),
		Title:      n.Title,
		Artist:     n.Artist,
		ProgressMs: n.ProgressMs,
		DurationMs: n.DurationMs,
		DeviceName: n.DeviceName,
	}
	// Only a track Encore's own catalogue holds is offered as a link. Spotify
	// names a track the instant it starts playing; Encore learns about it when
	// enrichment gets round to it, and a link in between would 404.
	if n.TrackKnown {
		out.TrackID = n.TrackID
	}
	return out
}
