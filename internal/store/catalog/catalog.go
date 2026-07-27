// Package catalog is the shared music catalogue and the queue that enriches it:
// artists, albums, tracks, the links between them, the name aliases that let a
// names-only import converge on a real track, and the per-user artist blacklist.
//
// The catalogue is global rather than per-user, so two accounts that played the
// same track share one row and the instance asks Spotify about it exactly once.
// Ingestion never writes anything but the 'pending' state; everything that talks
// to Spotify goes through the claim/mark cycle in queue.go, which is written so
// that several worker processes can run it at the same time without handing the
// same row to two of them.
//
// Spotify ids are base-62 text rather than UUIDs, so they travel as plain
// strings. Only user ids go through store.UUIDArg.
package catalog

import (
	"fmt"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/store"
)

// Repo is the catalogue repository.
type Repo struct{ db *store.Store }

// New builds the repository.
func New(db *store.Store) *Repo { return &Repo{db: db} }

// Kind names one of the three catalogue tables that share the enrichment state
// machine. It exists so the queue and the failure bookkeeping are written once
// instead of three times.
type Kind string

const (
	// KindTrack selects the tracks table.
	KindTrack Kind = "track"
	// KindAlbum selects the albums table.
	KindAlbum Kind = "album"
	// KindArtist selects the artists table.
	KindArtist Kind = "artist"
)

// Kinds lists every catalogue kind, in the order a worker should drain them:
// tracks first, because a resolved track is what makes a listen displayable at
// all, then the albums and artists it referenced.
var Kinds = []Kind{KindTrack, KindAlbum, KindArtist}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindTrack, KindAlbum, KindArtist:
		return true
	}
	return false
}

// String renders the kind for logs and metric labels.
func (k Kind) String() string { return string(k) }

// table maps a kind onto its physical table.
//
// The queue statements are assembled with this value because the three tables
// carry identical enrichment columns. The mapping is a closed switch over an
// enum and rejects everything else, so no caller-supplied text can ever reach
// the statement text.
func (k Kind) table() (string, error) {
	switch k {
	case KindTrack:
		return "tracks", nil
	case KindAlbum:
		return "albums", nil
	case KindArtist:
		return "artists", nil
	}
	return "", fmt.Errorf("%w: unknown catalogue kind %q", domain.ErrValidation, string(k))
}

// scanner is the part of pgx.Row and pgx.Rows that the row decoders below need.
// Declaring it here keeps this package free of a direct pgx dependency, so the
// same decoder serves a single-row lookup and a multi-row scan.
type scanner interface {
	Scan(dest ...any) error
}

// metadataState converts a stored state. An unrecognised value is reported as
// pending rather than rejected: a row written by a newer binary must still be
// readable by an older one, and pending is the state that simply asks for the
// entity to be fetched again.
func metadataState(s string) domain.MetadataState {
	st := domain.MetadataState(s)
	if !st.Valid() {
		return domain.MetadataPending
	}
	return st
}

// dedupeIDs drops empty ids and repeats while preserving order.
//
// Spotify occasionally lists the same artist twice on a track, and the link
// tables use ON CONFLICT, which Postgres refuses to apply to the same row twice
// within one statement. Removing the repeats here keeps the SQL simple and the
// positions contiguous.
func dedupeIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// clampLimit keeps a caller-supplied row limit inside sane bounds so a hand-made
// request cannot ask the database for an unbounded result set.
func clampLimit(limit, fallback, max int) int {
	if limit <= 0 {
		limit = fallback
	}
	if limit > max {
		return max
	}
	return limit
}
