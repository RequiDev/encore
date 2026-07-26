package accounts

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// Sessions is the repository for the sessions table.
//
// Only the SHA-256 of the cookie value is ever stored or looked up, so nothing
// in this table can be replayed as a login by whoever reads it.
type Sessions struct{ db *store.Store }

// NewSessions builds the repository.
func NewSessions(db *store.Store) *Sessions { return &Sessions{db: db} }

// sessionColumns and sessionColumnsS are the same list, unqualified and
// qualified with the alias `s`. Both read ip through host() so the address
// arrives as plain text; the column is inet, which the driver would otherwise
// hand back as a network type. They must stay in step with sessionDest.
const sessionColumns = `id, user_id, token_hash, csrf_token, expires_at, ` +
	`created_at, last_seen_at, user_agent, host(ip)`

const sessionColumnsS = `s.id, s.user_id, s.token_hash, s.csrf_token, s.expires_at, ` +
	`s.created_at, s.last_seen_at, s.user_agent, host(s.ip)`

// sessionDest lists the scan destinations for sessionColumns, in order.
func sessionDest(s *domain.Session, ip **string) []any {
	return []any{
		&s.ID, &s.UserID, &s.TokenHash, &s.CSRFToken, &s.ExpiresAt,
		&s.CreatedAt, &s.LastSeenAt, &s.UserAgent, ip,
	}
}

// insertSessionSQL casts the address through text so callers can pass a plain
// string; store.Nullable turns an unknown address into NULL rather than into an
// inet Postgres would reject.
const insertSessionSQL = `
INSERT INTO sessions (user_id, token_hash, csrf_token, expires_at, user_agent, ip)
VALUES ($1, $2, $3, $4, $5, $6::text::inet)
RETURNING ` + sessionColumns

// Create opens a session for a user who has just signed in. tokenHash is the
// SHA-256 of the cookie value, never the value itself.
func (r *Sessions) Create(ctx context.Context, q store.Querier, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, userAgent, ip string) (domain.Session, error) {
	if len(tokenHash) == 0 {
		return domain.Session{}, fmt.Errorf("%w: session token hash is required", domain.ErrValidation)
	}
	if csrfToken == "" {
		return domain.Session{}, fmt.Errorf("%w: session csrf token is required", domain.ErrValidation)
	}

	var (
		s    domain.Session
		addr *string
	)
	err := q.QueryRow(ctx, insertSessionSQL,
		store.UUIDArg(userID), tokenHash, csrfToken, expiresAt.UTC(), userAgent, store.Nullable(ip),
	).Scan(sessionDest(&s, &addr)...)
	if err != nil {
		return domain.Session{}, postgres.Classify("create session", err)
	}
	s.IP = store.Deref(addr)
	return s, nil
}

// getSessionByTokenHashSQL fetches the session and its owner together.
//
// One join rather than two round trips, because this runs on every
// authenticated request and is the hottest query in the application.
//
// Expiry is filtered in SQL, against the same clock that wrote expires_at, so an
// expired session is indistinguishable from a missing one — which is exactly the
// answer the caller wants. is_active is deliberately not filtered, so that a
// deactivated account can be reported as disabled rather than as merely
// unauthenticated.
const getSessionByTokenHashSQL = `
SELECT ` + sessionColumnsS + `, ` + userColumnsU + `
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now()`

// GetByTokenHash resolves a cookie into its session and the user who owns it.
//
// It returns domain.ErrNotFound when the token is unknown or the session has
// lapsed, and domain.ErrAccountDisabled when the account was deactivated after
// the session was opened.
func (r *Sessions) GetByTokenHash(ctx context.Context, q store.Querier, tokenHash []byte) (domain.Session, domain.User, error) {
	if len(tokenHash) == 0 {
		return domain.Session{}, domain.User{}, fmt.Errorf("%w: session token hash is required", domain.ErrValidation)
	}

	var (
		s    domain.Session
		u    domain.User
		addr *string
		role string
	)
	dest := append(sessionDest(&s, &addr), userDest(&u, &role)...)
	if err := q.QueryRow(ctx, getSessionByTokenHashSQL, tokenHash).Scan(dest...); err != nil {
		return domain.Session{}, domain.User{}, postgres.Classify("get session", err)
	}
	s.IP = store.Deref(addr)
	u.Role = domain.Role(role)

	if !u.IsActive {
		return domain.Session{}, domain.User{}, domain.ErrAccountDisabled
	}
	return s, u, nil
}

// Touch records that a session was used. Only last_seen_at moves: the expiry is
// fixed when the session is created, so a stolen cookie cannot be kept alive
// indefinitely simply by using it.
func (r *Sessions) Touch(ctx context.Context, q store.Querier, sessionID uuid.UUID) error {
	tag, err := q.Exec(ctx,
		`UPDATE sessions SET last_seen_at = now() WHERE id = $1`, store.UUIDArg(sessionID))
	if err != nil {
		return postgres.Classify("touch session", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("touch session: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete signs one session out.
func (r *Sessions) Delete(ctx context.Context, q store.Querier, sessionID uuid.UUID) error {
	tag, err := q.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, store.UUIDArg(sessionID))
	if err != nil {
		return postgres.Classify("delete session", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete session: %w", domain.ErrNotFound)
	}
	return nil
}

// DeleteAllForUser signs a user out everywhere and reports how many sessions
// were closed. Finding none is not an error.
func (r *Sessions) DeleteAllForUser(ctx context.Context, q store.Querier, userID uuid.UUID) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, store.UUIDArg(userID))
	if err != nil {
		return 0, postgres.Classify("delete sessions for user", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteExpired reaps lapsed sessions and reports how many were removed. Those
// rows are already refused by GetByTokenHash; this only stops the table growing
// without bound.
func (r *Sessions) DeleteExpired(ctx context.Context, q store.Querier, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, now.UTC())
	if err != nil {
		return 0, postgres.Classify("delete expired sessions", err)
	}
	return tag.RowsAffected(), nil
}
