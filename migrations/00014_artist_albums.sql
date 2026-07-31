-- +goose Up

-- What Spotify lists as an artist's own releases, so Encore can say "you have
-- heard 4 of this artist's 11 albums".
--
-- No stored field counts an artist's releases. albums.total_tracks answers the
-- same question one album down (§5.1), and there is no equivalent one level up:
-- `albums` holds only records somebody played, so counting rows there would
-- answer "how many of their albums have you played" with the numerator and call
-- it the denominator.
--
-- Global rather than per-user, like every other catalogue table: two listeners
-- on one instance who open the same artist share one listing and one fetch.
CREATE TABLE artist_albums (
    artist_id         text    NOT NULL REFERENCES artists (id) ON DELETE CASCADE,
    -- Deliberately without a foreign key to `albums`, and this matters more
    -- here than it did for album_tracks.track_id in 00013.
    --
    -- Most of a discography is records nobody has played, which are by
    -- definition absent from `albums`: that table is minted from listening. For
    -- a listener who knows one record by an artist, a foreign key would reject
    -- every other row in the listing — that is, almost all of it. Minting
    -- 'pending' albums for each instead would hand the enrichment worker
    -- hundreds of rows per artist view, for music nobody listened to, to learn
    -- names this very response already carried.
    album_id          text    NOT NULL,
    name              text    NOT NULL DEFAULT '',
    -- Spotify's album_group: 'album', 'single', 'compilation' or 'appears_on'.
    -- It is what the artist stands in relation to the record, not what the
    -- record is (album_type), which is why coverage is taken over it.
    --
    -- Every group is stored, not only 'album', and the filter is applied on
    -- read. Filtering at fetch time would store zero rows for an artist who has
    -- only released singles, which is indistinguishable on disk from a failed
    -- read — the ambiguity the sibling table below exists to remove — and would
    -- leave the page unable to say what it set aside, so "4 of 11 albums" would
    -- silently omit 340 other releases.
    --
    -- No CHECK constraint, on purpose. A group Spotify adds later must not make
    -- the INSERT fail: the write that stores the listing would be rejected, the
    -- row would stay 'fetching' from the claim, and the retry would be rejected
    -- the same way for ever — a permanent strand wearing the mask of a retry
    -- loop, which is the same failure mode store.Truncate exists to prevent one
    -- column over.
    album_group       text    NOT NULL DEFAULT '',
    -- NULL when Spotify gave no date. Stored as a date with its precision
    -- beside it, exactly as albums.release_date is, so "2016" and "2016-05-20"
    -- are both representable and distinguishable.
    release_date      date,
    release_precision text    NOT NULL DEFAULT '',
    -- Where the release fell in the walk. Kept only to break ties in the read
    -- order below, so a listing does not reshuffle between page views.
    position          integer NOT NULL DEFAULT 0,
    PRIMARY KEY (artist_id, album_id)
);

-- One row per artist Encore has tried to enumerate, holding the outcome of the
-- last attempt.
--
-- Separate from artist_albums for the reason 00013 gives at length: the absence
-- of rows there is ambiguous, and the difference is the whole feature:
--
--   never fetched     -> "Encore is asking Spotify for this discography"
--   the fetch failed  -> "Encore could not read this discography"
--   fetched, is empty -> impossible, and recorded as a failure; an artist in
--                        this catalogue is there because somebody played a
--                        track by them, so a 200 with no items means the artist
--                        is invisible to this application's market
--
-- Note what is *not* in that list: an artist whose releases are all singles is
-- an ordinary artist and an ordinary success. Zero rows is a failure; zero rows
-- **of album_group 'album'** is a fact about their catalogue, and the page says
-- so in its own words. The emptiness guard is on the whole listing, never on
-- the filtered subset.
--
-- 'fetching' doubles as a lease, expired by attempted_at, so a process killed
-- mid-fetch does not strand an artist in it for ever; without that, a browser
-- polling for the discography would poll for ever too.
CREATE TABLE artist_album_fetches (
    artist_id    text        PRIMARY KEY REFERENCES artists (id) ON DELETE CASCADE,
    status       text        NOT NULL
                             CHECK (status IN ('fetching', 'ok', 'failed')),
    -- When artist_albums was last replaced successfully. NULL until one
    -- succeeds, never cleared afterwards: a failure that follows a success
    -- leaves the older listing readable and says when it was read.
    fetched_at   timestamptz,
    -- When the most recent attempt started, successful or not. Drives both the
    -- lease and the retry backoff after a failure.
    attempted_at timestamptz NOT NULL,
    attempts     integer     NOT NULL DEFAULT 0,
    -- The last failure, for an operator reading the table. Never rendered to a
    -- listener.
    last_error   text        NOT NULL DEFAULT ''
);

-- No secondary index on either table.
--
-- Both are read by their own leading key and by nothing else: artist_albums by
-- (artist_id), which its primary key leads, and artist_album_fetches by
-- (artist_id), which is its primary key. Same reasoning as 00013.
--
-- In particular there is no index supporting "every artist whose discography is
-- stale", and none supporting a lookup by album_id. Nothing asks either
-- question: §5.2 rejects a background sweep explicitly, and album_id is only
-- ever read out of a listing, never searched for.

-- +goose Down
DROP TABLE artist_album_fetches;
DROP TABLE artist_albums;
