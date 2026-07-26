-- +goose Up
-- Instance-wide configuration that an administrator can change at runtime.
-- Anything settable from the UI lives here; anything that must be known before
-- the process can serve traffic lives in the environment.
CREATE TABLE app_settings (
    key        text        PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- New instances start open so the first visitor can become the administrator.
-- The registration handler closes this automatically once an admin exists only
-- if ENCORE_REGISTRATIONS_DEFAULT says so; otherwise the admin controls it.
INSERT INTO app_settings (key, value) VALUES ('registrations_enabled', 'true'::jsonb);

-- +goose Down
DROP TABLE app_settings;
