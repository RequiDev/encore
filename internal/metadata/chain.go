package metadata

import (
	"context"
	"log/slog"
	"time"

	"github.com/RequiDev/encore/internal/spotify"
)

// Batch is the outcome of one catalogue read.
//
// The two fields answer different questions, and conflating them is the mistake
// this type exists to prevent.
//
// Items is what was found, from whichever source found it.
//
// Declined is the subset of the requested ids that the *authoritative* source
// explicitly had nothing for. Only those may be marked permanently unavailable.
// When the authoritative source was never asked — it is rate limited, or it
// failed — Declined is empty, and ids missing from Items are simply still
// pending: they go back on the queue and are asked again later.
//
// Getting this wrong is destructive rather than merely wrong. "Unavailable" is a
// terminal state that the repair pass deliberately does not revisit, so marking
// a batch unavailable because a fallback happened not to know it would blank
// those tracks for the life of the instance.
type Batch[T any] struct {
	Items    []T
	Declined []string
}

// Chain reads from a primary source and, where the primary cannot help, from a
// fallback.
//
// With no fallback configured it is a thin pass-through, so the enrichment
// worker has one code path rather than two.
//
// Which source is asked first is a setting; who is believed is not. Only the
// primary's refusal marks an id unavailable, because that state is terminal and
// a fallback is not authoritative about what exists.
//
// A fallback failure is never fatal — an instance whose mirror is down behaves
// like one that never had it. docs/metadata-fallback.md has the full table.
type Chain struct {
	primary  Source
	fallback Source
	// preferFallback asks the fallback first and keeps Spotify for what it does
	// not have.
	//
	// Worth having because the two sources fail differently. Spotify is
	// authoritative and current but rationed — a development-mode application
	// exhausts its daily quota during one import. A local mirror is complete for
	// everything it was scraped from and answers instantly, but it is a
	// point-in-time copy, so anything released since is not in it.
	//
	// Asking the mirror first means the quota is spent only on what the mirror
	// lacks, which for a full scrape is new releases and little else. The cost is
	// that metadata already in the mirror is as fresh as the scrape, not as fresh
	// as Spotify.
	preferFallback bool

	// pausedUntil reports when the primary resumes, or the zero time when it is
	// not held back. Supplied by the caller because the pause belongs to the
	// Spotify limiter, which this package does not otherwise need to know about.
	pausedUntil func() time.Time
	now         func() time.Time
	lg          *slog.Logger
}

// ChainOption customises a Chain.
type ChainOption func(*Chain)

// WithPauseCheck supplies the primary's pause state.
//
// Without it a Chain assumes the primary is always available, which is correct
// for a source that has no rate limit and merely wasteful for one that does.
func WithPauseCheck(fn func() time.Time) ChainOption {
	return func(c *Chain) {
		if fn != nil {
			c.pausedUntil = fn
		}
	}
}

// WithChainLogger sets the logger.
func WithChainLogger(lg *slog.Logger) ChainOption {
	return func(c *Chain) {
		if lg != nil {
			c.lg = lg
		}
	}
}

// WithPreferredFallback asks the fallback before the primary.
//
// The fallback remains a fallback in the sense that matters: it can only add
// metadata. Anything it does not have still goes to the primary, and only the
// primary's refusal is ever treated as final.
func WithPreferredFallback(prefer bool) ChainOption {
	return func(c *Chain) { c.preferFallback = prefer }
}

// WithClock replaces the clock used to test the primary's pause.
func WithClock(now func() time.Time) ChainOption {
	return func(c *Chain) {
		if now != nil {
			c.now = now
		}
	}
}

// NewChain builds a Chain. A nil fallback is allowed and means "primary only".
func NewChain(primary, fallback Source, opts ...ChainOption) *Chain {
	c := &Chain{
		primary:     primary,
		fallback:    fallback,
		pausedUntil: func() time.Time { return time.Time{} },
		now:         time.Now,
		lg:          slog.Default(),
	}
	for _, o := range opts {
		o(c)
	}
	c.lg = c.lg.With("component", "metadata")
	return c
}

// HasFallback reports whether a second source is configured. The status endpoint
// uses it to explain a rate-limited instance that is nonetheless filling in.
func (c *Chain) HasFallback() bool { return c != nil && c.fallback != nil }

// Tracks reads a batch of tracks.
func (c *Chain) Tracks(ctx context.Context, ids []string) (Batch[spotify.Track], error) {
	return resolve(ctx, c, "tracks", ids,
		func(s Source, ctx context.Context, ids []string) ([]spotify.Track, error) {
			return s.GetTracks(ctx, ids)
		},
		func(t spotify.Track) string { return t.ID })
}

// Artists reads a batch of artists.
func (c *Chain) Artists(ctx context.Context, ids []string) (Batch[spotify.Artist], error) {
	return resolve(ctx, c, "artists", ids,
		func(s Source, ctx context.Context, ids []string) ([]spotify.Artist, error) {
			return s.GetArtists(ctx, ids)
		},
		func(a spotify.Artist) string { return a.ID })
}

// Albums reads a batch of albums.
func (c *Chain) Albums(ctx context.Context, ids []string) (Batch[spotify.Album], error) {
	return resolve(ctx, c, "albums", ids,
		func(s Source, ctx context.Context, ids []string) ([]spotify.Album, error) {
			return s.GetAlbums(ctx, ids)
		},
		func(a spotify.Album) string { return a.ID })
}

// resolve is the policy, written once for the three kinds.
func resolve[T any](
	ctx context.Context,
	c *Chain,
	kind string,
	ids []string,
	get func(Source, context.Context, []string) ([]T, error),
	idOf func(T) string,
) (Batch[T], error) {
	if len(ids) == 0 {
		return Batch[T]{}, nil
	}

	// Without a fallback this is exactly what the enrichment worker used to do
	// inline: fetch, then treat whatever did not come back as unavailable.
	if c.fallback == nil {
		items, err := get(c.primary, ctx, ids)
		if err != nil {
			return Batch[T]{}, err
		}
		return Batch[T]{Items: items, Declined: absent(ids, items, idOf)}, nil
	}

	paused := false
	if until := c.pausedUntil(); !until.IsZero() && until.After(c.now()) {
		paused = true
	}

	// The fallback goes first when it is preferred, and whenever the primary is
	// rate limited.
	//
	// A paused primary is skipped rather than tried: its limiter blocks until the
	// pause expires, which for an exhausted daily quota is most of a day, so
	// asking would stall the batch rather than fail it.
	if c.preferFallback || paused {
		items, err := get(c.fallback, ctx, ids)
		if err != nil {
			if paused {
				// Nowhere left to ask.
				return Batch[T]{}, err
			}
			// The preferred source is down; the primary is still there.
			c.lg.Warn("the preferred metadata source could not be reached; asking the primary",
				"kind", kind, "ids", len(ids), "error", err.Error())
			items = nil
		}
		items = filter(items, ids, idOf)
		missing := absent(ids, items, idOf)

		if len(missing) == 0 {
			return Batch[T]{Items: items}, nil
		}
		if paused {
			// The primary has not spoken, so nothing may be written off: an id the
			// fallback happens not to know must stay pending rather than become
			// permanently blank.
			c.lg.Info("primary metadata source is rate limited; served from the fallback",
				"kind", kind, "requested", len(ids), "served", len(items))
			return Batch[T]{Items: items}, nil
		}

		// Only the primary's answer is final, so it is what decides the rest.
		rest, err := get(c.primary, ctx, missing)
		if err != nil {
			// Whatever the fallback found still stands; the ids the primary was
			// going to answer for simply stay pending.
			c.lg.Warn("the primary metadata source failed for what the fallback lacked",
				"kind", kind, "ids", len(missing), "error", err.Error())
			return Batch[T]{Items: items}, nil
		}
		if len(rest) > 0 {
			c.lg.Info("filled metadata the preferred source does not have",
				"kind", kind, "ids", len(missing), "filled", len(rest))
		}
		return Batch[T]{
			Items:    append(items, rest...),
			Declined: absent(missing, rest, idOf),
		}, nil
	}

	items, err := get(c.primary, ctx, ids)
	if err != nil {
		// Including the 429 that declares the pause: that one batch behaves as it
		// always did, and every batch after it takes the branch above.
		return Batch[T]{}, err
	}

	missing := absent(ids, items, idOf)
	if len(missing) == 0 {
		return Batch[T]{Items: items}, nil
	}

	// The primary answered and had nothing for these. Before writing them off,
	// ask the source that exists precisely for the ids Spotify no longer serves.
	extra, err := get(c.fallback, ctx, missing)
	if err != nil {
		// The primary's answer is authoritative and complete on its own terms, so
		// a fallback that is down costs nothing but the chance of filling a hole.
		c.lg.Warn("the metadata fallback could not be reached; keeping the primary's answer",
			"kind", kind, "ids", len(missing), "error", err.Error())
		return Batch[T]{Items: items, Declined: missing}, nil
	}

	extra = filter(extra, missing, idOf)
	if len(extra) > 0 {
		c.lg.Info("filled metadata the primary source does not serve",
			"kind", kind, "ids", len(missing), "filled", len(extra))
	}
	return Batch[T]{
		Items:    append(items, extra...),
		Declined: absent(missing, extra, idOf),
	}, nil
}

// absent returns the requested ids that no item accounts for, in request order.
func absent[T any](requested []string, items []T, idOf func(T) string) []string {
	if len(requested) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(items))
	for _, item := range items {
		if id := idOf(item); id != "" {
			have[id] = struct{}{}
		}
	}
	out := make([]string, 0, len(requested))
	for _, id := range requested {
		if _, ok := have[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// filter drops anything the source returned that was not asked for.
//
// A fallback is somebody's own server rather than Spotify's, and a buggy or
// hostile one must not be able to write rows into the catalogue by volunteering
// ids nobody requested.
func filter[T any](items []T, requested []string, idOf func(T) string) []T {
	if len(items) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		want[id] = struct{}{}
	}
	out := items[:0:0]
	for _, item := range items {
		if _, ok := want[idOf(item)]; ok {
			out = append(out, item)
		}
	}
	return out
}
