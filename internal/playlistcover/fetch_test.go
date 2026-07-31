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

// oversizedPixelJPEG is a real, fully decodable 2100x2000 JPEG: 4,200,000
// pixels, over maxArtPixels, encoded small because the image is a single
// colour. Solid colour compresses to a few tens of kilobytes regardless of
// pixel count, nowhere near maxArtBytes, so only the pixel cap -- not the byte
// cap -- can be what rejects this fixture. maxArtPixels bounds the *product*
// of width and height rather than either edge alone: a wide, cheap-to-encode
// image is exactly the shape a per-edge-only cap would have missed.
func oversizedPixelJPEG(t *testing.T) []byte {
	t.Helper()
	const width, height = 2100, 2000
	if width*height <= maxArtPixels {
		t.Fatalf("fixture is %d pixels, want more than maxArtPixels (%d)", width*height, maxArtPixels)
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{10, 20, 30, 255})
		}
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
// userinfo checks are dropped; the check stops lowercasing the host; the bare
// spotifycdn.com apex stops being accepted (an inconsistency with the bare
// scdn.co apex, which must stay accepted); or the empty-label guard is
// removed and a host that is just ".scdn.co" starts passing by being equal to
// the suffix rather than by having a real label in front of it.
func TestAllowedArtHostAcceptsOnlySpotifysCDNs(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"https://i.scdn.co/image/ab67616d0000b273", true},
		{"https://mosaic.scdn.co/640/abc", true},
		{"https://scdn.co/image/abc", true},
		{"https://spotifycdn.com/image/abc", true}, // the other apex, same standing as scdn.co
		{"https://image-cdn-ak.spotifycdn.com/image/abc", true},
		{"https://I.SCDN.CO/image/abc", true},

		{"http://i.scdn.co/image/abc", false},               // not https
		{"https://i.scdn.co:8080/image/abc", false},         // explicit port
		{"https://user:pw@i.scdn.co/image/abc", false},      // userinfo
		{"https://evilscdn.co/image/abc", false},            // no leading dot
		{"https://notspotifycdn.com/image/abc", false},      // no leading dot
		{"https://i.scdn.co.evil.example/abc", false},       // suffix is not a suffix
		{"https://.scdn.co/image/abc", false},               // empty label, not a subdomain
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
// need not be a large download. The fixture here is tens of kilobytes on the
// wire, so a byte cap alone would wave it through; only checking
// image.DecodeConfig's reported dimensions before the full decode catches it.
//
// This test cannot also pin that DecodeConfig runs strictly *before*
// image.Decode: Fetch's return type carries no error, only a nil-or-not tile,
// so from outside the package a version that decodes first and then rejects
// on the same width*height check is indistinguishable from one that never
// decodes at all -- both leave got[0] nil. Moving image.Decode above the
// pixel check changes what gets allocated, not what gets returned, so it does
// not make this test fail. What this test does pin -- and what does make it
// fail -- is that the check exists and rejects, at whatever point it runs.
//
// Fails when: the width*height check against maxArtPixels is removed.
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

// TestFetchDropsANonOKStatus pins that a non-200 status is rejected on its own
// terms, not incidentally because whatever body came with it failed to
// decode. The body here is a real, valid, otherwise-acceptable JPEG served
// alongside a 404: if the status check were deleted, this body would decode
// clean and produce a tile, which is the only thing that distinguishes this
// case from serving an empty 404 body (which fails to decode either way and
// would pass this test for the wrong reason).
//
// Fails when: the resp.StatusCode != http.StatusOK check is removed — the
// valid JPEG below then decodes and the tile is kept despite the 404.
func TestFetchDropsANonOKStatus(t *testing.T) {
	body := tinyJPEG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, srv.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{srv.URL + "/x", "", "", ""})
	if got[0] != nil {
		t.Error("a valid body served with a 404 status produced a tile")
	}
}

// TestFetchBoundsARedirectLoopBetweenAllowedHosts pins maxArtRedirects as a
// backstop that is load-bearing on its own, independent of the host
// allowlist.
//
// Setting a custom CheckRedirect on an http.Client replaces net/http's
// default redirect limit outright rather than adding to it, so a client whose
// CheckRedirect only re-checks the allowlist has no cap underneath it at all.
// Two servers that are both on the allowlist and redirect to each other are
// indistinguishable from a legitimate multi-hop CDN to the host check alone —
// only a hop-count limit ends the loop, and without one this runs until the
// context deadline, sending tens of thousands of requests at two hosts that
// did nothing wrong.
//
// Fails when: the len(via) >= maxArtRedirects check is removed from
// CheckRedirect — hits then climb into the tens of thousands within the
// timeout instead of stopping at exactly maxArtRedirects.
func TestFetchBoundsARedirectLoopBetweenAllowedHosts(t *testing.T) {
	var hits atomic.Int32
	var a, b *httptest.Server
	a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, b.URL+"/x", http.StatusFound)
	}))
	defer a.Close()
	b = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, a.URL+"/x", http.StatusFound)
	}))
	defer b.Close()

	hostA, hostB := mustHost(t, a.URL), mustHost(t, b.URL)
	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == hostA || u.Host == hostB }

	got := f.Fetch(context.Background(), [Tiles]string{a.URL + "/x", "", "", ""})

	if got[0] != nil {
		t.Error("a redirect loop between two allowed hosts produced a tile")
	}
	if n := hits.Load(); n != maxArtRedirects {
		t.Errorf("%d requests were made chasing a redirect loop between two allowed hosts, want exactly maxArtRedirects (%d)", n, maxArtRedirects)
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
