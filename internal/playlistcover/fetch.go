package playlistcover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // album art is JPEG
	_ "image/png"  // and occasionally PNG
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/RequiDev/encore/internal/logging"
)

const (
	// artTimeout bounds the whole set of tile fetches rather than each one.
	// Four sequential five-second waits inside an HTTP handler somebody is
	// watching is most of a browser's patience spent on decoration; the fetches
	// run concurrently and share this budget.
	artTimeout = 6 * time.Second
	// maxArtBytes caps one downloaded image. Spotify's largest cover is well
	// under this.
	maxArtBytes = 2 << 20
	// maxArtPixels caps a decoded image's total pixel count (width * height),
	// checked from the header before any pixels are allocated. A per-edge cap
	// alone is not enough: capping only the edge at, say, 4000 still admits a
	// 4000x4000 image, and at 8 bytes/pixel (image.NRGBA64, the widest
	// standard color model image/png can hand back) that is 128 MB decoded
	// from a JPEG or PNG that can be under 200 KB on the wire — the
	// decompression bomb this cap exists to stop. Bounding the product instead
	// bounds worst-case memory directly: at 4,000,000 pixels and 8 bytes/pixel
	// that is a 32 MB ceiling per tile, four tiles is 128 MB, and the only
	// consumer downscales into a 320x320 mosaic cell against covers Spotify
	// itself caps at 640x640 — 4,000,000 is already far past anything
	// legitimate.
	maxArtPixels = 4_000_000
	// maxArtRedirects bounds a CDN's redirect chain. Every hop is re-checked
	// against the allowlist.
	maxArtRedirects = 3
)

var (
	errHostNotAllowed   = errors.New("host is not a Spotify CDN")
	errTooManyRedirects = errors.New("too many redirects")
)

// allowedArtHost reports whether a stored image URL may be fetched.
//
// The URL comes out of albums.image_url, a database column, and a stored URL is
// a stored URL whatever wrote it. This is a plain SSRF guard on a server-side
// fetch of an address Encore did not choose: without it, anything that can ever
// put a row in `albums` — a metadata source an operator runs, a future import
// path, a bug — could make the API container issue a GET to a cloud metadata
// endpoint or an internal service, and because the response is decoded as an
// image it could do it blind.
//
// Suffix matching on Spotify's two CDN domains, https only, no explicit port
// and no userinfo. The leading dot is load-bearing: "evilscdn.co" must not pass
// a check that "i.scdn.co" does. Both apexes are accepted on the same terms as
// their subdomains -- both are Spotify-owned names, so allowing the bare apex
// widens nothing an attacker could steer.
//
// A host that starts with "." (an empty label directly in front of the
// suffix, e.g. ".scdn.co") is rejected explicitly: strings.HasSuffix treats a
// string as its own suffix, so without this check a hostname that is exactly
// ".scdn.co" would match by being equal to the suffix rather than by having a
// real subdomain in front of it.
func allowedArtHost(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasPrefix(host, ".") {
		return false
	}
	return host == "scdn.co" || strings.HasSuffix(host, ".scdn.co") ||
		host == "spotifycdn.com" || strings.HasSuffix(host, ".spotifycdn.com")
}

// Fetcher reads album artwork from Spotify's CDN.
//
// It has its own http.Client and does not go through internal/spotify. The CDN
// is not the Web API: it spends no quota, needs no token, and must not be able
// to pause the rate limiter that enrichment depends on.
type Fetcher struct {
	http *http.Client
	// allow is the host predicate. A field rather than a direct call so a test
	// can point the fetcher at an httptest server and still exercise the
	// redirect check, which is the part that cannot be tested any other way.
	// Production never replaces it.
	allow func(*url.URL) bool
}

// NewFetcher builds a fetcher with the allowlist and every limit in place.
func NewFetcher() *Fetcher {
	f := &Fetcher{allow: allowedArtHost}
	f.http = &http.Client{
		Timeout: artTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxArtRedirects {
				return errTooManyRedirects
			}
			// Re-checked on every hop. net/http follows redirects by default,
			// so a check performed only on the original URL is defeated by a
			// CDN response that 302s somewhere else — which is exactly the
			// shape of the attack the allowlist exists to stop.
			if !f.allow(req.URL) {
				return fmt.Errorf("%w: %s", errHostNotAllowed, req.URL.Hostname())
			}
			return nil
		},
	}
	return f
}

// Fetch reads up to Tiles album covers concurrently and returns them in the
// order given, with nil in the place of any that could not be read.
//
// A tile that fails is a tile that is missing, never an error: the cover is
// best-effort and one slow CDN must not cost a listener their playlist. An
// empty URL is a slot the caller had nothing for and is skipped silently.
//
// Concurrent rather than sequential because this runs inside an HTTP request
// somebody is waiting on: four sequential timeouts is twenty-four seconds, and
// four concurrent ones is six.
func (f *Fetcher) Fetch(ctx context.Context, urls [Tiles]string) [Tiles]image.Image {
	ctx, cancel := context.WithTimeout(ctx, artTimeout)
	defer cancel()

	var (
		out [Tiles]image.Image
		wg  sync.WaitGroup
	)
	for i, raw := range urls {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A decoder panicking on a hostile image must cost this tile, not
			// the process: this goroutine has no caller to recover on its
			// behalf, and the default is to take the whole server down. Logged
			// rather than swallowed bare — an operator seeing a mosaic with a
			// tile silently missing has nothing else to tell them why.
			defer func() {
				if p := recover(); p != nil {
					logging.FromContext(ctx).Error("recovered from a panic decoding playlist cover artwork",
						"panic", fmt.Sprint(p))
				}
			}()
			// Written to its own index, never appended, so a neighbour's
			// failure cannot shift the mosaic and put a different picture on
			// the same playlist at the next rebuild.
			if img, err := f.fetchOne(ctx, raw); err == nil {
				out[i] = img
			}
		}()
	}
	wg.Wait()
	return out
}

// fetchOne reads and decodes one image, or explains why it did not.
func (f *Fetcher) fetchOne(ctx context.Context, raw string) (image.Image, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse artwork url: %w", err)
	}
	// Before the request, not after it. A guard that inspects the response has
	// already made the call, and an internal service that acts on a GET has
	// already acted.
	if !f.allow(u) {
		return nil, fmt.Errorf("%w: %s", errHostNotAllowed, u.Hostname())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build artwork request: %w", err)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch artwork: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch artwork: status %d", resp.StatusCode)
	}

	// One byte past the cap, so an oversized body is caught by its length
	// rather than by whether a truncated JPEG happens to fail to decode.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArtBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read artwork: %w", err)
	}
	if len(body) > maxArtBytes {
		return nil, fmt.Errorf("artwork is larger than %d bytes", maxArtBytes)
	}

	// The header alone, before any pixels are allocated. Content-Type is not
	// consulted at all: a server can claim anything, and decoding is the only
	// check that means what it says.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("artwork is not an image: %w", err)
	}
	if cfg.Width*cfg.Height > maxArtPixels {
		return nil, fmt.Errorf("artwork is %dx%d (%d px), over the %d px cap",
			cfg.Width, cfg.Height, cfg.Width*cfg.Height, maxArtPixels)
	}

	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("decode artwork: %w", err)
	}
	return img, nil
}
