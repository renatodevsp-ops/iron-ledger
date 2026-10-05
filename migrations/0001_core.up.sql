-- 0001_core: event store, wallets and the append-only ledger.
--
-- Everything monetary is stored as a count of minor units in a BIGINT. No
-- float column exists anywhere in this schema, so a rounding error has nowhere
-- to happen: the value that goes in is the value that comes out.

-- ---------------------------------------------------------------------------
-- Event store
-- ---------------------------------------------------------------------------

CREATE TABLE events (
    id              BIGSERIAL PRIMARY KEY,
    event_id        UUID        NOT NULL,
    stream_id       TEXT        NOT NULL,
    stream_position BIGINT      NOT NULL,
    event_type      TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    occurred_at     TIMESTAMPTZ NOT NULL,

    CONSTRAINT events_event_id_unique   UNIQUE (event_id),
    CONSTRAINT events_position_positive CHECK (stream_position > 0),
    -- The optimistic concurrency anchor: two writers cannot occupy the same
    -- position of the same stream, so a lost update is a constraint violation
    -- rather than a silent overwrite.
    CONSTRAINT events_stream_position_unique UNIQUE (stream_id, stream_position)
);

CREATE INDEX events_stream_idx ON events (stream_id, stream_position);
CREATE INDEX events_occurred_at_idx ON events (occurred_at);

-- ---------------------------------------------------------------------------
-- Wallets
-- ---------------------------------------------------------------------------

CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      VARCHAR(3)  NOT NULL,
    balance_minor BIGINT      NOT NULL DEFAULT 0,
    version       BIGINT      NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,

    -- A wallet can never hold a negative amount, whatever a bug or a
    -- concurrent writer attempts.
    CONSTRAINT wallets_balance_non_negative CHECK (balance_minor >= 0),
    CONSTRAINT wallets_version_positive      CHECK (version >= 1),
    CONSTRAINT wallets_currency_format       CHECK (currency ~ '^[A-Z]{3}$'),
    -- One wallet per player per currency: this is what makes opening a second
    -- wallet for the same player a conflict instead of a duplicated balance.
    CONSTRAINT wallets_player_currency_unique UNIQUE (player_id, currency)
);

CREATE INDEX wallets_player_idx ON wallets (player_id);

-- ---------------------------------------------------------------------------
-- Ledger
-- ---------------------------------------------------------------------------

CREATE TABLE wallet_ledger_entries (
    id                   UUID        PRIMARY KEY,
    wallet_id            UUID        NOT NULL REFERENCES wallets (id),
    transaction_id       UUID        NOT NULL,
    direction            VARCHAR(6)  NOT NULL,
    amount_minor         BIGINT      NOT NULL,
    balance_before_minor BIGINT      NOT NULL,
    balance_after_minor  BIGINT      NOT NULL,
    currency             VARCHAR(3)  NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL,

    CONSTRAINT ledger_direction        CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ledger_amount_positive  CHECK (amount_minor > 0),
    CONSTRAINT ledger_after_non_negative CHECK (balance_after_minor >= 0),
    CONSTRAINT ledger_currency_format  CHECK (currency ~ '^[A-Z]{3}$'),
    -- The entry must add up, in the database, for every row that exists.
    CONSTRAINT ledger_balances CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR
        (direction = 'DEBIT'  AND balance_after_minor = balance_before_minor - amount_minor)
    ),
    -- A wager transaction moves a wallet at most once. This is the last line of
    -- defence against a duplicated debit surviving any application-level
    -- deduplication bug.
    CONSTRAINT ledger_wallet_transaction_unique UNIQUE (wallet_id, transaction_id)
);

-- Ledger pages are ordered by id, which is a UUIDv7 and therefore time ordered.
CREATE INDEX ledger_wallet_order_idx ON wallet_ledger_entries (wallet_id, id);

-- The ledger is append-only. A correction is a new pair of entries, never an
-- edit, so the audit trail stays trustworthy.
CREATE FUNCTION reject_ledger_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only; % is not permitted', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_ledger_entries_no_update
    BEFORE UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_mutation();

CREATE TRIGGER wallet_ledger_entries_no_delete
    BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_mutation();

CREATE TRIGGER wallet_ledger_entries_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION reject_ledger_mutation();
