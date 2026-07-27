-- +goose Up

-- Catalogue rows Encore mints from an import rather than from Spotify.
--
-- Both export formats name the artist and the album of every play and identify
-- neither: there is a spotify_track_uri and nothing else. Until now those names
-- were parsed and dropped, so a freshly imported history had no artists at all
-- until enrichment drained — which on a development-mode application whose daily
-- quota is exhausted can mean never.
--
-- A local row carries a name and nothing else. 'local' is a terminal state, not
-- a stage of the pending -> resolved machine: the enrichment queues claim
-- 'pending' and 'failed', so these are never offered to Spotify, which could not
-- answer for them anyway.
ALTER TABLE artists DROP CONSTRAINT artists_metadata_state_check;
ALTER TABLE artists ADD CONSTRAINT artists_metadata_state_check
    CHECK (metadata_state IN ('pending', 'resolved', 'unavailable', 'failed', 'local'));

ALTER TABLE albums DROP CONSTRAINT albums_metadata_state_check;
ALTER TABLE albums ADD CONSTRAINT albums_metadata_state_check
    CHECK (metadata_state IN ('pending', 'resolved', 'unavailable', 'failed', 'local'));

-- Folding a local artist into the Spotify one that turns out to be the same
-- person is a lookup by normalised name, and it runs once per resolved batch.
CREATE INDEX artists_local_name_norm_idx ON artists (name_norm)
    WHERE metadata_state = 'local';

CREATE INDEX albums_local_name_norm_idx ON albums (name_norm)
    WHERE metadata_state = 'local';

-- +goose Down
DROP INDEX albums_local_name_norm_idx;
DROP INDEX artists_local_name_norm_idx;

DELETE FROM albums WHERE metadata_state = 'local';
DELETE FROM artists WHERE metadata_state = 'local';

ALTER TABLE albums DROP CONSTRAINT albums_metadata_state_check;
ALTER TABLE albums ADD CONSTRAINT albums_metadata_state_check
    CHECK (metadata_state IN ('pending', 'resolved', 'unavailable', 'failed'));

ALTER TABLE artists DROP CONSTRAINT artists_metadata_state_check;
ALTER TABLE artists ADD CONSTRAINT artists_metadata_state_check
    CHECK (metadata_state IN ('pending', 'resolved', 'unavailable', 'failed'));
