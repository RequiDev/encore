package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// searchResponse is the slice of /v1/search Encore uses.
type searchResponse struct {
	Tracks struct {
		Items []Track `json:"items"`
		Total int     `json:"total"`
		Limit int     `json:"limit"`
	} `json:"tracks"`
}

// SearchTrack finds the one catalogue track that best matches a normalised
// artist and title pair from a names-only export.
//
// This is the alias resolver's only call, and it costs one request per distinct
// pair, so it asks for a single result and takes Spotify's own relevance
// ordering as the answer. A pair with no match returns (nil, nil): that is a
// normal outcome for a local file or a track withdrawn from the catalogue, not
// a failure.
func (c *Client) SearchTrack(ctx context.Context, artist, title string) (*Track, error) {
	title = searchTerm(title)
	artist = searchTerm(artist)
	if title == "" {
		// Nothing can match, so do not spend a request finding that out.
		return nil, nil
	}

	q := `track:"` + title + `"`
	if artist != "" {
		q += ` artist:"` + artist + `"`
	}
	query := url.Values{}
	query.Set("q", q)
	query.Set("type", "track")
	query.Set("limit", "1")

	var resp searchResponse
	if err := c.getAsApp(ctx, "/v1/search", "search track", query, &resp); err != nil {
		return nil, fmt.Errorf("spotify: search track: %w", err)
	}
	for _, t := range resp.Tracks.Items {
		if t.ID == "" {
			continue
		}
		return &t, nil
	}
	return nil, nil
}

// searchTerm makes a name safe to embed in a field-qualified query. The double
// quote is the only character that could break out of the field, but control
// characters are dropped too so a corrupt export cannot inject a newline into
// the request line.
func searchTerm(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
