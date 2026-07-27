// Package metadata is where catalogue descriptions come from.
//
// A Source turns Spotify ids into track, artist and album objects; Chain puts
// one in front of another. The wire format is Spotify's own, so *spotify.Client
// is a Source without an adapter.
//
// Encore ships the interface, not a source. docs/metadata-fallback.md covers
// why a second one is worth having and what it must answer.
package metadata

import (
	"context"

	"github.com/RequiDev/encore/internal/spotify"
)

// Source supplies catalogue metadata for Spotify ids.
//
// An id the source cannot serve is absent from the result rather than an error
// for the whole batch, which is what makes sources chainable: callers compare
// what they asked for against what they got. Safe for concurrent use.
type Source interface {
	GetTracks(ctx context.Context, ids []string) ([]spotify.Track, error)
	GetArtists(ctx context.Context, ids []string) ([]spotify.Artist, error)
	GetAlbums(ctx context.Context, ids []string) ([]spotify.Album, error)
}

// compile-time proof that the Spotify client is itself a Source. If this ever
// stops holding, Chain and the enrichment worker stop composing.
var _ Source = (*spotify.Client)(nil)
