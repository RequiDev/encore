-- +goose Up

-- What the now-playing poller saw, kept only long enough for the
-- recently-played feed to catch up with it.
--
-- A log, not a latest. 00017's now_playing is keyed (user_id) and overwritten
-- every tick because a live card needs one current row; this is keyed
-- (user_id, track_id, observed_at) and appended to, because a fuzzy temporal
-- join needs history. One table cannot be both: a log has no "current row" and
-- a latest has no evidence to join against — it overwrites its own.
--
-- It exists because /me/player/recently-played reports what was played and
-- when, and nothing about how. Only an extended export fills shuffle and the
-- playback context columns, so a live-synced listen is permanently thinner than
-- an imported one covering the same evening. The poller sees exactly what is
-- missing, one tick at a time, and this is where it is written down until
-- internal/sync can attach it to the play it belongs to.
CREATE TABLE playback_observations (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- The Spotify catalogue id, and the join key.
    --
    -- Deliberately NOT a foreign key to tracks, for 00017's reason: Spotify
    -- names a track the moment it starts playing and Encore's catalogue learns
    -- of it only when enrichment gets round to it. A reference would fail the
    -- write for exactly the listener playing something new. The join in
    -- internal/store/listens/backfill.go matches this against listens.track_id,
    -- which is itself a foreign key, so an id naming nothing simply never
    -- matches.
    track_id text NOT NULL,

    -- When Encore looked. Not when the play started: this is a point sample of
    -- a player, and the backfill's window is what turns a point into a claim
    -- about a play.
    observed_at timestamptz NOT NULL,

    -- Spotify Connect's own device vocabulary -- 'Computer', 'Smartphone',
    -- 'Speaker', 'CastAudio' -- and NOT listens.platform's, which holds an
    -- export's free text such as 'Android OS 10 API 29 (samsung, SM-G970F)'.
    -- The two are different vocabularies for different questions and are
    -- deliberately never mixed: writing 'Smartphone' into platform would make
    -- every historical platform figure change meaning without changing shape.
    --
    -- No CHECK on its values. Spotify mints this set and can extend it without
    -- warning, which is exactly the judgement 00014 made for
    -- artist_albums.album_group and the opposite of the one 00016 and 00017
    -- made for values Encore's own classifiers produce.
    device_type text,

    -- The player's human name. Kept here and never copied onto listens: it is
    -- the only way an operator can tell two identical device types apart when a
    -- label looks wrong, it disappears within a day, and "Requi's iPhone" has
    -- no business becoming durable on the fact table.
    device_name text,

    -- The shuffle toggle at observed_at, or NULL when Spotify did not report
    -- one.
    --
    -- Nullable, and that is the whole point of the column's type. listens.shuffle
    -- follows the same rule (see 00005): NULL is "not reported", deliberately
    -- distinct from false. An observation that does not know must not be able
    -- to teach a listen that the answer was no.
    shuffle boolean,

    -- One row per (account, track, instant). Two ticks during the same play
    -- write two rows and both are evidence; a retried write at the same instant
    -- is a duplicate and is dropped by ON CONFLICT DO NOTHING in the
    -- repository. The key is also the join's access path: the backfill probes
    -- (user_id, track_id, observed_at range), which is exactly this prefix.
    PRIMARY KEY (user_id, track_id, observed_at),

    -- Encore only ever logs an item its own classifier called a catalogue
    -- track, which by construction has an id. An empty one would join to every
    -- listen whose track_id is also empty -- there are none, listens.track_id
    -- is a foreign key -- but it would sit in the table for a day meaning
    -- nothing, and a row that means nothing is the kind that later gets read as
    -- if it meant something.
    CONSTRAINT playback_observations_track_id_present CHECK (track_id <> ''),

    -- A row must say something. An observation with neither a shuffle state nor
    -- a device type has nothing to teach a listen, and the poller's own
    -- classifier declines to write one; this is the backstop that turns a
    -- future edit removing that guard into a loud failure rather than a table
    -- quietly filling with silence.
    CONSTRAINT playback_observations_says_something
        CHECK (shuffle IS NOT NULL OR device_type IS NOT NULL)
);

-- No index beyond the primary key.
--
-- The table is bounded by (connected accounts x 24h / ENCORE_NOWPLAYING_INTERVAL):
-- five accounts at thirty seconds is under fifteen thousand rows at its
-- absolute maximum, and every one expires within a day. The join reads the
-- primary key's leading columns; the reaper's DELETE scans, which on a table
-- that size costs less than the write amplification an observed_at index would
-- add to every single tick.

-- What a live-synced listen played on, when Encore was watching at the time.
--
-- A separate column from platform rather than a second vocabulary inside it:
-- see playback_observations.device_type above. NULL means "not observed", which
-- is the state of every row in every history that predates the poller and of
-- every row on an instance that never enables it -- which is to say, most of
-- them, for ever.
--
-- Nullable with no default, so this is a catalogue-only change in PostgreSQL 11
-- and later: no table rewrite, whatever the size of the history.
ALTER TABLE listens ADD COLUMN device_type text;

-- +goose Down
ALTER TABLE listens DROP COLUMN device_type;
DROP TABLE playback_observations;
