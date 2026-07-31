package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultArtistAlbumPages bounds one artist's discography when the caller does
// not say. Fifty a page, so forty pages is two thousand releases.
//
// This is a ceiling, not a cost: the walk stops the moment `next` comes back
// empty, so an artist with eleven albums makes one request whether the cap is
// twenty or forty. Raising it charges nobody who is not already up against
// it — only artists who genuinely have that many releases pay for the extra
// pages, and those are exactly the artists a smaller cap was failing outright.
//
// It was raised from twenty after review found that outcome was not
// hypothetical. Because a truncated walk is recorded as a failure and
// ReplaceArtistAlbums is all-or-nothing under delete-absent semantics, an
// artist whose combined releases exceeded the cap never stored anything: at
// twenty pages the result was not "occasionally a slightly short list", it was
// "this artist's panel reads 'could not be read' forever", retried at twenty
// requests a time against a limiter where a single 429 pauses Spotify access
// for every user on the instance. That population is real — compilation-heavy
// legacy acts, prolific remixers, classical composers catalogued as one
// Spotify artist — and appears_on is the unbounded contributor, since it
// counts every record the artist has ever guested on. Forty pages does not
// make that population impossible, only larger before it recurs; the
// alternative already committed to (partial data is not stored) is what keeps
// it a ceiling rather than a promise.
const defaultArtistAlbumPages = 40

// ArtistAlbum is one release Spotify lists for an artist.
//
// Group is Spotify's album_group, which says what the *artist* is to the
// release — 'album', 'single', 'compilation' or 'appears_on'. It is not
// album_type, which says what the record is: a record this artist merely
// guests on has album_type "album" and album_group "appears_on", and
// completion counts the second.
type ArtistAlbum struct {
	ID               string
	Name             string
	Group            string
	ReleaseDate      *time.Time
	ReleasePrecision string
}

// artistAlbumPage is one response from /v1/artists/{id}/albums.
type artistAlbumPage struct {
	Items []struct {
		ID                   string `json:"id"`
		Name                 string `json:"name"`
		AlbumGroup           string `json:"album_group"`
		ReleaseDate          string `json:"release_date"`
		ReleaseDatePrecision string `json:"release_date_precision"`
	} `json:"items"`
	Next string `json:"next"`
}

// ArtistAlbums reads every release Spotify lists for one artist.
//
// Offset paginated at fifty a page, the same shape as SavedTracks and
// AlbumTracks, and it reports truncation the same way: a page budget spent
// while Spotify still had a next page returns the pages already read
// *alongside* ErrTruncated. The partial listing is real data, but it is not the
// whole discography, and a caller that replaces a stored set from it deletes
// the tail. See ErrTruncated's own comment for why that has to be the caller's
// problem rather than silently handled here.
//
// The walk's termination never depends on what the server sends: offset is
// driven from the loop counter, not from `next`, so the number of requests
// made is bounded by maxPages regardless of whether Spotify serves an honest
// `next`, an absent one, a `total` that disagrees with the items actually
// returned, or the same page over and over. An empty items slice or a blank
// `next` ends the walk early; anything else runs for exactly the budget and
// then reports truncation.
//
// **No include_groups parameter is sent**, so every group comes back. Asking
// only for 'album' would cut a prolific artist from forty requests to one, and
// it is still wrong: completion counts albums and excludes singles,
// compilations and appearances, so the page has to be able to say what it
// excluded. "You have heard 4 of 11 albums" with 340 unmentioned singles is an
// overclaim by omission, and there is nothing on disk to write the missing
// sentence from if this never fetched them.
//
// It reads with the application token rather than a listener's: an artist's
// discography is public catalogue data and needs no user scope, so one instance
// makes one walk for an artist however many of its users open them.
//
// No market parameter is sent, so the ids are Spotify's canonical ones rather
// than relinked to a market — the same choice AlbumTracks makes, and the same
// known limitation: a listener whose play was recorded under a relinked album
// id will see that album listed as never played.
func (c *Client) ArtistAlbums(ctx context.Context, artistID string, maxPages int) ([]ArtistAlbum, error) {
	id := strings.TrimSpace(artistID)
	if !validID(id) {
		// The id becomes part of the request path rather than a query parameter,
		// so a malformed one must be refused here rather than sent. Ids Encore
		// minted locally from an export's names (domain.LocalArtistID) land here
		// too, and there is no artist on Spotify for them to ask about.
		return nil, fmt.Errorf("spotify: artist albums: %q is not a spotify artist id", artistID)
	}

	path := "/v1/artists/" + id + "/albums"
	var out []ArtistAlbum
	for page := range artistAlbumBudget(maxPages) {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		q.Set("offset", strconv.Itoa(page*maxLibraryPageSize))

		var p artistAlbumPage
		if err := c.getAsApp(ctx, path, "get artist albums", q, &p); err != nil {
			return nil, fmt.Errorf("spotify: artist albums: %w", err)
		}
		for _, item := range p.Items {
			// A null or empty id is a release Spotify will not serve. It has no id
			// to compare against a listen, so it is not something this listing can
			// say anything true about.
			if item.ID == "" {
				continue
			}
			date, precision := ParseReleaseDate(item.ReleaseDate, item.ReleaseDatePrecision)
			out = append(out, ArtistAlbum{
				ID:               item.ID,
				Name:             item.Name,
				Group:            item.AlbumGroup,
				ReleaseDate:      date,
				ReleasePrecision: precision,
			})
		}
		if len(p.Items) == 0 || strings.TrimSpace(p.Next) == "" {
			return out, nil
		}
	}
	// Every page read was full and still pointed at a next one: the budget ran
	// out before the discography did, so out is a prefix, not the whole thing.
	return out, fmt.Errorf("spotify: artist albums: %w", ErrTruncated)
}

// artistAlbumBudget clamps a caller's page limit, mirroring pageBudget.
func artistAlbumBudget(maxPages int) int {
	if maxPages <= 0 {
		return defaultArtistAlbumPages
	}
	return maxPages
}
