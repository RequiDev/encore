package spotify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// albumTrackMux serves an album of n tracks, fifty a page, counting requests.
func albumTrackMux(albumID string, n int, calls *atomic.Int32) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/albums/"+albumID+"/tracks", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

		var b strings.Builder
		b.WriteString(`{"items":[`)
		count := 0
		for i := offset; i < n && i < offset+50; i++ {
			if count > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":"track%04d0000000000000","name":"Song %d","disc_number":1,"track_number":%d}`,
				i, i+1, i+1)
			count++
		}
		b.WriteString(`]`)
		if offset+50 < n {
			fmt.Fprintf(&b, `,"next":"https://api.spotify.com/v1/albums/%s/tracks?offset=%d&limit=50"`,
				albumID, offset+50)
		} else {
			b.WriteString(`,"next":null`)
		}
		b.WriteString(`}`)
		_, _ = w.Write([]byte(b.String()))
	})
	return mux
}

// TestAlbumTracksFollowsEveryPage is the pagination guard. A 120-track album is
// three pages, and stopping after the first is the failure mode that would make
// the missing-track list quietly claim two thirds of a record was never played.
func TestAlbumTracksFollowsEveryPage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(albumTrackMux("4aawyAB9vmqN3uQ7FjRGTy", 120, &calls))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 0)
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 120 {
		t.Fatalf("got %d tracks, want 120", len(got))
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("made %d requests, want 3 (50 + 50 + 20)", n)
	}
	if got[0].Name != "Song 1" || got[119].Name != "Song 120" {
		t.Fatalf("first/last are %q/%q, want the first and last of the album",
			got[0].Name, got[119].Name)
	}
	if got[0].TrackNumber != 1 || got[0].DiscNumber != 1 {
		t.Fatalf("first track is disc %d track %d, want disc 1 track 1",
			got[0].DiscNumber, got[0].TrackNumber)
	}
}

// TestAlbumTracksReportsTruncation pins the property that keeps a partial
// listing away from a delete-absent write.
func TestAlbumTracksReportsTruncation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(albumTrackMux("4aawyAB9vmqN3uQ7FjRGTy", 120, &calls))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 1)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("error = %v, want one wrapping ErrTruncated", err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d tracks with the partial result, want the 50 already read", len(got))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("made %d requests, want 1 under a one-page budget", n)
	}
}

// TestAlbumTracksStopsOnAnEmptyPage guards the loop's other exit. A page with no
// items must end the walk even if `next` is present, or a misbehaving upstream
// pages to the budget on every album it serves.
func TestAlbumTracksStopsOnAnEmptyPage(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/albums/4aawyAB9vmqN3uQ7FjRGTy/tracks", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"items":[],"next":"https://api.spotify.com/v1/albums/x/tracks?offset=50"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 0)
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d tracks from an empty page, want 0", len(got))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("made %d requests, want 1: an empty page ends the walk", n)
	}
}

// TestAlbumTracksSkipsItemsWithNoID keeps a local or unplayable entry out of the
// listing. It has no id to compare against a listen, so it is not something this
// listing can say anything true about.
func TestAlbumTracksSkipsItemsWithNoID(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/albums/4aawyAB9vmqN3uQ7FjRGTy/tracks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[
			{"id":null,"name":"A local file","disc_number":1,"track_number":1},
			{"id":"5aawyAB9vmqN3uQ7FjRGTy","name":"Real","disc_number":1,"track_number":2}
		],"next":null}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 0)
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "5aawyAB9vmqN3uQ7FjRGTy" {
		t.Fatalf("got %+v, want only the entry that has an id", got)
	}
}

// TestAlbumTracksRefusesANonSpotifyID keeps a locally minted id out of a URL
// path. Unlike the batch endpoints it cannot be filtered by cleanIDs, because
// the id is the path rather than a parameter.
func TestAlbumTracksRefusesANonSpotifyID(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(&calls))
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if _, err := c.AlbumTracks(context.Background(), "local:album:x/../../v1/me", 0); err == nil {
		t.Fatal("AlbumTracks accepted an id that is not a Spotify id")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("made %d requests for a malformed id, want 0", n)
	}
}

// TestAlbumTracksSurfacesANotFound keeps "Spotify does not have this album"
// distinguishable from "the request failed", so the caller can log it usefully.
func TestAlbumTracksSurfacesANotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/albums/4aawyAB9vmqN3uQ7FjRGTy/tracks", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"status":404,"message":"non existing id"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	_, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 0)
	apiErr, ok := AsAPIError(err)
	if !ok || !apiErr.IsNotFound() {
		t.Fatalf("error = %v, want an *APIError with IsNotFound()", err)
	}
}
