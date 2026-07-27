package imports

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// MaxRawExcerptBytes bounds the copy of an offending record kept for diagnostics.
//
// The importer caps how many rejects it records per file; this caps how large
// each one may be, so that a file of pathologically long records cannot fill the
// disk through the diagnostics table.
const MaxRawExcerptBytes = 2048

// Reject is one record that can never be imported, kept with enough context for
// a user to understand what was wrong without re-opening a multi-gigabyte export.
type Reject struct {
	ID     int64
	FileID uuid.UUID
	// RecordIndex is the record's position in the file, counted from zero, so it
	// lines up with the checkpoint's record offset.
	RecordIndex int64
	Reason      domain.RejectReason
	Detail      string
	RawExcerpt  string
	CreatedAt   time.Time
}

// truncateExcerpt bounds an excerpt without splitting a UTF-8 sequence, since a
// half-written rune would be rejected by the database as invalid text.
func truncateExcerpt(s string) string {
	if len(s) <= MaxRawExcerptBytes {
		return s
	}
	cut := MaxRawExcerptBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

const addRejectsSQL = `
    INSERT INTO import_rejects (file_id, record_index, reason, detail, raw_excerpt)
    SELECT $1, t.record_index, t.reason, t.detail, t.raw_excerpt
    FROM unnest($2::bigint[], $3::text[], $4::text[], $5::text[])
        AS t(record_index, reason, detail, raw_excerpt)`

// AddRejects records a batch of unimportable records in one round trip.
//
// The importer calls this inside the same transaction as the batch the rejects
// came from, so the diagnostics and the checkpoint that accounts for them are
// committed together and a resumed import never reports the same bad record twice.
func (r *Repo) AddRejects(ctx context.Context, q store.Querier, fileID uuid.UUID, rejects []Reject) error {
	if len(rejects) == 0 {
		return nil
	}
	n := len(rejects)
	var (
		indexes  = make([]int64, n)
		reasons  = make([]string, n)
		details  = make([]string, n)
		excerpts = make([]string, n)
	)
	for i, rj := range rejects {
		indexes[i] = rj.RecordIndex
		reason := rj.Reason
		if reason == "" {
			// A reject with no reason would be useless to the user reading it.
			reason = domain.RejectMalformedRecord
		}
		reasons[i] = string(reason)
		details[i] = truncateExcerpt(rj.Detail)
		excerpts[i] = truncateExcerpt(rj.RawExcerpt)
	}

	if _, err := q.Exec(ctx, addRejectsSQL, store.UUIDArg(fileID), indexes, reasons, details, excerpts); err != nil {
		return postgres.Classify("record import rejects", err)
	}
	return nil
}

const listRejectsSQL = `
    SELECT id, file_id, record_index, reason, detail, raw_excerpt, created_at
    FROM import_rejects
    WHERE file_id = $1
    ORDER BY record_index, id
    LIMIT $2 OFFSET $3`

// ListRejects returns one page of a file's rejected records, oldest position
// first, with the unpaged total.
func (r *Repo) ListRejects(ctx context.Context, q store.Querier, fileID uuid.UUID, limit, offset int) ([]Reject, int64, error) {
	limit, offset = clampPage(limit, offset)

	total, err := r.CountRejects(ctx, q, fileID)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx, listRejectsSQL, store.UUIDArg(fileID), limit, offset)
	if err != nil {
		return nil, 0, postgres.Classify("list import rejects", err)
	}
	defer rows.Close()

	var out []Reject
	for rows.Next() {
		var (
			rj     Reject
			reason string
		)
		err := rows.Scan(&rj.ID, &rj.FileID, &rj.RecordIndex, &reason, &rj.Detail, &rj.RawExcerpt, &rj.CreatedAt)
		if err != nil {
			return nil, 0, postgres.Classify("scan import reject", err)
		}
		rj.Reason = domain.RejectReason(reason)
		out = append(out, rj)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, postgres.Classify("list import rejects", err)
	}
	return out, total, nil
}

// CountRejects returns how many records of a file were rejected. The importer
// uses it to enforce its per-file diagnostics cap.
func (r *Repo) CountRejects(ctx context.Context, q store.Querier, fileID uuid.UUID) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `SELECT count(*)::bigint FROM import_rejects WHERE file_id = $1`,
		store.UUIDArg(fileID)).Scan(&n)
	if err != nil {
		return 0, postgres.Classify("count import rejects", err)
	}
	return n, nil
}
