-- +goose Up

-- Per-user, per-local-day, per-track aggregate. Wide-range "top tracks" and
-- "top artists" queries read this instead of scanning the fact table.
--
-- The day is the LOCAL day in the user's timezone at the moment the rollup was
-- computed, so changing your timezone marks your whole history dirty and it is
-- recomputed in the background.
CREATE TABLE listen_daily_rollup (
    user_id  uuid    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    day      date    NOT NULL,
    track_id text    NOT NULL,
    plays    integer NOT NULL,
    ms       bigint  NOT NULL,
    PRIMARY KEY (user_id, day, track_id)
);

CREATE INDEX listen_daily_rollup_day_idx ON listen_daily_rollup (user_id, day);

-- Days whose rollup no longer matches the fact table. Written in the same
-- transaction as the listens that dirtied them, so the marker can never be lost
-- while the rows survive.
--
-- Statistics queries check this table first: if any day in the requested range
-- is dirty they read the fact table directly. That is slower but always correct,
-- which is the property worth having.
CREATE TABLE rollup_dirty_days (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    day        date        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, day)
);

CREATE INDEX rollup_dirty_days_created_idx ON rollup_dirty_days (created_at);

-- +goose Down
DROP TABLE rollup_dirty_days;
DROP TABLE listen_daily_rollup;
