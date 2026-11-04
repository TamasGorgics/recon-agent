-- +goose Up
-- M1-03: ledger schema and database-level invariants.
--
-- This migration is the single owner of currencies, fx_rates, fee_schedules,
-- periods, accounts, ledger_txns, and ledger_entries (spec section 3.1). It
-- seeds the embedded ISO 4217 table and one broad open period, then installs
-- the DB-level invariants:
--   * FIN-10  every transaction balances per currency, checked at COMMIT
--   * FIN-11  posted ledger rows are immutable (UPDATE/DELETE/TRUNCATE)
--   * FIN-13  entries use the account currency and only active accounts
--   * FIN-14  ledger_txns.seq is a monotonic identity
--   * FIN-56  entries cannot post into a closed period
--
-- Later migrations must not ALTER these tables without updating the schema
-- ownership map in the M1 spec (D-1).

CREATE TABLE currencies (
    code     TEXT     PRIMARY KEY CHECK (code ~ '^[A-Z]{3}$'),
    exponent SMALLINT NOT NULL CHECK (exponent BETWEEN 0 AND 3)
);

CREATE TABLE fx_rates (
    id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    base   TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    quote  TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    rate   NUMERIC(24, 12) NOT NULL CHECK (rate > 0),
    as_of  TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL,
    CHECK (base <> quote)
);

CREATE TABLE fee_schedules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source      TEXT NOT NULL,
    kind        TEXT NOT NULL,
    currency    TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    pct_bps     INT NOT NULL CHECK (pct_bps >= 0),
    fixed_minor BIGINT NOT NULL CHECK (fixed_minor >= 0),
    valid_from  DATE NOT NULL,
    valid_to    DATE,
    CHECK (valid_to IS NULL OR valid_to >= valid_from)
);

CREATE TABLE periods (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    start_date DATE NOT NULL,
    end_date   DATE NOT NULL,
    status     TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    closed_at  TIMESTAMPTZ,
    closed_by  TEXT,
    CHECK (end_date >= start_date)
);

CREATE TABLE accounts (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code     TEXT NOT NULL UNIQUE,
    name     TEXT NOT NULL,
    type     TEXT NOT NULL CHECK (type IN ('asset', 'liability', 'equity', 'revenue', 'expense')),
    currency TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    status   TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive', 'closed'))
);

CREATE TABLE ledger_txns (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    seq              BIGINT GENERATED ALWAYS AS IDENTITY,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    booked_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    value_date       DATE NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN (
                           'charge', 'refund', 'payout', 'fee', 'chargeback', 'fx',
                           'adjustment', 'reversal'
                       )),
    idempotency_key  TEXT NOT NULL UNIQUE,
    request_hash     TEXT NOT NULL,
    description      TEXT NOT NULL DEFAULT '',
    period_id        UUID NOT NULL REFERENCES periods (id) ON DELETE RESTRICT,
    adjusts_period_id UUID REFERENCES periods (id) ON DELETE RESTRICT,
    reversal_of      UUID REFERENCES ledger_txns (id) ON DELETE RESTRICT,
    UNIQUE (seq),
    CHECK (adjusts_period_id IS NULL OR adjusts_period_id <> period_id),
    CHECK (reversal_of IS NULL OR reversal_of <> id)
);

CREATE TABLE ledger_entries (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    txn_id       UUID NOT NULL REFERENCES ledger_txns (id) ON DELETE RESTRICT,
    account_id   UUID NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    direction    TEXT NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency     TEXT NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    posted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    external_ref TEXT,
    metadata     JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX ledger_entries_txn_id_idx ON ledger_entries (txn_id);
CREATE INDEX ledger_entries_account_id_idx ON ledger_entries (account_id);
CREATE INDEX ledger_txns_value_date_idx ON ledger_txns (value_date);
CREATE INDEX ledger_txns_period_id_idx ON ledger_txns (period_id);

-- Seed the ISO 4217 table exactly as internal/money exposes it. The drift test
-- in internal/ledger compares every row (code and exponent) against
-- money.Currencies(); editing this list requires editing internal/money too.
INSERT INTO currencies (code, exponent) VALUES
    ('AED', 2),
    ('AUD', 2),
    ('BGN', 2),
    ('BHD', 3),
    ('BRL', 2),
    ('CAD', 2),
    ('CHF', 2),
    ('CLP', 0),
    ('CNY', 2),
    ('COP', 2),
    ('CZK', 2),
    ('DKK', 2),
    ('EGP', 2),
    ('EUR', 2),
    ('GBP', 2),
    ('HKD', 2),
    ('HUF', 2),
    ('IDR', 2),
    ('ILS', 2),
    ('INR', 2),
    ('ISK', 0),
    ('JOD', 3),
    ('JPY', 0),
    ('KES', 2),
    ('KRW', 0),
    ('KWD', 3),
    ('MXN', 2),
    ('MYR', 2),
    ('NGN', 2),
    ('NOK', 2),
    ('NZD', 2),
    ('OMR', 3),
    ('PHP', 2),
    ('PKR', 2),
    ('PLN', 2),
    ('QAR', 2),
    ('RON', 2),
    ('SAR', 2),
    ('SEK', 2),
    ('SGD', 2),
    ('THB', 2),
    ('TND', 3),
    ('TRY', 2),
    ('TWD', 2),
    ('UAH', 2),
    ('USD', 2),
    ('VND', 0),
    ('ZAR', 2);

-- One broad open period covers the M1 synthetic data window. Tests create
-- their own narrow open/closed periods and never rely on these boundaries (D-2).
INSERT INTO periods (id, start_date, end_date, status) VALUES
    ('11111111-1111-4111-8111-111111111111', '2020-01-01', '2030-12-31', 'open');

-- FIN-11: posted ledger rows are immutable. These are triggers rather than
-- grant-only protection so the maintenance/superuser role is constrained too.
-- +goose StatementBegin
CREATE FUNCTION ledger_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is not allowed on %: posted ledger rows are immutable (FIN-11)',
        TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_txns_reject_update
    BEFORE UPDATE ON ledger_txns
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();

CREATE TRIGGER ledger_txns_reject_delete
    BEFORE DELETE ON ledger_txns
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();

CREATE TRIGGER ledger_txns_reject_truncate
    BEFORE TRUNCATE ON ledger_txns
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();

CREATE TRIGGER ledger_entries_reject_update
    BEFORE UPDATE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();

CREATE TRIGGER ledger_entries_reject_delete
    BEFORE DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();

CREATE TRIGGER ledger_entries_reject_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();

-- FIN-13/FIN-56: a row may only post to an active account of the same currency
-- and into a transaction whose period is open. Adjustments for a closed period
-- belong to the current open period with adjusts_period_id set, so checking the
-- posting period (not value_date) is correct.
-- +goose StatementBegin
CREATE FUNCTION ledger_validate_entry() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    account_currency TEXT;
    account_status   TEXT;
    posting_period   UUID;
    period_status    TEXT;
BEGIN
    SELECT a.currency, a.status
      INTO account_currency, account_status
      FROM accounts a
     WHERE a.id = NEW.account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger entry references unknown account %', NEW.account_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF account_currency <> NEW.currency THEN
        RAISE EXCEPTION 'ledger entry currency % does not match account % currency %',
            NEW.currency, NEW.account_id, account_currency
            USING ERRCODE = 'check_violation';
    END IF;
    IF account_status <> 'active' THEN
        RAISE EXCEPTION 'ledger entry account % is % and cannot be posted to',
            NEW.account_id, account_status
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT t.period_id INTO posting_period
      FROM ledger_txns t
     WHERE t.id = NEW.txn_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger entry references unknown transaction %', NEW.txn_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    SELECT p.status INTO period_status
      FROM periods p
     WHERE p.id = posting_period;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger transaction % references unknown period %', NEW.txn_id, posting_period
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF period_status <> 'open' THEN
        RAISE EXCEPTION 'ledger entry cannot post to % period %', period_status, posting_period
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_validate
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_validate_entry();

-- FIN-10: deferred constraint triggers check at COMMIT, not at row insert.
-- The trigger on ledger_txns also rejects transactions with no entries; the
-- trigger on ledger_entries catches direct inserts into an existing transaction.
-- +goose StatementBegin
CREATE FUNCTION ledger_assert_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target_txn            UUID;
    entry_count           BIGINT;
    unbalanced_currencies TEXT;
BEGIN
    IF TG_TABLE_NAME = 'ledger_txns' THEN
        target_txn := NEW.id;
    ELSE
        target_txn := NEW.txn_id;
    END IF;

    SELECT count(*) INTO entry_count
      FROM ledger_entries
     WHERE txn_id = target_txn;
    IF entry_count = 0 THEN
        RAISE EXCEPTION 'ledger transaction % has no entries', target_txn
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT string_agg(currency, ', ' ORDER BY currency)
      INTO unbalanced_currencies
      FROM (
          SELECT e.currency
            FROM ledger_entries e
           WHERE e.txn_id = target_txn
           GROUP BY e.currency
          HAVING sum(CASE WHEN e.direction = 'debit' THEN e.amount_minor ELSE -e.amount_minor END) <> 0
      ) unbalanced;

    IF unbalanced_currencies IS NOT NULL THEN
        RAISE EXCEPTION 'ledger transaction % is unbalanced in % (debits must equal credits per currency)',
            target_txn, unbalanced_currencies
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_txns_balanced
    AFTER INSERT ON ledger_txns
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_balanced();

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_balanced();

-- +goose Down
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS ledger_txns;
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS periods;
DROP TABLE IF EXISTS fee_schedules;
DROP TABLE IF EXISTS fx_rates;
DROP TABLE IF EXISTS currencies;
DROP FUNCTION IF EXISTS ledger_assert_balanced();
DROP FUNCTION IF EXISTS ledger_validate_entry();
DROP FUNCTION IF EXISTS ledger_reject_mutation();
