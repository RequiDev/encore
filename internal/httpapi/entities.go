package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/stats"
)

// searchLimit bounds the type-ahead. The contains arm of the catalogue search is
// not index-driven, so the ceiling is deliberately low.
const (
	defaultSearchLimit = 10
	maxSearchLimit     = 50
)

// catalogRefs is the catalogue rows needed to render one response.
//
// Statistics answer in identifiers — the analytic queries have no business
// joining artwork and names — so every endpoint that returns entities resolves
// its identifiers here, in one batched pass per entity type rather than one
// query per row.
type catalogRefs struct {
	tracks  map[string]domain.Track
	albums  map[string]domain.Album
	artists map[string]domain.Artist
}

// resolveRefs loads the catalogue rows for a set of identifiers, following
// tracks to their albums and both to their artists.
func (s *Server) resolveRefs(ctx context.Context, trackIDs, albumIDs, artistIDs []string) (*catalogRefs, error) {
	refs := &catalogRefs{
		tracks:  map[string]domain.Track{},
		albums:  map[string]domain.Album{},
		artists: map[string]domain.Artist{},
	}

	if len(trackIDs) > 0 {
		tracks, err := s.catalog.GetTracks(ctx, s.querier, trackIDs)
		if err != nil {
			return nil, err
		}
		refs.tracks = tracks
		for _, t := range tracks {
			if t.AlbumID != "" {
				albumIDs = append(albumIDs, t.AlbumID)
			}
			artistIDs = append(artistIDs, t.ArtistIDs...)
		}
	}

	if len(albumIDs) > 0 {
		albums, err := s.catalog.GetAlbums(ctx, s.querier, albumIDs)
		if err != nil {
			return nil, err
		}
		refs.albums = albums
		for _, a := range albums {
			artistIDs = append(artistIDs, a.ArtistIDs...)
		}
	}

	if len(artistIDs) > 0 {
		artists, err := s.catalog.GetArtists(ctx, s.querier, artistIDs)
		if err != nil {
			return nil, err
		}
		refs.artists = artists
	}
	return refs, nil
}

// artistRefs renders a track's or album's credited artists, in the order the
// catalogue records them, skipping any the catalogue has never seen.
func (c *catalogRefs) artistRefs(ids []string) []ArtistRef {
	out := make([]ArtistRef, 0, len(ids))
	for _, id := range ids {
		if a, ok := c.artists[id]; ok {
			out = append(out, toArtistRef(a))
		}
	}
	return out
}

// albumRef renders a track's album, or nil when the track has none — a single
// released outside an album, or a track whose metadata has not been fetched yet.
func (c *catalogRefs) albumRef(id string) *AlbumRef {
	if id == "" {
		return nil
	}
	a, ok := c.albums[id]
	if !ok {
		return nil
	}
	ref := toAlbumRef(a)
	return &ref
}

// trackRef renders a track, or nil when the identifier addresses nothing.
func (c *catalogRefs) trackRef(id string) *TrackRef {
	t, ok := c.tracks[id]
	if !ok {
		return nil
	}
	return &TrackRef{
		ID:         t.ID,
		Name:       t.Name,
		DurationMs: t.DurationMs,
		Explicit:   t.Explicit,
		Album:      c.albumRef(t.AlbumID),
		Artists:    c.artistRefs(t.ArtistIDs),
	}
}

// trackEntity is trackRef for the places a null entity would break the shape of
// a ranked list. Every listened track has a catalogue row, created by ingestion
// before the listen is stored, so the bare fallback is belt and braces against a
// row deleted underneath a running query rather than an expected outcome.
func (c *catalogRefs) trackEntity(id string) TrackRef {
	if ref := c.trackRef(id); ref != nil {
		return *ref
	}
	return TrackRef{ID: id, Artists: []ArtistRef{}}
}

// albumEntity is albumRef with the same guarantee as trackEntity.
func (c *catalogRefs) albumEntity(id string) AlbumRef {
	if ref := c.albumRef(id); ref != nil {
		return *ref
	}
	return AlbumRef{ID: id}
}

// artistEntity is one artist reference with the same guarantee as trackEntity.
func (c *catalogRefs) artistEntity(id string) ArtistRef {
	if a, ok := c.artists[id]; ok {
		return toArtistRef(a)
	}
	return ArtistRef{ID: id}
}

// fullAlbum renders an album with its own artists, for the album page.
func (c *catalogRefs) fullAlbum(a domain.Album) Album {
	return Album{
		AlbumRef:    toAlbumRef(a),
		AlbumType:   a.AlbumType,
		TotalTracks: a.TotalTracks,
		Artists:     c.artistRefs(a.ArtistIDs),
	}
}

// --- ranked-list helpers ---------------------------------------------------

// countEntries turns the statistics layer's plain ranked counts into contract
// entries. These lists — an artist's own top tracks, say — are not something the
// caller's history has ever ranked, so there is no previous position to compare
// against and previousRank is null throughout.
func countEntries[T any](counts []stats.EntryCount, entity func(string) T) []TopEntry[T] {
	out := make([]TopEntry[T], 0, len(counts))
	for i, c := range counts {
		out = append(out, TopEntry[T]{
			Entity:   entity(c.ID),
			Plays:    c.Plays,
			MsPlayed: c.MsPlayed,
			Rank:     i + 1,
		})
	}
	return out
}

// countIDs collects the identifiers of a ranked count list.
func countIDs(counts []stats.EntryCount) []string {
	ids := make([]string, 0, len(counts))
	for _, c := range counts {
		ids = append(ids, c.ID)
	}
	return ids
}

// --- handlers --------------------------------------------------------------

// handleTrack answers GET /api/tracks/{id}.
func (s *Server) handleTrack(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := parseSpotifyIDPath(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	if _, err := s.catalog.GetTrack(ctx, s.querier, id); err != nil {
		writeError(w, r, err)
		return
	}
	st, err := s.stats.TrackStats(ctx, s.querier, user.ID, tr, user.Timezone, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	refs, err := s.resolveRefs(ctx, []string{id}, nil, nil)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, TrackDetail{
		Track: refs.trackEntity(id),
		Stats: toEntityStats(st.EntityStats),
	})
}

// handleArtist answers GET /api/artists/{id}.
func (s *Server) handleArtist(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := parseSpotifyIDPath(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	artist, err := s.catalog.GetArtist(ctx, s.querier, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	st, err := s.stats.ArtistStats(ctx, s.querier, user.ID, tr, user.Timezone, id, stats.EntityTopLimit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// The hour-of-day figures are the caller's own listening clock over the same
	// range: the statistics layer computes repartitions across the whole library,
	// and inventing an artist-scoped variant here would mean writing SQL in the
	// HTTP layer, which is precisely what this package must not do.
	hours, err := s.stats.HourRepartition(ctx, s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	blacklisted, err := s.catalog.BlacklistedArtistIDs(ctx, s.querier, user.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	refs, err := s.resolveRefs(ctx, countIDs(st.TopTracks), countIDs(st.TopAlbums), []string{id})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, ArtistDetail{
		Artist:          toArtist(artist),
		Stats:           toEntityStats(st.EntityStats),
		Share:           st.MsShare,
		TopTracks:       countEntries(st.TopTracks, refs.trackEntity),
		TopAlbums:       countEntries(st.TopAlbums, refs.albumEntity),
		HourRepartition: toHourBuckets(hours),
		Blacklisted:     containsString(blacklisted, id),
	})
}

// handleAlbum answers GET /api/albums/{id}.
func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := parseSpotifyIDPath(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	album, err := s.catalog.GetAlbum(ctx, s.querier, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	st, err := s.stats.AlbumStats(ctx, s.querier, user.ID, tr, user.Timezone, id, stats.EntityTopLimit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	refs, err := s.resolveRefs(ctx, countIDs(st.TopTracks), []string{id}, album.ArtistIDs)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, AlbumDetail{
		Album:     refs.fullAlbum(album),
		Stats:     toEntityStats(st.EntityStats),
		TopTracks: countEntries(st.TopTracks, refs.trackEntity),
	})
}

// handleSearch answers GET /api/search.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if _, err := requireUser(r); err != nil {
		writeError(w, r, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, r, ErrFieldInvalid("q", `"q" is required.`))
		return
	}
	limit, err := parseLimit(r, defaultSearchLimit, maxSearchLimit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	found, err := s.catalog.Search(ctx, s.querier, query, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}

	trackIDs := make([]string, 0, len(found.Tracks))
	for _, t := range found.Tracks {
		trackIDs = append(trackIDs, t.ID)
	}
	refs, err := s.resolveRefs(ctx, trackIDs, nil, nil)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := SearchResponse{
		Artists: make([]ArtistRef, 0, len(found.Artists)),
		Albums:  make([]AlbumRef, 0, len(found.Albums)),
		Tracks:  make([]TrackRef, 0, len(found.Tracks)),
	}
	for _, a := range found.Artists {
		out.Artists = append(out.Artists, toArtistRef(a))
	}
	for _, a := range found.Albums {
		out.Albums = append(out.Albums, toAlbumRef(a))
	}
	for _, t := range found.Tracks {
		out.Tracks = append(out.Tracks, refs.trackEntity(t.ID))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// containsString reports whether a sorted-or-not slice holds a value. The
// blacklists this is used on are a handful of entries, so a scan is cheaper than
// building an index.
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
