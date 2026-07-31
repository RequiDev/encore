-- +goose Up

-- The tracks Spotify lists for one album, so Encore can name the ones nobody
-- has ever played.
--
-- Phase 2b already computes completion — "you have heard 9 of 12" — from
-- albums.total_tracks and the fact table, with no Spotify call at all. What it
-- cannot do is name the other three, because nothing on disk says what an
-- album's tracks are. This table is that list and nothing more; it is not a
-- second catalogue and it is not an input to completion.
--
-- Global rather than per-user, like every other catalogue table: two listeners
-- on one instance who open the same album share one listing and one fetch.
CREATE TABLE album_tracks (
    album_id     text    NOT NULL REFERENCES albums (id) ON DELETE CASCADE,
    track_id     text    NOT NULL,
    -- Denormalised on purpose, and deliberately without a foreign key to
    -- tracks.
    --
    -- A track nobody has ever played is by definition absent from `tracks`:
    -- that table is minted from listening. Minting a 'pending' row for each one
    -- would hand the enrichment worker hundreds of rows per album view, for
    -- music nobody listened to, in order to learn a name the very same response
    -- already carried. The listing is a display cache; `tracks` stays the
    -- catalogue of what was actually heard.
    name         text    NOT NULL DEFAULT '',
    disc_number  integer NOT NULL DEFAULT 1,
    track_number integer NOT NULL DEFAULT 0,
    PRIMARY KEY (album_id, track_id)
);

-- One row per album Encore has tried to list, holding the outcome of the last
-- attempt.
--
-- Separate from album_tracks because the absence of rows there is ambiguous in
-- three ways, and the difference is the whole feature:
--
--   never fetched     -> "Encore is asking Spotify for this list"
--   the fetch failed  -> "Encore could not read this list"
--   fetched, is empty -> impossible, and recorded as a failure; there is no
--                        such record as an album with no tracks, so a 200 with
--                        no items means the album is invisible to this
--                        application's market or has been withdrawn
--
-- The design sketch in §5.2 puts a fetched_at on album_tracks instead. That
-- cannot express any of the three: it is per row, so it says nothing at all
-- when there are no rows, which is exactly the case that needs explaining. It
-- would also repeat one instant across every row of one album.
--
-- 'fetching' doubles as a lease. A second page view — another tab, another
-- browser, another API replica sharing this database — sees it and does not
-- start a duplicate request against a quota the whole application shares.
-- attempted_at is what expires that lease, so a process killed mid-fetch does
-- not strand an album in 'fetching' for ever; without that, a browser polling
-- for the listing would poll for ever too.
CREATE TABLE album_track_fetches (
    album_id     text        PRIMARY KEY REFERENCES albums (id) ON DELETE CASCADE,
    status       text        NOT NULL
                             CHECK (status IN ('fetching', 'ok', 'failed')),
    -- When the listing in album_tracks was last replaced successfully. NULL
    -- until one succeeds, and never cleared afterwards: a failure that follows
    -- a success leaves the older listing readable and says when it was read,
    -- rather than discarding a good answer because a later request timed out.
    fetched_at   timestamptz,
    -- When the most recent attempt started, successful or not. Drives both the
    -- lease above and the retry backoff after a failure.
    attempted_at timestamptz NOT NULL,
    attempts     integer     NOT NULL DEFAULT 0,
    -- The last failure, so an operator reading the table can see why without
    -- correlating logs. Never rendered to a listener.
    last_error   text        NOT NULL DEFAULT ''
);

-- No secondary index on either table.
--
-- Both are read by their own leading key and by nothing else: album_tracks by
-- (album_id), which its primary key leads, and album_track_fetches by
-- (album_id), which is its primary key. That is the same reasoning 00011 gives
-- for spotify_top_snapshots and 00012 for user_playlists.
--
-- In particular there is no index supporting "every album whose listing is
-- stale". Nothing asks that question and nothing is meant to: §5.2 rejects a
-- background sweep explicitly, because enumerating albums nobody has opened
-- spends the instance's quota on questions nobody asked. The only reader is one
-- album's own page view, by primary key.

-- +goose Down
DROP TABLE album_track_fetches;
DROP TABLE album_tracks;
