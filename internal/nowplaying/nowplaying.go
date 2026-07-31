// Package nowplaying polls what each connected listener is playing right now
// and records one row per account, so the dashboard can show it.
//
// Three properties define this package, and each is enforced by something other
// than a comment:
//
//  1. It never writes a listen. GET /me/player/recently-played remains the sole
//     ingestion path, because the sync poller's correctness rests on its cursor
//     advancing in the same transaction that commits the listens it covers, and
//     a second writer with a different view of what has been played would
//     produce duplicates that the dedupe rules catch by accident rather than by
//     design. This package therefore imports nothing that can write one, and a
//     test reads its own import list to say so.
//
//  2. It never parks an account. A 403 here means only that a grant does not
//     carry user-read-playback-state, which is the ordinary state of every
//     account connected before Phase 2a — see internal/sync/account.go's
//     forbidden(). Deps names the three-method Observations rather than
//     *accounts.Repo, so this package has no handle on the credentials
//     repository and could not park an account if it tried.
//
//  3. It cannot stall anything else. Every request goes out on internal/spotify's
//     classNowPlaying, a rate budget of its own: a 429 pauses this loop and is
//     never recorded, so it cannot 409 "sync now" for other users, stop
//     enrichment, or stop the recently-played poller.
//
// And it does nothing at all unless ENCORE_NOWPLAYING_INTERVAL is set. Not
// "defaults to off": Run returns before it lists a single account.
package nowplaying

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
)

// scopeReadPlaybackState is the grant this poller needs. It shipped in
// config.DefaultScopes() in Phase 2a, so an account connected before that
// carries neither it nor the four other Phase 2 scopes, and that is the
// ordinary state of an older account forever rather than a fault to repair.
const scopeReadPlaybackState = "user-read-playback-state"

// concurrency is how many accounts one tick checks at once.
//
// Not a configuration key, deliberately: this phase adds exactly one, and the
// interval is the only lever that changes what the feature costs. Four is
// enough to clear any instance inside a thirty-second tick and small enough that
// a tick never presents the whole instance to Spotify at once.
const concurrency = 4

// accountsPerTick bounds one tick's work. Accounts are handed out
// least-recently-checked first, so anything left over is picked up next tick
// rather than starved.
const accountsPerTick = 200

// tickJitter is the fraction of the interval each delay is randomised by, for
// the reason internal/sync gives: several worker containers started by the same
// deployment would otherwise poll on the same second for ever.
const tickJitter = 0.2

// SpotifyAPI is the part of *spotify.Client this package uses.
//
// One method, and that is the whole of this package's reach into Spotify. A nil
// result with a nil error means nothing is playing.
type SpotifyAPI interface {
	CurrentlyPlaying(ctx context.Context, accessToken string) (*spotify.Playback, error)
}

// Tokens supplies a usable Spotify access token for one account, refreshing and
// persisting it when necessary.
//
// This is *sync.Poller's exported AccessToken, exactly as internal/library uses
// it. Declared as an interface rather than imported directly so this package can
// be tested without a database — and, more importantly here, so that the one
// thing in this package which *can* park an account (a refresh Spotify rejects
// outright, which is broken for every feature and not only this one) is
// somebody else's method rather than this package's.
type Tokens interface {
	AccessToken(ctx context.Context, userID uuid.UUID) (string, error)
}

// Observations is the part of accounts.NowPlaying this package uses.
//
// An interface for the ordinary reason — the loop is exercised without a
// database — and the set is deliberately narrow: list, record, record a failure.
// There is no method here that touches any other table.
type Observations interface {
	ListDue(ctx context.Context, q store.Querier, olderThan time.Time, limit int) ([]accounts.DueAccount, error)
	Record(ctx context.Context, q store.Querier, userID uuid.UUID, n domain.NowPlaying) error
	RecordFailure(ctx context.Context, q store.Querier, userID uuid.UUID, t time.Time) error
}

// Deps are the collaborators a Watcher needs.
//
// NowPlaying is the single-table repository, not *accounts.Repo. That is load
// bearing: with no handle on accounts.Credentials this package cannot call
// MarkNeedsReauth, so the rule that an optional-scope 403 never parks an account
// holds by construction rather than by review.
type Deps struct {
	Store      *store.Store
	NowPlaying Observations
	Spotify    SpotifyAPI
	Tokens     Tokens
	Logger     *slog.Logger
	// Now is injectable so tests can control timestamps without sleeping.
	Now func() time.Time
}

// Watcher polls the player of every connected account that granted
// user-read-playback-state.
//
// It holds no durable state: which accounts are due lives in
// now_playing.checked_at, so a Watcher can be killed at any instant and the next
// process simply asks the database again.
type Watcher struct {
	cfg config.NowPlaying
	dep Deps
	now func() time.Time
	log *slog.Logger

	// rnd supplies the tick jitter in [0,1). Injectable so a test can make the
	// schedule deterministic.
	rnd func() float64
}

// New builds a Watcher. Every collaborator it names is required; the logger and
// the clock default to sensible values.
func New(cfg config.NowPlaying, deps Deps) (*Watcher, error) {
	if deps.Store == nil {
		return nil, errors.New("nowplaying: a store is required")
	}
	if deps.NowPlaying == nil {
		return nil, errors.New("nowplaying: the now-playing repository is required")
	}
	if deps.Spotify == nil {
		return nil, errors.New("nowplaying: a Spotify client is required")
	}
	if deps.Tokens == nil {
		return nil, errors.New("nowplaying: a token source is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	// No interval default. Zero means off, and inventing one here would defeat
	// the whole point of the key being opt-in.
	return &Watcher{
		cfg: cfg,
		dep: deps,
		now: deps.Now,
		log: deps.Logger.With("component", "nowplaying"),
		rnd: rand.Float64,
	}, nil
}

// Run checks every due account, forever, until ctx is cancelled.
//
// It returns nil immediately when no interval is configured, which the worker's
// supervisor treats as a loop that has finished and leaves stopped. That is the
// whole of "unset means off": not a loop that wakes and finds nothing to do, but
// a loop that never starts.
func (w *Watcher) Run(ctx context.Context) error {
	if !w.cfg.Enabled() {
		w.log.Info("now-playing polling is disabled; ENCORE_NOWPLAYING_INTERVAL is not set")
		return nil
	}
	w.log.Info("now-playing polling started",
		"interval", w.cfg.Interval.String(), "concurrency", concurrency)

	// The first delay is drawn from the whole interval rather than jittered
	// around it, which is what actually spreads a fleet that all started at
	// once; subsequent delays only keep them from converging again.
	timer := time.NewTimer(w.firstDelay())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			// Listing the work failed, which is an infrastructure problem
			// rather than an account problem: log it and wait for the next
			// tick instead of spinning against a database that is down.
			w.log.Error("now-playing tick failed", logging.Err(err))
		}
		timer.Reset(w.nextDelay())
	}
}

// RunOnce checks every account that is currently due and reports how many were
// actually checked.
//
// Exported so a worker supervisor, or a test, can drive one tick without owning
// the schedule.
func (w *Watcher) RunOnce(ctx context.Context) (int, error) {
	if !w.cfg.Enabled() {
		// Run's own guard already covers the loop, but this method is exported:
		// a "refresh now" control, or a later phase's caller, could reach it
		// directly. An instance that never set ENCORE_NOWPLAYING_INTERVAL never
		// opted in, and no caller may spend its Spotify quota on that decision's
		// behalf — least of all one that arrived here with a zero interval,
		// where the due predicate below degenerates into "every connected
		// account, every time".
		return 0, nil
	}

	due, err := w.dep.NowPlaying.ListDue(
		ctx, w.dep.Store.DB(), w.now().Add(-w.cfg.Interval+w.dueSlack()), accountsPerTick)
	if err != nil {
		return 0, fmt.Errorf("list accounts due for a playback check: %w", err)
	}
	if len(due) == 0 {
		return 0, nil
	}

	var (
		checked atomic.Int64
		wg      stdsync.WaitGroup
		sem     = make(chan struct{}, concurrency)
	)

	// No shared error group: one account's failure must never cancel another's
	// check, so each is isolated and reports itself.
dispatch:
	for _, account := range due {
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if w.check(ctx, account) {
				checked.Add(1)
			}
		}()
	}
	wg.Wait()

	return int(checked.Load()), nil
}

// check reads one account's player and records what it found.
//
// It never returns an error. Everything that can go wrong with one grant is
// recorded on that account's own row, where the card that concerns that listener
// shows it in words, because one broken connection must not cost anybody else
// their display.
func (w *Watcher) check(ctx context.Context, account accounts.DueAccount) bool {
	log := w.log.With("user", account.UserID.String())

	// Before the request, not through a 403. The SQL predicate in ListDue
	// already excludes a grant without this scope; this is the check that still
	// holds if somebody widens the predicate later, and it costs nothing.
	if !hasScope(account.Scopes, scopeReadPlaybackState) {
		log.Debug("account has not granted user-read-playback-state; skipping without a request")
		return false
	}

	token, err := w.dep.Tokens.AccessToken(ctx, account.UserID)
	if err != nil {
		if ctx.Err() != nil {
			// Shutting down, which is not this account's fault and not a failed
			// check. The same guard the Spotify call below gets, for the same
			// reason: the next tick picks the account up and nothing is lost.
			return false
		}
		w.recordFailure(ctx, account.UserID, log, "could not get an access token", err)
		return false
	}

	pb, err := w.dep.Spotify.CurrentlyPlaying(ctx, token)
	if err != nil {
		if ctx.Err() != nil {
			// Shutting down. The next tick picks the account up and nothing is
			// lost, so this is not a failure to record.
			return false
		}
		w.recordFailure(ctx, account.UserID, log, "could not read the player", err)
		return false
	}

	// pb == nil is a 204: nothing is playing. Not an error, and the commonest
	// answer this endpoint gives.
	if err := w.dep.NowPlaying.Record(ctx, w.dep.Store.DB(), account.UserID, observe(pb, w.now())); err != nil {
		if ctx.Err() != nil {
			// Cancelled between a good response and the write. The same
			// non-event as the two guards above, and the third of the three
			// places one is needed: the account keeps its previous row, stays
			// due, and the next process asks again. Logging it at Error would
			// put a line every clean shutdown deserves nothing for into the
			// severity reserved for things an operator has to act on.
			return false
		}
		log.Error("could not record what is playing", logging.Err(err))
		return false
	}
	return true
}

// recordFailure notes a check that did not succeed, so the card can say the
// display is stale and how stale it is.
//
// The observation columns are untouched: a failed check must not throw away the
// last true thing Encore knew. A 403 lands here like anything else — it is
// deliberately not special-cased into parking the account, because a grant
// without user-read-playback-state still syncs a listening history perfectly.
func (w *Watcher) recordFailure(
	ctx context.Context, userID uuid.UUID, log *slog.Logger, what string, cause error,
) {
	var paused *spotify.PausedError
	switch {
	case errors.As(cause, &paused):
		// Expected and self-healing: the poller's own budget is paused, so no
		// request will reach Spotify until it lifts. Nothing else is affected,
		// which is the entire reason this budget exists.
		log.Debug("now-playing checks are paused by a rate limit",
			"resumes_at", paused.Until.UTC().Format(time.RFC3339))
	default:
		log.Warn(what, logging.Err(cause))
	}

	// Detached from ctx: an account whose check just failed must still have that
	// recorded when the process is shutting down, or the card would report a
	// success that never happened.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := w.dep.NowPlaying.RecordFailure(fctx, w.dep.Store.DB(), userID, w.now()); err != nil {
		log.Error("could not record a failed playback check", logging.Err(err))
	}
}

// observe turns Spotify's answer into the row Encore stores.
//
// Pure, and the only place the "what is playing" question is answered, so every
// distinction the interface draws is decided here once rather than re-derived by
// each reader. A nil playback is a 204: nothing is playing.
func observe(pb *spotify.Playback, at time.Time) domain.NowPlaying {
	out := domain.NowPlaying{
		ObservedAt: at,
		CheckedAt:  at,
		State:      domain.PlaybackIdle,
		Kind:       domain.PlaybackItemNone,
	}
	if pb == nil {
		return out
	}

	out.Kind = kindOf(pb)
	if out.Kind == domain.PlaybackItemNone {
		// A 200 body carrying neither an item nor a type: 204 in a longer form.
		return out
	}
	if pb.IsPlaying {
		out.State = domain.PlaybackPlaying
	} else {
		out.State = domain.PlaybackPaused
	}
	if pb.Device != nil {
		out.DeviceName = pb.Device.Name
	}
	if pb.ProgressMs != nil && *pb.ProgressMs >= 0 {
		ms := *pb.ProgressMs
		out.ProgressMs = &ms
	}

	if item := pb.Item; item != nil && out.Kind != domain.PlaybackItemUnknown {
		out.Title = item.Name
		if item.DurationMs > 0 {
			d := item.DurationMs
			out.DurationMs = &d
		}
		switch out.Kind {
		case domain.PlaybackItemTrack:
			out.TrackID = item.ID
			out.Artist = artistNames(item.Artists)
		case domain.PlaybackItemLocal:
			// No id to keep: a local file has no catalogue identity, which is
			// exactly why it can be named and never linked.
			out.Artist = artistNames(item.Artists)
		case domain.PlaybackItemEpisode:
			// The show stands where an artist would. It is the same slot in the
			// interface — the line under the title — and a podcast has no
			// artist to put there.
			if item.Show != nil {
				out.Artist = item.Show.Name
			}
		}
	}
	// Nothing descriptive survives for an unknown item, which is why the branch
	// above skips it: Spotify's own label for an advert is not a title, and
	// rendering it as one would put an advertiser's name where a listener
	// expects their music. The interface has one sentence for this state and
	// needs no name to render it.
	return out
}

// kindOf classifies what is in the player.
//
// The order of the tests is load bearing. Type is read before the local-file
// check because an episode Spotify happened to send without an id would
// otherwise be reported as a local file, which carries a different sentence in
// the interface and a different claim about the listener's history.
func kindOf(pb *spotify.Playback) domain.PlaybackItemKind {
	item := pb.Item
	if item == nil {
		if pb.CurrentlyPlayingType == "" && !pb.IsPlaying {
			return domain.PlaybackItemNone
		}
		// An advert, or a type this client does not know.
		return domain.PlaybackItemUnknown
	}
	if item.Type == "episode" {
		return domain.PlaybackItemEpisode
	}
	if item.IsLocal || item.ID == "" {
		return domain.PlaybackItemLocal
	}
	if item.Type == "track" || item.Type == "" {
		// Spotify has been observed to omit the type on a track, so an empty
		// value is read as one rather than discarded as unknown.
		return domain.PlaybackItemTrack
	}
	return domain.PlaybackItemUnknown
}

// artistNames joins credited artists the way the interface reads them.
func artistNames(artists []spotify.Artist) string {
	names := make([]string, 0, len(artists))
	for _, a := range artists {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return strings.Join(names, ", ")
}

// hasScope reports whether a stored grant carries a scope.
//
// It splits on spaces for the reason spotify.MissingScopes does: Spotify returns
// granted scopes space-separated in one string, and an account connected before
// Encore split them has one such string in its column.
func hasScope(granted []string, want string) bool {
	for _, g := range granted {
		for f := range strings.SplitSeq(g, " ") {
			if f == want {
				return true
			}
		}
	}
	return false
}

// firstDelay spreads the first tick of freshly started processes across a whole
// interval.
func (w *Watcher) firstDelay() time.Duration {
	return time.Duration(w.rnd() * float64(w.cfg.Interval))
}

// dueSlack is how much less than a full interval still counts as due.
//
// It exists because the schedule and the queue would otherwise disagree.
// nextDelay draws each tick from [I − spread/2, I + spread/2], while a due
// predicate asking for a whole interval since the last check rejects every tick
// in the lower half of that range — so half of all ticks would find the account
// not yet due, skip it, and leave it for the tick after that. The mean effective
// period would be about 1.5 intervals and the worst case two: an operator asking
// for thirty seconds would get a card refreshed every forty-five on average, and
// up to a minute stale.
//
// internal/sync and internal/library have the same disagreement and are left
// alone, deliberately. It costs them little — nobody watches a daily library
// enumeration land — where here freshness is the entire product, and on the
// single-worker deployment this feature is aimed at the jitter buys nothing at
// all to pay for it.
//
// Asking with exactly spread/2 of slack, rather than dropping the jitter or
// widening it arbitrarily, keeps the two definitions derived from one constant:
// the queue is asked with the same tolerance the schedule is drawn with, so
// neither can be changed without the other following.
func (w *Watcher) dueSlack() time.Duration {
	return time.Duration(float64(w.cfg.Interval) * tickJitter / 2)
}

// nextDelay is the configured interval with symmetric jitter applied, so
// processes that happen to align drift apart again instead of staying in step.
func (w *Watcher) nextDelay() time.Duration {
	spread := float64(w.cfg.Interval) * tickJitter
	d := float64(w.cfg.Interval) - spread/2 + w.rnd()*spread
	if d < float64(time.Second) {
		return time.Second
	}
	return time.Duration(d)
}
