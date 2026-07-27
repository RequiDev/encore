package catalog

import (
	"context"

	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// EntityProgress is how far enrichment has got with one kind of catalogue entity.
//
// Named is counted separately from Resolved because the two diverge in the case
// that matters: an imported track carries its title, so it is displayable long
// before Spotify has supplied its album, artwork and duration. Reporting only
// "resolved" would tell a user everything is missing when most of what they can
// actually see is already there.
type EntityProgress struct {
	Total       int64 `json:"total"`
	Resolved    int64 `json:"resolved"`
	Pending     int64 `json:"pending"`
	Failed      int64 `json:"failed"`
	Unavailable int64 `json:"unavailable"`
	Named       int64 `json:"named"`
	// Local counts rows an import named but could not identify. The exports give
	// an artist and an album for every play and an id for neither, so these are
	// readable but have no artwork, genres or release dates, and no queue can
	// fetch them: they gain those only if a track of theirs resolves and the
	// merge folds them into a Spotify row.
	Local int64 `json:"local"`
}

// Progress is the enrichment state of the whole catalogue.
type Progress struct {
	Tracks         EntityProgress `json:"tracks"`
	Artists        EntityProgress `json:"artists"`
	Albums         EntityProgress `json:"albums"`
	AliasesTotal   int64          `json:"aliasesTotal"`
	AliasesPending int64          `json:"aliasesPending"`
}

// Outstanding is how much work the enrichment queues still hold.
func (p Progress) Outstanding() int64 {
	return p.Tracks.Pending + p.Artists.Pending + p.Albums.Pending + p.AliasesPending
}

// Complete reports whether there is nothing left to fetch.
func (p Progress) Complete() bool { return p.Outstanding() == 0 }

// progressSQL counts every catalogue table in one round trip.
//
// A status endpoint that a page polls should not cost eleven queries, and these
// are all cheap aggregate scans over the partial indexes the enrichment queues
// already maintain.
const progressSQL = `
WITH t AS (
    SELECT count(*) AS total,
           count(*) FILTER (WHERE metadata_state = 'resolved')    AS resolved,
           count(*) FILTER (WHERE metadata_state = 'pending')     AS pending,
           count(*) FILTER (WHERE metadata_state = 'failed')      AS failed,
           count(*) FILTER (WHERE metadata_state = 'unavailable') AS unavailable,
           count(*) FILTER (WHERE name <> '')                     AS named,
           0::bigint                                              AS local
    FROM tracks
),
ar AS (
    SELECT count(*), count(*) FILTER (WHERE metadata_state = 'resolved'),
           count(*) FILTER (WHERE metadata_state = 'pending'),
           count(*) FILTER (WHERE metadata_state = 'failed'),
           count(*) FILTER (WHERE metadata_state = 'unavailable'),
           count(*) FILTER (WHERE name <> ''),
           count(*) FILTER (WHERE metadata_state = 'local')
    FROM artists
),
al AS (
    SELECT count(*), count(*) FILTER (WHERE metadata_state = 'resolved'),
           count(*) FILTER (WHERE metadata_state = 'pending'),
           count(*) FILTER (WHERE metadata_state = 'failed'),
           count(*) FILTER (WHERE metadata_state = 'unavailable'),
           count(*) FILTER (WHERE name <> ''),
           count(*) FILTER (WHERE metadata_state = 'local')
    FROM albums
),
ali AS (
    SELECT count(*), count(*) FILTER (WHERE state = 'pending') FROM track_aliases
)
SELECT t.*, ar.*, al.*, ali.* FROM t, ar, al, ali`

// CatalogueProgress reports how much of the catalogue has been enriched.
func (r *Repo) CatalogueProgress(ctx context.Context, q store.Querier) (Progress, error) {
	var p Progress
	err := q.QueryRow(ctx, progressSQL).Scan(
		&p.Tracks.Total, &p.Tracks.Resolved, &p.Tracks.Pending,
		&p.Tracks.Failed, &p.Tracks.Unavailable, &p.Tracks.Named, &p.Tracks.Local,
		&p.Artists.Total, &p.Artists.Resolved, &p.Artists.Pending,
		&p.Artists.Failed, &p.Artists.Unavailable, &p.Artists.Named, &p.Artists.Local,
		&p.Albums.Total, &p.Albums.Resolved, &p.Albums.Pending,
		&p.Albums.Failed, &p.Albums.Unavailable, &p.Albums.Named, &p.Albums.Local,
		&p.AliasesTotal, &p.AliasesPending,
	)
	if err != nil {
		return Progress{}, postgres.Classify("read catalogue progress", err)
	}
	return p, nil
}
