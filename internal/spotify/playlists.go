package spotify

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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

// UserPlaylist is one entry of a listener's own playlist library — enough to
// give a bare playlist id (recorded against a play by an earlier phase) a
// name, plus the bookkeeping fields a reconciliation needs to tell whether
// that name is stale.
type UserPlaylist struct {
	ID          string
	Name        string
	OwnerID     string
	SnapshotID  string
	TotalTracks int
}

// userPlaylistPage is one response from /v1/me/playlists.
type userPlaylistPage struct {
	Items []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		SnapshotID string `json:"snapshot_id"`
		Owner      struct {
			ID string `json:"id"`
		} `json:"owner"`
		Tracks struct {
			Total int `json:"total"`
		} `json:"tracks"`
	} `json:"items"`
	Next string `json:"next"`
}

// UserPlaylists reads every playlist the listener owns or follows.
//
// Offset paginated, the identical shape to SavedTracks and SavedAlbums in
// internal/spotify/library.go: same page size, same "next" sentinel, same
// ErrTruncated signal when the page budget runs out before Spotify says
// there is nothing left. That signal exists for the same reason there as
// here — an earlier phase shipped a bug where a partial list came back with
// a nil error, and a caller reconciling it against a local table deleted the
// tail as though the short list were the whole of it, quietly losing rows
// for any listener whose library was larger than one page budget could
// cover. Task 4's playlist-context backfill reconciles this result the same
// way, so it needs the same warning, not a different one invented for this
// endpoint.
//
// Background rather than interactive: this runs on a worker tick, so it
// queues behind the catalogue budget rather than competing with somebody
// signing in.
func (c *Client) UserPlaylists(ctx context.Context, accessToken string, maxPages int) ([]UserPlaylist, error) {
	out := []UserPlaylist{}
	for page := range pageBudget(maxPages) {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		q.Set("offset", strconv.Itoa(page*maxLibraryPageSize))

		var p userPlaylistPage
		if err := c.get(ctx, "/v1/me/playlists", "get user playlists", q, accessToken, &p); err != nil {
			return nil, fmt.Errorf("spotify: user playlists: %w", err)
		}
		for _, item := range p.Items {
			out = append(out, UserPlaylist{
				ID:          item.ID,
				Name:        item.Name,
				OwnerID:     item.Owner.ID,
				SnapshotID:  item.SnapshotID,
				TotalTracks: item.Tracks.Total,
			})
		}
		if len(p.Items) == 0 || strings.TrimSpace(p.Next) == "" {
			return out, nil
		}
	}
	// Every page read was full and still pointed at a next one: the budget ran
	// out before the playlist list did, so out is a prefix, not the whole thing.
	return out, fmt.Errorf("spotify: user playlists: %w", ErrTruncated)
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
