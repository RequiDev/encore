package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// SharedEntry is one entity both users listened to in the range, with each side's
// play count.
type SharedEntry struct {
	ID     string
	PlaysA int64
	PlaysB int64
}

// Affinity is how much two users of the same instance have in common over a
// range.
type Affinity struct {
	// Score is the cosine similarity of the two users' artist play vectors, from
	// 0 (nothing in common) to 1 (identical proportions). Cosine is used rather
	// than a raw overlap count because it is unaffected by one user simply
	// listening more than the other, which is the property a "you two match 74%"
	// figure needs.
	Score             float64
	ArtistsA          int64
	ArtistsB          int64
	SharedArtistCount int64
	SharedArtists     []SharedEntry
	SharedAlbums      []SharedEntry
	SharedTracks      []SharedEntry
}

// Each user's own blacklist applies to their own side of the comparison, which
// falls out of the blacklist fragment joining on the listen's user_id.
func affinitySharedSQL(kind topKind) string {
	return fmt.Sprintf(`
WITH ua AS (%s),
     ub AS (%s)
SELECT ua.id, ua.plays, ub.plays
FROM ua JOIN ub ON ub.id = ua.id
ORDER BY least(ua.plays, ub.plays) DESC, (ua.plays + ub.plays) DESC, ua.id
LIMIT $5`,
		topSourceSQL(kind, "$1", "$2", "$3", "", false),
		topSourceSQL(kind, "$4", "$2", "$3", "", false))
}

var (
	sharedArtistsSQL = affinitySharedSQL(topArtists)
	sharedAlbumsSQL  = affinitySharedSQL(topAlbums)
	sharedTracksSQL  = affinitySharedSQL(topTracks)

	affinityScoreSQL = fmt.Sprintf(`
WITH ua AS (%s),
     ub AS (%s),
     j AS (SELECT ua.plays AS pa, ub.plays AS pb FROM ua JOIN ub ON ub.id = ua.id)
SELECT
    (SELECT count(*) FROM ua)::bigint,
    (SELECT count(*) FROM ub)::bigint,
    (SELECT count(*) FROM j)::bigint,
    coalesce((SELECT sum(pa::float8 * pb) FROM j), 0)::float8,
    coalesce((SELECT sqrt(sum(plays::float8 * plays)) FROM ua), 0)::float8,
    coalesce((SELECT sqrt(sum(plays::float8 * plays)) FROM ub), 0)::float8`,
		topSourceSQL(topArtists, "$1", "$2", "$3", "", false),
		topSourceSQL(topArtists, "$4", "$2", "$3", "", false))
)

// Affinity compares two users over the same range: what they share, and how
// similar their listening is overall.
//
// limit bounds each of the three shared lists; the score is computed over the
// whole artist vectors regardless of it.
func (s *Service) Affinity(ctx context.Context, q store.Querier, userIDA, userIDB uuid.UUID, r domain.TimeRange, limit int) (Affinity, error) {
	if err := checkScope(userIDA, r); err != nil {
		return Affinity{}, err
	}
	if userIDB == uuid.Nil {
		return Affinity{}, fmt.Errorf("%w: a second user is required", domain.ErrValidation)
	}
	if userIDA == userIDB {
		return Affinity{}, fmt.Errorf("%w: a user cannot be compared with themselves", domain.ErrValidation)
	}
	limit = clampLimit(limit)

	var dot, normA, normB float64
	out := Affinity{}
	err := q.QueryRow(ctx, affinityScoreSQL,
		store.UUIDArg(userIDA), r.From.UTC(), r.To.UTC(), store.UUIDArg(userIDB)).
		Scan(&out.ArtistsA, &out.ArtistsB, &out.SharedArtistCount, &dot, &normA, &normB)
	if err != nil {
		return Affinity{}, postgres.Classify("affinity score", err)
	}
	if normA > 0 && normB > 0 {
		out.Score = dot / (normA * normB)
	}

	if out.SharedArtists, err = s.shared(ctx, q, sharedArtistsSQL, "shared artists", userIDA, userIDB, r, limit); err != nil {
		return Affinity{}, err
	}
	if out.SharedAlbums, err = s.shared(ctx, q, sharedAlbumsSQL, "shared albums", userIDA, userIDB, r, limit); err != nil {
		return Affinity{}, err
	}
	if out.SharedTracks, err = s.shared(ctx, q, sharedTracksSQL, "shared tracks", userIDA, userIDB, r, limit); err != nil {
		return Affinity{}, err
	}
	return out, nil
}

func (s *Service) shared(ctx context.Context, q store.Querier, sql, op string, userIDA, userIDB uuid.UUID, r domain.TimeRange, limit int) ([]SharedEntry, error) {
	rows, err := q.Query(ctx, sql,
		store.UUIDArg(userIDA), r.From.UTC(), r.To.UTC(), store.UUIDArg(userIDB), limit)
	if err != nil {
		return nil, postgres.Classify(op, err)
	}
	defer rows.Close()

	var out []SharedEntry
	for rows.Next() {
		var e SharedEntry
		if err := rows.Scan(&e.ID, &e.PlaysA, &e.PlaysB); err != nil {
			return nil, postgres.Classify("scan "+op, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify(op, err)
	}
	return out, nil
}
