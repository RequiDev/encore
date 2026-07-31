-- +goose Up

-- What Encore last saw in one listener's Spotify player, and when it last
-- looked.
--
-- One row per user, overwritten every tick. Not a log: Phase 3c's
-- playback_observations is the log, keyed (user_id, track_id, observed_at) and
-- expiring after 24 hours, because a fuzzy temporal join needs history. A live
-- card needs the opposite — a single current row — and merging the two would
-- give the card a table it has to run DISTINCT ON against and give the backfill
-- a table that overwrites its own evidence.
--
-- It exists at all because the poller runs in encore-worker and the endpoint is
-- served by encore-api. Two processes, two containers; the database is the only
-- thing they share.
CREATE TABLE now_playing (
    user_id uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,

    -- The last *successful* observation. NULL until one has been made, which is
    -- the state the interface renders as "Encore has not checked yet" -- a
    -- different fact from "nothing is playing", and the pair the constraint
    -- below keeps inseparable.
    observed_at timestamptz,
    state       text NOT NULL DEFAULT 'unknown',
    kind        text NOT NULL DEFAULT 'none',

    -- The Spotify catalogue id, only ever set for a real track.
    --
    -- Deliberately NOT a foreign key to tracks. Spotify names a track the
    -- moment it starts playing; Encore's catalogue learns about it only when
    -- enrichment gets round to it, which may be hours later or never. A
    -- reference would fail the write for exactly the listener who is playing
    -- something new, which is the most interesting case there is. Whether the
    -- id names a row Encore holds is answered by a LEFT JOIN at read time, and
    -- decides only whether the title is a link.
    track_id text,

    -- What Spotify called it. Stored rather than joined, because a local file
    -- and a podcast episode have names and no catalogue identity at all, and
    -- because a track Encore has not enriched yet would otherwise display as
    -- blank.
    title  text NOT NULL DEFAULT '',
    artist text NOT NULL DEFAULT '',

    -- Progress at observed_at, and the item's length. Both nullable: an advert
    -- has neither, and a progress figure with no total says nothing.
    --
    -- Never extrapolated on read. The card states the age of the observation
    -- beside the figure rather than animating a bar from a fact that is up to
    -- one interval old, which would be a moving lie in place of a still truth.
    progress_ms integer,
    duration_ms integer,

    -- The player's name. The type is deliberately not stored: only the name is
    -- rendered, and the type is what Phase 3c's platform backfill wants, which
    -- belongs to its table. Empty when Spotify did not report a device --
    -- /v1/me/player/currently-playing is documented with the same object as
    -- /v1/me/player but is observed to omit it -- and the card then renders no
    -- device clause at all rather than inventing an unknown one.
    device_name text NOT NULL DEFAULT '',

    -- The last *attempt*, successful or not.
    --
    -- Separate from observed_at because they answer different questions. "When
    -- did Encore last look" is what makes a stale display honest; "when was
    -- this true" is what the figures above describe. Collapsing them would make
    -- a failed check look like an observation of an idle player.
    checked_at timestamptz NOT NULL,
    failed     boolean     NOT NULL DEFAULT false,

    -- A closed Go enum on both columns, so a value outside these sets is a bug
    -- in this repository and failing the write is how it gets found. This is
    -- the same judgement 00015 made for cover_state and the opposite of the one
    -- 00014 made for artist_albums.album_group -- that column holds a value
    -- Spotify mints and could extend without warning, these two are minted by
    -- Encore's own classifier.
    CONSTRAINT now_playing_state_known
        CHECK (state IN ('unknown', 'idle', 'playing', 'paused')),
    CONSTRAINT now_playing_kind_known
        CHECK (kind IN ('none', 'track', 'episode', 'local', 'unknown')),

    -- 'unknown' means exactly "never successfully observed", so it moves with
    -- observed_at's nullness in both directions. This is the constraint that
    -- makes "Encore has not checked yet" and "nothing is playing" impossible to
    -- confuse at the storage layer rather than merely by convention in the
    -- reader. 00016 enforces the same shape of pairing between playlists'
    -- cover_state and cover_at.
    CONSTRAINT now_playing_observed_at_matches_state
        CHECK ((state = 'unknown') = (observed_at IS NULL)),

    -- An idle player has no item and a playing one does. Without this, a row
    -- could claim to be playing nothing, or to be idle while naming a track,
    -- and every sentence the card can render about either would be false.
    CONSTRAINT now_playing_item_matches_state
        CHECK ((kind = 'none') = (state IN ('unknown', 'idle'))),

    -- When there is no item, nothing describes one. A title left over from the
    -- previous tick, sitting behind "Nothing is playing", is precisely the
    -- stale-claim defect this phase exists to rule out.
    CONSTRAINT now_playing_nothing_carries_nothing
        CHECK (kind <> 'none' OR (title = '' AND artist = '' AND track_id IS NULL
               AND progress_ms IS NULL AND duration_ms IS NULL AND device_name = '')),

    -- Only a real track has a catalogue id. A podcast episode's id is a show's,
    -- not a track's, and linking one to /tracks/{id} would be a dead link
    -- wearing a working one's clothes.
    CONSTRAINT now_playing_track_id_only_on_tracks
        CHECK (track_id IS NULL OR kind = 'track'),

    CONSTRAINT now_playing_progress_is_sane
        CHECK (progress_ms IS NULL OR progress_ms >= 0),
    CONSTRAINT now_playing_duration_is_sane
        CHECK (duration_ms IS NULL OR duration_ms > 0)
);

-- No index. The table holds one row per connected account, every read is by
-- primary key, and the poller's due query scans it joined to
-- spotify_credentials -- a table with the same number of rows. An index on
-- checked_at would cost a write per tick per account to save a scan of a
-- handful of rows.

-- +goose Down
DROP TABLE now_playing;
