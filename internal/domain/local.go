package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// LocalIDPrefix marks a catalogue row Encore minted rather than one Spotify
// identified. A Spotify id is base-62 and can never contain a colon, so the two
// kinds are unmistakable — and the client's id filter rejects these on sight.
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
// The exports name every artist and identify none, so the id comes from the
// normalised name: the same artist in another file or a re-import lands on the
// same row and the same statistics. An unusable name yields "".
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
