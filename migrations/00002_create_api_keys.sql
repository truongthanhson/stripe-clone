-- +goose Up
CREATE TABLE api_keys (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id bigint      NOT NULL REFERENCES merchants (id),
    mode        text        NOT NULL CHECK (mode IN ('test', 'live')),
    prefix      text        NOT NULL,
    secret_hash bytea       NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

CREATE INDEX api_keys_merchant_id_idx ON api_keys (merchant_id);

-- +goose Down
DROP TABLE api_keys;
