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
// The fallback is consulted in two situations, and they are different:
//
//   - The primary is rate limited. Spotify answers an exhausted daily quota with
//     a Retry-After of most of a day, and its limiter blocks rather than erroring
//     for the duration — so the pause is checked before the call, not discovered
//     from it. The whole batch goes to the fallback and nothing is concluded
//     about availability.
//
//   - The primary answered but had nothing for some ids. Those would otherwise
//     be marked unavailable for good, which is right when Spotify is the only
//     source and wrong as soon as there is another. The fallback is asked for
//     exactly those ids, and only what neither source has is declined.
//
// A fallback failure is never fatal: it is logged and the primary's answer
// stands, because an instance whose metadata mirror is down should behave like
// an instance that never had one.
type Chain struct {
	primary  Source
	fallback Source

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

	// The primary is held back. Asking it anyway would not fail fast — the
	// limiter blocks until the pause expires, which for an exhausted daily quota
	// is most of a day — so it is skipped entirely.
	//
	// Nothing is declined here: the primary has not spoken, and an id the
	// fallback happens not to know must stay pending rather than become
	// permanently blank.
	if until := c.pausedUntil(); !until.IsZero() && until.After(c.now()) {
		items, err := get(c.fallback, ctx, ids)
		if err != nil {
			return Batch[T]{}, err
		}
		c.lg.Info("primary metadata source is rate limited; served from the fallback",
			"kind", kind, "requested", len(ids), "served", len(items),
			"primary_resumes_at", until.UTC().Format(time.RFC3339))
		return Batch[T]{Items: filter(items, ids, idOf)}, nil
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
