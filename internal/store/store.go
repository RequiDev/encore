// Package store holds the database core: the connection pool, transaction
// helpers, encrypted-column support and the small set of helpers its
// sub-packages share.
//
// The repositories themselves live in sub-packages, one per area of
// responsibility (accounts, catalog, listens, imports, stats). Each takes a
// *Store and receives an explicit Querier per call, so exactly the same code
// runs inside and outside a transaction.
//
// Errors are translated through postgres.Classify, so callers see
// domain.ErrNotFound, domain.ErrConflict or a domain.TransientError rather than
// SQLSTATE codes.
package store

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RequiDev/encore/internal/crypto"
	"github.com/RequiDev/encore/internal/postgres"
)

// Querier is the subset of pgx shared by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store holds the pool and the secret material needed for encrypted columns.
type Store struct {
	pool   *pgxpool.Pool
	sealer *crypto.Sealer
}

// New builds a Store. The sealer is required: Spotify tokens are never written
// in plaintext.
func New(pool *pgxpool.Pool, sealer *crypto.Sealer) (*Store, error) {
	if pool == nil {
		return nil, errors.New("store: pool is required")
	}
	if sealer == nil {
		return nil, errors.New("store: sealer is required")
	}
	return &Store{pool: pool, sealer: sealer}, nil
}

// Pool exposes the underlying pool for health checks and metrics.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// DB returns the pool as a Querier, for statements that need no transaction.
func (s *Store) DB() Querier { return s.pool }

// InTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic.
//
// The importer depends on the atomicity this provides: a batch of listens and
// the checkpoint describing it are written in one call, so a crash can never
// leave a checkpoint claiming more progress than was actually committed.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) (err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return postgres.Classify("begin transaction", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			// Roll back even when ctx is already cancelled, otherwise the
			// connection returns to the pool mid-transaction.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err = fn(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return postgres.Classify("commit transaction", err)
	}
	return nil
}

// InTxSerializable is InTx with SERIALIZABLE isolation, for read-modify-write
// sequences that must not interleave. Callers must be prepared to retry:
// serialisation failures surface as domain.TransientError.
func (s *Store) InTxSerializable(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return postgres.Classify("begin serializable transaction", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return postgres.Classify("commit serializable transaction", err)
	}
	return nil
}

// Seal encrypts a secret for storage with AES-256-GCM.
func (s *Store) Seal(v string) ([]byte, error) {
	b, err := s.sealer.Seal(v)
	if err != nil {
		return nil, fmt.Errorf("seal secret: %w", err)
	}
	return b, nil
}

// Open decrypts a stored secret. A failure here almost always means
// ENCORE_ENCRYPTION_KEY changed; the message says so without echoing ciphertext.
func (s *Store) Open(b []byte) (string, error) {
	v, err := s.sealer.Open(b)
	if err != nil {
		return "", fmt.Errorf("stored secret could not be decrypted; check ENCORE_ENCRYPTION_KEY: %w", err)
	}
	return v, nil
}

// --- shared helpers --------------------------------------------------------

// Ptr returns a pointer to v, for building nullable query arguments.
func Ptr[T any](v T) *T { return &v }

// Nullable maps Go's zero value onto SQL NULL.
func Nullable[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

// Deref returns the pointed-to value, or the zero value for nil.
func Deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// UUIDArg renders a UUID for a query. UUIDs are passed as strings throughout so
// that pgx never has to guess how to encode github.com/google/uuid values.
func UUIDArg(id uuid.UUID) string { return id.String() }

// UUIDArgs renders a slice of UUIDs.
func UUIDArgs(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// NullUUIDArg renders an optional UUID.
func NullUUIDArg(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	return Ptr(id.String())
}

// Truncate bounds a string that is about to be stored in a text column,
// cutting on a rune boundary so the result is still valid UTF-8.
//
// Cutting at a byte offset instead can slice through a multi-byte rune and
// hand Postgres bytes it rejects outright ("invalid byte sequence for
// encoding \"UTF8\""). For a column that only ever records an error message,
// that failure is worse than the one it was trying to store: the write that
// was supposed to report "the fetch failed" itself fails, so nothing durable
// records the failure at all — any lease the caller took stays wherever it
// was left, to be picked up again once it expires and to fail the same write
// the same way, forever.
func Truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
