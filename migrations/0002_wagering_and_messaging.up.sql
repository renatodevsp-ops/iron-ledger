-- 0002_wagering_and_messaging: the wager transaction index, the inbox and the
-- transactional outbox.

-- ---------------------------------------------------------------------------
-- Wager transactions
-- ---------------------------------------------------------------------------

CREATE TABLE wager_transactions (
    id                      UUID         PRIMARY KEY,
    origin                  VARCHAR(9)   NOT NULL,
    provider_id             VARCHAR(128),
    external_transaction_id VARCHAR(128),
    idempotency_key         VARCHAR(255),
    payload_hash            CHAR(64)     NOT NULL,
    wallet_id               UUID         NOT NULL REFERENCES wallets (id),
    player_id               UUID         NOT NULL,
    round_id                VARCHAR(128),
    game_id                 VARCHAR(128),
    kind                    VARCHAR(16)  NOT NULL,
    amount_minor            BIGINT       NOT NULL,
    currency                VARCHAR(3)   NOT NULL,
    reference_external_id   VARCHAR(128),
    reference_transaction_id UUID        REFERENCES wager_transactions (id),
    reference_resolved_at   TIMESTAMPTZ,
    status                  VARCHAR(20)  NOT NULL,
    failure_code            VARCHAR(64),
    failure_message         TEXT,
    balance_after_minor     BIGINT,
    balance_after_currency  VARCHAR(3),
    pending_attempts        INT          NOT NULL DEFAULT 0,
    next_attempt_at         TIMESTAMPTZ,
    created_at              TIMESTAMPTZ  NOT NULL,
    updated_at              TIMESTAMPTZ  NOT NULL,

    CONSTRAINT wt_kind    CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    CONSTRAINT wt_status  CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    CONSTRAINT wt_origin  CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    CONSTRAINT wt_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wt_amount_non_negative CHECK (amount_minor >= 0),
    CONSTRAINT wt_balance_after_non_negative CHECK (balance_after_minor IS NULL OR balance_after_minor >= 0),
    CONSTRAINT wt_pending_attempts CHECK (pending_attempts >= 0),
    -- A LOSS records a bet that moved nothing, so it must carry exactly zero.
    -- Every other kind moves value, so it must carry a positive amount.
    CONSTRAINT wt_loss_is_zero   CHECK (kind <> 'LOSS' OR amount_minor = 0),
    CONSTRAINT wt_moving_is_positive CHECK (kind = 'LOSS' OR amount_minor > 0),
    -- A reversal must name the operation it reverses.
    CONSTRAINT wt_reversal_needs_reference
        CHECK (kind NOT IN ('REFUND', 'ROLLBACK') OR reference_external_id IS NOT NULL),
    -- Internal operations carry no provider identity and no round: the schema
    -- itself refuses to store one, so an internal credit can never be mistaken
    -- for a provider's bet.
    CONSTRAINT wt_origin_fields CHECK (
        (origin = 'EXTERNAL'
            AND provider_id IS NOT NULL
            AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL
            AND round_id IS NOT NULL
            AND game_id IS NOT NULL)
        OR
        (origin = 'INTERNAL'
            AND provider_id IS NULL
            AND external_transaction_id IS NULL
            AND idempotency_key IS NULL
            AND round_id IS NULL
            AND game_id IS NULL
            AND reference_external_id IS NULL)
    )
);

-- The provider's own identity of an operation. Replaying the same operation
-- under a different idempotency key is refused here, not by convention.
CREATE UNIQUE INDEX wt_provider_external_unique
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE provider_id IS NOT NULL;

-- The idempotency key, scoped to the provider. This index is what makes
-- deduplication survive a restart of every process.
CREATE UNIQUE INDEX wt_provider_idempotency_unique
    ON wager_transactions (provider_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- A referenced operation may be reversed successfully at most once. A REFUND of
-- a bet followed by a ROLLBACK of that same bet is refused here; rolling back
-- the refund itself is a different reference and is allowed.
CREATE UNIQUE INDEX wt_reference_reversed_unique
    ON wager_transactions (reference_transaction_id)
    WHERE reference_transaction_id IS NOT NULL
      AND status = 'PROCESSED'
      AND kind IN ('REFUND', 'ROLLBACK');

-- A player's initial credit in a currency happens exactly once.
CREATE UNIQUE INDEX wt_single_opening_unique
    ON wager_transactions (player_id, currency)
    WHERE kind = 'OPENING';

CREATE INDEX wt_wallet_idx ON wager_transactions (wallet_id);
CREATE INDEX wt_pending_reference_idx
    ON wager_transactions (next_attempt_at)
    WHERE status = 'PENDING_REFERENCE';

-- ---------------------------------------------------------------------------
-- Inbox: at-least-once delivery turned into at-most-once processing
-- ---------------------------------------------------------------------------

CREATE TABLE inbox_messages (
    consumer_name VARCHAR(64)  NOT NULL,
    message_id    VARCHAR(128) NOT NULL,
    payload_hash  CHAR(64)     NOT NULL,
    received_at   TIMESTAMPTZ  NOT NULL,
    completed_at  TIMESTAMPTZ,
    outcome       VARCHAR(32),

    CONSTRAINT inbox_identity_unique PRIMARY KEY (consumer_name, message_id)
);

CREATE INDEX inbox_pending_idx ON inbox_messages (received_at) WHERE completed_at IS NULL;

-- ---------------------------------------------------------------------------
-- Outbox: events published only after the commit that produced them
-- ---------------------------------------------------------------------------

CREATE TABLE outbox_messages (
    event_id       UUID         PRIMARY KEY,
    aggregate_type VARCHAR(32)  NOT NULL,
    aggregate_id   VARCHAR(64)  NOT NULL,
    event_type     VARCHAR(64)  NOT NULL,
    correlation_id VARCHAR(64),
    causation_id   VARCHAR(64),
    payload        JSONB        NOT NULL,
    occurred_at    TIMESTAMPTZ  NOT NULL,
    available_at   TIMESTAMPTZ  NOT NULL,
    attempts       INT          NOT NULL DEFAULT 0,
    claimed_by     VARCHAR(64),
    claimed_at     TIMESTAMPTZ,
    published_at   TIMESTAMPTZ,
    last_error     TEXT,

    CONSTRAINT outbox_attempts_non_negative CHECK (attempts >= 0)
);

-- The publisher's sweep: unpublished rows, soonest first.
CREATE INDEX outbox_pending_idx
    ON outbox_messages (available_at, occurred_at)
    WHERE published_at IS NULL;

-- Recovery of work abandoned by a publisher that died mid-flight.
CREATE INDEX outbox_claimed_idx
    ON outbox_messages (claimed_at)
    WHERE published_at IS NULL AND claimed_at IS NOT NULL;
