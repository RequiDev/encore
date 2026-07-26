package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recentlyPlayedScript describes what one page of the feed returns.
type recentlyPlayedScript struct {
	items int
	after string
}

func TestRecentlyPlayedPagination(t *testing.T) {
	watermark := time.Date(2026, time.July, 20, 9, 30, 0, 0, time.UTC)
	watermarkMillis := fmt.Sprintf("%d", watermark.UnixMilli())

	tests := []struct {
		name       string
		after      time.Time
		limit      int
		maxPages   int
		script     []recentlyPlayedScript
		wantItems  int
		wantAfters []string
		wantLimit  string
	}{
		{
			name:       "single page",
			after:      watermark,
			limit:      50,
			maxPages:   5,
			script:     []recentlyPlayedScript{{items: 2, after: ""}},
			wantItems:  2,
			wantAfters: []string{watermarkMillis},
			wantLimit:  "50",
		},
		{
			name:       "follows the forward cursor",
			after:      watermark,
			limit:      50,
			maxPages:   5,
			script:     []recentlyPlayedScript{{items: 3, after: "cursor-1"}, {items: 2, after: ""}},
			wantItems:  5,
			wantAfters: []string{watermarkMillis, "cursor-1"},
			wantLimit:  "50",
		},
		{
			name:       "stops at the page budget",
			after:      watermark,
			limit:      50,
			maxPages:   2,
			script:     []recentlyPlayedScript{{items: 50, after: "cursor-1"}, {items: 50, after: "cursor-2"}, {items: 50, after: "cursor-3"}},
			wantItems:  100,
			wantAfters: []string{watermarkMillis, "cursor-1"},
			wantLimit:  "50",
		},
		{
			name:       "stops on an empty page",
			after:      watermark,
			limit:      50,
			maxPages:   5,
			script:     []recentlyPlayedScript{{items: 0, after: "cursor-1"}},
			wantItems:  0,
			wantAfters: []string{watermarkMillis},
			wantLimit:  "50",
		},
		{
			name:       "stops when the cursor stops moving",
			after:      watermark,
			limit:      50,
			maxPages:   5,
			script:     []recentlyPlayedScript{{items: 1, after: "cursor-1"}, {items: 1, after: "cursor-1"}},
			wantItems:  2,
			wantAfters: []string{watermarkMillis, "cursor-1"},
			wantLimit:  "50",
		},
		{
			name:       "no watermark omits the cursor and clamps the limit",
			after:      time.Time{},
			limit:      500,
			maxPages:   1,
			script:     []recentlyPlayedScript{{items: 1, after: "cursor-1"}},
			wantItems:  1,
			wantAfters: []string{""},
			wantLimit:  "50",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				afters []string
				limits []string
				page   int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				afters = append(afters, r.URL.Query().Get("after"))
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
				_, _ = w.Write(recentlyPlayedBody(current, tc.script[current]))
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			items, err := c.RecentlyPlayed(context.Background(), "user-token", tc.after, tc.limit, tc.maxPages)
			if err != nil {
				t.Fatalf("RecentlyPlayed: %v", err)
			}
			if len(items) != tc.wantItems {
				t.Fatalf("got %d items, want %d", len(items), tc.wantItems)
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
			for _, l := range limits {
				if l != tc.wantLimit {
					t.Fatalf("limit = %q, want %q", l, tc.wantLimit)
				}
			}
			for _, it := range items {
				if it.PlayedAt.IsZero() || it.Track.ID == "" {
					t.Fatalf("item decoded incompletely: %+v", it)
				}
			}
		})
	}
}

// recentlyPlayedBody renders one scripted page of the feed.
func recentlyPlayedBody(page int, s recentlyPlayedScript) []byte {
	type wire struct {
		Items   []PlayHistory `json:"items"`
		Cursors struct {
			After  string `json:"after"`
			Before string `json:"before"`
		} `json:"cursors"`
	}
	var body wire
	body.Cursors.After = s.after
	base := time.Date(2026, time.July, 20, 10, 0, 0, 0, time.UTC)
	for i := range s.items {
		body.Items = append(body.Items, PlayHistory{
			Track: Track{
				ID:      fmt.Sprintf("track%02d%02d0000000000000", page, i),
				Name:    "Song",
				Type:    "track",
				Artists: []Artist{{ID: "artist000000000000000", Name: "Band"}},
			},
			PlayedAt: base.Add(time.Duration(page*100+i) * time.Minute),
		})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestCurrentUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me" {
			t.Errorf("path = %q, want /v1/me", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer user-token" {
			t.Errorf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"id":"listener","display_name":"Listener","email":"listener@example.com","product":"premium","images":[{"url":"small.jpg","width":64,"height":64},{"url":"large.jpg","width":640,"height":640}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	p, err := c.CurrentUser(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if p.ID != "listener" || p.Product != "premium" {
		t.Fatalf("profile = %+v", p)
	}
	if got := p.AvatarURL(); got != "large.jpg" {
		t.Errorf("AvatarURL = %q, want the largest image", got)
	}
}

func TestGetTracksChunksAndSkipsMissingEntries(t *testing.T) {
	tests := []struct {
		name       string
		ids        []string
		nullEvery  int
		wantChunks []int
		wantTracks int
	}{
		{
			name:       "one short chunk",
			ids:        catalogIDs("t", 10),
			wantChunks: []int{10},
			wantTracks: 10,
		},
		{
			name:       "exactly one full chunk",
			ids:        catalogIDs("t", 50),
			wantChunks: []int{50},
			wantTracks: 50,
		},
		{
			name:       "chunked at fifty",
			ids:        catalogIDs("t", 120),
			wantChunks: []int{50, 50, 20},
			wantTracks: 120,
		},
		{
			// A null element means Spotify has nothing for that id; it must be
			// absent from the result rather than failing the whole batch.
			name:       "null elements are dropped",
			ids:        catalogIDs("t", 60),
			nullEvery:  3,
			wantChunks: []int{50, 10},
			wantTracks: 39,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				chunks []int
			)
			var tokenCalls atomic.Int32
			mux := http.NewServeMux()
			mux.Handle("/api/token", appTokenHandler(&tokenCalls))
			mux.HandleFunc("/v1/tracks", func(w http.ResponseWriter, r *http.Request) {
				ids := strings.Split(r.URL.Query().Get("ids"), ",")
				mu.Lock()
				chunks = append(chunks, len(ids))
				mu.Unlock()

				var b strings.Builder
				b.WriteString(`{"tracks":[`)
				for i, id := range ids {
					if i > 0 {
						b.WriteByte(',')
					}
					if tc.nullEvery > 0 && i%tc.nullEvery == 0 {
						b.WriteString("null")
						continue
					}
					fmt.Fprintf(&b, `{"id":%q,"name":"Song","type":"track","duration_ms":180000}`, id)
				}
				b.WriteString(`]}`)
				_, _ = w.Write([]byte(b.String()))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			tracks, err := c.GetTracks(context.Background(), tc.ids)
			if err != nil {
				t.Fatalf("GetTracks: %v", err)
			}
			if len(tracks) != tc.wantTracks {
				t.Fatalf("got %d tracks, want %d", len(tracks), tc.wantTracks)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(chunks) != len(tc.wantChunks) {
				t.Fatalf("chunk sizes = %v, want %v", chunks, tc.wantChunks)
			}
			for i := range chunks {
				if chunks[i] != tc.wantChunks[i] {
					t.Fatalf("chunk sizes = %v, want %v", chunks, tc.wantChunks)
				}
			}
			if got := tokenCalls.Load(); got != 1 {
				t.Fatalf("token endpoint calls = %d, want 1 for the whole batch", got)
			}
		})
	}
}

func TestGetAlbumsChunksAtTwenty(t *testing.T) {
	var (
		mu     sync.Mutex
		chunks []int
	)
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/albums", func(w http.ResponseWriter, r *http.Request) {
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		mu.Lock()
		chunks = append(chunks, len(ids))
		mu.Unlock()

		var b strings.Builder
		b.WriteString(`{"albums":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":%q,"name":"Record","album_type":"ALBUM","release_date":"2011-03","release_date_precision":"month","total_tracks":12}`, id)
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	albums, err := c.GetAlbums(context.Background(), catalogIDs("a", 45))
	if err != nil {
		t.Fatalf("GetAlbums: %v", err)
	}
	if len(albums) != 45 {
		t.Fatalf("got %d albums, want 45", len(albums))
	}
	mu.Lock()
	defer mu.Unlock()
	want := []int{20, 20, 5}
	if len(chunks) != len(want) || chunks[0] != want[0] || chunks[1] != want[1] || chunks[2] != want[2] {
		t.Fatalf("chunk sizes = %v, want %v", chunks, want)
	}
}

func TestGetArtistsSkipsUnusableIDs(t *testing.T) {
	var requested []string
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/artists", func(w http.ResponseWriter, r *http.Request) {
		requested = strings.Split(r.URL.Query().Get("ids"), ",")
		var b strings.Builder
		b.WriteString(`{"artists":[`)
		for i, id := range requested {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":%q,"name":"Band","genres":["indie rock"],"popularity":61,"followers":{"total":123456}}`, id)
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	// A malformed id would make Spotify reject the whole batch, so it is filtered
	// out locally; the duplicate is collapsed for the same reason of economy.
	ids := []string{"artist000000000000001", "", "spotify:artist:nope", "artist000000000000001", "artist000000000000002"}
	artists, err := c.GetArtists(context.Background(), ids)
	if err != nil {
		t.Fatalf("GetArtists: %v", err)
	}
	if len(requested) != 2 {
		t.Fatalf("requested ids = %v, want the two usable ones", requested)
	}
	if len(artists) != 2 || artists[0].Followers.Total != 123456 {
		t.Fatalf("artists = %+v", artists)
	}
}

func TestGetTracksWithNoUsableIDsMakesNoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	tracks, err := c.GetTracks(context.Background(), []string{"", "  ", "short"})
	if err != nil || tracks != nil {
		t.Fatalf("GetTracks = (%v, %v), want (nil, nil)", tracks, err)
	}
}

func TestSearchTrack(t *testing.T) {
	tests := []struct {
		name    string
		artist  string
		title   string
		body    string
		wantQ   string
		wantID  string
		wantHit bool
	}{
		{
			name:    "match",
			artist:  "the band",
			title:   "the song",
			body:    `{"tracks":{"items":[{"id":"track0000000000000001","name":"The Song","type":"track"}],"total":1}}`,
			wantQ:   `track:"the song" artist:"the band"`,
			wantID:  "track0000000000000001",
			wantHit: true,
		},
		{
			name:   "no match",
			artist: "nobody",
			title:  "nothing",
			body:   `{"tracks":{"items":[],"total":0}}`,
			wantQ:  `track:"nothing" artist:"nobody"`,
		},
		{
			// Quotes would otherwise break out of the field-qualified term.
			name:    "quotes are neutralised",
			artist:  `sam "the man"`,
			title:   `say "hello"`,
			body:    `{"tracks":{"items":[{"id":"track0000000000000002","name":"Say Hello","type":"track"}],"total":1}}`,
			wantQ:   `track:"say hello" artist:"sam the man"`,
			wantID:  "track0000000000000002",
			wantHit: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotQ, gotType, gotLimit string
			mux := http.NewServeMux()
			mux.Handle("/api/token", appTokenHandler(nil))
			mux.HandleFunc("/v1/search", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				gotQ, gotType, gotLimit = q.Get("q"), q.Get("type"), q.Get("limit")
				_, _ = io.WriteString(w, tc.body)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			track, err := c.SearchTrack(context.Background(), tc.artist, tc.title)
			if err != nil {
				t.Fatalf("SearchTrack: %v", err)
			}
			if gotQ != tc.wantQ {
				t.Errorf("q = %q, want %q", gotQ, tc.wantQ)
			}
			if gotType != "track" || gotLimit != "1" {
				t.Errorf("type/limit = %q/%q, want track/1", gotType, gotLimit)
			}
			if tc.wantHit {
				if track == nil || track.ID != tc.wantID {
					t.Fatalf("track = %+v, want id %q", track, tc.wantID)
				}
			} else if track != nil {
				t.Fatalf("track = %+v, want nil for no match", track)
			}
		})
	}
}

func TestSearchTrackWithoutTitleMakesNoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	track, err := c.SearchTrack(context.Background(), "the band", "   ")
	if err != nil || track != nil {
		t.Fatalf("SearchTrack = (%v, %v), want (nil, nil)", track, err)
	}
}

// catalogIDs builds n ids of a plausible shape.
func catalogIDs(prefix string, n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = fmt.Sprintf("%s%019d", prefix, i)
	}
	return out
}
