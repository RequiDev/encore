// Package metadata is where catalogue descriptions come from.
//
// Encore's default and only required source is Spotify itself. But a
// self-hosted instance runs against a Spotify application in development mode,
// whose daily request quota is small, undocumented, and exhausted by the first
// large import — and Spotify answers an exhausted quota with a Retry-After of
// most of a day. Worse, some ids Spotify simply will not serve at all: a track
// delisted since it was played is marked unavailable and, being terminal, stays
// blank for ever.
//
// So this package lets an operator name a second source. A Source is anything
// that can turn Spotify ids into track, artist and album objects; Chain puts one
// in front of another and decides which answers. The wire format is Spotify's
// own, deliberately, so *spotify.Client satisfies Source without an adapter and
// anyone writing a second source has a specification that already exists.
//
// Encore ships the interface, not a source. What an operator serves from their
// own endpoint, and whether they hold the rights to serve it, is their affair —
// this package neither knows nor implies anything about where the data came
// from.
package metadata

import (
	"context"

	"github.com/RequiDev/encore/internal/spotify"
)

// Source supplies catalogue metadata for Spotify ids.
//
// The three methods mirror Spotify's batch endpoints exactly, including the one
// behaviour that matters most: an id the source cannot serve is *absent from the
// result* rather than an error for the whole batch. Callers compare what they
// asked for against what they got, so a source that knows nothing at all is
// indistinguishable from one that is merely incomplete — which is what makes
// chaining them safe.
//
// A Source must be safe for concurrent use.
type Source interface {
	GetTracks(ctx context.Context, ids []string) ([]spotify.Track, error)
	GetArtists(ctx context.Context, ids []string) ([]spotify.Artist, error)
	GetAlbums(ctx context.Context, ids []string) ([]spotify.Album, error)
}

// compile-time proof that the Spotify client is itself a Source. If this ever
// stops holding, Chain and the enrichment worker stop composing.
var _ Source = (*spotify.Client)(nil)
