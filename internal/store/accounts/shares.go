package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// ShareLimitPerUser caps how many links one account may hold at once.
//
// Not a resource concern — the rows are tiny. It is that a link is a bearer
// credential handed to somebody else, and an account with hundreds of them has
// lost track of who can see what. Revoking is one click; the cap makes sure the
// list stays short enough to read.
const ShareLimitPerUser = 25

// Shares stores read-only links to a user's aggregate statistics.
type Shares struct{ db *store.Store }

// NewShares builds the repository.
func NewShares(db *store.Store) *Shares { return &Shares{db: db} }

const shareColumns = `id, user_id, label, range_from, range_to, range_days,
                      expires_at, revoked_at, last_viewed_at, view_count, created_at`

// shareColumnsQualified is the same list for the query that joins users, where
// bare `id` would be ambiguous between the two tables.
const shareColumnsQualified = `s.id, s.user_id, s.label, s.range_from, s.range_to, s.range_days,
                               s.expires_at, s.revoked_at, s.last_viewed_at, s.view_count, s.created_at`

// Create records a new link. The caller mints the token and passes only its
// hash, so the plaintext exists in this process and in the owner's clipboard and
// nowhere else.
func (r *Shares) Create(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	tokenHash []byte,
	link domain.ShareLink,
) (domain.ShareLink, error) {
	const sql = `
        INSERT INTO share_links (user_id, token_hash, label, range_from, range_to, range_days, expires_at)
        SELECT $1, $2, $3, $4, $5, $6, $7
        WHERE (SELECT count(*) FROM share_links
               WHERE user_id = $1 AND revoked_at IS NULL) < $8
        RETURNING ` + shareColumns

	row := q.QueryRow(ctx, sql,
		userID, tokenHash, link.Label,
		nullTime(link.From), nullTime(link.To), nullInt(link.Days),
		nullTime(link.ExpiresAt), ShareLimitPerUser)

	out, err := scanShare(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// The insert selected nothing, which can only be the cap.
		return domain.ShareLink{}, fmt.Errorf(
			"%w: an account may hold at most %d links; revoke one first",
			domain.ErrValidation, ShareLimitPerUser)
	}
	if err != nil {
		return domain.ShareLink{}, postgres.Classify("create share link", err)
	}
	return out, nil
}

// ListForUser returns a user's links, newest first, revoked ones excluded.
func (r *Shares) ListForUser(ctx context.Context, q store.Querier, userID uuid.UUID) ([]domain.ShareLink, error) {
	rows, err := q.Query(ctx, `SELECT `+shareColumns+`
        FROM share_links WHERE user_id = $1 AND revoked_at IS NULL
        ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, postgres.Classify("list share links", err)
	}
	defer rows.Close()

	out := make([]domain.ShareLink, 0, 8)
	for rows.Next() {
		link, err := scanShare(rows)
		if err != nil {
			return nil, postgres.Classify("scan share link", err)
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list share links", err)
	}
	return out, nil
}

// Revoke retires one of a user's links. Scoped by user id in the statement, so
// knowing another account's link id achieves nothing.
func (r *Shares) Revoke(ctx context.Context, q store.Querier, userID, id uuid.UUID) error {
	tag, err := q.Exec(ctx,
		`UPDATE share_links SET revoked_at = now()
         WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return postgres.Classify("revoke share link", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no such link", domain.ErrNotFound)
	}
	return nil
}

// Resolve looks a link up by the hash of its token and returns it with its
// owner.
//
// A revoked or expired link is reported as not found rather than as a distinct
// state: a visitor holding a dead link learns only that it does not work, which
// is all they are entitled to know.
func (r *Shares) Resolve(
	ctx context.Context,
	q store.Querier,
	tokenHash []byte,
	now time.Time,
) (domain.ShareLink, domain.User, error) {
	row := q.QueryRow(ctx, `
        SELECT `+shareColumnsQualified+`,
               u.id, u.spotify_user_id, u.display_name, u.avatar_url, u.timezone
        FROM share_links s
        JOIN users u ON u.id = s.user_id
        WHERE s.token_hash = $1
          AND s.revoked_at IS NULL
          AND (s.expires_at IS NULL OR s.expires_at > $2)
          AND u.is_active`, tokenHash, now)

	var (
		link                  domain.ShareLink
		from, to, expires     *time.Time
		lastViewed            *time.Time
		days                  *int32
		user                  domain.User
		spotifyID, display    string
		avatarURL, timezoneID string
	)
	err := row.Scan(
		&link.ID, &link.UserID, &link.Label, &from, &to, &days,
		&expires, new(*time.Time), &lastViewed, &link.ViewCount, &link.CreatedAt,
		&user.ID, &spotifyID, &display, &avatarURL, &timezoneID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ShareLink{}, domain.User{}, fmt.Errorf("%w: no such link", domain.ErrNotFound)
	}
	if err != nil {
		return domain.ShareLink{}, domain.User{}, postgres.Classify("resolve share link", err)
	}

	applyShareTimes(&link, from, to, days, expires, lastViewed)
	user.SpotifyUserID = spotifyID
	user.DisplayName = display
	user.AvatarURL = avatarURL
	user.Timezone = timezoneID
	return link, user, nil
}

// Touch records that a link was used. Best effort: a share must still render if
// the counter cannot be written.
func (r *Shares) Touch(ctx context.Context, q store.Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx,
		`UPDATE share_links SET view_count = view_count + 1, last_viewed_at = now() WHERE id = $1`, id)
	if err != nil {
		return postgres.Classify("touch share link", err)
	}
	return nil
}

// --- scanning ---------------------------------------------------------------

func scanShare(row rowScanner) (domain.ShareLink, error) {
	var (
		link              domain.ShareLink
		from, to, expires *time.Time
		revoked, viewed   *time.Time
		days              *int32
	)
	if err := row.Scan(&link.ID, &link.UserID, &link.Label,
		&from, &to, &days, &expires, &revoked, &viewed,
		&link.ViewCount, &link.CreatedAt); err != nil {
		return domain.ShareLink{}, err
	}
	applyShareTimes(&link, from, to, days, expires, viewed)
	if revoked != nil {
		link.RevokedAt = revoked.UTC()
	}
	return link, nil
}

func applyShareTimes(link *domain.ShareLink, from, to *time.Time, days *int32, expires, viewed *time.Time) {
	if from != nil {
		link.From = from.UTC()
	}
	if to != nil {
		link.To = to.UTC()
	}
	if days != nil {
		link.Days = int(*days)
	}
	if expires != nil {
		link.ExpiresAt = expires.UTC()
	}
	if viewed != nil {
		link.LastViewedAt = viewed.UTC()
	}
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func nullInt(n int) *int32 {
	if n <= 0 {
		return nil
	}
	v := int32(n)
	return &v
}
