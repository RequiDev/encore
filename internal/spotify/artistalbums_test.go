package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// artistAlbumItem describes one release in a scripted page for
// /v1/artists/{id}/albums.
type artistAlbumItem struct {
	id, name, group, releaseDate, releasePrecision string
}

// artistAlbumsBody renders one scripted page of /v1/artists/{id}/albums.
// Items always marshals to "[]" rather than "null" when empty, matching what
// Spotify actually sends and keeping a nil-vs-empty-slice bug in the decoder
// from hiding behind Go's own JSON defaults.
func artistAlbumsBody(items []artistAlbumItem, next bool, offset int) []byte {
	type wireItem struct {
		ID                   string `json:"id"`
		Name                 string `json:"name"`
		AlbumGroup           string `json:"album_group"`
		ReleaseDate          string `json:"release_date"`
		ReleaseDatePrecision string `json:"release_date_precision"`
	}
	type wire struct {
		Items []wireItem `json:"items"`
		Next  string     `json:"next"`
	}
	body := wire{Items: make([]wireItem, 0, len(items))}
	for _, it := range items {
		body.Items = append(body.Items, wireItem{
			ID:                   it.id,
			Name:                 it.name,
			AlbumGroup:           it.group,
			ReleaseDate:          it.releaseDate,
			ReleaseDatePrecision: it.releasePrecision,
		})
	}
	if next {
		body.Next = fmt.Sprintf("https://api.spotify.com/v1/artists/x/albums?offset=%d&limit=50", offset+50)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

// fillerReleases builds n releases, none of which share Spotify's minimum
// length rule for an id fewer than 10 characters, so the truncation test
// below can page through something the size of Spotify's own limit.
func fillerReleases(n int) []artistAlbumItem {
	out := make([]artistAlbumItem, n)
	for i := range n {
		out[i] = artistAlbumItem{
			id: fmt.Sprintf("album%016d", i), name: "R", group: "album",
			releaseDate: "2016", releasePrecision: "year",
		}
	}
	return out
}

// TestArtistAlbumsFollowsEveryPage walks two pages and stops on the one with
// no next, which is what a discography longer than fifty releases needs. It
// also pins that no include_groups parameter is ever sent: every group must
// be fetched, or the page could not say what it excluded from a completion
// count.
func TestArtistAlbumsFollowsEveryPage(t *testing.T) {
	const artistID = "1BBBBBBBBBBBBBBBBBBBBB"
	script := [][]artistAlbumItem{
		{{id: "a1", name: "First", group: "album", releaseDate: "2016-05-20", releasePrecision: "day"}},
		{{id: "a2", name: "A Single", group: "single", releaseDate: "2018", releasePrecision: "year"}},
	}

	var (
		mu      sync.Mutex
		queries []string
		page    int
	)
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/artists/"+artistID+"/albums", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		current := page
		page++
		mu.Unlock()

		if current >= len(script) {
			t.Errorf("unexpected request %d beyond the script", current+1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		next := current < len(script)-1
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(artistAlbumsBody(script[current], next, current*maxLibraryPageSize))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.ArtistAlbums(context.Background(), artistID, 20)
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d releases, want 2: the walk stopped before the last page", len(got))
	}
	if got[1].Group != "single" {
		t.Fatalf("group = %q, want \"single\": album_group did not survive decoding", got[1].Group)
	}
	if got[1].ReleasePrecision != "year" || got[1].ReleaseDate == nil || got[1].ReleaseDate.Year() != 2018 {
		t.Fatalf("release = %+v, want a year-precision 2018", got[1])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 {
		t.Fatalf("made %d requests, want 2", len(queries))
	}
	if !strings.Contains(queries[1], "offset=50") {
		t.Fatalf("second request query = %q, want offset=50", queries[1])
	}
	for i, q := range queries {
		if strings.Contains(q, "include_groups") {
			t.Fatalf("request %d sent include_groups (%q); every group must be fetched, or the "+
				"page cannot say what it excluded from the count", i, q)
		}
	}
}

// TestArtistAlbumsReportsTruncation pins that a spent page budget is an error
// carrying real data, even against a server that would repeat the same page
// forever: it never stops of its own accord, and only the budget ends the
// walk. The caller must treat the result as a failure: ReplaceArtistAlbums is
// delete-absent, so writing this prefix would delete the tail of a
// discography that was correct.
func TestArtistAlbumsReportsTruncation(t *testing.T) {
	const artistID = "1BBBBBBBBBBBBBBBBBBBBB"
	full := fillerReleases(maxLibraryPageSize)

	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/artists/"+artistID+"/albums", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(artistAlbumsBody(full, true, 0))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.ArtistAlbums(context.Background(), artistID, 2)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("error = %v, want one wrapping ErrTruncated", err)
	}
	if len(got) != 2*maxLibraryPageSize {
		t.Fatalf("got %d releases with the partial result, want the %d already read",
			len(got), 2*maxLibraryPageSize)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("made %d requests, want 2 under a two-page budget even though the server "+
			"always claims there is more", n)
	}
}

// TestArtistAlbumsReturnsNilNilOnAnEmptyResponse pins the exact shape the
// service depends on. A 200 with no items is not an error at the transport
// level and must not be: it is the service's job to record it as a failure,
// and it can only do that if this returns no items and no error.
func TestArtistAlbumsReturnsNilNilOnAnEmptyResponse(t *testing.T) {
	const artistID = "1BBBBBBBBBBBBBBBBBBBBB"
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/artists/"+artistID+"/albums", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(artistAlbumsBody(nil, false, 0))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.ArtistAlbums(context.Background(), artistID, 20)
	if err != nil {
		t.Fatalf("error = %v, want nil: an empty page is not a transport error", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil so the caller can distinguish it from a stored listing", got)
	}
}

// TestArtistAlbumsStopsOnAnEmptyPage guards the loop's other exit. A page with
// no items must end the walk even if `next` is present, or a misbehaving
// upstream pages to the budget on every artist it serves.
func TestArtistAlbumsStopsOnAnEmptyPage(t *testing.T) {
	const artistID = "1BBBBBBBBBBBBBBBBBBBBB"
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/artists/"+artistID+"/albums", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(artistAlbumsBody(nil, true, 0))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.ArtistAlbums(context.Background(), artistID, 20)
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d releases from an empty page, want 0", len(got))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("made %d requests, want 1: an empty page ends the walk even when next is present", n)
	}
}

// TestArtistAlbumsSkipsItemsWithNoID keeps a keyless row out of a table whose
// primary key includes it, and pins that an all-keyless page collapses to the
// (nil, nil) shape above rather than to a listing of ghosts.
func TestArtistAlbumsSkipsItemsWithNoID(t *testing.T) {
	const artistID = "1BBBBBBBBBBBBBBBBBBBBB"
	items := []artistAlbumItem{
		{id: "", name: "Ghost", group: "album", releaseDate: "2016", releasePrecision: "year"},
		{id: "a1", name: "Real", group: "album", releaseDate: "2016", releasePrecision: "year"},
	}
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/artists/"+artistID+"/albums", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(artistAlbumsBody(items, false, 0))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.ArtistAlbums(context.Background(), artistID, 20)
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("got %+v, want only the release that has an id", got)
	}
}

// TestArtistAlbumsRefusesANonSpotifyID stops a malformed id reaching the
// request path. Ids Encore minted locally from an export's names
// (domain.LocalArtistID) land here too, and there is no artist on Spotify for
// them to ask about.
func TestArtistAlbumsRefusesANonSpotifyID(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(&calls))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	for _, id := range []string{"", "  ", "local:artist:someone", "not/a/spotify/id"} {
		if _, err := c.ArtistAlbums(context.Background(), id, 20); err == nil {
			t.Fatalf("ArtistAlbums(%q) succeeded; want it refused before the request went out", id)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("made %d requests for malformed ids, want 0", n)
	}
}

// TestArtistAlbumsSurfacesANotFound keeps "Spotify does not have this artist"
// distinguishable from "the request failed", so the caller can log it
// usefully.
func TestArtistAlbumsSurfacesANotFound(t *testing.T) {
	const artistID = "1BBBBBBBBBBBBBBBBBBBBB"
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/artists/"+artistID+"/albums", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"status":404,"message":"non existing id"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	_, err := c.ArtistAlbums(context.Background(), artistID, 20)
	apiErr, ok := AsAPIError(err)
	if !ok || !apiErr.IsNotFound() {
		t.Fatalf("error = %v, want an *APIError with IsNotFound()", err)
	}
}
