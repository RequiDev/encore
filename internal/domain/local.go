package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// LocalIDPrefix marks a catalogue row Encore minted itself rather than one
// Spotify identified.
//
// A Spotify id is base-62, so it can never contain a colon: the prefix makes the
// two kinds unmistakable, in the database, in a URL and in a log. It is also why
// the Spotify client's id filter rejects these on sight — a local id can never
// be sent to an endpoint that would only answer 400 for it.
const LocalIDPrefix = "local:"

// Local id namespaces. They are part of the identifier, so an artist and an
// album that happen to share a name still get different ids.
const (
	localArtistNamespace = LocalIDPrefix + "artist:"
	localAlbumNamespace  = LocalIDPrefix + "album:"
)

// localIDBytes is how much of the digest is kept. Sixty-four bits across the
// tens of thousands of names a large history holds leaves a collision chance far
// below the odds of the database losing the row some other way.
const localIDBytes = 8

// LocalArtistID derives a stable id for an artist known only by name.
//
// Both Spotify export formats name the artist of every play and identify none of
// them: there is a spotify_track_uri and nothing else. Waiting for the API to
// supply artist ids means an imported history has no artists at all until
// enrichment drains, which on a development-mode application whose daily quota
// is exhausted can be never.
//
// Deriving the id from the normalised name is what makes the row stable: the
// same artist in a second export, in another year's file, or in a re-import
// lands on the same id and the same statistics.
//
// An empty or unusable name yields "", which callers treat as "no artist".
func LocalArtistID(name string) string {
	norm := NormalizeArtist(name)
	if norm == "" {
		return ""
	}
	return localArtistNamespace + digest(norm)
}

// LocalAlbumID derives a stable id for an album known only by name.
//
// Keyed on the artist as well as the title, because album titles collide far
// more than they look like they should: a history of any size holds several
// "Greatest Hits" that have nothing to do with one another.
func LocalAlbumID(artistName, albumName string) string {
	album := NormalizeTitle(albumName)
	if album == "" {
		return ""
	}
	// The artist is part of the key but not required: a compilation with no album
	// artist still deserves a row of its own rather than being merged with every
	// other untitled-artist album of the same name.
	return localAlbumNamespace + digest(NormalizeArtist(artistName)+"\x00"+album)
}

// IsLocalID reports whether an id was minted by Encore rather than by Spotify.
func IsLocalID(id string) bool { return strings.HasPrefix(id, LocalIDPrefix) }

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:localIDBytes])
}
