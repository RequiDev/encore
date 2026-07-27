-- +goose Up

-- Read-only links to a user's aggregate statistics.
--
-- Only the hash of the token is stored, exactly as for sessions: the link is a
-- bearer credential, and a database leak must not hand somebody a working one.
--
-- A share exposes aggregates and nothing else. There is deliberately no column
-- that would let one expose the listening history: whether a link may show
-- individual plays is not a per-link preference, it is a property of the
-- feature, and the endpoint that serves a share simply has no way to reach them.
CREATE TABLE share_links (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea       NOT NULL UNIQUE,
    label      text        NOT NULL DEFAULT '',

    -- The range is pinned by the link, never by the viewer. Both bounds null
    -- means all time; range_days instead means a window ending now, so "what I
    -- have been listening to lately" stays current without being edited.
    range_from timestamptz,
    range_to   timestamptz,
    range_days integer     CHECK (range_days IS NULL OR range_days > 0),
    CONSTRAINT share_links_range_is_one_kind
        CHECK (range_days IS NULL OR (range_from IS NULL AND range_to IS NULL)),

    expires_at     timestamptz,
    revoked_at     timestamptz,
    last_viewed_at timestamptz,
    view_count     bigint      NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- The owner's management list, newest first.
CREATE INDEX share_links_user_idx ON share_links (user_id, created_at DESC);

-- +goose Down
DROP TABLE share_links;
