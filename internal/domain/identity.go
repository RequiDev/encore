package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"time"

	"github.com/google/uuid"
)

// DedupeBucketSeconds is the width of the timestamp bucket used by the exact
// duplicate key. It is 60s because the account-data export format only records
// stream end times to the minute, which is the coarsest precision Encore ingests.
//
// Changing this value invalidates every stored dedupe_key. Do not change it
// without a migration that recomputes them.
const DedupeBucketSeconds = 60

// TrackIdentity is the answer to "what was listened to", independent of when.
//
// Exports differ in fidelity: extended streaming history carries a Spotify track
// URI, while the account-data export carries only artist and track names. The
// identity therefore has two forms, and Encore converges the weaker form onto the
// stronger one as catalogue metadata resolves (see the relink pass in
// internal/enrich).
type TrackIdentity struct {
	// TrackID is the Spotify track id. Non-empty means "resolved".
	TrackID string
	// Artist and Title are the *normalised* names, used only when TrackID is empty.
	Artist string
	Title  string
}

// TrackIdentityFromID builds a resolved identity from a Spotify track id.
func TrackIdentityFromID(trackID string) TrackIdentity {
	return TrackIdentity{TrackID: trackID}
}

// TrackIdentityFromNames builds an unresolved identity, normalising as it goes.
// Callers may pass raw names straight from an export.
func TrackIdentityFromNames(artist, title string) TrackIdentity {
	return TrackIdentity{
		Artist: NormalizeArtist(artist),
		Title:  NormalizeTitle(title),
	}
}

// IsResolved reports whether the identity is anchored to a Spotify track id.
func (t TrackIdentity) IsResolved() bool { return t.TrackID != "" }

// IsZero reports whether the identity carries no usable information at all.
func (t TrackIdentity) IsZero() bool {
	return t.TrackID == "" && t.Artist == "" && t.Title == ""
}

// Key is the stable 32-byte hash of the identity.
//
//	resolved:   sha256("t:" || track_id)
//	unresolved: sha256("n:" || norm_artist || 0x00 || norm_title)
//
// The "t:"/"n:" domain separator prevents a track id from ever colliding with a
// name pair.
func (t TrackIdentity) Key() []byte {
	h := sha256.New()
	if t.IsResolved() {
		h.Write([]byte("t:"))
		h.Write([]byte(t.TrackID))
	} else {
		h.Write([]byte("n:"))
		h.Write([]byte(t.Artist))
		h.Write([]byte{0})
		h.Write([]byte(t.Title))
	}
	return h.Sum(nil)
}

// DedupeKey is the exact duplicate key enforced by UNIQUE (user_id, dedupe_key).
//
//	sha256( user_id[16] || identity_key[32] || floor(unix(playedAt)/60) as int64 BE )
//
// Two ingestions of the same listening event by the same user produce the same
// key regardless of which file or API response they arrived in, which is what
// makes re-running an import a no-op at the database level.
func DedupeKey(userID uuid.UUID, identity TrackIdentity, playedAt time.Time) []byte {
	var bucket [8]byte
	binary.BigEndian.PutUint64(bucket[:], uint64(TimeBucket(playedAt)))

	h := sha256.New()
	uid := userID // avoid aliasing the caller's array
	h.Write(uid[:])
	h.Write(identity.Key())
	h.Write(bucket[:])
	return h.Sum(nil)
}

// TimeBucket returns the bucket index for a timestamp, using floor division so
// that pre-epoch times (which should never occur, but must not alias) behave.
func TimeBucket(t time.Time) int64 {
	s := t.UTC().Unix()
	if s < 0 {
		return -((-s + DedupeBucketSeconds - 1) / DedupeBucketSeconds)
	}
	return s / DedupeBucketSeconds
}
