-- +goose Up

-- Identity comes exclusively from Spotify, so spotify_user_id is the natural key
-- and there is no password column anywhere in this schema.
CREATE TABLE users (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    spotify_user_id text        NOT NULL UNIQUE,
    display_name    text        NOT NULL DEFAULT '',
    email           text        NOT NULL DEFAULT '',
    avatar_url      text        NOT NULL DEFAULT '',
    product         text        NOT NULL DEFAULT '',
    role            text        NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    is_active       boolean     NOT NULL DEFAULT true,
    timezone        text        NOT NULL DEFAULT 'UTC',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    last_login_at   timestamptz
);

-- Administrator listings and the "is there an admin yet?" bootstrap check.
CREATE INDEX users_role_idx ON users (role) WHERE role = 'admin';

-- The OAuth grant lives apart from the user so that revoking or re-linking a
-- Spotify account never risks touching listening history.
CREATE TABLE spotify_credentials (
    user_id           uuid        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    -- AES-256-GCM sealed with ENCORE_ENCRYPTION_KEY; never stored in plaintext.
    access_token_enc  bytea       NOT NULL,
    refresh_token_enc bytea       NOT NULL,
    token_expires_at  timestamptz NOT NULL,
    scopes            text[]      NOT NULL DEFAULT '{}',
    sync_state        text        NOT NULL DEFAULT 'ok'
                                  CHECK (sync_state IN ('ok', 'needs_reauth', 'error')),
    -- Watermark for the recently-played poller: the newest played_at already
    -- durably committed. Advanced only after the batch commits.
    sync_cursor_at    timestamptz,
    last_sync_at      timestamptz,
    last_sync_error   text        NOT NULL DEFAULT '',
    connected_at      timestamptz NOT NULL DEFAULT now()
);

-- The sync scheduler asks for "accounts due for a poll, healthiest first".
CREATE INDEX spotify_credentials_sync_idx
    ON spotify_credentials (last_sync_at NULLS FIRST)
    WHERE sync_state <> 'needs_reauth';

-- Server-side sessions. Only the SHA-256 of the cookie value is stored, so a
-- database leak cannot be replayed as a login.
CREATE TABLE sessions (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   bytea       NOT NULL UNIQUE,
    csrf_token   text        NOT NULL,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    user_agent   text        NOT NULL DEFAULT '',
    ip           inet
);

CREATE INDEX sessions_user_idx ON sessions (user_id);
-- Supports the periodic reaper that deletes expired sessions.
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- Short-lived PKCE state for an in-flight authorisation request. Rows are
-- single-use and are deleted on callback; the reaper clears abandoned ones.
CREATE TABLE oauth_states (
    state_hash        bytea       PRIMARY KEY,
    code_verifier_enc bytea       NOT NULL,
    redirect_to       text        NOT NULL DEFAULT '',
    -- Set when the flow is a re-link for an already signed-in user rather than
    -- a fresh sign-in, so the callback can refuse an identity swap.
    link_user_id      uuid        REFERENCES users (id) ON DELETE CASCADE,
    created_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL
);

CREATE INDEX oauth_states_expires_idx ON oauth_states (expires_at);

-- +goose Down
DROP TABLE oauth_states;
DROP TABLE sessions;
DROP TABLE spotify_credentials;
DROP TABLE users;
