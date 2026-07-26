package accounts

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// Credentials is the repository for the spotify_credentials table.
//
// Access and refresh tokens are sealed with AES-256-GCM before they reach the
// database and opened again on the way out, so a database dump on its own does
// not hand over anyone's Spotify account.
type Credentials struct{ db *store.Store }

// NewCredentials builds the repository.
func NewCredentials(db *store.Store) *Credentials { return &Credentials{db: db} }

// maxSyncErrorLen bounds what is stored in last_sync_error. The column is
// unbounded text, but a driver error carrying a whole HTTP response body has no
// business being replayed to the user on the account page.
const maxSyncErrorLen = 500

const credentialColumns = `user_id, access_token_enc, refresh_token_enc, token_expires_at, scopes, ` +
	`sync_state, sync_cursor_at, last_sync_at, last_sync_error, connected_at`

// Get loads a user's Spotify grant with its tokens decrypted.
func (r *Credentials) Get(ctx context.Context, q store.Querier, userID uuid.UUID) (domain.SpotifyCredentials, error) {
	var (
		c            domain.SpotifyCredentials
		accessEnc    []byte
		refreshEnc   []byte
		state        string
		lastSyncErr  string
		scopes       []string
		expiresAt    time.Time
		connectedAt  time.Time
		syncCursorAt *time.Time
		lastSyncAt   *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT `+credentialColumns+` FROM spotify_credentials WHERE user_id = $1`,
		store.UUIDArg(userID),
	).Scan(&c.UserID, &accessEnc, &refreshEnc, &expiresAt, &scopes,
		&state, &syncCursorAt, &lastSyncAt, &lastSyncErr, &connectedAt)
	if err != nil {
		return domain.SpotifyCredentials{}, postgres.Classify("get spotify credentials", err)
	}

	access, err := r.db.Open(accessEnc)
	if err != nil {
		return domain.SpotifyCredentials{}, fmt.Errorf("open access token: %w", err)
	}
	refresh, err := r.db.Open(refreshEnc)
	if err != nil {
		return domain.SpotifyCredentials{}, fmt.Errorf("open refresh token: %w", err)
	}

	c.AccessToken = access
	c.RefreshToken = refresh
	c.TokenExpiresAt = expiresAt
	c.Scopes = scopes
	c.SyncState = domain.SyncState(state)
	c.SyncCursorAt = syncCursorAt
	c.LastSyncAt = lastSyncAt
	c.LastSyncError = lastSyncErr
	c.ConnectedAt = connectedAt
	return c, nil
}

// upsertCredentialsSQL writes a freshly granted authorisation.
//
// Sync bookkeeping is carried forward rather than reset because re-linking the
// same Spotify account is a repair, not a new history: rewinding sync_cursor_at
// would make the poller re-fetch and re-deduplicate everything it already has.
const upsertCredentialsSQL = `
INSERT INTO spotify_credentials (
    user_id, access_token_enc, refresh_token_enc, token_expires_at, scopes,
    sync_state, sync_cursor_at, last_sync_at, last_sync_error, connected_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (user_id) DO UPDATE SET
    access_token_enc  = EXCLUDED.access_token_enc,
    refresh_token_enc = EXCLUDED.refresh_token_enc,
    token_expires_at  = EXCLUDED.token_expires_at,
    scopes            = EXCLUDED.scopes,
    sync_state        = EXCLUDED.sync_state,
    sync_cursor_at    = GREATEST(spotify_credentials.sync_cursor_at, EXCLUDED.sync_cursor_at),
    last_sync_at      = COALESCE(EXCLUDED.last_sync_at, spotify_credentials.last_sync_at),
    last_sync_error   = EXCLUDED.last_sync_error,
    connected_at      = now()`

// Upsert stores a grant obtained from the authorisation-code exchange, replacing
// any previous one for that user.
func (r *Credentials) Upsert(ctx context.Context, q store.Querier, creds domain.SpotifyCredentials) error {
	if creds.UserID == uuid.Nil {
		return fmt.Errorf("%w: credentials need a user", domain.ErrValidation)
	}
	if creds.AccessToken == "" || creds.RefreshToken == "" {
		return fmt.Errorf("%w: a Spotify grant needs both an access and a refresh token", domain.ErrValidation)
	}
	state := creds.SyncState
	if state == "" {
		state = domain.SyncStateOK
	}
	if !state.Valid() {
		return fmt.Errorf("%w: unknown sync state %q", domain.ErrValidation, string(state))
	}

	accessEnc, err := r.db.Seal(creds.AccessToken)
	if err != nil {
		return err
	}
	refreshEnc, err := r.db.Seal(creds.RefreshToken)
	if err != nil {
		return err
	}

	// The column is NOT NULL DEFAULT '{}', and a nil slice would encode as NULL.
	scopes := creds.Scopes
	if scopes == nil {
		scopes = []string{}
	}

	_, err = q.Exec(ctx, upsertCredentialsSQL,
		store.UUIDArg(creds.UserID), accessEnc, refreshEnc, creds.TokenExpiresAt.UTC(), scopes,
		string(state), creds.SyncCursorAt, creds.LastSyncAt, truncate(creds.LastSyncError, maxSyncErrorLen))
	if err != nil {
		return postgres.Classify("upsert spotify credentials", err)
	}
	return nil
}

// updateTokensSQL refreshes the tokens without disturbing sync bookkeeping.
//
// An empty refresh token leaves the stored one in place. Spotify returns
// refresh_token only on the first exchange and occasionally on rotation, so
// treating "absent" as "clear it" would revoke the account's ability to refresh
// on the very next poll.
const updateTokensSQL = `
UPDATE spotify_credentials SET
    access_token_enc  = $2,
    refresh_token_enc = COALESCE($3::bytea, refresh_token_enc),
    token_expires_at  = $4,
    sync_state        = CASE WHEN sync_state = 'needs_reauth' THEN 'ok' ELSE sync_state END,
    last_sync_error   = CASE WHEN sync_state = 'needs_reauth' THEN '' ELSE last_sync_error END
WHERE user_id = $1`

// UpdateTokens stores the result of a token refresh. A successful refresh also
// clears a needs_reauth flag, because a working access token is proof that the
// grant is valid again.
func (r *Credentials) UpdateTokens(ctx context.Context, q store.Querier, userID uuid.UUID, accessToken, refreshToken string, expiresAt time.Time) error {
	if accessToken == "" {
		return fmt.Errorf("%w: access token must not be empty", domain.ErrValidation)
	}
	accessEnc, err := r.db.Seal(accessToken)
	if err != nil {
		return err
	}
	// nil, not an empty slice: only SQL NULL means "keep what is stored".
	var refreshEnc []byte
	if refreshToken != "" {
		if refreshEnc, err = r.db.Seal(refreshToken); err != nil {
			return err
		}
	}

	tag, err := q.Exec(ctx, updateTokensSQL, store.UUIDArg(userID), accessEnc, refreshEnc, expiresAt.UTC())
	if err != nil {
		return postgres.Classify("update spotify tokens", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update spotify tokens: %w", domain.ErrNotFound)
	}
	return nil
}

// markSyncResultSQL records the outcome of one poll.
//
// needs_reauth outranks a plain error: only a new grant or a successful refresh
// clears it, so a failing poll cannot quietly downgrade an account that the user
// has to fix by hand into one the scheduler keeps retrying.
const markSyncResultSQL = `
UPDATE spotify_credentials SET
    sync_state = CASE
        WHEN $2::text = 'ok'                THEN 'ok'
        WHEN sync_state = 'needs_reauth'    THEN 'needs_reauth'
        ELSE $2::text
    END,
    last_sync_at    = now(),
    last_sync_error = $3,
    sync_cursor_at  = GREATEST(sync_cursor_at, $4::timestamptz)
WHERE user_id = $1`

// MarkSyncResult records that a poll finished, moving the watermark forward and
// storing the failure when there was one.
//
// cursorAt may be nil, and never moves the watermark backwards: the cursor is
// only allowed to advance, because a rewind would re-fetch history the importer
// has already deduplicated.
func (r *Credentials) MarkSyncResult(ctx context.Context, q store.Querier, userID uuid.UUID, cursorAt *time.Time, syncErr error) error {
	state := domain.SyncStateOK
	message := ""
	if syncErr != nil {
		state = domain.SyncStateError
		message = truncate(syncErr.Error(), maxSyncErrorLen)
	}
	var cursor *time.Time
	if cursorAt != nil {
		utc := cursorAt.UTC()
		cursor = &utc
	}

	tag, err := q.Exec(ctx, markSyncResultSQL, store.UUIDArg(userID), string(state), message, cursor)
	if err != nil {
		return postgres.Classify("mark sync result", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark sync result: %w", domain.ErrNotFound)
	}
	return nil
}

// MarkNeedsReauth parks an account until its owner goes through OAuth again. It
// is used when Spotify rejects the refresh token, which no amount of retrying
// can fix.
func (r *Credentials) MarkNeedsReauth(ctx context.Context, q store.Querier, userID uuid.UUID, reason string) error {
	const sql = `
        UPDATE spotify_credentials SET
            sync_state      = 'needs_reauth',
            last_sync_at    = now(),
            last_sync_error = $2
        WHERE user_id = $1`
	tag, err := q.Exec(ctx, sql, store.UUIDArg(userID), truncate(reason, maxSyncErrorLen))
	if err != nil {
		return postgres.Classify("mark needs reauth", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark needs reauth: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes a user's grant, disconnecting the account from Spotify without
// touching a single listening record.
func (r *Credentials) Delete(ctx context.Context, q store.Querier, userID uuid.UUID) error {
	tag, err := q.Exec(ctx, `DELETE FROM spotify_credentials WHERE user_id = $1`, store.UUIDArg(userID))
	if err != nil {
		return postgres.Classify("delete spotify credentials", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete spotify credentials: %w", domain.ErrNotFound)
	}
	return nil
}

// listDueForSyncSQL drives the poller's work queue.
//
// The ordering is the reason spotify_credentials_sync_idx exists: never-polled
// accounts come first, then the least recently polled, so a newly connected
// account gets its history quickly and no account can be starved. Deactivated
// users are excluded because polling them would spend Spotify's rate limit on
// data nobody can see.
const listDueForSyncSQL = `
SELECT c.user_id
FROM spotify_credentials c
JOIN users u ON u.id = c.user_id
WHERE c.sync_state <> 'needs_reauth'
  AND u.is_active
  AND (c.last_sync_at IS NULL OR c.last_sync_at < $1)
ORDER BY c.last_sync_at NULLS FIRST
LIMIT $2`

// ListDueForSync returns the users whose Spotify account should be polled now.
func (r *Credentials) ListDueForSync(ctx context.Context, q store.Querier, olderThan time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, listDueForSyncSQL, olderThan.UTC(), clampLimit(limit))
	if err != nil {
		return nil, postgres.Classify("list accounts due for sync", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, postgres.Classify("scan account due for sync", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list accounts due for sync", err)
	}
	return out, nil
}

// truncate bounds a string that is about to be stored in a text column, cutting
// on a rune boundary so that the result is still valid UTF-8 and Postgres does
// not reject it.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
