package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
)

// stubMirror is a minimal implementation of the published contract, used to
// prove the contract is implementable from the documentation alone.
type stubMirror struct {
	server   *httptest.Server
	requests []string
	auth     []string
	status   int
	body     string
}

func newStubMirror(t *testing.T) *stubMirror {
	t.Helper()
	s := &stubMirror{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests = append(s.requests, r.URL.Path+"?"+r.URL.RawQuery)
		s.auth = append(s.auth, r.Header.Get("Authorization"))

		if s.status != 0 {
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte(s.body))
			return
		}

		key := strings.TrimPrefix(r.URL.Path, "/v1/")
		requested := strings.Split(r.URL.Query().Get("ids"), ",")
		items := make([]any, 0, len(requested))
		for _, id := range requested {
			// "unknown" ids come back as null, exactly as Spotify does.
			if strings.HasPrefix(id, "unknown") {
				items = append(items, nil)
				continue
			}
			items = append(items, map[string]any{"id": id, "name": "Name " + id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{key: items})
	}))
	t.Cleanup(s.server.Close)
	return s
}

func mirrorFor(t *testing.T, s *stubMirror, over func(*config.MetadataFallback)) *Mirror {
	t.Helper()
	cfg := config.MetadataFallback{URL: s.server.URL, Timeout: 5 * time.Second, BatchSize: 50}
	if over != nil {
		over(&cfg)
	}
	m, err := NewMirror(cfg, WithHTTPClient(s.server.Client()))
	if err != nil {
		t.Fatalf("NewMirror: %v", err)
	}
	return m
}

func TestMirrorReadsTheThreeEndpoints(t *testing.T) {
	stub := newStubMirror(t)
	m := mirrorFor(t, stub, nil)
	ctx := context.Background()

	tracks, err := m.GetTracks(ctx, []string{"trackaaaaa", "unknownaaa"})
	if err != nil {
		t.Fatalf("GetTracks: %v", err)
	}
	// A null entry is absent from the result, not an error and not a blank row.
	if len(tracks) != 1 || tracks[0].ID != "trackaaaaa" {
		t.Fatalf("GetTracks returned %+v, want just the known track", tracks)
	}

	artists, err := m.GetArtists(ctx, []string{"artistaaaa"})
	if err != nil || len(artists) != 1 || artists[0].Name != "Name artistaaaa" {
		t.Fatalf("GetArtists returned %+v, %v", artists, err)
	}

	albums, err := m.GetAlbums(ctx, []string{"albumaaaaa"})
	if err != nil || len(albums) != 1 || albums[0].ID != "albumaaaaa" {
		t.Fatalf("GetAlbums returned %+v, %v", albums, err)
	}

	want := []string{
		"/v1/tracks?ids=trackaaaaa%2Cunknownaaa",
		"/v1/artists?ids=artistaaaa",
		"/v1/albums?ids=albumaaaaa",
	}
	for i, w := range want {
		if stub.requests[i] != w {
			t.Fatalf("request %d was %q, want %q", i, stub.requests[i], w)
		}
	}
}

func TestMirrorSendsTheBearerTokenOnlyWhenConfigured(t *testing.T) {
	stub := newStubMirror(t)
	plain := mirrorFor(t, stub, nil)
	if _, err := plain.GetTracks(context.Background(), []string{"trackaaaaa"}); err != nil {
		t.Fatalf("GetTracks: %v", err)
	}
	if stub.auth[0] != "" {
		t.Fatalf("an unauthenticated mirror was sent %q", stub.auth[0])
	}

	authed := mirrorFor(t, stub, func(c *config.MetadataFallback) { c.Token = "s3cret" })
	if _, err := authed.GetTracks(context.Background(), []string{"trackaaaaa"}); err != nil {
		t.Fatalf("GetTracks: %v", err)
	}
	if stub.auth[1] != "Bearer s3cret" {
		t.Fatalf("authorization header was %q, want the bearer token", stub.auth[1])
	}
}

// TestMirrorChunksLargeRequests: the batch limit is part of the contract, so an
// implementer never has to handle an unbounded ids list.
func TestMirrorChunksLargeRequests(t *testing.T) {
	stub := newStubMirror(t)
	m := mirrorFor(t, stub, func(c *config.MetadataFallback) { c.BatchSize = 2 })

	var want []string
	for i := range 5 {
		want = append(want, fmt.Sprintf("track%05d", i))
	}
	got, err := m.GetTracks(context.Background(), want)
	if err != nil {
		t.Fatalf("GetTracks: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d tracks, want 5", len(got))
	}
	if len(stub.requests) != 3 {
		t.Fatalf("five ids at a batch of two took %d requests, want 3", len(stub.requests))
	}
}

// TestMirrorRejectsCredentialsWithoutRetrying: a 401 is a configuration error.
// Retrying it only lengthens the log, and the message has to name the setting.
func TestMirrorRejectsCredentialsWithoutRetrying(t *testing.T) {
	stub := newStubMirror(t)
	stub.status = http.StatusUnauthorized
	stub.body = `{"error":"nope"}`
	m := mirrorFor(t, stub, func(c *config.MetadataFallback) { c.Token = "wrong" })

	_, err := m.GetTracks(context.Background(), []string{"trackaaaaa"})
	if err == nil {
		t.Fatal("a 401 produced no error")
	}
	if !strings.Contains(err.Error(), "ENCORE_METADATA_FALLBACK_TOKEN") {
		t.Fatalf("the error does not say which setting to check: %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("the error leaks the token: %v", err)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("a rejected credential was retried %d times", len(stub.requests))
	}
}

// TestMirrorRejectsAnUnrecognisedEnvelope: a response Encore cannot read must
// name the document that specifies what it should have been, because the person
// reading this error is the one writing the source.
func TestMirrorRejectsAnUnrecognisedEnvelope(t *testing.T) {
	stub := newStubMirror(t)
	stub.status = http.StatusOK
	stub.body = `{"items":[{"id":"trackaaaaa"}]}`
	m := mirrorFor(t, stub, nil)

	_, err := m.GetTracks(context.Background(), []string{"trackaaaaa"})
	if err == nil {
		t.Fatal("a response with the wrong envelope key was accepted")
	}
	if !strings.Contains(err.Error(), "docs/metadata-fallback.md") {
		t.Fatalf("the error does not point at the contract: %v", err)
	}
}

func TestNewMirrorRejectsUnusableURLs(t *testing.T) {
	for _, c := range []struct{ name, url string }{
		{"empty", ""},
		{"relative", "/v1"},
		{"no host", "https://"},
		{"wrong scheme", "ftp://example.test"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewMirror(config.MetadataFallback{URL: c.url}); err == nil {
				t.Fatalf("NewMirror(%q) succeeded; a fallback that silently does nothing "+
					"looks exactly like the problem it was configured to solve", c.url)
			}
		})
	}
}

// TestMirrorFiltersMalformedIDs: one corrupt id must not spoil a batch, whichever
// source is answering.
func TestMirrorFiltersMalformedIDs(t *testing.T) {
	stub := newStubMirror(t)
	m := mirrorFor(t, stub, nil)

	if _, err := m.GetTracks(context.Background(),
		[]string{"trackaaaaa", "no", "  ", "trackaaaaa"}); err != nil {
		t.Fatalf("GetTracks: %v", err)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("made %d requests, want 1", len(stub.requests))
	}
	// Short and blank ids dropped, the duplicate collapsed.
	if stub.requests[0] != "/v1/tracks?ids=trackaaaaa" {
		t.Fatalf("request was %q, want only the one valid id once", stub.requests[0])
	}
}
