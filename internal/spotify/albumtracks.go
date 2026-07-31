package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// defaultAlbumTrackPages bounds one album's listing when the caller does not
// say. Fifty a page, so twenty pages is a thousand tracks — longer than any
// released album — and short enough that a paging bug cannot spend the
// instance's whole quota on a single record.
const defaultAlbumTrackPages = 20

// AlbumTrack is one entry of an album's own track listing.
//
// Spotify answers /v1/albums/{id}/tracks with "simplified" track objects: no
// album, no popularity, no ISRC. Everything needed to name a track nobody has
// ever played is present, which is the whole purpose of reading it.
type AlbumTrack struct {
	ID          string
	Name        string
	DiscNumber  int
	TrackNumber int
}

// albumTrackPage is one response from /v1/albums/{id}/tracks.
type albumTrackPage struct {
	Items []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DiscNumber  int    `json:"disc_number"`
		TrackNumber int    `json:"track_number"`
	} `json:"items"`
	Next string `json:"next"`
}

// AlbumTracks reads every track Spotify lists for one album.
//
// Offset paginated at fifty a page, the same shape as SavedTracks, and it
// reports truncation the same way: a page budget spent while Spotify still had
// a next page returns the pages already read *alongside* ErrTruncated. The
// partial listing is real data, but it is not the whole listing, and a caller
// that replaces a stored set from it deletes the tail. See ErrTruncated's own
// comment for why that has to be the caller's problem rather than silently
// handled here.
//
// It reads with the application token rather than a listener's: an album's
// track list is public catalogue data and needs no user scope, so one instance
// makes one request for an album however many of its users open it.
//
// No market parameter is sent, so the ids are Spotify's canonical ones rather
// than relinked to a market. A listener whose play was recorded under a
// relinked id will therefore see that track listed as never played; that is a
// known limitation, documented in docs/api.md, not something to paper over by
// guessing at equivalences.
func (c *Client) AlbumTracks(ctx context.Context, albumID string, maxPages int) ([]AlbumTrack, error) {
	id := strings.TrimSpace(albumID)
	if !validID(id) {
		// The id becomes part of the request path rather than a query parameter,
		// so a malformed one must be refused here rather than sent. Ids Encore
		// minted locally from an export's names (domain.LocalAlbumID) land here
		// too, and there is no album on Spotify for them to ask about.
		return nil, fmt.Errorf("spotify: album tracks: %q is not a spotify album id", albumID)
	}

	path := "/v1/albums/" + id + "/tracks"
	var out []AlbumTrack
	for page := range albumTrackBudget(maxPages) {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		q.Set("offset", strconv.Itoa(page*maxLibraryPageSize))

		var p albumTrackPage
		if err := c.getAsApp(ctx, path, "get album tracks", q, &p); err != nil {
			return nil, fmt.Errorf("spotify: album tracks: %w", err)
		}
		for _, item := range p.Items {
			// A null or empty id is a local file, or a track Spotify will not
			// serve. It has no id to compare against a listen, so it is not
			// something this listing can say anything true about.
			if item.ID == "" {
				continue
			}
			out = append(out, AlbumTrack{
				ID:          item.ID,
				Name:        item.Name,
				DiscNumber:  item.DiscNumber,
				TrackNumber: item.TrackNumber,
			})
		}
		if len(p.Items) == 0 || strings.TrimSpace(p.Next) == "" {
			return out, nil
		}
	}
	// Every page read was full and still pointed at a next one: the budget ran
	// out before the album did, so out is a prefix, not the whole thing.
	return out, fmt.Errorf("spotify: album tracks: %w", ErrTruncated)
}

// albumTrackBudget clamps a caller's page limit, mirroring pageBudget.
func albumTrackBudget(maxPages int) int {
	if maxPages <= 0 {
		return defaultAlbumTrackPages
	}
	return maxPages
}
