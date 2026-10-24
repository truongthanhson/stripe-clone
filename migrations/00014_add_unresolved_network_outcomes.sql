-- +goose Up
-- When the card network does not answer, the outcome is unknown: the money may or may not have
-- moved. That is recorded explicitly, with what was being attempted, so the resolver can ask again
-- with the same idempotent request instead of guessing.
ALTER TABLE payment_intents
    ADD COLUMN unresolved_op     text CHECK (unresolved_op IN ('authorize', 'capture', 'void')),
    ADD COLUMN unresolved_amount bigint,                    -- the amount of an unresolved capture
    ADD COLUMN unresolved_since  timestamptz,
    ADD CONSTRAINT payment_intents_unresolved_check CHECK ((unresolved_op IS NULL) = (unresolved_since IS NULL));

CREATE INDEX payment_intents_unresolved_idx ON payment_intents (unresolved_since) WHERE unresolved_op IS NOT NULL;
CREATE INDEX payment_intents_processing_idx ON payment_intents (updated_at) WHERE status = 'processing';

ALTER TABLE refunds ADD COLUMN unresolved_since timestamptz;
CREATE INDEX refunds_pending_idx ON refunds (created_at) WHERE status = 'pending';

-- +goose Down
DROP INDEX refunds_pending_idx;
ALTER TABLE refunds DROP COLUMN unresolved_since;
DROP INDEX payment_intents_processing_idx;
DROP INDEX payment_intents_unresolved_idx;
ALTER TABLE payment_intents
    DROP CONSTRAINT payment_intents_unresolved_check,
    DROP COLUMN unresolved_since,
    DROP COLUMN unresolved_amount,
    DROP COLUMN unresolved_op;
