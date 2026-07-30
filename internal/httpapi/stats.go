package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/stats"
)

// affinityListLimit is how many shared entities of each kind a comparison
// returns. It is a fixed page rather than a caller-supplied one because the
// score, not the list, is the point of the endpoint.
const affinityListLimit = 20

// callerAndRange is the prologue every range-scoped statistic shares: identify
// the caller, then read the window they asked about.
func (s *Server) callerAndRange(r *http.Request) (domain.User, domain.TimeRange, error) {
	user, err := requireUser(r)
	if err != nil {
		return domain.User{}, domain.TimeRange{}, err
	}
	tr, err := parseRange(r, user, s.now())
	if err != nil {
		return domain.User{}, domain.TimeRange{}, err
	}
	return user, tr, nil
}

// handleSummary answers GET /api/stats/summary.
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	summary, err := s.stats.Summary(r.Context(), s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toSummary(summary))
}

// topQuery is one of the three ranked lists, parameterised by the statistics
// call and the entity kind its identifiers address.
type topQuery func(ctx context.Context, userID domain.User, tr domain.TimeRange, limit, offset int) (stats.TopPage, error)

// handleTopTracks answers GET /api/stats/top/tracks.
func (s *Server) handleTopTracks(w http.ResponseWriter, r *http.Request) {
	s.serveTop(w, r,
		func(ctx context.Context, u domain.User, tr domain.TimeRange, limit, offset int) (stats.TopPage, error) {
			return s.stats.TopTracks(ctx, s.querier, u.ID, tr, u.Timezone, limit, offset)
		},
		func(ctx context.Context, page stats.TopPage) (any, error) {
			refs, err := s.resolveRefs(ctx, topIDs(page), nil, nil)
			if err != nil {
				return nil, err
			}
			return topPage(page, refs.trackEntity), nil
		})
}

// handleTopArtists answers GET /api/stats/top/artists.
func (s *Server) handleTopArtists(w http.ResponseWriter, r *http.Request) {
	s.serveTop(w, r,
		func(ctx context.Context, u domain.User, tr domain.TimeRange, limit, offset int) (stats.TopPage, error) {
			return s.stats.TopArtists(ctx, s.querier, u.ID, tr, u.Timezone, limit, offset)
		},
		func(ctx context.Context, page stats.TopPage) (any, error) {
			refs, err := s.resolveRefs(ctx, nil, nil, topIDs(page))
			if err != nil {
				return nil, err
			}
			return topPage(page, refs.artistEntity), nil
		})
}

// handleTopAlbums answers GET /api/stats/top/albums.
func (s *Server) handleTopAlbums(w http.ResponseWriter, r *http.Request) {
	s.serveTop(w, r,
		func(ctx context.Context, u domain.User, tr domain.TimeRange, limit, offset int) (stats.TopPage, error) {
			return s.stats.TopAlbums(ctx, s.querier, u.ID, tr, u.Timezone, limit, offset)
		},
		func(ctx context.Context, page stats.TopPage) (any, error) {
			refs, err := s.resolveRefs(ctx, nil, topIDs(page), nil)
			if err != nil {
				return nil, err
			}
			return topPage(page, refs.albumEntity), nil
		})
}

// serveTop is the shared body of the three ranked lists.
func (s *Server) serveTop(w http.ResponseWriter, r *http.Request, query topQuery, render func(context.Context, stats.TopPage) (any, error)) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	limit, offset, err := parsePage(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	page, err := query(ctx, user, tr, limit, offset)
	if err != nil {
		writeError(w, r, err)
		return
	}
	body, err := render(ctx, page)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, body)
}

// topIDs collects the identifiers a ranked page addresses.
func topIDs(page stats.TopPage) []string {
	ids := make([]string, 0, len(page.Entries))
	for _, e := range page.Entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// topPage renders a ranked page with its entities resolved.
func topPage[T any](page stats.TopPage, entity func(string) T) Page[TopEntry[T]] {
	items := make([]TopEntry[T], 0, len(page.Entries))
	for _, e := range page.Entries {
		items = append(items, TopEntry[T]{
			Entity:       entity(e.ID),
			Plays:        e.Plays,
			MsPlayed:     e.MsPlayed,
			Rank:         e.Rank,
			PreviousRank: previousRank(e.PreviousRank),
		})
	}
	return Page[TopEntry[T]]{Items: items, Total: page.Total}
}

// handleTimeline answers GET /api/stats/timeline.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	interval, err := parseInterval(r, tr)
	if err != nil {
		writeError(w, r, err)
		return
	}
	points, err := s.stats.Timeline(r.Context(), s.querier, user.ID, tr, user.Timezone, interval)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, TimelineResponse{
		Interval: string(interval),
		Buckets:  toTimeline(points),
	})
}

// handleHourRepartition answers GET /api/stats/repartition/hour.
func (s *Server) handleHourRepartition(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	buckets, err := s.stats.HourRepartition(r.Context(), s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toHourBuckets(buckets))
}

// handleWeekdayRepartition answers GET /api/stats/repartition/weekday.
func (s *Server) handleWeekdayRepartition(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	buckets, err := s.stats.WeekdayRepartition(r.Context(), s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toWeekdayBuckets(buckets))
}

// handleHeatmap answers GET /api/stats/repartition/heatmap.
func (s *Server) handleHeatmap(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cells, err := s.stats.HourWeekdayHeatmap(r.Context(), s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]HeatmapCell, 0, len(cells))
	for _, c := range cells {
		out = append(out, HeatmapCell{Weekday: c.Weekday, Hour: c.Hour, Plays: c.Plays, MsPlayed: c.MsPlayed})
	}
	writeJSON(w, r, http.StatusOK, out)
}

// toWeekdayBuckets renders the seven days of the local week.
func toWeekdayBuckets(buckets []stats.WeekdayBucket) []RepartitionBucket {
	out := make([]RepartitionBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, RepartitionBucket{Key: b.Weekday, Plays: b.Plays, MsPlayed: b.MsPlayed})
	}
	return out
}

// toHourBuckets renders the 24 hours of the local day.
func toHourBuckets(buckets []stats.HourBucket) []RepartitionBucket {
	out := make([]RepartitionBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, RepartitionBucket{Key: b.Hour, Plays: b.Plays, MsPlayed: b.MsPlayed})
	}
	return out
}

// handleSessions answers GET /api/stats/sessions.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	limit, err := parseLimit(r, 10, maxPageLimit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	sessions, err := s.stats.LongestSessions(ctx, s.querier, user.ID, tr, domain.SessionGap, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	body, err := s.renderSessions(ctx, sessions)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, body)
}

// renderSessions resolves the track lists of a set of sessions in one pass.
func (s *Server) renderSessions(ctx context.Context, sessions []domain.ListeningSession) ([]ListeningSession, error) {
	var ids []string
	for _, sess := range sessions {
		ids = append(ids, sess.TrackIDs...)
	}
	refs, err := s.resolveRefs(ctx, ids, nil, nil)
	if err != nil {
		return nil, err
	}

	out := make([]ListeningSession, 0, len(sessions))
	for _, sess := range sessions {
		tracks := make([]TrackRef, 0, len(sess.TrackIDs))
		for _, id := range sess.TrackIDs {
			if ref := refs.trackRef(id); ref != nil {
				tracks = append(tracks, *ref)
			}
		}
		out = append(out, ListeningSession{
			StartedAt:  ts(sess.StartedAt),
			EndedAt:    ts(sess.EndedAt),
			TrackCount: sess.TrackCount,
			MsPlayed:   sess.MsPlayed,
			Tracks:     tracks,
		})
	}
	return out, nil
}

// handleDiscovery answers GET /api/stats/discovery.
func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	interval, err := parseInterval(r, tr)
	if err != nil {
		writeError(w, r, err)
		return
	}
	points, err := s.stats.Discovery(r.Context(), s.querier, user.ID, tr, user.Timezone, interval)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toDiscovery(points))
}

// handleStreaks answers GET /api/stats/streaks. It is deliberately not
// range-scoped: a streak is a fact about a whole listening history.
func (s *Server) handleStreaks(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	streaks, err := s.stats.Streaks(r.Context(), s.querier, user.ID, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}

	top := make([]Streak, 0, len(streaks.Top))
	for _, st := range streaks.Top {
		if rendered := toStreak(st); rendered != nil {
			top = append(top, *rendered)
		}
	}
	writeJSON(w, r, http.StatusOK, StreaksResponse{
		Current: toStreak(streaks.Current),
		Longest: toStreak(streaks.Longest),
		Top:     top,
	})
}

// handleCompare answers GET /api/stats/compare.
//
// With no parameters at all it compares the default window against the equally
// long window immediately before it, which is the "this month against last
// month" the dashboard opens on.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	now := s.now()

	b, err := parseNamedRange(r, user, now, "bFrom", "bTo")
	if err != nil {
		writeError(w, r, err)
		return
	}
	a := b.Previous()
	q := r.URL.Query()
	if q.Get("aFrom") != "" || q.Get("aTo") != "" {
		if a, err = parseNamedRange(r, user, now, "aFrom", "aTo"); err != nil {
			writeError(w, r, err)
			return
		}
	}

	comparison, err := s.stats.Compare(r.Context(), s.querier, user.ID, a, b, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, CompareResponse{
		A: ComparePeriod{From: ts(a.From), To: ts(a.To), Summary: toSummary(comparison.A)},
		B: ComparePeriod{From: ts(b.From), To: ts(b.To), Summary: toSummary(comparison.B)},
		Delta: CompareDelta{
			Listens:         comparison.Delta.Listens,
			MsPlayed:        comparison.Delta.MsPlayed,
			DistinctTracks:  comparison.Delta.DistinctTracks,
			DistinctArtists: comparison.Delta.DistinctArtists,
			DistinctAlbums:  comparison.Delta.DistinctAlbums,
		},
	})
}

// handleYearInReview answers GET /api/stats/year-in-review.
func (s *Server) handleYearInReview(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	year, err := parseYear(r, s.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	review, err := s.stats.YearInReview(ctx, s.querier, user.ID, year, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Every identifier the retrospective mentions is resolved in one pass:
	// three ranked lists and the track list of the longest session.
	var trackIDs, albumIDs, artistIDs []string
	for _, e := range review.TopTracks {
		trackIDs = append(trackIDs, e.ID)
	}
	for _, e := range review.TopAlbums {
		albumIDs = append(albumIDs, e.ID)
	}
	for _, e := range review.TopArtists {
		artistIDs = append(artistIDs, e.ID)
	}
	if review.LongestSession != nil {
		trackIDs = append(trackIDs, review.LongestSession.TrackIDs...)
	}
	refs, err := s.resolveRefs(ctx, trackIDs, albumIDs, artistIDs)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := YearInReview{
		Year:       review.Year,
		Summary:    toSummary(review.Summary),
		TopTracks:  entriesOf(review.TopTracks, refs.trackEntity),
		TopArtists: entriesOf(review.TopArtists, refs.artistEntity),
		TopAlbums:  entriesOf(review.TopAlbums, refs.albumEntity),
		NewArtists: review.NewArtists,
	}
	if !review.BusiestDay.Day.IsZero() {
		out.BusiestDay = &BusiestDay{
			Day:      localDay(review.BusiestDay.Day),
			Plays:    review.BusiestDay.Plays,
			MsPlayed: review.BusiestDay.MsPlayed,
		}
	}
	if review.LongestSession != nil {
		rendered, err := s.renderSessions(ctx, []domain.ListeningSession{*review.LongestSession})
		if err != nil {
			writeError(w, r, err)
			return
		}
		if len(rendered) > 0 {
			out.LongestSession = &rendered[0]
		}
	}
	writeJSON(w, r, http.StatusOK, out)
}

// entriesOf renders a ranked list that already carries its ranks.
func entriesOf[T any](entries []stats.TopEntry, entity func(string) T) []TopEntry[T] {
	out := make([]TopEntry[T], 0, len(entries))
	for _, e := range entries {
		out = append(out, TopEntry[T]{
			Entity:       entity(e.ID),
			Plays:        e.Plays,
			MsPlayed:     e.MsPlayed,
			Rank:         e.Rank,
			PreviousRank: previousRank(e.PreviousRank),
		})
	}
	return out
}

// handleExtras answers GET /api/stats/extras.
func (s *Server) handleExtras(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	summary, err := s.stats.Summary(ctx, s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	release, err := s.stats.AverageAlbumReleaseYear(ctx, s.querier, user.ID, tr)
	if err != nil {
		writeError(w, r, err)
		return
	}
	perTrack, err := s.stats.AverageArtistsPerTrack(ctx, s.querier, user.ID, tr)
	if err != nil {
		writeError(w, r, err)
		return
	}
	albums, err := s.stats.CompletedAlbums(ctx, s.querier, user.ID, tr)
	if err != nil {
		writeError(w, r, err)
		return
	}
	completed := toCompletedAlbums(albums)

	out := StatsExtras{
		DifferentArtists:       summary.DistinctArtists,
		AverageArtistsPerTrack: perTrack.Average,
		AlbumsCompleted:        &completed,
	}
	// A range with no album-attributed listening has no average release year at
	// all, which is a different statement from an average of zero.
	if release.Listens > 0 {
		year := release.AverageYear
		out.AverageAlbumReleaseYear = &year
	}
	writeJSON(w, r, http.StatusOK, out)
}

// handleAffinity answers GET /api/stats/affinity/{userId}.
//
// It is the one endpoint where one user learns something about another, and it
// is deliberately limited to overlap: shared entities with each side's play
// counts, never raw listening timestamps.
func (s *Server) handleAffinity(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	otherID, err := parseUUIDPath(r, "userId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	other, err := s.users.GetByID(ctx, s.querier, otherID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !other.IsActive {
		// A deactivated account is invisible rather than forbidden, so the
		// endpoint cannot be used to enumerate who once had an account.
		writeError(w, r, ErrNotFoundf("That user does not exist."))
		return
	}

	affinity, err := s.stats.Affinity(ctx, s.querier, user.ID, other.ID, tr, affinityListLimit)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var trackIDs, albumIDs, artistIDs []string
	for _, e := range affinity.SharedTracks {
		trackIDs = append(trackIDs, e.ID)
	}
	for _, e := range affinity.SharedAlbums {
		albumIDs = append(albumIDs, e.ID)
	}
	for _, e := range affinity.SharedArtists {
		artistIDs = append(artistIDs, e.ID)
	}
	refs, err := s.resolveRefs(ctx, trackIDs, albumIDs, artistIDs)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, AffinityResponse{
		User:    toPublicUser(other),
		Score:   affinity.Score,
		Artists: sharedEntries(affinity.SharedArtists, refs.artistEntity),
		Albums:  sharedEntries(affinity.SharedAlbums, refs.albumEntity),
		Tracks:  sharedEntries(affinity.SharedTracks, refs.trackEntity),
	})
}

// sharedEntries renders one side of a comparison with its entities resolved.
func sharedEntries[T any](shared []stats.SharedEntry, entity func(string) T) []AffinityEntry[T] {
	out := make([]AffinityEntry[T], 0, len(shared))
	for _, e := range shared {
		out = append(out, AffinityEntry[T]{Entity: entity(e.ID), PlaysA: e.PlaysA, PlaysB: e.PlaysB})
	}
	return out
}

// genreTimelineMaxSeries bounds how many genres one timeline may carry. Eight is
// where a stacked area chart stops being readable and the ninth series is noise.
const genreTimelineMaxSeries = 8

// handleGenres answers GET /api/stats/genres.
func (s *Server) handleGenres(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	limit, offset, err := parsePage(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	page, err := s.stats.TopGenres(r.Context(), s.querier, user.ID, tr, user.Timezone, limit, offset)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toGenres(page))
}

// handleGenreTimeline answers GET /api/stats/genres/timeline.
//
// The genres are a repeated query parameter rather than the server picking them,
// so a chart's series stay stable while the ranking beneath it is paged. Asking
// for none means "the range's top ones", which is what a first page load wants.
func (s *Server) handleGenreTimeline(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	interval, err := parseInterval(r, tr)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	// A caller may repeat ?genre= (a re-submitted form, a hand-built link, or a
	// bookmarked one), and genreTimelineSQL's series CTE cross-joins the list
	// verbatim: a repeated genre would cross-join to a duplicate (bucket, genre)
	// row, silently breaking the one-row-per-bucket-per-genre contract every
	// timeline promises. Deduplicating here, before the cap and before the
	// service call, is what keeps that contract regardless of what the query
	// string contains.
	genres := dedupeGenres(r.URL.Query()["genre"])
	if len(genres) > genreTimelineMaxSeries {
		writeError(w, r, fmt.Errorf("%w: at most %d genres may be charted at once",
			domain.ErrValidation, genreTimelineMaxSeries))
		return
	}
	if len(genres) == 0 {
		page, err := s.stats.TopGenres(ctx, s.querier, user.ID, tr, user.Timezone, genreTimelineMaxSeries, 0)
		if err != nil {
			writeError(w, r, err)
			return
		}
		for _, g := range page.Genres {
			genres = append(genres, g.Genre)
		}
	}

	points, err := s.stats.GenreTimeline(ctx, s.querier, user.ID, tr, user.Timezone, interval, genres)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := GenreTimelineResponse{
		Interval: string(interval),
		Points:   make([]GenreTimelinePoint, 0, len(points)),
	}
	for _, p := range points {
		out.Points = append(out.Points, GenreTimelinePoint{
			Bucket: p.Bucket, Genre: p.Genre, Plays: p.Plays, MsPlayed: p.MsPlayed,
		})
	}
	writeJSON(w, r, http.StatusOK, out)
}

// dedupeGenres drops repeats from a caller's genre list while preserving the
// order they first appeared in, so the series in a chart lands in a stable,
// predictable order rather than whatever a map would give back.
func dedupeGenres(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, g := range in {
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	return out
}

// handleTaste answers GET /api/stats/taste.
func (s *Server) handleTaste(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	t, err := s.stats.Taste(r.Context(), s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toTaste(t))
}

// handlePlaybackContext answers GET /api/stats/context.
func (s *Server) handlePlaybackContext(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	c, err := s.stats.PlaybackContext(r.Context(), s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toPlaybackContext(c))
}

// handleListUsers answers GET /api/users: who else is on this instance.
//
// Deactivated accounts and the caller are left out, and each entry carries only
// what a comparison page needs to offer a choice.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	caller, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	users, _, err := s.users.ListUsers(r.Context(), s.querier, maxPageLimit, 0)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]PublicUser, 0, len(users))
	for _, u := range users {
		if u.ID == caller.ID || !u.IsActive {
			continue
		}
		out = append(out, toPublicUser(u))
	}
	writeJSON(w, r, http.StatusOK, out)
}
