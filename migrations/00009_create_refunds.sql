-- +goose Up
CREATE TABLE refunds (
    id                text        PRIMARY KEY,                     -- re_...
    merchant_id       bigint      NOT NULL REFERENCES merchants (id),
    livemode          boolean     NOT NULL,
    payment_intent_id text        NOT NULL REFERENCES payment_intents (id),
    amount            bigint      NOT NULL CHECK (amount > 0),
    currency          text        NOT NULL,
    status            text        NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    reason            text        CHECK (reason IN ('duplicate', 'fraudulent', 'requested_by_customer')),
    failure_reason    text,
    metadata          jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(metadata) = 'object'),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX refunds_owner_created_idx ON refunds (merchant_id, livemode, created_at DESC, id DESC);
CREATE INDEX refunds_payment_intent_idx ON refunds (payment_intent_id);

-- +goose Down
DROP TABLE refunds;
