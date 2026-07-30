package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// TopTimeRange selects which of Spotify's three rolling windows a top-items
// read draws its ranking from.
type TopTimeRange string

const (
	// TopShortTerm is approximately the last four weeks.
	TopShortTerm TopTimeRange = "short_term"
	// TopMediumTerm is approximately the last six months.
	TopMediumTerm TopTimeRange = "medium_term"
	// TopLongTerm is calculated from several years of listening history,
	// including new data as Spotify accumulates it.
	TopLongTerm TopTimeRange = "long_term"
)

// maxTopPageSize is Spotify's own cap on /v1/me/top/{type}. Unlike the other
// paged endpoints in this package (see maxLibraryPageSize), it is also the
// only page Encore ever asks for: see the no-pagination note on TopArtists.
const maxTopPageSize = 50

// topArtistsPage is one response from /v1/me/top/artists.
type topArtistsPage struct {
	Items []Artist `json:"items"`
	Next  string   `json:"next"`
}

// topTracksPage is one response from /v1/me/top/tracks.
type topTracksPage struct {
	Items []Track `json:"items"`
	Next  string  `json:"next"`
}

// clampTopLimit bounds a caller's limit to Spotify's page cap, and gives a
// non-positive value that same cap rather than passing it through literally:
// a limit of 0 would ask Spotify to return nothing, which is never what a
// caller means by "unspecified."
func clampTopLimit(limit int) int {
	if limit <= 0 || limit > maxTopPageSize {
		return maxTopPageSize
	}
	return limit
}

// TopArtists reads the listener's top artists for one rolling time range, as
// Spotify itself ranks them.
//
// GET /v1/me/top/artists is offset-paginated, but this deliberately reads only
// the single first page and never follows the response's next URL. Fifty is
// the whole picture this method exists to serve: the point of reading
// Spotify's own ranking is to diff it against Encore's for the listener's top
// fifty, and nobody needs a rank-51 comparison. One request per call is also
// what keeps a full comparison — three time ranges, two item types — at six
// requests total rather than an open-ended crawl. If a future reader is
// tempted to add a maxPages parameter here, don't: that would be solving a
// problem this endpoint was never asked to have.
//
// Background rather than interactive: this runs on a worker tick, so it
// queues behind the catalogue budget through c.get rather than competing with
// somebody signing in.
func (c *Client) TopArtists(ctx context.Context, accessToken string, tr TopTimeRange, limit int) ([]Artist, error) {
	q := url.Values{}
	q.Set("time_range", string(tr))
	q.Set("limit", strconv.Itoa(clampTopLimit(limit)))

	var p topArtistsPage
	if err := c.get(ctx, "/v1/me/top/artists", "get top artists", q, accessToken, &p); err != nil {
		return nil, fmt.Errorf("spotify: top artists: %w", err)
	}
	out := p.Items
	if out == nil {
		// Distinguishes "Spotify has no ranking for this listener yet" (an empty
		// slice) from a call that never reached this point (nil, alongside an
		// error) above.
		out = []Artist{}
	}
	return out, nil
}

// TopTracks reads the listener's top tracks for one rolling time range.
// Single page, for exactly the reason given on TopArtists.
func (c *Client) TopTracks(ctx context.Context, accessToken string, tr TopTimeRange, limit int) ([]Track, error) {
	q := url.Values{}
	q.Set("time_range", string(tr))
	q.Set("limit", strconv.Itoa(clampTopLimit(limit)))

	var p topTracksPage
	if err := c.get(ctx, "/v1/me/top/tracks", "get top tracks", q, accessToken, &p); err != nil {
		return nil, fmt.Errorf("spotify: top tracks: %w", err)
	}
	out := p.Items
	if out == nil {
		out = []Track{}
	}
	return out, nil
}
