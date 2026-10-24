-- +goose Up
-- A merchant signs in to the dashboard with their email and a password (bcrypt). Failed attempts
-- are counted per merchant in a 15-minute window, so a password cannot be guessed at speed.
ALTER TABLE merchants
    ADD COLUMN password_hash           text,
    ADD COLUMN login_attempts          integer NOT NULL DEFAULT 0,
    ADD COLUMN login_window_started_at timestamptz;

CREATE UNIQUE INDEX merchants_email_lower_idx ON merchants (lower(email));

-- One row per signed-in browser. Only a hash of the cookie's token is stored, so a copy of this
-- table cannot be used to sign in.
CREATE TABLE dashboard_sessions (
    token_hash  bytea       PRIMARY KEY,
    merchant_id bigint      NOT NULL REFERENCES merchants (id),
    livemode    boolean     NOT NULL DEFAULT false,      -- which data the dashboard shows
    csrf_token  text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);

CREATE INDEX dashboard_sessions_expires_at_idx ON dashboard_sessions (expires_at);

-- +goose Down
DROP TABLE dashboard_sessions;
DROP INDEX merchants_email_lower_idx;
ALTER TABLE merchants
    DROP COLUMN login_window_started_at,
    DROP COLUMN login_attempts,
    DROP COLUMN password_hash;
