// Package postgres owns the database connection pool, migration execution and
// the classification of database errors into Encore's transient/permanent
// taxonomy. It is the only package that imports pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/domain"
)

// Pool is the shared connection pool. Its size is the backpressure valve for the
// importer: when every connection is in use, the batch flush blocks and the file
// reader stops pulling records, so an unbounded in-memory queue can never form.
type Pool = pgxpool.Pool

// Connect opens and verifies a pool.
func Connect(ctx context.Context, cfg config.Database, lg *slog.Logger) (*Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// The DSN may embed a password; never echo it back.
		return nil, fmt.Errorf("parse ENCORE_DATABASE_URL: %w", redactErr(err))
	}
	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = time.Hour
	pcfg.MaxConnIdleTime = 30 * time.Minute
	pcfg.HealthCheckPeriod = time.Minute
	pcfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	if cfg.StatementTimeout > 0 {
		if pcfg.ConnConfig.RuntimeParams == nil {
			pcfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		ms := cfg.StatementTimeout.Milliseconds()
		pcfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", ms)
	}
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["application_name"] = "encore"

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", redactErr(err))
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout+5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", redactErr(err))
	}
	if lg != nil {
		lg.Info("database connected", "max_conns", cfg.MaxConns, "min_conns", cfg.MinConns)
	}
	return pool, nil
}

// redactErr strips anything password-shaped out of a connection error so that a
// bad DSN does not put credentials into logs or API responses.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(scrub(err.Error()))
}

// Health reports whether the database answers a trivial query.
func Health(ctx context.Context, pool *Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return redactErr(err)
	}
	return nil
}

// Stats exposes pool utilisation for /metrics and readiness diagnostics.
type Stats struct {
	TotalConns    int32 `json:"total_conns"`
	IdleConns     int32 `json:"idle_conns"`
	AcquiredConns int32 `json:"acquired_conns"`
	MaxConns      int32 `json:"max_conns"`
}

// PoolStats snapshots the pool.
func PoolStats(pool *Pool) Stats {
	s := pool.Stat()
	return Stats{
		TotalConns:    s.TotalConns(),
		IdleConns:     s.IdleConns(),
		AcquiredConns: s.AcquiredConns(),
		MaxConns:      s.MaxConns(),
	}
}

// --- error classification --------------------------------------------------

// Postgres error classes Encore treats as worth retrying.
const (
	classConnectionException   = "08"
	classOperatorIntervention  = "57"
	classInsufficientResources = "53"
	classSystemError           = "58"
	codeSerializationFailure   = "40001"
	codeDeadlockDetected       = "40P01"
	codeLockNotAvailable       = "55P03"
	codeCannotConnectNow       = "57P03"
	codeUniqueViolation        = "23505"
	codeForeignKeyViolation    = "23503"
	codeCheckViolation         = "23514"
	codeNotNullViolation       = "23502"
	codeQueryCanceled          = "57014"
)

// Classify turns a database error into Encore's taxonomy so that callers do not
// have to know Postgres error codes.
//
//   - a dropped connection, deadlock, serialisation failure or admin shutdown is
//     transient: the same statement may well succeed on the next attempt;
//   - a constraint violation is permanent: retrying cannot help;
//   - no rows is domain.ErrNotFound.
//
// This is what lets the importer distinguish "the database went away, back off
// and resume from the checkpoint" from "this record can never be stored".
func Classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	// A cancelled context is the caller's decision, not a database failure.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", op, err)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeUniqueViolation:
			return fmt.Errorf("%s: %w", op, domain.ErrConflict)
		case codeForeignKeyViolation, codeCheckViolation, codeNotNullViolation:
			return fmt.Errorf("%s: %w: %s", op, domain.ErrValidation, pgErr.Message)
		case codeSerializationFailure, codeDeadlockDetected, codeLockNotAvailable, codeCannotConnectNow, codeQueryCanceled:
			return domain.Transient(op, errors.New(scrub(pgErr.Message)))
		}
		switch pgErr.Code[:2] {
		case classConnectionException, classOperatorIntervention, classInsufficientResources, classSystemError:
			return domain.Transient(op, errors.New(scrub(pgErr.Message)))
		}
		return fmt.Errorf("%s: %s (SQLSTATE %s)", op, scrub(pgErr.Message), pgErr.Code)
	}

	// Not a server-side error: a broken pipe, a closed pool, a DNS failure. These
	// are exactly the conditions a resumable importer must survive.
	if isNetworkish(err) {
		return domain.Transient(op, redactErr(err))
	}
	return fmt.Errorf("%s: %w", op, redactErr(err))
}

// IsUniqueViolation reports whether err is a duplicate-key error, which the
// listen insert path treats as "already have it" rather than as a failure.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == codeUniqueViolation
}

func isNetworkish(err error) bool {
	msg := err.Error()
	for _, s := range []string{
		"connection refused", "connection reset", "broken pipe", "closed pool",
		"conn closed", "EOF", "i/o timeout", "no such host", "unexpected EOF",
		"failed to connect", "server closed the connection", "connection closed",
	} {
		if containsFold(msg, s) {
			return true
		}
	}
	return false
}

func containsFold(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	hl, nl := len(haystack), len(needle)
	for i := 0; i+nl <= hl; i++ {
		match := true
		for j := range nl {
			a, b := haystack[i+j], needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// scrub removes anything that looks like a credential from a message that may be
// shown to a user or written to a log.
func scrub(s string) string {
	out := []byte(s)
	// Rewrite "://user:secret@host" as "://user:xxxxx@host".
	for i := 0; i+3 < len(out); i++ {
		if out[i] == ':' && out[i+1] == '/' && out[i+2] == '/' {
			at := -1
			for j := i + 3; j < len(out); j++ {
				if out[j] == '@' {
					at = j
					break
				}
				if out[j] == ' ' || out[j] == '/' {
					break
				}
			}
			if at < 0 {
				continue
			}
			colon := -1
			for j := i + 3; j < at; j++ {
				if out[j] == ':' {
					colon = j
					break
				}
			}
			if colon < 0 {
				continue
			}
			replaced := append([]byte{}, out[:colon+1]...)
			replaced = append(replaced, []byte("xxxxx")...)
			replaced = append(replaced, out[at:]...)
			out = replaced
		}
	}
	return string(out)
}
