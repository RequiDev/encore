-- +goose Up

-- Playlists Encore has created on Spotify, and the definitions that produced
-- them.
--
-- The definition is stored so that "rebuild" means re-running the same question
-- rather than asking the owner to describe it again. Nothing runs it on a
-- schedule: a playlist that changed under its owner, or silently overwrote an
-- edit they made in Spotify, would be worse than one that is merely out of date.
CREATE TABLE playlists (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name        text        NOT NULL,

    -- The playlist on Spotify. Unique per user rather than globally: two
    -- accounts on one instance could in principle be the same Spotify listener.
    spotify_id  text        NOT NULL,
    spotify_url text        NOT NULL DEFAULT '',

    mode        text        NOT NULL
                            CHECK (mode IN ('top', 'min_plays', 'discoveries', 'forgotten')),
    sort        text        NOT NULL CHECK (sort IN ('plays', 'time')),
    track_limit integer     NOT NULL CHECK (track_limit BETWEEN 1 AND 500),
    min_plays   integer     NOT NULL DEFAULT 0 CHECK (min_plays >= 0),
    range_from  timestamptz,
    range_to    timestamptz,
    CONSTRAINT playlists_range_is_whole
        CHECK ((range_from IS NULL) = (range_to IS NULL)),

    track_count integer     NOT NULL DEFAULT 0,
    built_at    timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),

    UNIQUE (user_id, spotify_id)
);

CREATE INDEX playlists_user_idx ON playlists (user_id, created_at DESC);

-- +goose Down
DROP TABLE playlists;
