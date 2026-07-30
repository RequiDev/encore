-- +goose Up

-- What the listener was playing from, when Encore recorded the play live.
--
-- Both nullable with no default, so this is a metadata-only ALTER in
-- PostgreSQL — instant regardless of how many rows `listens` already holds,
-- exactly like the extended-export columns 00005 added the same way.
--
-- NULL means "not reported", which is deliberately distinct from "played from
-- nothing" — and it is the ordinary case, because NO export format carries
-- context at all: an imported row (source = 1 or 2) can never have one. Only
-- a row this instance synced live (source = 0) can, and even then only when
-- Spotify's /me/player/recently-played happened to include it.
ALTER TABLE listens ADD COLUMN context_type text;
ALTER TABLE listens ADD COLUMN context_id   text;

-- Deliberately no index on listens (context_id) or (context_type, context_id).
--
-- The statistic this feeds groups by context within one user's range:
--   SELECT context_type, context_id, count(*) FROM listens
--   WHERE user_id = $1 AND played_at >= $2 AND played_at < $3
--     AND context_type IS NOT NULL
--   GROUP BY 1, 2
-- listens_user_played_idx already leads on (user_id, played_at), so it scopes
-- the WHERE clause to one user's range exactly as it does for every other
-- range-filtered query on this table; what it does not do is cover
-- context_type/context_id, so the scan fetches those two columns from the
-- heap for each row the range already narrowed it to, rather than answering
-- index-only.
--
-- That gap is not new: internal/stats/context.go groups this same range by
-- platform, conn_country and reason_end today, three more columns absent from
-- that INCLUDE list, with no dedicated index of their own — see its
-- module comment. Those queries have shipped and perform adequately because
-- the row count a heap fetch touches is bounded by the range, not by the
-- table: a user's month is at most a few thousand rows regardless of how many
-- million the table holds. context_type/context_id would cost the same and
-- gain the same, so singling them out for an index the sibling columns do not
-- have would be inconsistent as well as unearned.
--
-- Extending listens_user_played_idx's own INCLUDE list was also considered
-- and rejected: that index is on the hottest write path in the project — one
-- entry per listen, sync or import — and context_id is NULL for the large
-- majority of rows forever (see above), so widening it would grow every
-- index entry, and the WAL and vacuum cost that comes with it, to save a heap
-- fetch that is already cheap relative to the range. A future task adding a
-- blacklist join to this same query (excluding rows by artist) would need the
-- heap regardless, since blacklist membership is not carried in any index
-- either — so paying for a wider covering index would not even remove the
-- heap access from the plan, just move it.
--
-- If this statistic turns out to matter at ranges wide enough that heap
-- fetches show up in practice — a lifetime range on an account with a decade
-- of live-synced history — the fix is a partial index on
-- (user_id, played_at) WHERE context_type IS NOT NULL, not a plain one: it
-- would only ever index the minority of rows that can carry context in the
-- first place. That is a decision for whoever has the query plan showing it
-- is needed, not a speculative one made here before the statistic is even
-- built.

-- The listener's own playlists, so a context_id can be named.
--
-- Enumerated on the same daily worker tick as the library and top snapshots,
-- and reconciled the same way: a full listing replaces what is stored,
-- because Spotify has no delta endpoint here either. See migrations/00010's
-- and 00011's own comments for why that shape (delete-absent, no history)
-- applies to every one of these enumerations, not just this one.
CREATE TABLE user_playlists (
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    playlist_id  text        NOT NULL,
    name         text        NOT NULL DEFAULT '',
    owner_id     text        NOT NULL DEFAULT '',
    total_tracks integer     NOT NULL DEFAULT 0,
    -- Spotify's opaque version marker. Stored so a later phase can tell
    -- whether a playlist changed without re-reading its tracks; nothing uses
    -- it yet.
    snapshot_id  text        NOT NULL DEFAULT '',
    fetched_at   timestamptz NOT NULL,
    PRIMARY KEY (user_id, playlist_id)
);

-- No secondary index here either. A context_id joins back to exactly one
-- (user_id, playlist_id) pair, which the primary key already is — the same
-- reasoning 00011 gives for spotify_top_snapshots needing no index beyond its
-- own primary key.

-- +goose Down
DROP TABLE user_playlists;
ALTER TABLE listens DROP COLUMN context_id;
ALTER TABLE listens DROP COLUMN context_type;
