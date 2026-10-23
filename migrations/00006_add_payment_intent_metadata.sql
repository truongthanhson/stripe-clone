-- +goose Up
ALTER TABLE payment_intents
    ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(metadata) = 'object');

-- +goose Down
ALTER TABLE payment_intents DROP COLUMN metadata;
