package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// maxRecentlyPlayedLimit is the page size Spotify allows.
	maxRecentlyPlayedLimit = 50
	// defaultRecentlyPlayedPages bounds a poll that forgot to say how far back it
	// is willing to walk. Fifty items a page, so ten pages is five hundred plays:
	// far more than any listener produces between two sync intervals.
	defaultRecentlyPlayedPages = 10
)

// CurrentUser reads the profile of the listener the access token belongs to.
// Email and Product are present only when the grant includes user-read-email
// and user-read-private.
//
// Interactive: this is the second half of signing in, and the person is watching
// a browser tab. It must not queue behind a catalogue quota it did not spend.
func (c *Client) CurrentUser(ctx context.Context, accessToken string) (*UserProfile, error) {
	var p UserProfile
	if err := c.getClass(ctx, "/v1/me", "get current user", nil, accessToken, &p, true); err != nil {
		return nil, fmt.Errorf("spotify: current user: %w", err)
	}
	if p.ID == "" {
		return nil, fmt.Errorf("spotify: current user: profile carried no id")
	}
	return &p, nil
}

// recentlyPlayedPage is one response from the recently-played feed.
type recentlyPlayedPage struct {
	Items   []PlayHistory `json:"items"`
	Next    string        `json:"next"`
	Limit   int           `json:"limit"`
	Href    string        `json:"href"`
	Cursors struct {
		After  string `json:"after"`
		Before string `json:"before"`
	} `json:"cursors"`
}

// RecentlyPlayed reads the plays that happened after a watermark, following the
// forward cursor until the feed is exhausted or maxPages have been fetched.
//
// after is the caller's durable watermark, normally the newest played_at already
// committed; the zero time asks for the most recent page instead. Spotify keeps
// only the last fifty plays per account, so a gap longer than that is lost
// whatever the cursor says, which is why history is imported rather than polled.
//
// Items are returned in the order Spotify supplied them, newest first within
// each page. On any error nothing is returned, so a caller cannot mistake a
// partial read for a complete one and advance its watermark too far.
func (c *Client) RecentlyPlayed(ctx context.Context, accessToken string, after time.Time, limit int, maxPages int) ([]PlayHistory, error) {
	if limit <= 0 || limit > maxRecentlyPlayedLimit {
		limit = maxRecentlyPlayedLimit
	}
	if maxPages <= 0 {
		maxPages = defaultRecentlyPlayedPages
	}

	cursor := ""
	if !after.IsZero() {
		cursor = strconv.FormatInt(after.UTC().UnixMilli(), 10)
	}

	var out []PlayHistory
	seen := make(map[string]struct{}, maxPages)
	for range maxPages {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(limit))
		if cursor != "" {
			q.Set("after", cursor)
		}

		var page recentlyPlayedPage
		if err := c.get(ctx, "/v1/me/player/recently-played", "get recently played", q, accessToken, &page); err != nil {
			return nil, fmt.Errorf("spotify: recently played: %w", err)
		}
		out = append(out, page.Items...)

		next := strings.TrimSpace(page.Cursors.After)
		if len(page.Items) == 0 || next == "" || next == cursor {
			break
		}
		// A cursor that comes back round would page for ever. Bounded by maxPages
		// anyway, but stopping here keeps a misbehaving upstream from spending the
		// whole rate budget on one account.
		if _, repeat := seen[next]; repeat {
			break
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return out, nil
}
