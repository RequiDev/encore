package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
)

// SyncUser polls one account's recently-played feed and ingests what it finds.
//
// It is the whole of the sync behaviour, and it is also what POST /api/sync/now
// calls, so a listener who presses "sync now" and the background tick take
// exactly the same path. A second concurrent call for the same account returns
// ErrAlreadyRunning rather than reading the feed twice.
//
// The returned SyncResult is meaningful even alongside an error: a poll that
// fetched a page and then failed to commit it reports what it saw.
func (p *Poller) SyncUser(ctx context.Context, userID uuid.UUID) (SyncResult, error) {
	if userID == uuid.Nil {
		return SyncResult{}, fmt.Errorf("%w: sync needs a user", domain.ErrValidation)
	}
	if !p.claim(userID) {
		return SyncResult{}, ErrAlreadyRunning
	}
	defer p.release(userID)

	res, err := p.syncUser(ctx, userID)
	if err != nil {
		// A cancelled context is the process shutting down, not a poll that
		// failed; recording it would make a deployment look like an outage.
		if ctx.Err() == nil {
			p.stat.SyncRun(resultFailure)
		}
		return res, err
	}

	p.stat.SyncListens(int64(res.Imported))
	p.stat.SyncLastSuccess(p.now())
	if res.Fetched == 0 {
		// An idle account and a working one must not look identical on a
		// dashboard, so a poll that found nothing is its own result.
		p.stat.SyncRun(resultSkipped)
	} else {
		p.stat.SyncRun(resultSuccess)
	}

	log := p.log.With("user", userID.String())
	if res.Imported > 0 {
		log.Info("recently-played sync ingested plays",
			"fetched", res.Fetched, "imported", res.Imported, "duplicates", res.Duplicates,
			"skipped", res.Skipped, "invalid", res.Invalid)
	} else {
		log.Debug("recently-played sync found nothing new",
			"fetched", res.Fetched, "duplicates", res.Duplicates,
			"skipped", res.Skipped, "invalid", res.Invalid)
	}
	return res, nil
}

// syncUser is SyncUser without the claim, the metrics and the logging.
func (p *Poller) syncUser(ctx context.Context, userID uuid.UUID) (SyncResult, error) {
	creds, err := p.dep.Accounts.Credentials.Get(ctx, p.dep.Store.DB(), userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return SyncResult{}, ErrNotConnected
		}
		return SyncResult{}, fmt.Errorf("load spotify credentials: %w", err)
	}
	if creds.SyncState == domain.SyncStateNeedsReauth {
		// Parked by an earlier poll. Asking Spotify again would be a certain
		// failure, and certain failures are the expensive kind to repeat.
		return SyncResult{}, ErrNeedsReauth
	}

	res, err := p.poll(ctx, userID, creds)
	if err != nil {
		p.recordFailure(ctx, userID, err)
		return res, err
	}
	return res, nil
}

// poll runs one account's fetch-convert-commit cycle.
func (p *Poller) poll(ctx context.Context, userID uuid.UUID, creds domain.SpotifyCredentials) (SyncResult, error) {
	var res SyncResult

	token, err := p.accessToken(ctx, userID, creds, false)
	if err != nil {
		return res, err
	}
	after, err := p.cursor(ctx, userID, creds)
	if err != nil {
		return res, err
	}

	plays, err := p.fetch(ctx, userID, creds, token, after)
	if err != nil {
		return res, err
	}

	now := p.now()
	b := prepare(userID, plays, now)
	res.Fetched = len(plays)
	res.Skipped, res.Invalid = b.skipped, b.invalid
	res.NewestPlayedAt = b.newest
	if b.invalid > 0 {
		// Counted rather than fatal: one nonsensical entry must not stop the
		// rest of the page from being stored.
		p.log.Warn("discarded unusable plays from the recently-played feed",
			"user", userID.String(), "invalid", b.invalid, "fetched", len(plays))
	}

	// The timezone is only needed to decide which local day the new rows make
	// dirty, so it is read only when there are rows. Most polls find nothing and
	// have no reason to touch the users table at all.
	timezone := "UTC"
	if len(b.staged) > 0 {
		if timezone, err = p.timezone(ctx, userID); err != nil {
			return res, err
		}
	}

	inserted, err := p.commit(ctx, userID, b, timezone)
	if err != nil {
		return res, err
	}
	res.Imported = int(inserted)
	res.Duplicates = len(b.staged) - res.Imported
	return res, nil
}

// fetch reads the feed, refreshing the token once if Spotify rejects it.
//
// A 401 against a token that had not reached its stated expiry means the grant
// was revoked or rotated early, which one forced refresh fixes. A second
// rejection is a real failure rather than something to loop on.
func (p *Poller) fetch(ctx context.Context, userID uuid.UUID, creds domain.SpotifyCredentials, token string, after time.Time) ([]spotify.PlayHistory, error) {
	plays, err := p.dep.Spotify.RecentlyPlayed(ctx, token, after, recentlyPlayedLimit, recentlyPlayedPages)
	if unauthorised(err) {
		token, err = p.accessToken(ctx, userID, creds, true)
		if err != nil {
			return nil, err
		}
		plays, err = p.dep.Spotify.RecentlyPlayed(ctx, token, after, recentlyPlayedLimit, recentlyPlayedPages)
	}
	switch {
	case err == nil:
		return plays, nil
	case unauthorised(err), forbidden(err):
		// A freshly issued token that still cannot read the listener's own play
		// history means the grant no longer carries the scope. Only a new
		// authorisation can restore it, so park the account instead of polling
		// it into the rate limit every minute.
		return nil, p.markNeedsReauth(ctx, userID,
			"Spotify refused to read this account's listening history. Reconnect the account to restore synchronisation.")
	default:
		return nil, fmt.Errorf("fetch recently played: %w", err)
	}
}

// accessToken returns a usable access token, refreshing and persisting one when
// the stored token has expired or force asks for a new one regardless.
//
// Spotify omits refresh_token from a refresh response unless it has rotated, so
// an empty value is passed straight through to the store, which reads it as
// "keep what is there". Writing the empty value would leave the account unable
// to refresh ever again.
func (p *Poller) accessToken(ctx context.Context, userID uuid.UUID, creds domain.SpotifyCredentials, force bool) (string, error) {
	if !force && creds.AccessTokenValid(p.now()) {
		return creds.AccessToken, nil
	}
	if creds.RefreshToken == "" {
		return "", p.markNeedsReauth(ctx, userID,
			"No refresh token is stored for this connection. Reconnect the account to restore synchronisation.")
	}

	tok, err := p.dep.Spotify.RefreshToken(ctx, creds.RefreshToken)
	if err != nil {
		if errors.Is(err, spotify.ErrInvalidGrant) {
			// The listener revoked access, or Spotify invalidated the grant.
			// Nothing but a new authorisation will change that answer.
			return "", p.markNeedsReauth(ctx, userID,
				"Spotify rejected the stored authorisation. Reconnect the account to restore synchronisation.")
		}
		return "", fmt.Errorf("refresh spotify token: %w", err)
	}

	if err := p.dep.Accounts.Credentials.UpdateTokens(
		ctx, p.dep.Store.DB(), userID, tok.AccessToken, tok.RefreshToken, tok.ExpiresAt); err != nil {
		return "", fmt.Errorf("store refreshed spotify token: %w", err)
	}
	p.log.Debug("refreshed spotify access token", "user", userID.String(),
		"expires_at", tok.ExpiresAt.UTC().Format(time.RFC3339), "rotated", tok.RefreshToken != "")
	return tok.AccessToken, nil
}

// cursor decides the "after" watermark for one poll.
//
// The stored sync_cursor_at is authoritative. A newly connected account has
// none, so the newest listen already in the database is used instead — that is
// what stops a fresh connection re-reading plays a history import already
// holds — and an account with no history at all falls back to a bounded
// lookback rather than to the beginning of time.
func (p *Poller) cursor(ctx context.Context, userID uuid.UUID, creds domain.SpotifyCredentials) (time.Time, error) {
	now := p.now()
	if creds.SyncCursorAt != nil && !creds.SyncCursorAt.IsZero() {
		return notAfter(creds.SyncCursorAt.UTC(), now), nil
	}

	latest, err := p.dep.Listens.LatestListenAt(ctx, p.dep.Store.DB(), userID)
	if err != nil {
		return time.Time{}, fmt.Errorf("read latest listen: %w", err)
	}
	if latest != nil && !latest.IsZero() {
		return notAfter(latest.UTC(), now), nil
	}
	return now.Add(-p.cfg.InitialLookback).UTC(), nil
}

// timezone resolves the owning user's timezone, which decides which local day
// each committed listen marks dirty for the statistics rollups.
func (p *Poller) timezone(ctx context.Context, userID uuid.UUID) (string, error) {
	user, err := p.dep.Accounts.Users.GetByID(ctx, p.dep.Store.DB(), userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", ErrNotConnected
		}
		return "", fmt.Errorf("load user for sync: %w", err)
	}
	if user.Timezone == "" {
		return "UTC", nil
	}
	return user.Timezone, nil
}

// markNeedsReauth parks an account and returns ErrNeedsReauth, so a caller can
// simply return what this gives back.
//
// The write uses a detached context: an account whose grant has just been
// refused must be parked even if the process is shutting down, or the next
// worker will spend its rate limit rediscovering the same answer.
func (p *Poller) markNeedsReauth(ctx context.Context, userID uuid.UUID, reason string) error {
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()

	if err := p.dep.Accounts.Credentials.MarkNeedsReauth(mctx, p.dep.Store.DB(), userID, reason); err != nil {
		// Report the write failure rather than ErrNeedsReauth: the account has
		// not actually been parked, so it must stay in the rotation.
		return fmt.Errorf("park account for re-authorisation: %w", err)
	}
	return ErrNeedsReauth
}

// recordFailure stores the reason a poll failed on the credential row, where the
// account page can show it.
//
// The cursor is deliberately left alone: a failed poll has committed nothing, so
// the next one must ask for the same window again.
func (p *Poller) recordFailure(ctx context.Context, userID uuid.UUID, cause error) {
	switch {
	case errors.Is(cause, ErrNeedsReauth):
		// Already recorded, with a message aimed at the listener rather than at
		// an operator. Overwriting it would only make it vaguer.
		return
	case errors.Is(cause, ErrNotConnected):
		// There is no row left to write to.
		return
	case ctx.Err() != nil:
		// Shutdown, not a fault of the account.
		return
	}

	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()

	if err := p.dep.Accounts.Credentials.MarkSyncResult(mctx, p.dep.Store.DB(), userID, nil, cause); err != nil {
		p.log.Error("could not record sync failure", "user", userID.String(), logging.Err(err))
	}
}

// unauthorised reports a 401 from Spotify: the token is missing, expired or has
// been revoked.
func unauthorised(err error) bool {
	apiErr, ok := spotify.AsAPIError(err)
	return ok && apiErr.IsUnauthorized()
}

// forbidden reports a 403: the token is valid but does not carry the scope the
// endpoint needs.
func forbidden(err error) bool {
	apiErr, ok := spotify.AsAPIError(err)
	return ok && apiErr.IsForbidden()
}

// notAfter keeps a watermark from running ahead of the present. A cursor in the
// future asks Spotify for plays that cannot exist yet, and would hide every real
// play until the clock caught up with it.
func notAfter(t, now time.Time) time.Time {
	if t.After(now) {
		return now.UTC()
	}
	return t
}

// AccessToken returns a usable access token for one account, refreshing it when
// the stored one has expired.
//
// Exported because the API needs it too. Creating a playlist acts on the
// listener's own account and so must use the listener's own token, and the
// refresh dance — including marking an account needs_reauth when Spotify has
// revoked the grant — belongs here rather than duplicated in a handler.
func (p *Poller) AccessToken(ctx context.Context, userID uuid.UUID) (string, error) {
	creds, err := p.dep.Accounts.Credentials.Get(ctx, p.dep.Store.DB(), userID)
	if err != nil {
		return "", err
	}
	return p.accessToken(ctx, userID, creds, false)
}
