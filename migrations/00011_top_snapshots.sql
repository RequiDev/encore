-- +goose Up

-- Spotify's own ranking of a listener's top artists and tracks, as of the last
-- capture.
--
-- Only the latest capture per (user, kind, time_range) is kept: a refresh
-- replaces the whole set in one transaction. Retaining history would enable a
-- "how Spotify's view of you drifted" view and is deliberately not built — but
-- the primary key is shaped so adding captured_at to it later is a migration
-- rather than a rewrite.
--
-- Not foreign-keyed to the catalogue, for the same reason the library tables are
-- not: Spotify can rank an entity this instance has never seen, and enrichment
-- mints it afterwards.
CREATE TABLE spotify_top_snapshots (
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind        text        NOT NULL CHECK (kind IN ('artist', 'track')),
    time_range  text        NOT NULL CHECK (time_range IN ('short_term', 'medium_term', 'long_term')),
    -- Spotify's own rank, 1-based, as returned.
    position    integer     NOT NULL CHECK (position > 0),
    entity_id   text        NOT NULL,
    -- When this capture was taken. Identical across one (user, kind, range) set,
    -- because a refresh writes the whole set at one instant; carried per row so
    -- reading one set needs no join.
    captured_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, kind, time_range, position)
);

-- No secondary index here. The diff reads one whole set at a time — an
-- equality match on (user_id, kind, time_range) plus an ordered scan on
-- position — and a primary key in PostgreSQL *is* a unique btree on exactly
-- those columns in that order. A second index with the same column list
-- would carry the write and disk cost of every insert and vacuum without
-- serving a query the PK's own btree doesn't already answer.

-- +goose Down
DROP TABLE spotify_top_snapshots;
