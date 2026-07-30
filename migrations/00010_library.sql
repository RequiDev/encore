-- +goose Up

-- What a listener has saved and who they follow, as of the last enumeration.
--
-- Spotify has no "what changed" endpoint for any of these, so every sync is a
-- full enumeration reconciled against what is already here. That is why there
-- is no updated_at: a row either reflects the last successful run or was
-- deleted by it.
--
-- Deliberately NOT foreign-keyed to the catalogue. A saved track need not be in
-- `tracks` yet — enrichment mints and resolves it afterwards — and making these
-- inserts wait on catalogue rows would order the reconciliation transaction
-- behind work that has nothing to do with it.
CREATE TABLE user_saved_tracks (
    user_id  uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    track_id text        NOT NULL,
    -- When Spotify says it was saved. Nullable: the field is not guaranteed and
    -- an older grant may not carry it.
    added_at timestamptz,
    PRIMARY KEY (user_id, track_id)
);

CREATE TABLE user_saved_albums (
    user_id  uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    album_id text        NOT NULL,
    added_at timestamptz,
    PRIMARY KEY (user_id, album_id)
);

-- Spotify reports no "followed at" for artists, so there is nothing to record
-- beyond the fact itself.
CREATE TABLE user_followed_artists (
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    artist_id text NOT NULL,
    PRIMARY KEY (user_id, artist_id)
);

-- The primary keys serve the user-scoped direction. These serve the reverse:
-- "who else saved this track", and the joins from a catalogue id back to a
-- library that Phase 2c-ii's statistics walk.
CREATE INDEX user_saved_tracks_track_idx      ON user_saved_tracks (track_id);
CREATE INDEX user_saved_albums_album_idx      ON user_saved_albums (album_id);
CREATE INDEX user_followed_artists_artist_idx ON user_followed_artists (artist_id);

-- When this account's library was last enumerated in full.
--
-- NULL means never, which is distinct from "enumerated and found empty" and
-- must stay distinct: every account is NULL the moment this ships, and
-- reporting that as "you have saved nothing" would be a plausible-looking lie.
ALTER TABLE spotify_credentials ADD COLUMN library_synced_at timestamptz;

-- +goose Down
ALTER TABLE spotify_credentials DROP COLUMN library_synced_at;
DROP TABLE user_followed_artists;
DROP TABLE user_saved_albums;
DROP TABLE user_saved_tracks;
