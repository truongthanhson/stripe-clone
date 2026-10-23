-- +goose Up
CREATE TABLE checkout_sessions (
    id                     text        PRIMARY KEY,                       -- cs_test_... / cs_live_...
    merchant_id            bigint      NOT NULL REFERENCES merchants (id),
    livemode               boolean     NOT NULL,
    -- The PaymentIntent is created with the session and is the single source of truth for the
    -- payment; the session's status only records how that ended.
    payment_intent_id      text        NOT NULL UNIQUE REFERENCES payment_intents (id),
    status                 text        NOT NULL CHECK (status IN ('open', 'complete', 'expired')),
    currency               text        NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
    amount_total           bigint      NOT NULL CHECK (amount_total > 0),
    line_items             jsonb       NOT NULL CHECK (jsonb_typeof(line_items) = 'array'),
    success_url            text        NOT NULL,
    cancel_url             text,
    customer_id            text        REFERENCES customers (id),
    customer_email         text,
    customer_details_email text,                                        -- what the buyer typed on the page
    client_reference_id    text,
    metadata               jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(metadata) = 'object'),
    expires_at             timestamptz NOT NULL,
    completed_at           timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'complete') = (completed_at IS NOT NULL))
);

CREATE INDEX checkout_sessions_owner_created_idx
    ON checkout_sessions (merchant_id, livemode, created_at DESC, id DESC);

-- +goose Down
DROP TABLE checkout_sessions;
