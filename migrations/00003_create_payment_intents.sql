-- +goose Up
CREATE TABLE payment_intents (
    id             text        PRIMARY KEY,
    merchant_id    bigint      NOT NULL REFERENCES merchants (id),
    livemode       boolean     NOT NULL,
    amount         bigint      NOT NULL CHECK (amount > 0),
    currency       text        NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
    status         text        NOT NULL CHECK (status IN (
                       'requires_payment_method', 'requires_confirmation', 'requires_action',
                       'processing', 'requires_capture', 'succeeded', 'canceled')),
    capture_method text        NOT NULL CHECK (capture_method IN ('automatic', 'manual')),
    description    text,
    payment_method text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Every read is scoped to one merchant and one mode; newest first is the list order.
CREATE INDEX payment_intents_owner_created_idx
    ON payment_intents (merchant_id, livemode, created_at DESC, id DESC);

-- +goose Down
DROP TABLE payment_intents;
