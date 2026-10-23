-- +goose Up
-- Existing merchants stay on the version they integrated against; new merchants are created on the
-- latest version by the application (internal/apiversion.Latest), not by this default.
ALTER TABLE merchants ADD COLUMN api_version text NOT NULL DEFAULT '2026-10-01';

ALTER TABLE payment_intents DROP CONSTRAINT payment_intents_capture_method_check;
ALTER TABLE payment_intents ADD CONSTRAINT payment_intents_capture_method_check
    CHECK (capture_method IN ('automatic', 'automatic_async', 'manual'));

-- +goose Down
UPDATE payment_intents SET capture_method = 'automatic' WHERE capture_method = 'automatic_async';
ALTER TABLE payment_intents DROP CONSTRAINT payment_intents_capture_method_check;
ALTER TABLE payment_intents ADD CONSTRAINT payment_intents_capture_method_check
    CHECK (capture_method IN ('automatic', 'manual'));

ALTER TABLE merchants DROP COLUMN api_version;
