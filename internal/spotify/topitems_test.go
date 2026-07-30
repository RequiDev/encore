package spotify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// --- Top artists: /v1/me/top/artists, deliberately single-page ---

func TestTopArtistsTimeRangeAndPath(t *testing.T) {
	tests := []struct {
		name string
		tr   TopTimeRange
		want string
	}{
		{"short term", TopShortTerm, "short_term"},
		{"medium term", TopMediumTerm, "medium_term"},
		{"long term", TopLongTerm, "long_term"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotTimeRange string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotTimeRange = r.URL.Query().Get("time_range")
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"items":[{"id":"artist000000000000001","name":"Band","type":"artist"}],"next":null}`)
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			artists, err := c.TopArtists(context.Background(), "user-token", tc.tr, 20)
			if err != nil {
				t.Fatalf("TopArtists: %v", err)
			}
			if gotPath != "/v1/me/top/artists" {
				t.Errorf("path = %q, want /v1/me/top/artists", gotPath)
			}
			if gotTimeRange != tc.want {
				t.Errorf("time_range = %q, want %q", gotTimeRange, tc.want)
			}
			if len(artists) != 1 || artists[0].ID == "" {
				t.Fatalf("artists = %+v, decoded incompletely", artists)
			}
		})
	}
}

func TestTopTracksTimeRangeAndPath(t *testing.T) {
	tests := []struct {
		name string
		tr   TopTimeRange
		want string
	}{
		{"short term", TopShortTerm, "short_term"},
		{"medium term", TopMediumTerm, "medium_term"},
		{"long term", TopLongTerm, "long_term"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotTimeRange string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotTimeRange = r.URL.Query().Get("time_range")
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"items":[{"id":"track00000000000000001","name":"Song","type":"track"}],"next":null}`)
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			tracks, err := c.TopTracks(context.Background(), "user-token", tc.tr, 20)
			if err != nil {
				t.Fatalf("TopTracks: %v", err)
			}
			if gotPath != "/v1/me/top/tracks" {
				t.Errorf("path = %q, want /v1/me/top/tracks", gotPath)
			}
			if gotTimeRange != tc.want {
				t.Errorf("time_range = %q, want %q", gotTimeRange, tc.want)
			}
			if len(tracks) != 1 || tracks[0].ID == "" {
				t.Fatalf("tracks = %+v, decoded incompletely", tracks)
			}
		})
	}
}

// TestTopArtistsLimitIsClamped pins both halves of the clamp: an oversized
// limit is capped at Spotify's own maximum of 50, and a non-positive one is
// not passed through literally (which would ask Spotify for limit=0 items)
// but given the same sensible default instead.
func TestTopArtistsLimitIsClamped(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		want  string
	}{
		{"within bounds", 10, "10"},
		{"exactly the max", 50, "50"},
		{"over the max is clamped", 500, "50"},
		{"zero gets a default", 0, "50"},
		{"negative gets a default", -3, "50"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotLimit string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotLimit = r.URL.Query().Get("limit")
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"items":[],"next":null}`)
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			if _, err := c.TopArtists(context.Background(), "user-token", TopShortTerm, tc.limit); err != nil {
				t.Fatalf("TopArtists: %v", err)
			}
			if gotLimit != tc.want {
				t.Errorf("limit = %q, want %q", gotLimit, tc.want)
			}
		})
	}
}

// TestTopTracksLimitIsClamped mirrors TestTopArtistsLimitIsClamped for the
// tracks endpoint.
func TestTopTracksLimitIsClamped(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		want  string
	}{
		{"within bounds", 10, "10"},
		{"exactly the max", 50, "50"},
		{"over the max is clamped", 500, "50"},
		{"zero gets a default", 0, "50"},
		{"negative gets a default", -3, "50"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotLimit string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotLimit = r.URL.Query().Get("limit")
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"items":[],"next":null}`)
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			if _, err := c.TopTracks(context.Background(), "user-token", TopShortTerm, tc.limit); err != nil {
				t.Fatalf("TopTracks: %v", err)
			}
			if gotLimit != tc.want {
				t.Errorf("limit = %q, want %q", gotLimit, tc.want)
			}
		})
	}
}

func TestTopArtistsEmptyItemsIsEmptyNotNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[],"next":null}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	artists, err := c.TopArtists(context.Background(), "user-token", TopShortTerm, 50)
	if err != nil {
		t.Fatalf("TopArtists: %v", err)
	}
	if artists == nil {
		t.Fatal("TopArtists returned nil for an empty ranking, want a non-nil empty slice")
	}
	if len(artists) != 0 {
		t.Fatalf("got %d artists, want 0", len(artists))
	}
}

func TestTopTracksEmptyItemsIsEmptyNotNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[],"next":null}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	tracks, err := c.TopTracks(context.Background(), "user-token", TopShortTerm, 50)
	if err != nil {
		t.Fatalf("TopTracks: %v", err)
	}
	if tracks == nil {
		t.Fatal("TopTracks returned nil for an empty ranking, want a non-nil empty slice")
	}
	if len(tracks) != 0 {
		t.Fatalf("got %d tracks, want 0", len(tracks))
	}
}

// TestTopArtistsForbiddenIsNotRetried mirrors the equivalent library.go tests:
// a 403 must surface unchanged as an *APIError with IsForbidden true, and must
// not be retried.
func TestTopArtistsForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"status":403,"message":"Insufficient client scope"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	artists, err := c.TopArtists(context.Background(), "user-token", TopShortTerm, 50)
	if artists != nil {
		t.Fatalf("TopArtists returned artists alongside an error: %+v", artists)
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

func TestTopTracksForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"status":403,"message":"Insufficient client scope"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	tracks, err := c.TopTracks(context.Background(), "user-token", TopShortTerm, 50)
	if tracks != nil {
		t.Fatalf("TopTracks returned tracks alongside an error: %+v", tracks)
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

// TestTopArtistsMakesOnlyOneRequestEvenWithNext pins the pagination decision:
// the endpoint is offset-paginated and this response carries a non-empty next
// URL, but TopArtists must not follow it. A future reader "fixing" this into a
// paginating loop would make this test fail with a second request.
func TestTopArtistsMakesOnlyOneRequestEvenWithNext(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[{"id":"artist000000000000001","name":"Band","type":"artist"}],"next":"https://api.spotify.com/v1/me/top/artists?time_range=short_term&offset=50&limit=50"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	artists, err := c.TopArtists(context.Background(), "user-token", TopShortTerm, 50)
	if err != nil {
		t.Fatalf("TopArtists: %v", err)
	}
	if len(artists) != 1 {
		t.Fatalf("got %d artists, want 1", len(artists))
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server calls = %d, want 1: TopArtists must not follow next", got)
	}
}

// TestTopTracksMakesOnlyOneRequestEvenWithNext mirrors
// TestTopArtistsMakesOnlyOneRequestEvenWithNext for the tracks endpoint.
func TestTopTracksMakesOnlyOneRequestEvenWithNext(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[{"id":"track00000000000000001","name":"Song","type":"track"}],"next":"https://api.spotify.com/v1/me/top/tracks?time_range=short_term&offset=50&limit=50"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	tracks, err := c.TopTracks(context.Background(), "user-token", TopShortTerm, 50)
	if err != nil {
		t.Fatalf("TopTracks: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(tracks))
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server calls = %d, want 1: TopTracks must not follow next", got)
	}
}
