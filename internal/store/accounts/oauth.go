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

// OAuthStates is the repository for the oauth_states table: the short-lived PKCE
// state of an authorisation request that has left for Spotify but not yet come
// back.
//
// As with sessions, only the hash of the state parameter is stored, and the code
// verifier is sealed, so the table gives away neither the state an attacker
// would have to forge nor the verifier they would have to present.
type OAuthStates struct{ db *store.Store }

// NewOAuthStates builds the repository.
func NewOAuthStates(db *store.Store) *OAuthStates { return &OAuthStates{db: db} }

// Create records an authorisation request that is about to be started.
//
// linkUserID is set when the flow re-links an already signed-in user rather than
// signing someone in, which is what lets the callback refuse an identity swap.
func (r *OAuthStates) Create(ctx context.Context, q store.Querier, stateHash []byte, codeVerifier, redirectTo string, linkUserID *uuid.UUID, expiresAt time.Time) error {
	if len(stateHash) == 0 {
		return fmt.Errorf("%w: oauth state hash is required", domain.ErrValidation)
	}
	if codeVerifier == "" {
		return fmt.Errorf("%w: pkce code verifier is required", domain.ErrValidation)
	}

	verifierEnc, err := r.db.Seal(codeVerifier)
	if err != nil {
		return err
	}

	const sql = `
        INSERT INTO oauth_states (state_hash, code_verifier_enc, redirect_to, link_user_id, expires_at)
        VALUES ($1, $2, $3, $4, $5)`
	if _, err := q.Exec(ctx, sql,
		stateHash, verifierEnc, redirectTo, store.NullUUIDArg(linkUserID), expiresAt.UTC()); err != nil {
		return postgres.Classify("create oauth state", err)
	}
	return nil
}

// consumeOAuthStateSQL deletes and returns the row in one statement.
//
// DELETE ... RETURNING is what makes the state single-use: the database, not the
// application, decides which of two concurrent callbacks gets the row, so a
// replayed state cannot be exchanged twice. Expired rows are deleted too and
// reported as missing, so a stale state is spent rather than left lying around
// for the reaper.
const consumeOAuthStateSQL = `
DELETE FROM oauth_states
WHERE state_hash = $1
RETURNING code_verifier_enc, redirect_to, link_user_id, expires_at > now()`

// Consume exchanges a state parameter for the PKCE verifier that goes with it,
// destroying it in the process. It returns domain.ErrNotFound for an unknown,
// already used or expired state.
func (r *OAuthStates) Consume(ctx context.Context, q store.Querier, stateHash []byte) (codeVerifier string, redirectTo string, linkUserID *uuid.UUID, err error) {
	if len(stateHash) == 0 {
		return "", "", nil, fmt.Errorf("%w: oauth state hash is required", domain.ErrValidation)
	}

	var (
		verifierEnc []byte
		linkID      *string
		live        bool
	)
	if err := q.QueryRow(ctx, consumeOAuthStateSQL, stateHash).
		Scan(&verifierEnc, &redirectTo, &linkID, &live); err != nil {
		return "", "", nil, postgres.Classify("consume oauth state", err)
	}
	if !live {
		return "", "", nil, fmt.Errorf("consume oauth state: %w", domain.ErrNotFound)
	}

	verifier, err := r.db.Open(verifierEnc)
	if err != nil {
		return "", "", nil, fmt.Errorf("open pkce verifier: %w", err)
	}
	if linkID != nil {
		id, perr := uuid.Parse(*linkID)
		if perr != nil {
			return "", "", nil, fmt.Errorf("consume oauth state: link user id is not a uuid: %w", perr)
		}
		linkUserID = &id
	}
	return verifier, redirectTo, linkUserID, nil
}

// DeleteExpired reaps authorisation requests that were never completed and
// reports how many were removed.
func (r *OAuthStates) DeleteExpired(ctx context.Context, q store.Querier, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM oauth_states WHERE expires_at <= $1`, now.UTC())
	if err != nil {
		return 0, postgres.Classify("delete expired oauth states", err)
	}
	return tag.RowsAffected(), nil
}
