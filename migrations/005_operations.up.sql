-- 005_operations.sql
-- The operations table doubles as the durable idempotency record
-- (Constitution Principle VI). The claim and the financial effect commit in the
-- same transaction, so a repeat can never double-apply and the state survives a
-- full restart of every instance: nothing here is held in process memory.

CREATE TABLE IF NOT EXISTS operations (
	id              UUID        PRIMARY KEY,
	wallet_id       UUID        NOT NULL,
	tenant_id       TEXT        NOT NULL,
	idempotency_key TEXT        NOT NULL,
	request_hash    BYTEA       NOT NULL,
	operation_type  TEXT        NOT NULL,
	transaction_id  TEXT        NOT NULL,
	bet_id          UUID,
	amount          BIGINT      NOT NULL,
	currency        CHAR(3)     NOT NULL,
	status          TEXT        NOT NULL,
	result_code     TEXT        NOT NULL,
	result_body     JSONB       NOT NULL,
	channel         TEXT        NOT NULL,
	actor           TEXT        NOT NULL,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

	CONSTRAINT operations_type_chk     CHECK (operation_type IN ('BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
	CONSTRAINT operations_amount_chk   CHECK (amount > 0),
	CONSTRAINT operations_currency_chk CHECK (currency IN ('BRL', 'USD')),
	CONSTRAINT operations_channel_chk  CHECK (channel IN ('API', 'SQS')),
	CONSTRAINT operations_status_chk   CHECK (status = 'COMPLETED'),
	CONSTRAINT operations_wallet_key_uniq UNIQUE (wallet_id, idempotency_key),
	CONSTRAINT operations_wallet_txn_uniq UNIQUE (wallet_id, transaction_id),

	-- The wallet always exists: the transaction locked its row before this
	-- insert, so this reference is checked immediately and fails fast on a bug.
	CONSTRAINT operations_wallet_id_fkey FOREIGN KEY (wallet_id) REFERENCES wallets (id),

	-- The bet reference is DEFERRED. The idempotency claim has to be written
	-- before the effect it protects, because the claim is what makes the effect
	-- exactly-once; the bet, however, does not exist until a few statements
	-- later. Deferring the check to COMMIT lets both live in one transaction
	-- without weakening the guarantee: if the transaction commits without the
	-- bet, the commit itself fails.
	CONSTRAINT operations_bet_id_fkey FOREIGN KEY (bet_id) REFERENCES bets (id)
		DEFERRABLE INITIALLY DEFERRED
);

-- The deferred foreign keys from 003 and 004, installed once every referenced
-- table exists.
ALTER TABLE bets
	ADD CONSTRAINT bets_settled_by_operation_fk
	FOREIGN KEY (settled_by_operation_id) REFERENCES operations (id);

ALTER TABLE ledger_entries
	ADD CONSTRAINT ledger_entries_operation_fk
	FOREIGN KEY (operation_id) REFERENCES operations (id);

-- A bet can only be settled once. The bets_one_settlement partial index in 003
-- only covers the bet row, so the operations side is constrained here: a second
-- WIN or LOSS for the same bet is a constraint violation, not an application
-- race (FR-020). REFUND and ROLLBACK are excluded because they must be able to
-- carry the same bet_id.
CREATE UNIQUE INDEX IF NOT EXISTS operations_one_settlement_per_bet
	ON operations (bet_id)
	WHERE bet_id IS NOT NULL AND operation_type IN ('WIN', 'LOSS');

CREATE INDEX IF NOT EXISTS operations_wallet_created_idx ON operations (wallet_id, created_at DESC);
CREATE INDEX IF NOT EXISTS operations_tenant_idx ON operations (tenant_id, created_at DESC);
