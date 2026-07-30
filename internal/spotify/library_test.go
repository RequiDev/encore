package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Saved tracks: /v1/me/tracks, offset paginated ---

// savedTrackScript describes what one page of the saved-tracks feed returns.
type savedTrackScript struct {
	items int
	next  bool
}

// savedTracksBody renders one scripted page of /v1/me/tracks.
func savedTracksBody(page int, s savedTrackScript) []byte {
	type item struct {
		AddedAt time.Time `json:"added_at"`
		Track   Track     `json:"track"`
	}
	type wire struct {
		Items []item `json:"items"`
		Next  string `json:"next"`
	}
	var body wire
	if s.next {
		body.Next = fmt.Sprintf("https://api.spotify.com/v1/me/tracks?offset=%d&limit=50", (page+1)*50)
	}
	base := time.Date(2026, time.July, 20, 10, 0, 0, 0, time.UTC)
	for i := range s.items {
		body.Items = append(body.Items, item{
			AddedAt: base.Add(time.Duration(page*100+i) * time.Minute),
			Track: Track{
				ID:   fmt.Sprintf("track%02d%02d0000000000000", page, i),
				Name: "Song",
				Type: "track",
			},
		})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestSavedTracksPagination(t *testing.T) {
	tests := []struct {
		name        string
		maxPages    int
		script      []savedTrackScript
		wantItems   int
		wantOffsets []string
	}{
		{
			name:        "single page, exhausted correctly",
			maxPages:    5,
			script:      []savedTrackScript{{items: 2, next: false}},
			wantItems:   2,
			wantOffsets: []string{"0"},
		},
		{
			name:        "three pages followed to the end, in order",
			maxPages:    5,
			script:      []savedTrackScript{{items: 50, next: true}, {items: 50, next: true}, {items: 10, next: false}},
			wantItems:   110,
			wantOffsets: []string{"0", "50", "100"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				offsets []string
				limits  []string
				page    int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/me/tracks" {
					t.Errorf("path = %q, want /v1/me/tracks", r.URL.Path)
				}
				mu.Lock()
				offsets = append(offsets, r.URL.Query().Get("offset"))
				limits = append(limits, r.URL.Query().Get("limit"))
				current := page
				page++
				mu.Unlock()

				if current >= len(tc.script) {
					t.Errorf("unexpected request %d beyond the script", current+1)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(savedTracksBody(current, tc.script[current]))
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			items, err := c.SavedTracks(context.Background(), "user-token", tc.maxPages)
			if err != nil {
				t.Fatalf("SavedTracks: %v", err)
			}
			if len(items) != tc.wantItems {
				t.Fatalf("got %d items, want %d", len(items), tc.wantItems)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(offsets) != len(tc.wantOffsets) {
				t.Fatalf("offsets = %v, want %v", offsets, tc.wantOffsets)
			}
			for i := range offsets {
				if offsets[i] != tc.wantOffsets[i] {
					t.Fatalf("offsets = %v, want %v", offsets, tc.wantOffsets)
				}
			}
			for _, l := range limits {
				if l != "50" {
					t.Fatalf("limit = %q, want 50", l)
				}
			}
			for i, it := range items {
				if it.Track.ID == "" || it.AddedAt.IsZero() {
					t.Fatalf("item %d decoded incompletely: %+v", i, it)
				}
			}
			// Confirm ordering: page 0's items precede page 1's, and so on.
			for i := 1; i < len(items); i++ {
				if items[i-1].AddedAt.After(items[i].AddedAt) {
					t.Fatalf("items out of order at %d: %+v then %+v", i, items[i-1], items[i])
				}
			}
		})
	}
}

// TestSavedTracksStopsAtMaxPages proves the page budget terminates a stub that
// always claims there is another page. A stub scripted with a fixed number of
// responses would merely error when the script ran out; this one answers every
// request identically, so a missing (or broken) maxPages guard would not fail
// the test — it would loop until the process runs out of memory or the test
// binary's own timeout kills it.
//
// It also pins that stopping at the cap is reported, not silent: the pages
// already read are still returned, but wrapped in ErrTruncated, so a caller
// that reconciles this against a local table (internal/library) can tell the
// difference between "that was everything" and "the budget ran out first."
func TestSavedTracksStopsAtMaxPages(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(savedTracksBody(0, savedTrackScript{items: 50, next: true}))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	items, err := c.SavedTracks(context.Background(), "user-token", 3)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("SavedTracks err = %v, want ErrTruncated: the page budget ran out with more remaining", err)
	}
	if len(items) != 150 {
		t.Fatalf("got %d items, want 150 (3 pages of 50) even though the enumeration was truncated", len(items))
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("server calls = %d, want 3: maxPages must terminate the loop", got)
	}
}

func TestSavedTracksForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"status":403,"message":"Insufficient client scope"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	items, err := c.SavedTracks(context.Background(), "user-token", 5)
	if items != nil {
		t.Fatalf("SavedTracks returned items alongside an error: %+v", items)
	}
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("error %v is not an APIError", err)
	}
	if !apiErr.IsForbidden() {
		t.Errorf("IsForbidden = false for status %d", apiErr.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server calls = %d, want 1: a 403 must not be retried", got)
	}
}

// --- Saved albums: /v1/me/albums, offset paginated ---

// savedAlbumScript describes what one page of the saved-albums feed returns.
type savedAlbumScript struct {
	items int
	next  bool
}

// savedAlbumsBody renders one scripted page of /v1/me/albums.
func savedAlbumsBody(page int, s savedAlbumScript) []byte {
	type item struct {
		AddedAt time.Time `json:"added_at"`
		Album   Album     `json:"album"`
	}
	type wire struct {
		Items []item `json:"items"`
		Next  string `json:"next"`
	}
	var body wire
	if s.next {
		body.Next = fmt.Sprintf("https://api.spotify.com/v1/me/albums?offset=%d&limit=50", (page+1)*50)
	}
	base := time.Date(2026, time.July, 20, 10, 0, 0, 0, time.UTC)
	for i := range s.items {
		body.Items = append(body.Items, item{
			AddedAt: base.Add(time.Duration(page*100+i) * time.Minute),
			Album: Album{
				ID:        fmt.Sprintf("album%02d%02d0000000000000", page, i),
				Name:      "Record",
				AlbumType: "album",
			},
		})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestSavedAlbumsPagination(t *testing.T) {
	tests := []struct {
		name        string
		maxPages    int
		script      []savedAlbumScript
		wantItems   int
		wantOffsets []string
	}{
		{
			name:        "single page, exhausted correctly",
			maxPages:    5,
			script:      []savedAlbumScript{{items: 3, next: false}},
			wantItems:   3,
			wantOffsets: []string{"0"},
		},
		{
			name:        "three pages followed to the end, in order",
			maxPages:    5,
			script:      []savedAlbumScript{{items: 50, next: true}, {items: 50, next: true}, {items: 5, next: false}},
			wantItems:   105,
			wantOffsets: []string{"0", "50", "100"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				offsets []string
				limits  []string
				page    int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/me/albums" {
					t.Errorf("path = %q, want /v1/me/albums", r.URL.Path)
				}
				mu.Lock()
				offsets = append(offsets, r.URL.Query().Get("offset"))
				limits = append(limits, r.URL.Query().Get("limit"))
				current := page
				page++
				mu.Unlock()

				if current >= len(tc.script) {
					t.Errorf("unexpected request %d beyond the script", current+1)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(savedAlbumsBody(current, tc.script[current]))
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			items, err := c.SavedAlbums(context.Background(), "user-token", tc.maxPages)
			if err != nil {
				t.Fatalf("SavedAlbums: %v", err)
			}
			if len(items) != tc.wantItems {
				t.Fatalf("got %d items, want %d", len(items), tc.wantItems)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(offsets) != len(tc.wantOffsets) {
				t.Fatalf("offsets = %v, want %v", offsets, tc.wantOffsets)
			}
			for i := range offsets {
				if offsets[i] != tc.wantOffsets[i] {
					t.Fatalf("offsets = %v, want %v", offsets, tc.wantOffsets)
				}
			}
			for _, l := range limits {
				if l != "50" {
					t.Fatalf("limit = %q, want 50", l)
				}
			}
			for i, it := range items {
				if it.Album.ID == "" || it.AddedAt.IsZero() {
					t.Fatalf("item %d decoded incompletely: %+v", i, it)
				}
			}
			for i := 1; i < len(items); i++ {
				if items[i-1].AddedAt.After(items[i].AddedAt) {
					t.Fatalf("items out of order at %d: %+v then %+v", i, items[i-1], items[i])
				}
			}
		})
	}
}

// TestSavedAlbumsStopsAtMaxPages mirrors TestSavedTracksStopsAtMaxPages: a stub
// that always claims another page exists, bounded only by the page budget,
// and the same ErrTruncated signal alongside the partial result.
func TestSavedAlbumsStopsAtMaxPages(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(savedAlbumsBody(0, savedAlbumScript{items: 50, next: true}))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	items, err := c.SavedAlbums(context.Background(), "user-token", 3)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("SavedAlbums err = %v, want ErrTruncated: the page budget ran out with more remaining", err)
	}
	if len(items) != 150 {
		t.Fatalf("got %d items, want 150 (3 pages of 50) even though the enumeration was truncated", len(items))
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("server calls = %d, want 3: maxPages must terminate the loop", got)
	}
}

func TestSavedAlbumsForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"status":403,"message":"Insufficient client scope"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	items, err := c.SavedAlbums(context.Background(), "user-token", 5)
	if items != nil {
		t.Fatalf("SavedAlbums returned items alongside an error: %+v", items)
	}
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("error %v is not an APIError", err)
	}
	if !apiErr.IsForbidden() {
		t.Errorf("IsForbidden = false for status %d", apiErr.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server calls = %d, want 1: a 403 must not be retried", got)
	}
}

// --- Followed artists: /v1/me/following?type=artist, cursor paginated and
// nested under "artists" ---

// followedArtistScript describes what one page of the followed-artists feed
// returns.
type followedArtistScript struct {
	items int
	after string
}

// followedArtistsBody renders one scripted page of /v1/me/following, correctly
// nested under the "artists" object the way Spotify actually sends it.
func followedArtistsBody(page int, s followedArtistScript) []byte {
	type wire struct {
		Artists struct {
			Items   []Artist `json:"items"`
			Cursors struct {
				After string `json:"after"`
			} `json:"cursors"`
		} `json:"artists"`
	}
	var body wire
	body.Artists.Cursors.After = s.after
	for i := range s.items {
		body.Artists.Items = append(body.Artists.Items, Artist{
			ID:   fmt.Sprintf("artist%02d%02d000000000000", page, i),
			Name: "Band",
			Type: "artist",
		})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestFollowedArtistsPagination(t *testing.T) {
	tests := []struct {
		name       string
		maxPages   int
		script     []followedArtistScript
		wantItems  int
		wantAfters []string
	}{
		{
			name:       "single page, exhausted correctly",
			maxPages:   5,
			script:     []followedArtistScript{{items: 2, after: ""}},
			wantItems:  2,
			wantAfters: []string{""},
		},
		{
			name:     "three pages followed to the end, in order",
			maxPages: 5,
			script: []followedArtistScript{
				{items: 50, after: "cursor-1"},
				{items: 50, after: "cursor-2"},
				{items: 10, after: ""},
			},
			wantItems:  110,
			wantAfters: []string{"", "cursor-1", "cursor-2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				afters []string
				types  []string
				page   int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/me/following" {
					t.Errorf("path = %q, want /v1/me/following", r.URL.Path)
				}
				mu.Lock()
				afters = append(afters, r.URL.Query().Get("after"))
				types = append(types, r.URL.Query().Get("type"))
				current := page
				page++
				mu.Unlock()

				if current >= len(tc.script) {
					t.Errorf("unexpected request %d beyond the script", current+1)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(followedArtistsBody(current, tc.script[current]))
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			artists, err := c.FollowedArtists(context.Background(), "user-token", tc.maxPages)
			if err != nil {
				t.Fatalf("FollowedArtists: %v", err)
			}
			if len(artists) != tc.wantItems {
				t.Fatalf("got %d artists, want %d", len(artists), tc.wantItems)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(afters) != len(tc.wantAfters) {
				t.Fatalf("after parameters = %v, want %v", afters, tc.wantAfters)
			}
			for i := range afters {
				if afters[i] != tc.wantAfters[i] {
					t.Fatalf("after parameters = %v, want %v", afters, tc.wantAfters)
				}
			}
			for _, ty := range types {
				if ty != "artist" {
					t.Fatalf("type = %q, want artist", ty)
				}
			}
			for i, a := range artists {
				if a.ID == "" {
					t.Fatalf("artist %d decoded incompletely: %+v", i, a)
				}
			}
		})
	}
}

// TestFollowedArtistsIgnoresFlatTopLevelItems pins the nesting hazard: Spotify
// wraps this one page under "artists", unlike every other paged endpoint
// Encore reads. A response with items at the top level must decode to nothing,
// not silently succeed by accident of Go's zero-value decoding.
func TestFollowedArtistsIgnoresFlatTopLevelItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"artist000000000000001","name":"Band","type":"artist"}],"cursors":{"after":"cursor-1"},"next":null}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	artists, err := c.FollowedArtists(context.Background(), "user-token", 5)
	if err != nil {
		t.Fatalf("FollowedArtists: %v", err)
	}
	if len(artists) != 0 {
		t.Fatalf("got %d artists from a flat top-level response, want 0: the page must be read from \"artists\"", len(artists))
	}
}

// TestFollowedArtistsStopsAtMaxPages proves the page budget terminates a stub
// whose cursor always advances, so there is no natural end to reach: only
// maxPages stops it. Without the guard this would hang exactly like the saved
// tracks and saved albums equivalents. Because the cursor was still advancing
// when the budget ran out, this is truncation, not exhaustion — unlike
// TestFollowedArtistsStopsOnRepeatedCursor below, where the cursor itself
// says there is nothing more.
func TestFollowedArtistsStopsAtMaxPages(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(followedArtistsBody(int(n), followedArtistScript{items: 50, after: fmt.Sprintf("cursor-%d", n)}))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	artists, err := c.FollowedArtists(context.Background(), "user-token", 3)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("FollowedArtists err = %v, want ErrTruncated: the page budget ran out with more remaining", err)
	}
	if len(artists) != 150 {
		t.Fatalf("got %d artists, want 150 (3 pages of 50) even though the enumeration was truncated", len(artists))
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("server calls = %d, want 3: maxPages must terminate the loop", got)
	}
}

// TestFollowedArtistsStopsOnRepeatedCursor exercises the seen-cursor guard
// specifically, not just the simpler "cursor didn't move at all" check: the
// server cycles cursor-A -> cursor-B -> cursor-A -> ..., a two-step loop that a
// same-as-last-time comparison alone would never catch (the cursor is always
// different from the immediately preceding one). maxPages is set generously
// (10) so that if the seen-cursor guard were missing, the loop would still be
// bounded by the page budget but would run for all 10 pages instead of
// stopping at 3 - making a missing guard a clear, deterministic test failure
// rather than a hang.
func TestFollowedArtistsStopsOnRepeatedCursor(t *testing.T) {
	// A true two-value cycle, indexed with modulo so it repeats forever rather
	// than settling on a final value: consecutive cursors are never equal to
	// each other, only to one seen two requests back.
	cycle := []string{"cursor-A", "cursor-B"}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		idx := (int(n) - 1) % len(cycle)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(followedArtistsBody(idx, followedArtistScript{items: 1, after: cycle[idx]}))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	artists, err := c.FollowedArtists(context.Background(), "user-token", 10)
	if err != nil {
		t.Fatalf("FollowedArtists: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("server calls = %d, want 3: a repeated after cursor must terminate the loop before the page budget does", got)
	}
	if len(artists) != 3 {
		t.Fatalf("got %d artists, want 3 (one per request before the repeat was detected)", len(artists))
	}
}

func TestFollowedArtistsForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"status":403,"message":"Insufficient client scope"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	artists, err := c.FollowedArtists(context.Background(), "user-token", 5)
	if artists != nil {
		t.Fatalf("FollowedArtists returned artists alongside an error: %+v", artists)
	}
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("error %v is not an APIError", err)
	}
	if !apiErr.IsForbidden() {
		t.Errorf("IsForbidden = false for status %d", apiErr.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server calls = %d, want 1: a 403 must not be retried", got)
	}
}
