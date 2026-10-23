-- +goose Up
-- A double-entry ledger. Every movement of money is a transaction whose entries sum to zero;
-- entries are never updated or deleted, so every balance can be explained from history.
-- Sign convention: a positive amount is a debit, a negative amount is a credit.

CREATE TABLE ledger_accounts (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code        text        NOT NULL UNIQUE,           -- e.g. merchant:7:test:usd:pending
    merchant_id bigint      REFERENCES merchants (id), -- NULL for platform accounts
    livemode    boolean     NOT NULL,
    currency    text        NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
    type        text        NOT NULL CHECK (type IN ('asset', 'liability', 'revenue')),
    -- balance caches SUM(entries.amount); it is updated in the same transaction as the entries,
    -- and the ledger tests check that the two always agree.
    balance     bigint      NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ledger_transactions (
    id          text        PRIMARY KEY,               -- txn_...
    merchant_id bigint      NOT NULL REFERENCES merchants (id),
    livemode    boolean     NOT NULL,
    currency    text        NOT NULL,
    type        text        NOT NULL,                  -- charge, refund, ...
    source_id   text        NOT NULL,                  -- the pi_ or re_ that caused it
    description text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- One posting per business event: posting the same capture twice is a no-op, not double money.
    UNIQUE (type, source_id)
);

CREATE INDEX ledger_transactions_owner_created_idx
    ON ledger_transactions (merchant_id, livemode, created_at DESC, id DESC);

CREATE TABLE ledger_entries (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id text        NOT NULL REFERENCES ledger_transactions (id),
    account_id     bigint      NOT NULL REFERENCES ledger_accounts (id),
    amount         bigint      NOT NULL CHECK (amount <> 0),
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ledger_entries_transaction_idx ON ledger_entries (transaction_id);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id);

-- Append-only: history cannot be edited. A mistake is fixed with a new, reversing transaction.
-- +goose StatementBegin
CREATE FUNCTION ledger_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger is append-only: % on % is not allowed', TG_OP, TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_append_only BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_transactions_append_only BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();

-- Balanced and single-currency, checked at COMMIT (deferred), once all entries of a transaction exist.
-- The application checks the same before writing; this is the guarantee that holds even if it is wrong.
-- +goose StatementBegin
CREATE FUNCTION ledger_check_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    total      bigint;
    currencies int;
BEGIN
    SELECT COALESCE(SUM(e.amount), 0), COUNT(DISTINCT a.currency)
      INTO total, currencies
      FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
     WHERE e.transaction_id = NEW.transaction_id;
    IF total <> 0 THEN
        RAISE EXCEPTION 'ledger transaction % is unbalanced: entries sum to %', NEW.transaction_id, total;
    END IF;
    IF currencies > 1 THEN
        RAISE EXCEPTION 'ledger transaction % mixes currencies', NEW.transaction_id;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_transaction();

-- +goose Down
DROP TABLE ledger_entries;
DROP TABLE ledger_transactions;
DROP TABLE ledger_accounts;
DROP FUNCTION ledger_check_transaction();
DROP FUNCTION ledger_append_only();
