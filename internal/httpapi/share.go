package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/crypto"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
)

// shareTopLimit is how many entries of each kind a shared page carries.
//
// Fixed rather than paged on purpose. A share is one page a visitor reads, not
// an API somebody browses, and a pagination parameter is one more thing a viewer
// could turn into a way to walk the whole catalogue.
const shareTopLimit = 25

// handleCreateShare answers POST /api/shares.
//
// The token is returned exactly once, in this response. Only its hash is stored,
// so nobody — including whoever runs the instance — can recover it afterwards.
// Losing it means revoking the link and making another, which is the correct
// outcome for a bearer credential.
func (s *Server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var body CreateShareRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	link := domain.ShareLink{Label: strings.TrimSpace(body.Label), Days: body.Days}
	if body.From != nil {
		link.From = body.From.UTC()
	}
	if body.To != nil {
		link.To = body.To.UTC()
	}
	if body.ExpiresAt != nil {
		link.ExpiresAt = body.ExpiresAt.UTC()
	}

	now := s.now()
	if err := domain.ValidateShare(link.Label, link.From, link.To, link.Days, link.ExpiresAt, now); err != nil {
		writeError(w, r, err)
		return
	}

	token, err := crypto.NewToken()
	if err != nil {
		writeError(w, r, err)
		return
	}

	created, err := s.shares.Create(r.Context(), s.querier, user.ID, crypto.HashToken(token), link)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := shareDTO(created, s.cfg.Instance.WebURL, now)
	out.URL = shareURL(s.cfg.Instance.WebURL, token)
	out.Token = token
	writeJSON(w, r, http.StatusCreated, out)
}

// handleListShares answers GET /api/shares.
func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	links, err := s.shares.ListForUser(r.Context(), s.querier, user.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	now := s.now()
	out := make([]ShareResponse, 0, len(links))
	for _, l := range links {
		// No URL and no token: the plaintext is gone, and a listing that pretended
		// otherwise would be inviting somebody to copy a link that does not work.
		out = append(out, shareDTO(l, s.cfg.Instance.WebURL, now))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// handleRevokeShare answers DELETE /api/shares/{id}.
func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, ErrInvalidRequest("That is not a valid link id.", nil))
		return
	}
	if err := s.shares.Revoke(r.Context(), s.querier, user.ID, id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSharedStats answers GET /api/share/{token}, with no session at all.
//
// This is the only handler in Encore that serves one person's data to an
// unauthenticated caller, so what it may reach is decided here and nowhere else:
// it composes a fixed set of aggregates and has no path to the listening
// history, to another user, or to anything the link's owner did not share.
//
// The range comes from the link and never from the query string. A viewer who
// could widen it would be choosing what to see rather than being shown it.
func (s *Server) handleSharedStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.PathValue("token")
	if token == "" {
		writeError(w, r, ErrNotFoundf("That link does not exist, or has been revoked."))
		return
	}

	link, owner, err := s.shares.Resolve(ctx, s.querier, crypto.HashToken(token), s.now())
	if err != nil {
		// Revoked, expired, deactivated and never-existed are one answer. A
		// visitor learns that the link does not work and nothing else.
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, r, ErrNotFoundf("That link does not exist, or has been revoked."))
			return
		}
		writeError(w, r, err)
		return
	}

	// Never indexed. A shared link is unguessable, which is worth nothing if a
	// crawler publishes it.
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")

	first, _, err := s.listens.Bounds(ctx, s.querier, owner.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// An all-time link starts where the history does, so the charts do not open
	// with years of empty buckets before the owner's first listen.
	var firstListen time.Time
	if first != nil {
		firstListen = *first
	}
	rng := link.Range(s.now(), firstListen)
	tz := owner.Timezone

	out := SharedStatsResponse{
		Label:       link.Label,
		DisplayName: owner.DisplayName,
		AvatarURL:   owner.AvatarURL,
		Timezone:    tz,
		Rolling:     link.Rolling(),
		RangeDays:   link.Days,
		From:        rng.From.UTC(),
		To:          rng.To.UTC(),
	}

	summary, err := s.stats.Summary(ctx, s.querier, owner.ID, rng, tz)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out.Summary = toSummary(summary)

	tracks, err := s.stats.TopTracks(ctx, s.querier, owner.ID, rng, tz, shareTopLimit, 0)
	if err != nil {
		writeError(w, r, err)
		return
	}
	artists, err := s.stats.TopArtists(ctx, s.querier, owner.ID, rng, tz, shareTopLimit, 0)
	if err != nil {
		writeError(w, r, err)
		return
	}
	albums, err := s.stats.TopAlbums(ctx, s.querier, owner.ID, rng, tz, shareTopLimit, 0)
	if err != nil {
		writeError(w, r, err)
		return
	}
	refs, err := s.resolveRefs(ctx, topIDs(tracks), topIDs(albums), topIDs(artists))
	if err != nil {
		writeError(w, r, err)
		return
	}
	out.Tracks = topPage(tracks, refs.trackEntity)
	out.Artists = topPage(artists, refs.artistEntity)
	out.Albums = topPage(albums, refs.albumEntity)

	interval := domain.SuggestInterval(rng)
	out.Interval = string(interval)
	points, err := s.stats.Timeline(ctx, s.querier, owner.ID, rng, tz, interval)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out.Timeline = toTimeline(points)

	hours, err := s.stats.HourRepartition(ctx, s.querier, owner.ID, rng, tz)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out.Hours = toHourBuckets(hours)

	weekdays, err := s.stats.WeekdayRepartition(ctx, s.querier, owner.ID, rng, tz)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out.Weekdays = toWeekdayBuckets(weekdays)

	// Best effort, and after everything that matters has succeeded: a view
	// counter is not worth failing a page over.
	if err := s.shares.Touch(ctx, s.querier, link.ID); err != nil {
		logging.FromContext(ctx).Warn("could not record a share view", logging.Err(err))
	}

	writeJSON(w, r, http.StatusOK, out)
}

// shareURL is where a visitor opens a link. It points at the web client, which
// is what a person is given, rather than at the API path behind it.
func shareURL(webURL, token string) string {
	return strings.TrimRight(webURL, "/") + "/share/" + token
}

func shareDTO(l domain.ShareLink, webURL string, now time.Time) ShareResponse {
	out := ShareResponse{
		ID:        l.ID.String(),
		Label:     l.Label,
		Rolling:   l.Rolling(),
		RangeDays: l.Days,
		ViewCount: l.ViewCount,
		CreatedAt: l.CreatedAt.UTC(),
		Active:    l.Active(now),
	}
	if !l.From.IsZero() {
		from := l.From.UTC()
		out.From = &from
	}
	if !l.To.IsZero() {
		to := l.To.UTC()
		out.To = &to
	}
	if !l.ExpiresAt.IsZero() {
		exp := l.ExpiresAt.UTC()
		out.ExpiresAt = &exp
	}
	if !l.LastViewedAt.IsZero() {
		seen := l.LastViewedAt.UTC()
		out.LastViewedAt = &seen
	}
	return out
}
