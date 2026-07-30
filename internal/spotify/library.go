package spotify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// maxLibraryPageSize is Spotify's cap on all three of these endpoints.
	maxLibraryPageSize = 50
	// defaultLibraryPages bounds an enumeration whose caller did not say how far
	// it is willing to walk. Fifty a page, so 200 pages is 10,000 items — larger
	// than any personal library, and small enough that a misbehaving upstream
	// cannot spend the whole quota here.
	defaultLibraryPages = 200
)

// ErrTruncated reports that an enumeration stopped because its page budget
// was spent while Spotify still had a next page to offer, not because the
// listing was exhausted.
//
// It is returned wrapped alongside the partial result rather than in place of
// it — unlike every other error these three functions return, where nothing
// is returned at all — because the pages already read are still real data;
// what is missing is the caller's guarantee that they are the whole of it. A
// caller that reconciles this result against a local table (as
// internal/library does) must treat that partial listing as unsafe to use as
// the authoritative set: a shorter-than-actual result deletes rows that are
// still saved, just never reached.
var ErrTruncated = errors.New("spotify: enumeration stopped at the page budget with more remaining")

// SavedTrack is one entry of a listener's saved tracks.
type SavedTrack struct {
	Track   Track
	AddedAt time.Time
}

// SavedAlbum is one entry of a listener's saved albums.
type SavedAlbum struct {
	Album   Album
	AddedAt time.Time
}

// savedTrackPage is one response from /v1/me/tracks.
type savedTrackPage struct {
	Items []struct {
		AddedAt time.Time `json:"added_at"`
		Track   Track     `json:"track"`
	} `json:"items"`
	Next string `json:"next"`
}

// savedAlbumPage is one response from /v1/me/albums.
type savedAlbumPage struct {
	Items []struct {
		AddedAt time.Time `json:"added_at"`
		Album   Album     `json:"album"`
	} `json:"items"`
	Next string `json:"next"`
}

// followedArtistPage is one response from /v1/me/following.
//
// Note the extra nesting: unlike every other paged endpoint Encore reads, this
// one wraps its page in an object named for the type being followed.
type followedArtistPage struct {
	Artists struct {
		Items   []Artist `json:"items"`
		Next    string   `json:"next"`
		Cursors struct {
			After string `json:"after"`
		} `json:"cursors"`
	} `json:"artists"`
}

// pageBudget clamps a caller's page limit.
func pageBudget(maxPages int) int {
	if maxPages <= 0 {
		return defaultLibraryPages
	}
	return maxPages
}

// SavedTracks reads every track in the listener's library.
//
// Offset paginated. Background rather than interactive: this runs on a worker
// tick, so it queues behind the catalogue budget rather than competing with
// somebody signing in.
func (c *Client) SavedTracks(ctx context.Context, accessToken string, maxPages int) ([]SavedTrack, error) {
	var out []SavedTrack
	for page := range pageBudget(maxPages) {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		q.Set("offset", strconv.Itoa(page*maxLibraryPageSize))

		var p savedTrackPage
		if err := c.get(ctx, "/v1/me/tracks", "get saved tracks", q, accessToken, &p); err != nil {
			return nil, fmt.Errorf("spotify: saved tracks: %w", err)
		}
		for _, item := range p.Items {
			out = append(out, SavedTrack{Track: item.Track, AddedAt: item.AddedAt})
		}
		if len(p.Items) == 0 || strings.TrimSpace(p.Next) == "" {
			return out, nil
		}
	}
	// Every page read was full and still pointed at a next one: the budget ran
	// out before the library did, so out is a prefix, not the whole thing.
	return out, fmt.Errorf("spotify: saved tracks: %w", ErrTruncated)
}

// SavedAlbums reads every album in the listener's library. Offset paginated,
// exactly as SavedTracks.
func (c *Client) SavedAlbums(ctx context.Context, accessToken string, maxPages int) ([]SavedAlbum, error) {
	var out []SavedAlbum
	for page := range pageBudget(maxPages) {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		q.Set("offset", strconv.Itoa(page*maxLibraryPageSize))

		var p savedAlbumPage
		if err := c.get(ctx, "/v1/me/albums", "get saved albums", q, accessToken, &p); err != nil {
			return nil, fmt.Errorf("spotify: saved albums: %w", err)
		}
		for _, item := range p.Items {
			out = append(out, SavedAlbum{Album: item.Album, AddedAt: item.AddedAt})
		}
		if len(p.Items) == 0 || strings.TrimSpace(p.Next) == "" {
			return out, nil
		}
	}
	// Every page read was full and still pointed at a next one: the budget ran
	// out before the library did, so out is a prefix, not the whole thing.
	return out, fmt.Errorf("spotify: saved albums: %w", ErrTruncated)
}

// FollowedArtists reads every artist the listener follows.
//
// Cursor paginated rather than offset, and nested under an "artists" object —
// the only endpoint Encore reads that does either. The repeat-cursor guard is
// the same one RecentlyPlayed carries: a cursor that comes back round would
// page for ever, and the page budget alone would spend the whole allowance
// discovering it.
func (c *Client) FollowedArtists(ctx context.Context, accessToken string, maxPages int) ([]Artist, error) {
	var out []Artist
	seen := make(map[string]struct{}, pageBudget(maxPages))
	cursor := ""

	for range pageBudget(maxPages) {
		q := url.Values{}
		q.Set("type", "artist")
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		if cursor != "" {
			q.Set("after", cursor)
		}

		var p followedArtistPage
		if err := c.get(ctx, "/v1/me/following", "get followed artists", q, accessToken, &p); err != nil {
			return nil, fmt.Errorf("spotify: followed artists: %w", err)
		}
		out = append(out, p.Artists.Items...)

		next := strings.TrimSpace(p.Artists.Cursors.After)
		// A cursor that stalls or repeats is exhaustion, not truncation: Spotify
		// itself has stopped offering anything new, which is a different
		// condition from the page budget cutting the walk short below.
		if len(p.Artists.Items) == 0 || next == "" || next == cursor {
			return out, nil
		}
		if _, repeat := seen[next]; repeat {
			return out, nil
		}
		seen[next] = struct{}{}
		cursor = next
	}
	// Every page read was full and the cursor still advanced to something new:
	// the budget ran out before the follow list did, so out is a prefix, not
	// the whole thing.
	return out, fmt.Errorf("spotify: followed artists: %w", ErrTruncated)
}
