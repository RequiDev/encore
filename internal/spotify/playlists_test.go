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
)

// --- User playlists: /v1/me/playlists, offset paginated ---

// userPlaylistScript describes what one page of the user-playlists feed
// returns.
type userPlaylistScript struct {
	items int
	next  bool
}

// userPlaylistsBody renders one scripted page of /v1/me/playlists.
func userPlaylistsBody(page int, s userPlaylistScript) []byte {
	type owner struct {
		ID string `json:"id"`
	}
	type tracks struct {
		Total int `json:"total"`
	}
	type item struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		SnapshotID string `json:"snapshot_id"`
		Owner      owner  `json:"owner"`
		Tracks     tracks `json:"tracks"`
	}
	type wire struct {
		Items []item `json:"items"`
		Next  string `json:"next"`
	}
	var body wire
	if s.next {
		body.Next = fmt.Sprintf("https://api.spotify.com/v1/me/playlists?offset=%d&limit=50", (page+1)*50)
	}
	for i := range s.items {
		body.Items = append(body.Items, item{
			ID:         fmt.Sprintf("playlist%02d%02d00000000000", page, i),
			Name:       "Mix",
			SnapshotID: fmt.Sprintf("snapshot-%d-%d", page, i),
			Owner:      owner{ID: fmt.Sprintf("owner%02d%02d000000000000", page, i)},
			Tracks:     tracks{Total: page*100 + i},
		})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestUserPlaylistsPagination(t *testing.T) {
	tests := []struct {
		name        string
		maxPages    int
		script      []userPlaylistScript
		wantItems   int
		wantOffsets []string
	}{
		{
			name:        "single page, exhausted correctly",
			maxPages:    5,
			script:      []userPlaylistScript{{items: 2, next: false}},
			wantItems:   2,
			wantOffsets: []string{"0"},
		},
		{
			name:        "three pages followed to the end, in order",
			maxPages:    5,
			script:      []userPlaylistScript{{items: 50, next: true}, {items: 50, next: true}, {items: 10, next: false}},
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
				if r.URL.Path != "/v1/me/playlists" {
					t.Errorf("path = %q, want /v1/me/playlists", r.URL.Path)
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
				_, _ = w.Write(userPlaylistsBody(current, tc.script[current]))
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			items, err := c.UserPlaylists(context.Background(), "user-token", tc.maxPages)
			if err != nil {
				t.Fatalf("UserPlaylists: %v", err)
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
				if it.ID == "" {
					t.Fatalf("item %d decoded incompletely: %+v", i, it)
				}
			}
			// Confirm ordering: page 0's items precede page 1's, and so on, by
			// checking the page number embedded in each synthetic id.
			for i := 1; i < len(items); i++ {
				if items[i-1].ID > items[i].ID {
					t.Fatalf("items out of order at %d: %+v then %+v", i, items[i-1], items[i])
				}
			}
		})
	}
}

// TestUserPlaylistsFieldExtraction pins that owner.id, snapshot_id and
// tracks.total — all nested one level deeper than the flat fields Encore's
// other paged endpoints decode — land in the right struct fields rather than
// being silently dropped by a mismatched tag.
func TestUserPlaylistsFieldExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"playlist000000000001","name":"Road Trip","snapshot_id":"snap-abc123","owner":{"id":"listener000000000001"},"tracks":{"total":42}}],"next":null}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	items, err := c.UserPlaylists(context.Background(), "user-token", 5)
	if err != nil {
		t.Fatalf("UserPlaylists: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	got := items[0]
	want := UserPlaylist{
		ID:          "playlist000000000001",
		Name:        "Road Trip",
		OwnerID:     "listener000000000001",
		SnapshotID:  "snap-abc123",
		TotalTracks: 42,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestUserPlaylistsStopsAtMaxPages proves the page budget terminates a stub
// that always claims there is another page. A stub scripted with a fixed
// number of responses would merely error when the script ran out; this one
// answers every request identically, so a missing (or broken) maxPages guard
// would not fail the test — it would loop until the process runs out of
// memory or the test binary's own timeout kills it.
//
// It also pins that stopping at the cap is reported, not silent: the pages
// already read are still returned, but wrapped in ErrTruncated, so a caller
// that reconciles this against a local table (Task 4's playlist-context
// backfill) can tell the difference between "that was every playlist" and
// "the budget ran out first" — the same distinction library.go's SavedTracks
// makes, for the same reason: a partial list mistaken for the complete set
// would delete rows that are still real.
func TestUserPlaylistsStopsAtMaxPages(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(userPlaylistsBody(0, userPlaylistScript{items: 50, next: true}))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	items, err := c.UserPlaylists(context.Background(), "user-token", 3)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("UserPlaylists err = %v, want ErrTruncated: the page budget ran out with more remaining", err)
	}
	if len(items) != 150 {
		t.Fatalf("got %d items, want 150 (3 pages of 50) even though the enumeration was truncated", len(items))
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("server calls = %d, want 3: maxPages must terminate the loop", got)
	}
}

func TestUserPlaylistsForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"status":403,"message":"Insufficient client scope"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	items, err := c.UserPlaylists(context.Background(), "user-token", 5)
	if items != nil {
		t.Fatalf("UserPlaylists returned items alongside an error: %+v", items)
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

// TestUserPlaylistsEmptyIsEmptySlice pins that an empty result is an empty
// slice, never nil, across every shape Spotify might send it in. A body of
// {"items":[]} alone would pass even without a nil-guard, since
// encoding/json already decodes a JSON array into a non-nil empty Go slice on
// its own — a tautological test an earlier task in this project shipped by
// mistake. The key-absent and explicit-null bodies below are the ones that
// actually exercise the guard, because encoding/json leaves the destination
// field nil in both of those cases.
func TestUserPlaylistsEmptyIsEmptySlice(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "items key absent", body: `{}`},
		{name: "items explicit null", body: `{"items":null,"next":null}`},
		{name: "items explicit empty array", body: `{"items":[],"next":null}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			items, err := c.UserPlaylists(context.Background(), "user-token", 5)
			if err != nil {
				t.Fatalf("UserPlaylists: %v", err)
			}
			if items == nil {
				t.Fatalf("UserPlaylists returned nil, want a non-nil empty slice")
			}
			if len(items) != 0 {
				t.Fatalf("got %d items, want 0", len(items))
			}
		})
	}
}
