-- +goose Up

-- The fact table. Append-only, one row per playback event per user.
--
-- played_at is always normalised to the START of playback in UTC, whatever the
-- source reported, because that is the only anchor the three ingestion paths can
-- agree on:
--   sync          played_at from /me/player/recently-played
--   extended      ts - ms_played          (ts is the stream end time)
--   account data  endTime - msPlayed      (endTime is minute precision)
CREATE TABLE listens (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    played_at      timestamptz NOT NULL,
    -- 0 = millisecond, 1 = second, 2 = minute. Drives the width of the
    -- cross-source duplicate window.
    ts_precision   smallint    NOT NULL DEFAULT 1,

    -- Set once the listen is anchored to a catalogue track. NULL means the row
    -- came from a names-only export and is awaiting alias resolution.
    track_id       text        REFERENCES tracks (id) ON DELETE SET NULL,
    alias_artist   text,
    alias_title    text,

    -- sha256("t:"||track_id) when resolved, else sha256("n:"||artist||0x00||title).
    identity_key   bytea       NOT NULL,
    -- sha256(user_id || identity_key || floor(unix(played_at)/60)).
    dedupe_key     bytea       NOT NULL,

    ms_played      integer     NOT NULL CHECK (ms_played >= 0),
    -- 0 = sync, 1 = account data, 2 = extended.
    source         smallint    NOT NULL,

    -- Nulled rather than cascaded when an import job is deleted: the listening
    -- data is the user's, the job record is only bookkeeping.
    import_file_id uuid        REFERENCES import_files (id) ON DELETE SET NULL,

    -- Playback context, present only in extended streaming history. NULL means
    -- "not reported", which is deliberately distinct from false.
    platform     text,
    conn_country text,
    reason_start text,
    reason_end   text,
    shuffle      boolean,
    skipped      boolean,
    offline      boolean,
    incognito    boolean,

    created_at   timestamptz NOT NULL DEFAULT now(),

    -- A row must be identifiable one way or the other.
    CONSTRAINT listens_identity_ck CHECK (
        track_id IS NOT NULL OR (alias_artist IS NOT NULL AND alias_title IS NOT NULL)
    ),
    -- The exact duplicate rule, enforced by the database rather than by
    -- application logic. Re-importing a file inserts zero rows.
    CONSTRAINT listens_dedupe_uk UNIQUE (user_id, dedupe_key)
);

-- Every range-filtered query starts here. INCLUDE makes the timeline and
-- listening-time aggregations index-only scans, which is what keeps a decade of
-- history responsive.
CREATE INDEX listens_user_played_idx
    ON listens (user_id, played_at) INCLUDE (ms_played, track_id);

-- Track detail pages, "top tracks in range", and the per-track first/last listen.
CREATE INDEX listens_user_track_played_idx
    ON listens (user_id, track_id, played_at) WHERE track_id IS NOT NULL;

-- The cross-source duplicate probe: "does this user already have a listen with
-- this identity within +/- 60s of this instant?"
CREATE INDEX listens_user_identity_played_idx
    ON listens (user_id, identity_key, played_at);

-- Post-import verification counts committed rows per file, and deleting a job's
-- rows on demand uses the same index.
CREATE INDEX listens_import_file_idx
    ON listens (import_file_id) WHERE import_file_id IS NOT NULL;

-- The relink pass finds every unresolved listen for an alias that just resolved.
-- Deliberately not user-scoped: one alias resolution repairs all users at once.
CREATE INDEX listens_unresolved_identity_idx
    ON listens (identity_key) WHERE track_id IS NULL;

-- Artists a user has excluded from all statistics.
CREATE TABLE user_blacklisted_artists (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    artist_id  text        NOT NULL REFERENCES artists (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, artist_id)
);

-- +goose Down
DROP TABLE user_blacklisted_artists;
DROP TABLE listens;
