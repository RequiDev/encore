package domain

import "time"

// PlaybackObservation is one look at a listener's player, kept only long enough
// for the recently-played feed to catch up with it.
//
// It describes an instant, not a play. Turning it into a claim about a play is
// the backfill's job and is inherently uncertain; this type carries only what
// was actually seen.
type PlaybackObservation struct {
	// TrackID is the Spotify catalogue id, and the join key. Never empty: an
	// observation of anything without one is not recorded at all.
	TrackID string
	// ObservedAt is when Encore looked.
	ObservedAt time.Time
	// Shuffle is the shuffle toggle at ObservedAt, or nil when Spotify did not
	// report it.
	//
	// A pointer for the reason listens.shuffle is nullable: nil is "not
	// reported", which is a different fact from false. An observation that does
	// not know must not be able to teach a listen that the answer was no.
	Shuffle *bool
	// DeviceType is Spotify Connect's own vocabulary — "Computer",
	// "Smartphone", "Speaker" — and is not listens.platform's, which holds an
	// export's free text. Empty when Spotify reported no device.
	DeviceType string
	// DeviceName is the player's human name. It stays in this log and never
	// reaches listens.
	DeviceName string
}

// SaysSomething reports whether this observation has anything to teach a listen.
//
// The predicate the poller consults before spending a row, and the Go half of
// the playback_observations_says_something constraint. Keeping it here rather
// than inline at the call site means the storage rule and the classifier cannot
// drift apart, because there is one sentence stating it.
func (o PlaybackObservation) SaysSomething() bool {
	return o.Shuffle != nil || o.DeviceType != ""
}
