package spotify

import (
	"bytes"
	"context"
	"encoding/base64"
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

// TestUpdatePlaylistDetailsSendsBothFields pins that one request carries the
// name and the description together, so there is no state in which Spotify has
// the new name and the old description.
//
// Fails when: the two are split into separate requests, or either key is
// dropped from the body.
func TestUpdatePlaylistDetailsSendsBothFields(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth string
		gotBody                     map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	err := c.UpdatePlaylistDetails(context.Background(), "user-token", "playlist01",
		"Heavy rotation", "Your 100 most played tracks of all time.")
	if err != nil {
		t.Fatalf("UpdatePlaylistDetails: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/v1/playlists/playlist01" {
		t.Errorf("path = %q, want /v1/playlists/playlist01", gotPath)
	}
	if gotAuth != "Bearer user-token" {
		t.Errorf("authorization = %q, want the listener's own token", gotAuth)
	}
	if gotBody["name"] != "Heavy rotation" {
		t.Errorf("name = %v, want Heavy rotation", gotBody["name"])
	}
	if gotBody["description"] != "Your 100 most played tracks of all time." {
		t.Errorf("description = %v, want the generated sentence", gotBody["description"])
	}
}

// TestUpdatePlaylistDescriptionSendsNoName pins the one property this call
// exists for.
//
// A rebuild refreshes the description, because it names the date of the last
// build and the rebuild has just made it false. It must do that without
// touching the name: the listener may have renamed the playlist in the Spotify
// app, Encore never recorded that, and overwriting it would destroy something
// nothing here could restore.
//
// The absence of the key is the assertion. An empty name is not "leave it
// alone" — Spotify takes it, and the playlist ends up with no name at all.
//
// Fails when: "name" is added to the body under any value, including "".
func TestUpdatePlaylistDescriptionSendsNoName(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth string
		gotBody                     map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	err := c.UpdatePlaylistDescription(context.Background(), "user-token", "playlist01",
		"Your 100 most played tracks of all time.")
	if err != nil {
		t.Fatalf("UpdatePlaylistDescription: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/v1/playlists/playlist01" {
		t.Errorf("path = %q, want /v1/playlists/playlist01", gotPath)
	}
	if gotAuth != "Bearer user-token" {
		t.Errorf("authorization = %q, want the listener's own token", gotAuth)
	}
	if gotBody["description"] != "Your 100 most played tracks of all time." {
		t.Errorf("description = %v, want the generated sentence", gotBody["description"])
	}
	if name, present := gotBody["name"]; present {
		t.Errorf("the body carries name = %q; a partial update must omit it entirely, or "+
			"a name the listener chose in Spotify is overwritten by this call", name)
	}
}

// TestSetPlaylistCoverSendsBase64UnderImageJPEG pins the body shape Spotify
// documents: base64 text, Content-Type image/jpeg, not multipart and not JSON.
//
// Fails when: the raw JPEG is sent instead of its base64 (the decoded bytes
// then differ), or the content type reverts to application/json.
func TestSetPlaylistCoverSendsBase64UnderImageJPEG(t *testing.T) {
	want := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}

	var gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", want); err != nil {
		t.Fatalf("SetPlaylistCover: %v", err)
	}

	if gotType != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", gotType)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(gotBody))
	if err != nil {
		t.Fatalf("body is not base64: %v", err)
	}
	if !bytes.Equal(decoded, want) {
		t.Errorf("decoded body = %v, want %v", decoded, want)
	}
}

// TestSetPlaylistCoverRefusesAnEmptyImage is the delete-absent rule wearing a
// different hat: this call *replaces* whatever cover the playlist has, so a
// zero-length body is a partial input reaching a replace.
//
// Fails when: the length guard is removed — the request then reaches Spotify,
// which answers 400 *after* the listener has been told a cover was set.
func TestSetPlaylistCoverRefusesAnEmptyImage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	for _, empty := range [][]byte{nil, {}} {
		if err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", empty); err == nil {
			t.Errorf("SetPlaylistCover(%v): want an error, got nil", empty)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("%d requests reached Spotify, want 0", got)
	}
}

// TestSetPlaylistCoverRefusesAnOversizedImage is the other end of the same
// delete-absent discipline as the empty-image guard: a binary JPEG over
// MaxPlaylistCoverBytes is refused before it is ever base64-encoded or sent,
// rather than reaching Spotify and being told no after the fact.
//
// Fails when: the len(jpeg) > MaxPlaylistCoverBytes guard is removed, or its
// comparison is inverted so an oversized image passes instead of a
// within-bounds one.
func TestSetPlaylistCoverRefusesAnOversizedImage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	oversized := make([]byte, MaxPlaylistCoverBytes+1)
	if err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", oversized); err == nil {
		t.Error("SetPlaylistCover: want an error for an image over MaxPlaylistCoverBytes, got nil")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("%d requests reached Spotify, want 0", got)
	}
}

// TestSetPlaylistCoverRebuildsTheBodyOnRetry pins that the raw body survives a
// retry.
//
// This is the specific bug a raw []byte body invites: an io.Reader built once
// and handed to a retry loop is drained by the first attempt, so the second
// sends nothing — and "nothing" here means replacing a cover with an empty
// image. attempt() must build the reader from r.raw on every call.
//
// Fails when: the body reader is hoisted out of attempt() into do(), which is
// the natural-looking refactor that breaks it.
func TestSetPlaylistCoverRebuildsTheBodyOnRetry(t *testing.T) {
	want := []byte{0xFF, 0xD8, 0xFF, 0xDB, 1, 2, 3, 4}

	var calls atomic.Int32
	var lastLen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastLen.Store(int32(len(body)))
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", want); err != nil {
		t.Fatalf("SetPlaylistCover: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if got := int(lastLen.Load()); got != base64.StdEncoding.EncodedLen(len(want)) {
		t.Fatalf("the last attempt sent %d bytes, want %d — the body was not rebuilt",
			got, base64.StdEncoding.EncodedLen(len(want)))
	}
}

// TestPlaylistWritesDrawOnTheSignInBudget is the instance-wide safety property
// of this whole feature.
//
// A 429 on a background request pauses the catalogue limiter *and* records
// app_settings.spotify_paused_until, which 409s "sync now" for every user on
// the instance and stops enrichment. Both playlist writes are interactive, so
// a 429 on either pauses only the sign-in budget and never reaches the pause
// observer at all.
//
// Fails when: interactive: true is dropped from either request — the pause
// observer then fires and the assertion below catches it.
func TestPlaylistWritesDrawOnTheSignInBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var paused atomic.Int32
	c := newTestClient(t, srv, newFakeClock(),
		WithPauseObserver(func(time.Time) { paused.Add(1) }))

	if err := c.UpdatePlaylistDetails(context.Background(), "user-token", "playlist01", "n", "d"); err == nil {
		t.Fatal("UpdatePlaylistDetails: want an error on a 429")
	}
	if err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", []byte{1, 2, 3}); err == nil {
		t.Fatal("SetPlaylistCover: want an error on a 429")
	}
	if got := paused.Load(); got != 0 {
		t.Fatalf("the pause observer fired %d times; a playlist write must never "+
			"pause Spotify instance-wide", got)
	}
}

// TestPlaylistWriteForbiddenIsNotRetried pins that a missing scope costs one
// request, not six.
//
// Fails when: classify stops wrapping non-429 4xx in retry.Stop, or a caller
// grows its own retry around these methods.
func TestPlaylistWriteForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", []byte{1, 2, 3})
	apiErr, ok := AsAPIError(err)
	if !ok || !apiErr.IsForbidden() {
		t.Fatalf("error = %v, want a 403 APIError", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a scope failure spends quota to fail identically", got)
	}
}
