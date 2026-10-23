-- +goose Up
-- One row per Idempotency-Key a merchant sends. While response_status is NULL the request is still
-- running (locked_at says since when); afterwards the stored response is replayed for 24 hours.
CREATE TABLE idempotency_keys (
    merchant_id     bigint      NOT NULL REFERENCES merchants (id),
    livemode        boolean     NOT NULL,
    key             text        NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    fingerprint     bytea       NOT NULL,
    response_status integer,
    response_body   bytea,
    created_at      timestamptz NOT NULL DEFAULT now(),
    locked_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, livemode, key)
);

CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
