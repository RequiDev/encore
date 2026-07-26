package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Batch sizes imposed by Spotify on the "several" endpoints. The enrichment
// workers size their queries from these constants.
const (
	MaxTrackIDsPerRequest  = 50
	MaxArtistIDsPerRequest = 50
	MaxAlbumIDsPerRequest  = 20
)

// GetTracks reads full track objects for up to any number of ids, chunked to the
// batch size Spotify accepts.
//
// Spotify answers with a null in place of an id it cannot serve — deleted,
// region-locked, or relinked to something that no longer exists. Those ids are
// simply absent from the result rather than being an error for the whole batch,
// so the caller compares what it asked for against what it got and marks the
// difference unavailable.
func (c *Client) GetTracks(ctx context.Context, ids []string) ([]Track, error) {
	clean := cleanIDs(ids)
	if len(clean) == 0 {
		return nil, nil
	}
	out := make([]Track, 0, len(clean))
	for _, batch := range chunk(clean, MaxTrackIDsPerRequest) {
		var page struct {
			Tracks []*Track `json:"tracks"`
		}
		if err := c.getAsApp(ctx, "/v1/tracks", "get tracks", idQuery(batch), &page); err != nil {
			return nil, fmt.Errorf("spotify: get tracks: %w", err)
		}
		for _, t := range page.Tracks {
			if t == nil || t.ID == "" {
				continue
			}
			out = append(out, *t)
		}
	}
	return out, nil
}

// GetArtists reads full artist objects, chunked at fifty ids per request.
// Unavailable ids are absent from the result, as for GetTracks.
func (c *Client) GetArtists(ctx context.Context, ids []string) ([]Artist, error) {
	clean := cleanIDs(ids)
	if len(clean) == 0 {
		return nil, nil
	}
	out := make([]Artist, 0, len(clean))
	for _, batch := range chunk(clean, MaxArtistIDsPerRequest) {
		var page struct {
			Artists []*Artist `json:"artists"`
		}
		if err := c.getAsApp(ctx, "/v1/artists", "get artists", idQuery(batch), &page); err != nil {
			return nil, fmt.Errorf("spotify: get artists: %w", err)
		}
		for _, a := range page.Artists {
			if a == nil || a.ID == "" {
				continue
			}
			out = append(out, *a)
		}
	}
	return out, nil
}

// GetAlbums reads full album objects, chunked at twenty ids per request, which
// is the lower limit Spotify imposes on this endpoint. Unavailable ids are
// absent from the result, as for GetTracks.
func (c *Client) GetAlbums(ctx context.Context, ids []string) ([]Album, error) {
	clean := cleanIDs(ids)
	if len(clean) == 0 {
		return nil, nil
	}
	out := make([]Album, 0, len(clean))
	for _, batch := range chunk(clean, MaxAlbumIDsPerRequest) {
		var page struct {
			Albums []*Album `json:"albums"`
		}
		if err := c.getAsApp(ctx, "/v1/albums", "get albums", idQuery(batch), &page); err != nil {
			return nil, fmt.Errorf("spotify: get albums: %w", err)
		}
		for _, a := range page.Albums {
			if a == nil || a.ID == "" {
				continue
			}
			out = append(out, *a)
		}
	}
	return out, nil
}

// getAsApp performs a catalogue read with the application token.
//
// A 401 on a token that had not yet expired means Spotify retired it early; the
// cached copy is dropped and the read is tried once more, so one revoked token
// costs a single request rather than a whole enrichment batch.
func (c *Client) getAsApp(ctx context.Context, path, label string, query url.Values, out any) error {
	token, err := c.AppToken(ctx)
	if err != nil {
		return err
	}
	err = c.get(ctx, path, label, query, token, out)
	if apiErr, ok := AsAPIError(err); !ok || !apiErr.IsUnauthorized() {
		return err
	}

	c.invalidateAppTokenIf(token)
	retryToken, retryErr := c.AppToken(ctx)
	if retryErr != nil {
		return err
	}
	return c.get(ctx, path, label, query, retryToken, out)
}

// idQuery renders the comma-separated ids parameter shared by the batch
// endpoints.
func idQuery(ids []string) url.Values {
	return url.Values{"ids": {strings.Join(ids, ",")}}
}

// cleanIDs trims, de-duplicates and drops ids that cannot be Spotify ids.
//
// Filtering locally matters because Spotify rejects a whole batch when a single
// id is malformed: one corrupt row in an import would otherwise stall
// enrichment for the forty-nine good ids travelling with it.
func cleanIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if !validID(id) {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// validID checks the base-62 shape of a Spotify id without asserting a fixed
// length, since Spotify has never formally guaranteed twenty-two characters.
func validID(s string) bool {
	if len(s) < 10 || len(s) > 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

// chunk splits s into runs of at most n.
func chunk[T any](s []T, n int) [][]T {
	if n < 1 {
		n = 1
	}
	out := make([][]T, 0, (len(s)+n-1)/n)
	for i := 0; i < len(s); i += n {
		end := min(i+n, len(s))
		out = append(out, s[i:end])
	}
	return out
}
