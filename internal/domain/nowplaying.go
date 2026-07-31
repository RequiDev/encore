package domain

import "time"

// PlaybackState is what a listener's player was doing when Encore last managed
// to look.
type PlaybackState string

const (
	// PlaybackUnknown means no successful observation has ever been made for
	// this account.
	//
	// It is not "nothing is playing". That is PlaybackIdle, and the difference
	// is the whole reason this is an enum with four values rather than a
	// boolean: an interface that renders "we have not looked" as "nothing is
	// playing" states a fact about somebody's evening that nobody checked.
	PlaybackUnknown PlaybackState = "unknown"
	// PlaybackIdle means Encore looked and Spotify said nothing is playing.
	// The endpoint answers 204 No Content, which is the commonest case and is
	// not an error.
	PlaybackIdle PlaybackState = "idle"
	// PlaybackPlaying means something is playing.
	PlaybackPlaying PlaybackState = "playing"
	// PlaybackPaused means something is loaded and stopped.
	PlaybackPaused PlaybackState = "paused"
)

// PlaybackItemKind is what sort of thing is in the player, and therefore what
// Encore is able to say about it truthfully.
type PlaybackItemKind string

const (
	// PlaybackItemNone means there is nothing in the player at all.
	PlaybackItemNone PlaybackItemKind = "none"
	// PlaybackItemTrack is a Spotify catalogue track: the only kind that can
	// carry a TrackID, and the only kind that ever becomes a listen.
	PlaybackItemTrack PlaybackItemKind = "track"
	// PlaybackItemEpisode is a podcast episode. Encore's ingestion skips these,
	// so one will never appear in a listening history, and the interface says
	// so rather than letting somebody assume otherwise.
	PlaybackItemEpisode PlaybackItemKind = "episode"
	// PlaybackItemLocal is a file on the listener's own machine. It has a name
	// and no catalogue identity, so it can be shown and never linked.
	PlaybackItemLocal PlaybackItemKind = "local"
	// PlaybackItemUnknown is an advert, or a type this client does not know.
	//
	// Nothing descriptive is kept for one: Spotify's own label for an advert is
	// not a title, and rendering it as one would put an advertiser's name where
	// a listener expects their music.
	PlaybackItemUnknown PlaybackItemKind = "unknown"
)

// NowPlaying is the last thing Encore saw in a listener's player, and when it
// last looked.
//
// The two timestamps answer different questions and are kept apart on purpose.
// ObservedAt is when the figures below were true; CheckedAt is when Encore last
// tried at all. A display that had only one of them could not tell a stale
// truth from a fresh one, or a failure from an idle player.
type NowPlaying struct {
	// ObservedAt is when State and everything under it were true. Zero until a
	// check has succeeded.
	ObservedAt time.Time
	State      PlaybackState
	Kind       PlaybackItemKind
	// TrackID is the Spotify catalogue id, set only when Kind is
	// PlaybackItemTrack. It may name a track Encore's own catalogue has never
	// heard of; see TrackKnown.
	TrackID string
	// Title and Artist are what Spotify called it. Stored rather than joined,
	// because a local file and a podcast have names and no catalogue identity,
	// and an unenriched track would otherwise display as blank.
	Title  string
	Artist string
	// ProgressMs is progress at ObservedAt, never extrapolated. DurationMs is
	// the item's length. Both nil for an item that has neither.
	ProgressMs *int
	DurationMs *int
	// DeviceName is the player's name, empty when Spotify did not report one.
	DeviceName string
	// CheckedAt is when the poller last tried, successfully or not.
	CheckedAt time.Time
	// Failed reports that the attempt at CheckedAt did not succeed. Everything
	// above it is then the previous successful observation, or nothing.
	Failed bool
	// TrackKnown reports that TrackID names a row in Encore's own catalogue, so
	// a link to it will resolve. Computed at read time by a join; never stored,
	// because it changes when enrichment catches up rather than when the
	// listener changes track.
	TrackKnown bool
}

// Observed reports whether a successful observation has ever been recorded.
//
// The one predicate every reader should use for "does Encore know". Reading
// State directly invites the mistake this whole type is shaped to prevent:
// PlaybackUnknown is not a kind of silence, it is the absence of a look.
func (n NowPlaying) Observed() bool {
	return n.State != PlaybackUnknown && !n.ObservedAt.IsZero()
}
