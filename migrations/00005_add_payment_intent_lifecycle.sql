-- +goose Up
ALTER TABLE payment_intents
    ADD COLUMN amount_capturable   bigint      NOT NULL DEFAULT 0,
    ADD COLUMN amount_received     bigint      NOT NULL DEFAULT 0,
    -- attempts counts confirmations; with the ID it forms the reference the card network deduplicates on.
    ADD COLUMN attempts            integer     NOT NULL DEFAULT 0,
    ADD COLUMN authorization_id    text,
    ADD COLUMN last_payment_error  jsonb,
    ADD COLUMN canceled_at         timestamptz,
    ADD COLUMN cancellation_reason text CHECK (cancellation_reason IN
        ('duplicate', 'fraudulent', 'requested_by_customer', 'abandoned')),
    ADD CONSTRAINT payment_intents_amount_capturable_check CHECK (amount_capturable BETWEEN 0 AND amount),
    ADD CONSTRAINT payment_intents_amount_received_check CHECK (amount_received BETWEEN 0 AND amount),
    -- Money can only have been received by a PaymentIntent that succeeded.
    ADD CONSTRAINT payment_intents_received_only_if_succeeded CHECK (amount_received = 0 OR status = 'succeeded');

-- +goose Down
ALTER TABLE payment_intents
    DROP CONSTRAINT payment_intents_received_only_if_succeeded,
    DROP CONSTRAINT payment_intents_amount_received_check,
    DROP CONSTRAINT payment_intents_amount_capturable_check,
    DROP COLUMN cancellation_reason,
    DROP COLUMN canceled_at,
    DROP COLUMN last_payment_error,
    DROP COLUMN authorization_id,
    DROP COLUMN attempts,
    DROP COLUMN amount_received,
    DROP COLUMN amount_capturable;
