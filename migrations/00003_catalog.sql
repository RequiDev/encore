-- +goose Up

-- The catalogue is global rather than per-user: two users who both listen to the
-- same track share one row, so enrichment is done once for the whole instance.
--
-- Every catalogue table carries the same enrichment state machine:
--   pending -> resolved | unavailable | failed
-- Ingestion only ever writes 'pending'. It never calls the Spotify API, so an
-- outage or a rate limit cannot delay or lose a listening record.

CREATE TABLE artists (
    id              text        PRIMARY KEY,
    name            text        NOT NULL DEFAULT '',
    name_norm       text        NOT NULL DEFAULT '',
    genres          text[]      NOT NULL DEFAULT '{}',
    popularity      integer     NOT NULL DEFAULT 0,
    followers       bigint      NOT NULL DEFAULT 0,
    image_url       text        NOT NULL DEFAULT '',
    metadata_state  text        NOT NULL DEFAULT 'pending'
                                CHECK (metadata_state IN ('pending', 'resolved', 'unavailable', 'failed')),
    fetch_attempts  integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz,
    fetched_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE albums (
    id                text        PRIMARY KEY,
    name              text        NOT NULL DEFAULT '',
    name_norm         text        NOT NULL DEFAULT '',
    album_type        text        NOT NULL DEFAULT '',
    release_date      date,
    release_precision text        NOT NULL DEFAULT '',
    total_tracks      integer     NOT NULL DEFAULT 0,
    image_url         text        NOT NULL DEFAULT '',
    metadata_state    text        NOT NULL DEFAULT 'pending'
                                  CHECK (metadata_state IN ('pending', 'resolved', 'unavailable', 'failed')),
    fetch_attempts    integer     NOT NULL DEFAULT 0,
    next_attempt_at   timestamptz,
    fetched_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tracks (
    id              text        PRIMARY KEY,
    name            text        NOT NULL DEFAULT '',
    name_norm       text        NOT NULL DEFAULT '',
    album_id        text        REFERENCES albums (id) ON DELETE SET NULL,
    duration_ms     integer     NOT NULL DEFAULT 0,
    explicit        boolean     NOT NULL DEFAULT false,
    popularity      integer     NOT NULL DEFAULT 0,
    isrc            text        NOT NULL DEFAULT '',
    metadata_state  text        NOT NULL DEFAULT 'pending'
                                CHECK (metadata_state IN ('pending', 'resolved', 'unavailable', 'failed')),
    fetch_attempts  integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz,
    fetched_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE track_artists (
    track_id  text    NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    artist_id text    NOT NULL REFERENCES artists (id) ON DELETE CASCADE,
    position  integer NOT NULL DEFAULT 0,
    PRIMARY KEY (track_id, artist_id)
);

CREATE TABLE album_artists (
    album_id  text    NOT NULL REFERENCES albums (id) ON DELETE CASCADE,
    artist_id text    NOT NULL REFERENCES artists (id) ON DELETE CASCADE,
    position  integer NOT NULL DEFAULT 0,
    PRIMARY KEY (album_id, artist_id)
);

-- Enrichment queue scans. Partial indexes keep them proportional to the work
-- outstanding rather than to the size of the catalogue, which matters once a
-- large history has resolved and 99% of rows are permanently 'resolved'.
CREATE INDEX artists_fetch_queue_idx ON artists (next_attempt_at NULLS FIRST, id)
    WHERE metadata_state IN ('pending', 'failed');
CREATE INDEX albums_fetch_queue_idx ON albums (next_attempt_at NULLS FIRST, id)
    WHERE metadata_state IN ('pending', 'failed');
CREATE INDEX tracks_fetch_queue_idx ON tracks (next_attempt_at NULLS FIRST, id)
    WHERE metadata_state IN ('pending', 'failed');

-- Album detail pages and "top albums" resolve tracks through their album, and
-- artist pages walk artist -> tracks.
CREATE INDEX tracks_album_idx ON tracks (album_id) WHERE album_id IS NOT NULL;
CREATE INDEX track_artists_artist_idx ON track_artists (artist_id, track_id);
CREATE INDEX album_artists_artist_idx ON album_artists (artist_id, album_id);

-- Free-text search over the catalogue for the in-app search box.
CREATE INDEX artists_name_norm_idx ON artists (name_norm text_pattern_ops);
CREATE INDEX albums_name_norm_idx ON albums (name_norm text_pattern_ops);
CREATE INDEX tracks_name_norm_idx ON tracks (name_norm text_pattern_ops);

-- Maps a normalised (artist, title) pair from a names-only account-data export
-- onto a real catalogue track. This is the bridge that lets an account-data
-- import and an extended import of the same period converge on one identity;
-- see docs/import.md, "Layer 3 - relink reconciliation".
CREATE TABLE track_aliases (
    artist_norm     text        NOT NULL,
    title_norm      text        NOT NULL,
    track_id        text        REFERENCES tracks (id) ON DELETE SET NULL,
    state           text        NOT NULL DEFAULT 'pending'
                                CHECK (state IN ('pending', 'resolved', 'unavailable', 'failed')),
    fetch_attempts  integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz,
    resolved_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (artist_norm, title_norm)
);

CREATE INDEX track_aliases_queue_idx ON track_aliases (next_attempt_at NULLS FIRST)
    WHERE state IN ('pending', 'failed');
CREATE INDEX track_aliases_track_idx ON track_aliases (track_id) WHERE track_id IS NOT NULL;

-- +goose Down
DROP TABLE track_aliases;
DROP TABLE album_artists;
DROP TABLE track_artists;
DROP TABLE tracks;
DROP TABLE albums;
DROP TABLE artists;
