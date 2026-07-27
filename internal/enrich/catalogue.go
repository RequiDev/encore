package enrich

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store/catalog"
)

// RunTracksOnce claims one batch of pending track ids, reads them from
// /v1/tracks and records the outcome. It reports how many ids it claimed, which
// is zero when the queue is empty.
func (w *Worker) RunTracksOnce(ctx context.Context) (int, error) {
	return w.runCatalogueOnce(ctx, catalog.KindTrack)
}

// RunAlbumsOnce is RunTracksOnce for albums, whose batch endpoint accepts twenty
// ids rather than fifty.
func (w *Worker) RunAlbumsOnce(ctx context.Context) (int, error) {
	return w.runCatalogueOnce(ctx, catalog.KindAlbum)
}

// RunArtistsOnce is RunTracksOnce for artists.
func (w *Worker) RunArtistsOnce(ctx context.Context) (int, error) {
	return w.runCatalogueOnce(ctx, catalog.KindArtist)
}

// runCatalogueOnce is one claim-fetch-record cycle for a single kind.
//
// The three kinds differ only in the endpoint they read and the link table they
// rewrite; the queue mechanics, the failure bookkeeping and the unavailable
// handling are identical, so they are written once here.
func (w *Worker) runCatalogueOnce(ctx context.Context, kind catalog.Kind) (int, error) {
	ids, err := w.dep.Catalog.ClaimPending(ctx, w.db(), kind, batchSize(w.cfg, kind), claimLease)
	if err != nil {
		return 0, wrap("claim pending "+kind.String()+"s", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	if err := w.fetchAndStore(ctx, kind, ids); err != nil {
		if ctx.Err() != nil {
			return len(ids), err
		}
		var stErr *storeError
		if errors.As(err, &stErr) {
			// Spotify answered; the database did not take the answer. Spending the
			// rows' attempt budget on a database that is briefly unreachable would
			// park a healthy catalogue, so the claim is left to expire instead.
			return len(ids), wrap("store "+kind.String()+"s", err)
		}
		if rateLimited(err) {
			// The client has already paused every request in the process for as
			// long as Spotify asked. Adding a second backoff here would make the
			// pause longer than it was told to be, so the claim is simply left to
			// expire and these ids come back once the pause has cleared.
			w.stat.EnrichRateLimited()
			w.log.Warn("enrichment rate limited by spotify", "kind", kind.String(), "ids", len(ids))
			return len(ids), nil
		}
		// Record the failure before reporting it, so that a persistent problem
		// advances the backoff instead of re-claiming the same ids every cycle.
		if markErr := w.markFailed(ctx, kind, ids); markErr != nil {
			w.log.Error("could not record enrichment failure", "kind", kind.String(), logging.Err(markErr))
		}
		return len(ids), wrap("enrich "+kind.String()+"s", err)
	}
	return len(ids), nil
}

// batchSize is how many ids of one kind may travel in a single request.
//
// Spotify's own limits are the ceiling — fifty tracks, fifty artists, twenty
// albums — and ENCORE_ENRICH_BATCH_SIZE may only lower them, which is what an
// operator reaches for when they want enrichment to tread more lightly.
func batchSize(cfg config.Enrich, kind catalog.Kind) int {
	limit := spotify.MaxTrackIDsPerRequest
	switch kind {
	case catalog.KindAlbum:
		limit = spotify.MaxAlbumIDsPerRequest
	case catalog.KindArtist:
		limit = spotify.MaxArtistIDsPerRequest
	}
	if cfg.BatchSize > 0 && cfg.BatchSize < limit {
		return cfg.BatchSize
	}
	return limit
}

// storeError marks a failure that happened while writing a fetched batch rather
// than while fetching it.
//
// The distinction decides who is blamed for it. A batch Spotify would not answer
// has failed and its rows' attempt counters should advance; a batch the database
// would not accept has not, and advancing the counters would park a perfectly
// healthy catalogue because Postgres was restarting.
type storeError struct{ err error }

func (e *storeError) Error() string { return e.err.Error() }
func (e *storeError) Unwrap() error { return e.err }

// stored wraps a write failure, passing nil through unchanged.
func stored(err error) error {
	if err == nil {
		return nil
	}
	return &storeError{err: err}
}

// fetchAndStore reads one batch from Spotify and writes what came back.
func (w *Worker) fetchAndStore(ctx context.Context, kind catalog.Kind, ids []string) error {
	switch kind {
	case catalog.KindTrack:
		tracks, err := w.dep.Spotify.GetTracks(ctx, ids)
		if err != nil {
			return err
		}
		return w.storeTracks(ctx, ids, tracks)

	case catalog.KindAlbum:
		albums, err := w.dep.Spotify.GetAlbums(ctx, ids)
		if err != nil {
			return err
		}
		return w.storeAlbums(ctx, ids, albums)

	case catalog.KindArtist:
		artists, err := w.dep.Spotify.GetArtists(ctx, ids)
		if err != nil {
			return err
		}
		return w.storeArtists(ctx, ids, artists)
	}
	return fmt.Errorf("%w: unknown catalogue kind %q", domain.ErrValidation, kind.String())
}

// storeTracks writes a fetched batch of tracks.
//
// The upsert, the credit lists and the unavailable marks commit together so a
// track is never visible as resolved without the artists it is credited to;
// half a batch would show up in the API as a track by nobody.
func (w *Worker) storeTracks(ctx context.Context, requested []string, tracks []spotify.Track) error {
	rows := make([]domain.Track, 0, len(tracks))
	got := make([]string, 0, len(tracks))
	for _, t := range tracks {
		row := t.ToDomainTrack()
		if row.ID == "" {
			continue
		}
		rows = append(rows, row)
		got = append(got, row.ID)
	}
	missing := missingIDs(requested, got)

	// A track response embeds simplified artist and album objects: an id and a
	// name and nothing else. Recording only the ids, which is what this used to
	// do, left every artist blank in the interface until the separate artist
	// queue drained — a wait measured in days on a development-mode application
	// whose daily quota has run out. The names are free here, so keep them.
	seedArtists, seedAlbums := simplifiedFrom(tracks)

	err := w.dep.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := w.dep.Catalog.UpsertTracks(ctx, tx, rows); err != nil {
			return err
		}
		if err := w.dep.Catalog.SeedArtists(ctx, tx, seedArtists); err != nil {
			return err
		}
		if err := w.dep.Catalog.SeedAlbums(ctx, tx, seedAlbums); err != nil {
			return err
		}
		for _, row := range rows {
			if err := w.dep.Catalog.ReplaceTrackArtists(ctx, tx, row.ID, row.ArtistIDs); err != nil {
				return err
			}
		}
		return w.dep.Catalog.MarkUnavailable(ctx, tx, catalog.KindTrack, missing)
	})
	if err != nil {
		return stored(err)
	}
	w.report(catalog.KindTrack, len(rows), missing)
	return nil
}

// storeAlbums writes a fetched batch of albums, with their credits.
func (w *Worker) storeAlbums(ctx context.Context, requested []string, albums []spotify.Album) error {
	rows := make([]domain.Album, 0, len(albums))
	got := make([]string, 0, len(albums))
	for _, a := range albums {
		row := a.ToDomainAlbum()
		if row.ID == "" {
			continue
		}
		rows = append(rows, row)
		got = append(got, row.ID)
	}
	missing := missingIDs(requested, got)

	err := w.dep.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := w.dep.Catalog.UpsertAlbums(ctx, tx, rows); err != nil {
			return err
		}
		for _, row := range rows {
			if err := w.dep.Catalog.ReplaceAlbumArtists(ctx, tx, row.ID, row.ArtistIDs); err != nil {
				return err
			}
		}
		return w.dep.Catalog.MarkUnavailable(ctx, tx, catalog.KindAlbum, missing)
	})
	if err != nil {
		return stored(err)
	}
	w.report(catalog.KindAlbum, len(rows), missing)
	return nil
}

// storeArtists writes a fetched batch of artists. Artists have no link table of
// their own, so this is the upsert and the unavailable marks alone.
func (w *Worker) storeArtists(ctx context.Context, requested []string, artists []spotify.Artist) error {
	rows := make([]domain.Artist, 0, len(artists))
	got := make([]string, 0, len(artists))
	for _, a := range artists {
		row := a.ToDomainArtist()
		if row.ID == "" {
			continue
		}
		rows = append(rows, row)
		got = append(got, row.ID)
	}
	missing := missingIDs(requested, got)

	err := w.dep.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := w.dep.Catalog.UpsertArtists(ctx, tx, rows); err != nil {
			return err
		}
		return w.dep.Catalog.MarkUnavailable(ctx, tx, catalog.KindArtist, missing)
	})
	if err != nil {
		return stored(err)
	}
	w.report(catalog.KindArtist, len(rows), missing)
	return nil
}

// report publishes the outcome of one stored batch. Entities Spotify has nothing
// for count as failed: they are out of the queue for good, and an instance whose
// catalogue is full of them has a problem worth seeing on a dashboard.
func (w *Worker) report(kind catalog.Kind, resolved int, missing []string) {
	if resolved > 0 {
		w.stat.EnrichResolved(kind.String(), int64(resolved))
	}
	if len(missing) > 0 {
		w.stat.EnrichFailed(kind.String(), int64(len(missing)))
		w.log.Info("catalogue entities unavailable from spotify",
			"kind", kind.String(), "count", len(missing))
	}
}

// markFailed records a failed fetch for a whole claimed batch and schedules the
// next attempt.
//
// MarkFetchFailed writes the attempt count absolutely rather than incrementing
// it, so the current counts have to be read back first. That read is on the
// failure path only: a healthy batch costs a claim and an upsert, nothing more.
// Ids are grouped by their new attempt number so the schedule is still one
// statement per distinct backoff rather than one per id.
func (w *Worker) markFailed(ctx context.Context, kind catalog.Kind, ids []string) error {
	rows, err := w.catalogueRows(ctx, kind, ids)
	if err != nil {
		return err
	}

	groups := make(map[int32][]string, 2)
	order := make([]int32, 0, 2)
	for _, id := range ids {
		attempts := rows[id].attempts + 1
		if _, seen := groups[attempts]; !seen {
			order = append(order, attempts)
		}
		groups[attempts] = append(groups[attempts], id)
	}

	parked := 0
	for _, attempts := range order {
		batch := groups[attempts]
		if err := w.dep.Catalog.MarkFetchFailed(ctx, w.db(), kind, batch, attempts, w.nextAttempt(attempts)); err != nil {
			return err
		}
		if attempts >= domain.BackoffAttempts {
			parked += len(batch)
		}
	}

	w.stat.EnrichFailed(kind.String(), int64(len(ids)))
	if parked > 0 {
		// These rows have spent their whole attempt budget. Say so once, here,
		// rather than leaving an operator to infer it from the backlog gauge.
		w.log.Warn("catalogue entities parked after exhausting their retries",
			"kind", kind.String(), "count", parked, "attempts", domain.BackoffAttempts)
	}
	return nil
}

// catalogueRow is the slice of a catalogue row the bookkeeping needs: how many
// attempts it has already made, and whether it is parked in the failed state.
type catalogueRow struct {
	attempts int32
	state    domain.MetadataState
}

// catalogueRows reads the queue columns of a set of ids. Ids that are no longer
// in the catalogue are simply absent, which the callers treat as a fresh row:
// the marking statements match nothing for them anyway.
func (w *Worker) catalogueRows(ctx context.Context, kind catalog.Kind, ids []string) (map[string]catalogueRow, error) {
	out := make(map[string]catalogueRow, len(ids))
	switch kind {
	case catalog.KindTrack:
		found, err := w.dep.Catalog.GetTracks(ctx, w.db(), ids)
		if err != nil {
			return nil, err
		}
		for id, t := range found {
			out[id] = catalogueRow{attempts: t.FetchAttempts, state: t.MetadataState}
		}

	case catalog.KindAlbum:
		found, err := w.dep.Catalog.GetAlbums(ctx, w.db(), ids)
		if err != nil {
			return nil, err
		}
		for id, a := range found {
			out[id] = catalogueRow{attempts: a.FetchAttempts, state: a.MetadataState}
		}

	case catalog.KindArtist:
		found, err := w.dep.Catalog.GetArtists(ctx, w.db(), ids)
		if err != nil {
			return nil, err
		}
		for id, a := range found {
			out[id] = catalogueRow{attempts: a.FetchAttempts, state: a.MetadataState}
		}

	default:
		return nil, fmt.Errorf("%w: unknown catalogue kind %q", domain.ErrValidation, kind.String())
	}
	return out, nil
}

// simplifiedFrom pulls the artist and album names out of a batch of track
// responses.
//
// These are Spotify's "simplified" objects: an id and a name, without the
// genres, images or popularity that the dedicated endpoints return. They are
// enough to display, and they cost nothing extra, so they are recorded as seeds
// while the rows stay in the enrichment queue for the rest.
func simplifiedFrom(tracks []spotify.Track) ([]domain.Artist, []domain.Album) {
	artists := make([]domain.Artist, 0, len(tracks)*2)
	albums := make([]domain.Album, 0, len(tracks))
	seenArtist := make(map[string]struct{}, len(tracks)*2)
	seenAlbum := make(map[string]struct{}, len(tracks))

	for _, t := range tracks {
		for _, a := range t.Artists {
			if a.ID == "" || a.Name == "" {
				continue
			}
			if _, dup := seenArtist[a.ID]; dup {
				continue
			}
			seenArtist[a.ID] = struct{}{}
			artists = append(artists, domain.Artist{
				ID: a.ID, Name: a.Name, NameNorm: domain.NormalizeArtist(a.Name),
			})
		}
		for _, a := range t.Album.Artists {
			if a.ID == "" || a.Name == "" {
				continue
			}
			if _, dup := seenArtist[a.ID]; dup {
				continue
			}
			seenArtist[a.ID] = struct{}{}
			artists = append(artists, domain.Artist{
				ID: a.ID, Name: a.Name, NameNorm: domain.NormalizeArtist(a.Name),
			})
		}
		if t.Album.ID == "" || t.Album.Name == "" {
			continue
		}
		if _, dup := seenAlbum[t.Album.ID]; dup {
			continue
		}
		seenAlbum[t.Album.ID] = struct{}{}
		albums = append(albums, domain.Album{
			ID: t.Album.ID, Name: t.Album.Name, NameNorm: domain.NormalizeTitle(t.Album.Name),
		})
	}
	return artists, albums
}
