package spotify

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Device is the player a listener is using.
//
// A pointer everywhere it appears: GET /v1/me/player/currently-playing is
// documented with the same response object as GET /v1/me/player but is observed
// to omit this, so a caller must be able to say "no device reported" rather
// than "a device with no name".
type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// IsActive and VolumePercent are decoded but unused. They are here because
	// dropping fields from a response object makes the next reader wonder
	// whether Spotify stopped sending them.
	IsActive      bool `json:"is_active"`
	VolumePercent *int `json:"volume_percent"`
}

// Show is the podcast an episode belongs to.
type Show struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Publisher string `json:"publisher"`
}

// PlaybackItem is whatever is in the player.
//
// It is not spotify.Track, and cannot be: this endpoint returns a union of two
// object types under one key, and an episode carries a show where a track
// carries artists. Track has nowhere to put a show, so decoding an episode into
// one would silently produce a track with no artist.
type PlaybackItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is "track" or "episode". Spotify has been observed to omit it on a
	// track, so an empty value is read as a track rather than as unknown.
	Type       string `json:"type"`
	URI        string `json:"uri"`
	DurationMs int    `json:"duration_ms"`
	// IsLocal marks a file on the listener's own machine. It has no catalogue
	// identity, so Encore can neither link it nor ever record it as a listen.
	IsLocal bool     `json:"is_local"`
	Artists []Artist `json:"artists"`
	Show    *Show    `json:"show"`
}

// Playback is what Spotify says is in the player right now.
type Playback struct {
	Timestamp  int64 `json:"timestamp"`
	ProgressMs *int  `json:"progress_ms"`
	IsPlaying  bool  `json:"is_playing"`
	// CurrentlyPlayingType is "track", "episode", "ad" or "unknown". An advert
	// arrives as "ad" with a null Item, which is why the item alone cannot
	// classify a response.
	CurrentlyPlayingType string        `json:"currently_playing_type"`
	Item                 *PlaybackItem `json:"item"`
	Device               *Device       `json:"device"`
	Context              *PlayContext  `json:"context"`
}

// CurrentlyPlaying reads what the listener is playing right now, or nil when
// nothing is.
//
// A nil result with a nil error is the endpoint's commonest answer and is not a
// failure: Spotify replies 204 No Content when the player is idle. The caller
// records that as "nothing is playing", which is a different fact from "Encore
// has not managed to look", and neither is an error.
//
// additional_types=episode is required for a podcast to arrive with a name.
// Without it Spotify answers item: null with currently_playing_type "episode",
// and a named episode becomes something no interface can describe.
//
// This is the only caller of classNowPlaying, and that is the whole design: a
// 429 here pauses this budget alone. It never reaches the pause observer, so it
// never writes app_settings.spotify_paused_until, so it can never 409 "sync
// now" for every user or stop enrichment. See requestClass.
func (c *Client) CurrentlyPlaying(ctx context.Context, accessToken string) (*Playback, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("spotify: currently playing: no access token")
	}

	q := url.Values{}
	q.Set("additional_types", "episode")

	var (
		body   Playback
		status int
	)
	if err := c.do(ctx, request{
		method: http.MethodGet,
		url:    c.endpoint("/v1/me/player/currently-playing", q),
		label:  "get currently playing",
		bearer: accessToken,
		out:    &body,
		status: &status,
		class:  classNowPlaying,
	}); err != nil {
		return nil, fmt.Errorf("spotify: currently playing: %w", err)
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &body, nil
}
