-- +goose Up
CREATE TABLE customers (
    id          text        PRIMARY KEY,                       -- cus_...
    merchant_id bigint      NOT NULL REFERENCES merchants (id),
    livemode    boolean     NOT NULL,
    email       text,
    name        text,
    description text,
    metadata    jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(metadata) = 'object'),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- Deleting keeps the row (payments still point at it) but hides it, as Stripe does.
    deleted_at  timestamptz
);

CREATE INDEX customers_owner_created_idx ON customers (merchant_id, livemode, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX customers_owner_email_idx ON customers (merchant_id, livemode, lower(email))
    WHERE deleted_at IS NULL;

ALTER TABLE payment_intents ADD COLUMN customer_id text REFERENCES customers (id);
CREATE INDEX payment_intents_customer_idx ON payment_intents (customer_id) WHERE customer_id IS NOT NULL;

-- +goose Down
ALTER TABLE payment_intents DROP COLUMN customer_id;
DROP TABLE customers;
