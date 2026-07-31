package playlistcover

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// tinyJPEG is a valid 8x8 image, small enough to inline anywhere.
func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range 64 {
		img.Set(i%8, i/8, color.RGBA{uint8(i * 4), 20, 60, 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// oversizedJPEG is a real, fully decodable 1500x1500 JPEG that encodes to
// roughly 2.5 MB: over maxArtBytes, but under maxArtPixels on each edge, so
// nothing about it should be rejected except its size.
//
// The pixels are random rather than uniform: a solid-colour image compresses
// to a few hundred bytes regardless of its dimensions, which would not exceed
// the cap and would defeat the point of this fixture. Noise is close to
// incompressible, so the encoded size actually tracks the pixel count.
func oversizedJPEG(t *testing.T) []byte {
	t.Helper()
	const dim = 1500
	img := image.NewRGBA(image.Rect(0, 0, dim, dim))
	r := rand.New(rand.NewSource(1))
	if _, err := r.Read(img.Pix); err != nil {
		t.Fatalf("fill noise: %v", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if buf.Len() <= maxArtBytes {
		t.Fatalf("fixture is %d bytes, want more than maxArtBytes (%d); it must actually exceed the cap", buf.Len(), maxArtBytes)
	}
	return buf.Bytes()
}

// oversizedPixelJPEG is a real, fully decodable JPEG one pixel wider than
// maxArtPixels, encoded small: a single-colour 4001x1 strip is a couple of
// kilobytes, nowhere near maxArtBytes. Only the pixel cap stands between this
// fixture and a decoded tile.
func oversizedPixelJPEG(t *testing.T) []byte {
	t.Helper()
	const width = maxArtPixels + 1
	img := image.NewRGBA(image.Rect(0, 0, width, 1))
	for x := 0; x < width; x++ {
		img.Set(x, 0, color.RGBA{10, 20, 30, 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if buf.Len() >= maxArtBytes {
		t.Fatalf("fixture is %d bytes, want well under maxArtBytes (%d); the byte cap must not be what rejects it", buf.Len(), maxArtBytes)
	}
	return buf.Bytes()
}

// TestAllowedArtHostAcceptsOnlySpotifysCDNs is the SSRF guard, table-driven.
//
// Fails when: the leading dot is dropped from either suffix (evilscdn.co and
// notspotifycdn.com then pass); the https requirement is dropped; the port or
// userinfo checks are dropped; or the check stops lowercasing the host.
func TestAllowedArtHostAcceptsOnlySpotifysCDNs(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"https://i.scdn.co/image/ab67616d0000b273", true},
		{"https://mosaic.scdn.co/640/abc", true},
		{"https://scdn.co/image/abc", true},
		{"https://image-cdn-ak.spotifycdn.com/image/abc", true},
		{"https://I.SCDN.CO/image/abc", true},

		{"http://i.scdn.co/image/abc", false},               // not https
		{"https://i.scdn.co:8080/image/abc", false},         // explicit port
		{"https://user:pw@i.scdn.co/image/abc", false},      // userinfo
		{"https://evilscdn.co/image/abc", false},            // no leading dot
		{"https://notspotifycdn.com/image/abc", false},      // no leading dot
		{"https://i.scdn.co.evil.example/abc", false},       // suffix is not a suffix
		{"https://169.254.169.254/latest/meta-data", false}, // the address this guard exists for
		{"https://localhost/image/abc", false},
		{"https://10.0.0.5/image/abc", false},
	}

	for _, tc := range tests {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.raw, err)
		}
		if got := allowedArtHost(u); got != tc.want {
			t.Errorf("allowedArtHost(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestFetchNeverRequestsADisallowedHost pins that the guard runs before the
// request, not after it.
//
// A guard that rejects the *response* has already made the request, which is
// the entire attack: an internal service that acts on a GET has acted by then.
//
// Fails when: allowedArtHost is called after f.http.Do, or not at all — the
// counter below then reads 1.
func TestFetchNeverRequestsADisallowedHost(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	f := NewFetcher()
	got := f.Fetch(context.Background(), [Tiles]string{internal.URL + "/image/abc", "", "", ""})

	if hits.Load() != 0 {
		t.Fatalf("%d requests reached a disallowed host, want 0", hits.Load())
	}
	for i, img := range got {
		if img != nil {
			t.Errorf("tile %d is non-nil for a refused fetch", i)
		}
	}
}

// TestFetchRefusesARedirectOffTheAllowlist is the hole a host check alone
// leaves open.
//
// net/http follows redirects by default, so a CDN URL that 302s to an internal
// address defeats a check performed only on the original URL. CheckRedirect
// must re-apply the allowlist on every hop.
//
// Fails when: CheckRedirect is removed from the client, or stops calling
// allowedArtHost — the redirect target below then records a hit.
func TestFetchRefusesARedirectOffTheAllowlist(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internalHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	// The fetcher is pointed at a stub standing in for the CDN by overriding the
	// allowlist for this test only, so the redirect hop is the thing under test
	// rather than the first hop.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/image/abc", http.StatusFound)
	}))
	defer cdn.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, cdn.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{cdn.URL + "/image/abc", "", "", ""})

	if internalHits.Load() != 0 {
		t.Fatalf("%d requests followed a redirect off the allowlist, want 0", internalHits.Load())
	}
	if got[0] != nil {
		t.Error("a refused redirect produced a tile")
	}
}

// TestFetchDropsAnOversizedBody pins the byte cap by length rather than by
// whether an oversized body happens to fail decoding for an unrelated reason.
//
// The fixture is a real, valid, fully decodable JPEG rather than filler
// bytes: filler that never parses as an image would make this test pass for
// the wrong reason and could never catch the byte cap being removed, since
// image.DecodeConfig would already refuse it as "not an image" regardless of
// size. Using a genuine oversized photo means the only thing standing between
// this body and a decoded tile is the cap itself.
//
// Fails when: the LimitReader or the explicit length check is removed — the
// ~2.5 MB body below then decodes cleanly and the tile is kept.
func TestFetchDropsAnOversizedBody(t *testing.T) {
	huge := oversizedJPEG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(huge)
	}))
	defer srv.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, srv.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{srv.URL + "/image/abc", "", "", ""})
	if got[0] != nil {
		t.Error("an oversized body produced a tile")
	}
}

// TestFetchDropsAnOversizedImage pins the pixel cap: a decompression bomb
// need not be a large download. The fixture here is a couple of kilobytes on
// the wire, so a byte cap alone would wave it through; only checking
// image.DecodeConfig's reported dimensions before the full decode catches it.
//
// Fails when: the width/height check against maxArtPixels is removed, or
// image.Decode runs before that check instead of after it — the tile is then
// kept despite exceeding the cap.
func TestFetchDropsAnOversizedImage(t *testing.T) {
	body := oversizedPixelJPEG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, srv.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{srv.URL + "/image/abc", "", "", ""})
	if got[0] != nil {
		t.Error("an image over the pixel cap produced a tile")
	}
}

// TestFetchDropsANonImage pins that the check is a decode rather than a
// content-type header, which any server can lie about.
//
// Fails when: the decode check is replaced by a Content-Type comparison — the
// stub below sets image/jpeg and serves HTML.
func TestFetchDropsANonImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("<!doctype html><html><body>not an image</body></html>"))
	}))
	defer srv.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, srv.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{srv.URL + "/x", "", "", ""})
	if got[0] != nil {
		t.Error("an HTML body decoded as an image")
	}
}

// TestFetchKeepsPositionsWhenOneTileFails pins that a failed fetch leaves a
// hole rather than shifting the others.
//
// A mosaic whose cells silently reorder when a CDN is slow would put a
// different picture on the same playlist on each rebuild.
//
// Fails when: Fetch appends successes to a slice instead of writing to the
// index it was given — tile 0 would then hold what tile 1 fetched.
func TestFetchKeepsPositionsWhenOneTileFails(t *testing.T) {
	body := tinyJPEG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, srv.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{
		srv.URL + "/bad", srv.URL + "/a", srv.URL + "/b", srv.URL + "/c",
	})
	if got[0] != nil {
		t.Error("tile 0 is non-nil for a 404")
	}
	for i := 1; i < Tiles; i++ {
		if got[i] == nil {
			t.Errorf("tile %d is nil; a neighbour's failure shifted the results", i)
		}
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}
