package spotify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// ScopePlaylistPrivate is the grant needed to create and fill a playlist.
//
// Deliberately the private one only. Encore never asks to publish anything to a
// listener's followers; a playlist it creates is visible to its owner and to
// whoever they choose to show it to, which is a decision that belongs to them
// and not to a statistics application.
const ScopePlaylistPrivate = "playlist-modify-private"

// MaxPlaylistItemsPerRequest is Spotify's cap on one add or replace call.
const MaxPlaylistItemsPerRequest = 100

// Playlist is the part of a created playlist Encore keeps.
type Playlist struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	URI          string       `json:"uri"`
	ExternalURLs ExternalURLs `json:"external_urls"`
}

// URL is where a person opens the playlist, preferring the web link Spotify
// supplies and falling back to one built from the id.
func (p Playlist) URL() string {
	if p.ExternalURLs.Spotify != "" {
		return p.ExternalURLs.Spotify
	}
	if p.ID == "" {
		return ""
	}
	return "https://open.spotify.com/playlist/" + p.ID
}

// CreatePlaylist makes an empty private playlist owned by the listener.
//
// Interactive: somebody pressed a button and is waiting, so this draws on the
// sign-in budget rather than queueing behind a catalogue quota it did not spend.
func (c *Client) CreatePlaylist(
	ctx context.Context,
	accessToken, spotifyUserID, name, description string,
) (*Playlist, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("create playlist: no access token")
	}
	if spotifyUserID == "" {
		return nil, fmt.Errorf("create playlist: no spotify user id")
	}

	var out Playlist
	err := c.do(ctx, request{
		method: http.MethodPost,
		url:    c.endpoint("/v1/users/"+spotifyUserID+"/playlists", nil),
		label:  "create playlist",
		bearer: accessToken,
		json: map[string]any{
			"name":          name,
			"description":   description,
			"public":        false,
			"collaborative": false,
		},
		out:         &out,
		interactive: true,
	})
	if err != nil {
		return nil, fmt.Errorf("spotify: create playlist: %w", err)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("spotify: create playlist: response carried no id")
	}
	return &out, nil
}

// ReplacePlaylistItems sets a playlist's contents to exactly the given tracks.
//
// The first call replaces, the rest append, which is what makes a rebuild
// idempotent: the playlist ends up holding these tracks in this order however
// many times it is run and whatever was in it before.
//
// Sending nothing empties the playlist, which is the honest result of a
// definition that now matches no tracks.
func (c *Client) ReplacePlaylistItems(
	ctx context.Context,
	accessToken, playlistID string,
	trackIDs []string,
) error {
	if accessToken == "" {
		return fmt.Errorf("replace playlist items: no access token")
	}
	if playlistID == "" {
		return fmt.Errorf("replace playlist items: no playlist id")
	}

	uris := make([]string, 0, len(trackIDs))
	for _, id := range cleanIDs(trackIDs) {
		uris = append(uris, "spotify:track:"+id)
	}

	batches := chunk(uris, MaxPlaylistItemsPerRequest)
	if len(batches) == 0 {
		// An empty replace still has to happen: the playlist may hold a previous
		// build that no longer matches.
		batches = [][]string{{}}
	}

	for i, batch := range batches {
		method, label := http.MethodPost, "add playlist items"
		if i == 0 {
			method, label = http.MethodPut, "replace playlist items"
		}
		err := c.do(ctx, request{
			method:      method,
			url:         c.endpoint("/v1/playlists/"+playlistID+"/tracks", nil),
			label:       label,
			bearer:      accessToken,
			json:        map[string]any{"uris": batch},
			interactive: true,
		})
		if err != nil {
			return fmt.Errorf("spotify: %s: %w", label, err)
		}
	}
	return nil
}

// HasScope reports whether a granted scope string includes one Encore needs.
//
// Spotify returns the granted scopes space-separated, and a token issued before
// a feature existed simply will not carry its scope — which is the signal to ask
// for it rather than to fail with a 403 the user cannot act on.
func HasScope(granted []string, want string) bool {
	for _, s := range granted {
		for f := range strings.SplitSeq(s, " ") {
			if f == want {
				return true
			}
		}
	}
	return false
}
