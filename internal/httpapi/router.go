package httpapi

import (
	"net/http"
	"strings"
)

// buildHandler assembles the routing table and wraps it in the middleware chain.
//
// The chain is, outermost first: panic recovery, request id, request log,
// metrics, CORS, security headers and the body limit. Only the /api subtree then
// passes through session resolution and the CSRF check — the operational
// endpoints deliberately do not, so that /healthz never so much as looks at a
// cookie, let alone the database that backs one.
func (s *Server) buildHandler() http.Handler {
	api := http.NewServeMux()
	s.registerAPI(api)

	root := http.NewServeMux()
	root.Handle("/api/", s.session(s.csrf(api)))
	s.registerOperational(root)
	s.route(root, "/", s.handleNoRoute)

	var h http.Handler = root
	h = s.limitBody(h)
	h = s.securityHeaders(h)
	h = s.cors(h)
	h = s.observeMetrics(h)
	h = s.requestLogger(h)
	h = s.requestID(h)
	h = s.recoverer(h)
	return h
}

// route registers a handler and remembers the pattern it was mounted on.
//
// The template, not the concrete path, is what the log line and the metric are
// labelled with: a label carrying a job id or a Spotify id would give the
// metric one time series per entity and eventually cost more than the
// application it observes.
func (s *Server) route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	template := pattern
	if _, path, ok := strings.Cut(pattern, " "); ok {
		template = path
	}
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec := routeFrom(r.Context()); rec != nil {
			rec.template = template
		}
		h(w, r)
	}))
}

// registerAPI mounts every path in docs/api.md.
func (s *Server) registerAPI(mux *http.ServeMux) {
	// Authentication.
	s.route(mux, "GET /api/auth/spotify/login", s.handleLogin)
	s.route(mux, "GET /api/auth/spotify/callback", s.handleCallback)
	s.route(mux, "GET /api/auth/spotify/relink", s.handleRelink)
	s.route(mux, "POST /api/auth/logout", s.handleLogout)

	// Session and account.
	s.route(mux, "GET /api/me", s.handleMe)
	s.route(mux, "PATCH /api/me", s.handleUpdateMe)
	s.route(mux, "DELETE /api/me", s.handleDeleteMe)
	s.route(mux, "GET /api/me/export", s.handleExport)
	s.route(mux, "POST /api/sync/now", s.handleSyncNow)

	// Users and administration.
	s.route(mux, "GET /api/users", s.handleListUsers)
	s.route(mux, "GET /api/admin/settings", s.handleGetSettings)
	s.route(mux, "PATCH /api/admin/settings", s.handleUpdateSettings)
	s.route(mux, "GET /api/admin/users", s.handleAdminListUsers)
	s.route(mux, "PATCH /api/admin/users/{id}", s.handleAdminUpdateUser)
	s.route(mux, "DELETE /api/admin/users/{id}", s.handleAdminDeleteUser)

	// Statistics.
	s.route(mux, "GET /api/stats/summary", s.handleSummary)
	s.route(mux, "GET /api/stats/top/tracks", s.handleTopTracks)
	s.route(mux, "GET /api/stats/top/artists", s.handleTopArtists)
	s.route(mux, "GET /api/stats/top/albums", s.handleTopAlbums)
	s.route(mux, "GET /api/stats/timeline", s.handleTimeline)
	s.route(mux, "GET /api/stats/repartition/hour", s.handleHourRepartition)
	s.route(mux, "GET /api/stats/repartition/weekday", s.handleWeekdayRepartition)
	s.route(mux, "GET /api/stats/repartition/heatmap", s.handleHeatmap)
	s.route(mux, "GET /api/stats/sessions", s.handleSessions)
	s.route(mux, "GET /api/stats/discovery", s.handleDiscovery)
	s.route(mux, "GET /api/stats/streaks", s.handleStreaks)
	s.route(mux, "GET /api/stats/compare", s.handleCompare)
	s.route(mux, "GET /api/stats/year-in-review", s.handleYearInReview)
	s.route(mux, "GET /api/stats/extras", s.handleExtras)
	s.route(mux, "GET /api/stats/affinity/{userId}", s.handleAffinity)

	// Entities.
	s.route(mux, "GET /api/tracks/{id}", s.handleTrack)
	s.route(mux, "GET /api/artists/{id}", s.handleArtist)
	s.route(mux, "GET /api/albums/{id}", s.handleAlbum)
	s.route(mux, "GET /api/search", s.handleSearch)
	s.route(mux, "GET /api/status", s.handleStatus)

	// Sharing. The first three belong to the owner; the last is the only path in
	// Encore that answers with somebody's data and no session, which is why it
	// composes its own fixed payload rather than reusing a statistics handler.
	s.route(mux, "GET /api/shares", s.handleListShares)
	s.route(mux, "POST /api/shares", s.handleCreateShare)
	s.route(mux, "DELETE /api/shares/{id}", s.handleRevokeShare)
	s.route(mux, "GET /api/share/{token}", s.handleSharedStats)

	// Playlists. The write scope is asked for only here, and only when somebody
	// uses the feature: Encore's default grant stays read-only.
	s.route(mux, "GET /api/auth/spotify/playlists", s.handleAuthorizePlaylists)
	s.route(mux, "GET /api/playlists", s.handleListPlaylists)
	s.route(mux, "POST /api/playlists", s.handleCreatePlaylist)
	s.route(mux, "POST /api/playlists/{id}/rebuild", s.handleRebuildPlaylist)
	s.route(mux, "DELETE /api/playlists/{id}", s.handleForgetPlaylist)

	// Listening history.
	s.route(mux, "GET /api/history", s.handleHistory)

	// Artist blacklist.
	s.route(mux, "GET /api/blacklist", s.handleListBlacklist)
	s.route(mux, "POST /api/blacklist", s.handleAddBlacklist)
	s.route(mux, "DELETE /api/blacklist/{artistId}", s.handleRemoveBlacklist)

	// Imports.
	s.route(mux, "POST /api/imports", s.handleCreateImport)
	s.route(mux, "GET /api/imports", s.handleListImports)
	s.route(mux, "GET /api/imports/{id}", s.handleGetImport)
	s.route(mux, "DELETE /api/imports/{id}", s.handleDeleteImport)
	s.route(mux, "POST /api/imports/{id}/cancel", s.handleCancelImport)
	s.route(mux, "POST /api/imports/{id}/retry", s.handleRetryImport)
	s.route(mux, "GET /api/imports/{id}/rejects", s.handleImportRejects)

	// Anything else under /api answers with the API's own error envelope rather
	// than net/http's plain-text page, so a client only ever has to parse one
	// shape.
	s.route(mux, "/api/", s.handleNoRoute)
}

// registerOperational mounts the endpoints a scraper and an orchestrator use.
// They live outside /api so they can be probed without touching the API surface.
func (s *Server) registerOperational(mux *http.ServeMux) {
	s.route(mux, "GET /healthz", s.handleHealthz)
	s.route(mux, "GET /readyz", s.handleReadyz)
	s.route(mux, "GET /metrics", s.handleMetrics)
}

// handleNoRoute answers a path nothing is mounted on.
func (s *Server) handleNoRoute(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, ErrNotFoundf("No route matches this request."))
}
