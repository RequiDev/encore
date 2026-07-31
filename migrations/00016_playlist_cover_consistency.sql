-- +goose Up

-- cover_state and cover_at are meant to move together: 'none' is exactly the
-- state that has never been attempted, and cover_at is exactly the record of
-- when the state was last written (00015's own comments say both things).
-- Nothing enforced that relationship until now, and the reader that decides
-- what a listener sees branches directly on cover_at's nullness -- a stray
-- non-null cover_at behind cover_state = 'none', or any of the other three
-- states with no timestamp, would misstate whether an attempt ever happened,
-- which is the exact class of silent misrepresentation this phase exists to
-- rule out. playlists_range_is_whole, three columns up in this same table,
-- already enforces the same shape of relationship between range_from and
-- range_to; this is that pattern applied to the pair 00015 introduced.
ALTER TABLE playlists ADD CONSTRAINT playlists_cover_at_matches_state
    CHECK ((cover_state = 'none') = (cover_at IS NULL));

-- cover_error is constrained too, but only in one direction: cover_state must
-- be 'failed' whenever cover_error is non-empty, not the reverse. It is
-- listener-facing text -- domain.PlaylistCover.Error is documented as being
-- "in the listener's own terms" -- unlike artist_album_fetches.last_error,
-- which 00014 documents as never rendered to anyone. A message left over from
-- a past failure that has since been superseded (a retry that succeeded, or
-- an account that went on to grant consent) would read as a live complaint
-- about a picture that is in fact fine or merely pending consent, which is
-- the same misrepresentation cover_at is constrained against above.
--
-- The reverse implication is deliberately not required. Forcing every
-- 'failed' row to carry non-empty text would fail a write that has nothing
-- more specific to say, and a blank reason on a failure is a lesser defect
-- than a stale one being shown as current -- the second is a false claim
-- about the account's state, the first is merely an unhelpful true one.
ALTER TABLE playlists ADD CONSTRAINT playlists_cover_error_only_on_failure
    CHECK (cover_state = 'failed' OR cover_error = '');

-- +goose Down
ALTER TABLE playlists DROP CONSTRAINT playlists_cover_error_only_on_failure;
ALTER TABLE playlists DROP CONSTRAINT playlists_cover_at_matches_state;
