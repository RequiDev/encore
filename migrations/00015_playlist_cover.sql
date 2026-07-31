-- +goose Up

-- What happened the last time Encore tried to give this playlist a cover.
--
-- Cover generation is best-effort and can never fail the playlist operation
-- that triggered it: a playlist that exists with Spotify's grey placeholder is
-- a far better outcome than a create that reports failure because a CDN was
-- slow. So the outcome has to live somewhere durable, or the interface has no
-- way to say "the tracks are right, the picture is not" and no way to offer a
-- retry.
--
-- Four states, and the fourth is why this is not a boolean:
--
--   none          -> never attempted; every row that predates this migration
--   ready         -> Spotify accepted an uploaded cover
--   failed        -> an attempt was made and did not finish; cover_error says
--                    why, in words aimed at the listener
--   unauthorised  -> the account has not granted ugc-image-upload
--
-- 'unauthorised' is kept apart from 'failed' because the fix differs: one is a
-- retry, the other is a trip through Spotify's consent screen. Rendering the
-- second as the first tells somebody to press a button that cannot work.
--
-- A CHECK constraint is right here, unlike artist_albums.album_group in 00014
-- where one was rejected at length. That column holds a value *Spotify* mints
-- and could extend without warning, so a CHECK would turn a new Spotify group
-- into a permanent write failure. These four are minted by Encore's own code
-- from a closed Go enum, so a value outside the set is a bug in this
-- repository and failing the write is how it gets found.
ALTER TABLE playlists ADD COLUMN cover_state text NOT NULL DEFAULT 'none'
    CHECK (cover_state IN ('none', 'ready', 'failed', 'unauthorised'));

-- How many of the mosaic's four tiles came from real album artwork.
--
-- The denominator is always 4 -- the grid wants four tiles however many
-- distinct albums the playlist happens to contain -- so this is the numerator
-- of the coverage figure the playlist row states in words. 0 means the cover
-- is the generated pattern rather than a mosaic; the interface derives that
-- from this column rather than storing a second one that could disagree.
ALTER TABLE playlists ADD COLUMN cover_tiles integer NOT NULL DEFAULT 0
    CHECK (cover_tiles BETWEEN 0 AND 4);

-- Why the last attempt failed, in the listener's own terms. Empty in every
-- other state. Bounded at the call site by store.Truncate, rune-safely: this
-- string can carry a Spotify error body, and a byte-boundary cut through a
-- multi-byte rune would make the write that records the failure itself fail.
ALTER TABLE playlists ADD COLUMN cover_error text NOT NULL DEFAULT '';

-- When the state above was last written. NULL while cover_state is 'none',
-- which is distinct from "attempted at an unknown time" and stays distinct.
ALTER TABLE playlists ADD COLUMN cover_at timestamptz;

-- No index. Cover state is only ever read as part of a playlist row the
-- listener already asked for by (user_id) or (id, user_id), both of which
-- playlists_user_idx and the primary key already lead. Nothing asks "which
-- playlists have a failed cover" -- there is no sweep and no retry worker,
-- because a retry is a button.

-- +goose Down
ALTER TABLE playlists DROP COLUMN cover_at;
ALTER TABLE playlists DROP COLUMN cover_error;
ALTER TABLE playlists DROP COLUMN cover_tiles;
ALTER TABLE playlists DROP COLUMN cover_state;
