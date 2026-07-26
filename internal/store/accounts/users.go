package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// Users is the repository for the users table.
type Users struct{ db *store.Store }

// NewUsers builds the repository.
func NewUsers(db *store.Store) *Users { return &Users{db: db} }

// userColumns and userColumnsU are the same list, unqualified and qualified with
// the alias `u`. They are kept next to userDest, which must stay in the same
// order.
const userColumns = `id, spotify_user_id, display_name, email, avatar_url, product, ` +
	`role, is_active, timezone, created_at, updated_at, last_login_at`

const userColumnsU = `u.id, u.spotify_user_id, u.display_name, u.email, u.avatar_url, u.product, ` +
	`u.role, u.is_active, u.timezone, u.created_at, u.updated_at, u.last_login_at`

// userDest lists the scan destinations for userColumns, in order. role is scanned
// through a string because domain.Role is a named type the driver does not know.
func userDest(u *domain.User, role *string) []any {
	return []any{
		&u.ID, &u.SpotifyUserID, &u.DisplayName, &u.Email, &u.AvatarURL, &u.Product,
		role, &u.IsActive, &u.Timezone, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt,
	}
}

// scanUser reads userColumns, followed by any extra destinations the statement
// appends after them.
func scanUser(row rowScanner, extra ...any) (domain.User, error) {
	var (
		u    domain.User
		role string
	)
	if err := row.Scan(append(userDest(&u, &role), extra...)...); err != nil {
		return domain.User{}, err
	}
	u.Role = domain.Role(role)
	return u, nil
}

// GetByID loads one user.
func (r *Users) GetByID(ctx context.Context, q store.Querier, id uuid.UUID) (domain.User, error) {
	row := q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, store.UUIDArg(id))
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, postgres.Classify("get user", err)
	}
	return u, nil
}

// GetBySpotifyUserID loads one user by the natural key Spotify gives us. It
// ignores is_active on purpose: the sign-in path has to be able to tell a
// disabled account apart from an unknown one.
func (r *Users) GetBySpotifyUserID(ctx context.Context, q store.Querier, spotifyUserID string) (domain.User, error) {
	row := q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE spotify_user_id = $1`, spotifyUserID)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, postgres.Classify("get user by spotify id", err)
	}
	return u, nil
}

// ListUsers returns one page of users oldest first, together with the total
// number of users so the caller can render pagination.
//
// The count is a separate statement rather than a window function because a
// window count returns nothing at all for a page past the end of the table,
// which is exactly the case where the caller still needs the total.
func (r *Users) ListUsers(ctx context.Context, q store.Querier, limit, offset int) ([]domain.User, int64, error) {
	limit, offset = clampPage(limit, offset)

	var total int64
	if err := q.QueryRow(ctx, `SELECT count(*)::bigint FROM users`).Scan(&total); err != nil {
		return nil, 0, postgres.Classify("count users", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	// id breaks ties so that two accounts created in the same transaction cannot
	// swap places between pages.
	rows, err := q.Query(ctx,
		`SELECT `+userColumns+` FROM users ORDER BY created_at, id LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, 0, postgres.Classify("list users", err)
	}
	defer rows.Close()

	out := make([]domain.User, 0, limit)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, postgres.Classify("scan user", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, postgres.Classify("list users", err)
	}
	return out, total, nil
}

// SpotifyProfile is the subset of Spotify's /v1/me response Encore stores. Empty
// fields mean "not reported", which is common: email arrives only with the
// user-read-email scope, and avatar_url only when the account has a picture.
type SpotifyProfile struct {
	SpotifyUserID string
	DisplayName   string
	Email         string
	AvatarURL     string
	Product       string
}

// upsertUserSQL resolves a completed OAuth flow into exactly one user row.
//
// It is a single statement because sign-in is the one place where a read
// followed by a write would be racy in a way that matters: two simultaneous
// first sign-ins that both read an empty users table would both conclude they
// are the founder. Deciding the role inside the INSERT, and letting the unique
// constraint on spotify_user_id settle a duplicate identity, removes that
// window.
//
// The refresh branch requires is_active, so a disabled account produces no rows
// at all and the caller can tell it apart from an unknown identity.
var upsertUserSQL = `
WITH existing AS (
    SELECT 1 FROM users WHERE spotify_user_id = $1
),
inserted AS (
    INSERT INTO users (spotify_user_id, display_name, email, avatar_url, product, role, timezone, last_login_at)
    SELECT $1, $2, $3, $4, $5,
           CASE WHEN NOT EXISTS (SELECT 1 FROM users) THEN 'admin' ELSE 'user' END,
           $6, now()
    WHERE $7::boolean AND NOT EXISTS (SELECT 1 FROM existing)
    ON CONFLICT (spotify_user_id) DO NOTHING
    RETURNING ` + userColumns + `
),
refreshed AS (
    UPDATE users u SET
        display_name  = COALESCE(NULLIF($2::text, ''), u.display_name),
        email         = COALESCE(NULLIF($3::text, ''), u.email),
        avatar_url    = COALESCE(NULLIF($4::text, ''), u.avatar_url),
        product       = COALESCE(NULLIF($5::text, ''), u.product),
        last_login_at = now(),
        updated_at    = now()
    WHERE u.spotify_user_id = $1 AND u.is_active
    RETURNING ` + userColumnsU + `
)
SELECT ` + userColumns + `, true FROM inserted
UNION ALL
SELECT ` + userColumns + `, false FROM refreshed`

// UpsertFromSpotify turns a completed OAuth flow into a signed-in user, creating
// the account when the identity is new, and reports whether it created one.
//
// The very first account ever created becomes the administrator; every later one
// is an ordinary user. An unknown identity is refused with
// domain.ErrRegistrationsDisabled when allowRegistration is false, and a known
// but deactivated one with domain.ErrAccountDisabled.
//
// defaultTimezone seeds a newly created account only; it never overwrites the
// timezone of an existing user, who may have chosen their own.
func (r *Users) UpsertFromSpotify(ctx context.Context, q store.Querier, p SpotifyProfile, defaultTimezone string, allowRegistration bool) (domain.User, bool, error) {
	if p.SpotifyUserID == "" {
		return domain.User{}, false, fmt.Errorf("%w: spotify user id is required", domain.ErrValidation)
	}

	tz := defaultTimezone
	if tz == "" || domain.ValidateTimezone(tz) != nil {
		// config validates ENCORE_DEFAULT_TIMEZONE at startup, so this is belt and
		// braces; a bad default must never be the reason a sign-in fails.
		tz = "UTC"
	}

	row := q.QueryRow(ctx, upsertUserSQL,
		p.SpotifyUserID, p.DisplayName, p.Email, p.AvatarURL, p.Product, tz, allowRegistration)

	var created bool
	u, err := scanUser(row, &created)
	if err == nil {
		return u, created, nil
	}
	cerr := postgres.Classify("upsert user from spotify", err)
	if !errors.Is(cerr, domain.ErrNotFound) {
		return domain.User{}, false, cerr
	}

	// No row came back. Either the identity is unknown and registration is closed,
	// or the account is deactivated, or a concurrent sign-in for the same identity
	// won the ON CONFLICT race. Only a second look can tell those apart.
	existing, gerr := r.GetBySpotifyUserID(ctx, q, p.SpotifyUserID)
	switch {
	case gerr == nil && !existing.IsActive:
		return domain.User{}, false, domain.ErrAccountDisabled
	case gerr == nil:
		return existing, false, nil
	case errors.Is(gerr, domain.ErrNotFound):
		return domain.User{}, false, domain.ErrRegistrationsDisabled
	default:
		return domain.User{}, false, gerr
	}
}

// ProfileUpdate carries the fields a Spotify profile refresh may change. An empty
// string leaves the stored value alone, because Spotify omits fields it has no
// value for and overwriting a known email with "" would silently lose it.
type ProfileUpdate struct {
	DisplayName string
	Email       string
	AvatarURL   string
	Product     string
	// LastLoginAt is set when this refresh accompanies a sign-in. nil leaves the
	// recorded login time untouched.
	LastLoginAt *time.Time
}

// UpdateProfile applies a profile refresh and returns the stored user.
func (r *Users) UpdateProfile(ctx context.Context, q store.Querier, id uuid.UUID, p ProfileUpdate) (domain.User, error) {
	const sql = `
        UPDATE users SET
            display_name  = COALESCE(NULLIF($2::text, ''), display_name),
            email         = COALESCE(NULLIF($3::text, ''), email),
            avatar_url    = COALESCE(NULLIF($4::text, ''), avatar_url),
            product       = COALESCE(NULLIF($5::text, ''), product),
            last_login_at = COALESCE($6::timestamptz, last_login_at),
            updated_at    = now()
        WHERE id = $1
        RETURNING ` + userColumns

	row := q.QueryRow(ctx, sql, store.UUIDArg(id),
		p.DisplayName, p.Email, p.AvatarURL, p.Product, p.LastLoginAt)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, postgres.Classify("update user profile", err)
	}
	return u, nil
}

// SetTimezone changes the timezone statistics bucket in. The name is validated
// against the runtime's tzdata before it is stored, so a typo is a 400 at the
// moment it is made rather than a silent fallback to UTC on every later query.
func (r *Users) SetTimezone(ctx context.Context, q store.Querier, id uuid.UUID, timezone string) (domain.User, error) {
	if err := domain.ValidateTimezone(timezone); err != nil {
		return domain.User{}, err
	}
	const sql = `UPDATE users SET timezone = $2, updated_at = now() WHERE id = $1 RETURNING ` + userColumns
	row := q.QueryRow(ctx, sql, store.UUIDArg(id), timezone)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, postgres.Classify("set user timezone", err)
	}
	return u, nil
}

// SetRole promotes or demotes a user. Refusing to demote the last administrator
// is the caller's decision, taken with CountAdmins in the same transaction.
func (r *Users) SetRole(ctx context.Context, q store.Querier, id uuid.UUID, role domain.Role) (domain.User, error) {
	if !role.Valid() {
		return domain.User{}, fmt.Errorf("%w: unknown role %q", domain.ErrValidation, string(role))
	}
	const sql = `UPDATE users SET role = $2, updated_at = now() WHERE id = $1 RETURNING ` + userColumns
	row := q.QueryRow(ctx, sql, store.UUIDArg(id), string(role))
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, postgres.Classify("set user role", err)
	}
	return u, nil
}

// SetActive enables or disables an account. Disabling does not delete sessions;
// the sign-in path refuses a deactivated user on the next request anyway, and
// keeping the rows lets an accidental deactivation be undone without forcing
// everyone to sign in again.
func (r *Users) SetActive(ctx context.Context, q store.Querier, id uuid.UUID, active bool) (domain.User, error) {
	const sql = `UPDATE users SET is_active = $2, updated_at = now() WHERE id = $1 RETURNING ` + userColumns
	row := q.QueryRow(ctx, sql, store.UUIDArg(id), active)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, postgres.Classify("set user active", err)
	}
	return u, nil
}

// DeleteUser removes an account and, through ON DELETE CASCADE, its credentials,
// sessions, listens and imports. There is no soft delete: a user who asks to be
// forgotten is forgotten.
func (r *Users) DeleteUser(ctx context.Context, q store.Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `DELETE FROM users WHERE id = $1`, store.UUIDArg(id))
	if err != nil {
		return postgres.Classify("delete user", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete user: %w", domain.ErrNotFound)
	}
	return nil
}

// CountAdmins counts the administrators who could actually sign in.
//
// Deactivated administrators are excluded deliberately: they cannot rescue the
// instance, so counting them would let the last usable administrator be demoted
// or deleted and lock everyone out of the settings.
func (r *Users) CountAdmins(ctx context.Context, q store.Querier) (int64, error) {
	var n int64
	err := q.QueryRow(ctx,
		`SELECT count(*)::bigint FROM users WHERE role = 'admin' AND is_active`).Scan(&n)
	if err != nil {
		return 0, postgres.Classify("count admins", err)
	}
	return n, nil
}
