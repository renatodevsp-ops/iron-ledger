-- 004_ledger_entries.sql
-- The append-only ledger. Once a row is written it is never updated or
-- deleted: 009_immutability_guards.sql installs the trigger and
-- 010_roles_and_grants.sql adds the REVOKE, so the guarantee does not depend on
-- a single mechanism (research.md D-5).
--
-- operation_id references operations, created in 005; the foreign key is added
-- there to keep the migration sequence free of circular references.

CREATE TABLE IF NOT EXISTS ledger_entries (
	id                 BIGSERIAL   PRIMARY KEY,
	entry_uid          UUID        NOT NULL,
	wallet_id          UUID        NOT NULL REFERENCES wallets (id),
	tenant_id          TEXT        NOT NULL,
	operation_id       UUID        NOT NULL,
	bet_id             UUID        REFERENCES bets (id),
	entry_type         TEXT        NOT NULL,
	direction          SMALLINT    NOT NULL,
	amount_minor       BIGINT      NOT NULL,
	currency           CHAR(3)     NOT NULL,
	balance_after_minor BIGINT     NOT NULL,
	reverses_entry_id  BIGINT      REFERENCES ledger_entries (id),
	sequence           BIGINT      NOT NULL,
	occurred_at        TIMESTAMPTZ NOT NULL,
	recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

	CONSTRAINT ledger_entries_uid_uniq     UNIQUE (entry_uid),
	CONSTRAINT ledger_entries_type_chk     CHECK (entry_type IN ('BET_DEBIT', 'WIN_CREDIT', 'REFUND_CREDIT', 'ROLLBACK_CREDIT', 'ROLLBACK_DEBIT')),
	CONSTRAINT ledger_entries_dir_chk      CHECK (direction IN (-1, 1)),
	CONSTRAINT ledger_entries_amount_chk   CHECK (amount_minor > 0),
	CONSTRAINT ledger_entries_currency_chk CHECK (currency IN ('BRL', 'USD')),
	CONSTRAINT ledger_entries_balance_chk  CHECK (balance_after_minor >= 0),
	CONSTRAINT ledger_entries_wallet_seq_uniq UNIQUE (wallet_id, sequence)
);

-- The reconciliation read is SUM(direction * amount_minor) for one wallet, and
-- the point-in-time read is the last entry at or before a sequence. Both are
-- served by this index.
CREATE INDEX IF NOT EXISTS ledger_entries_wallet_sequence_idx
	ON ledger_entries (wallet_id, sequence);

CREATE INDEX IF NOT EXISTS ledger_entries_operation_idx
	ON ledger_entries (operation_id);

CREATE INDEX IF NOT EXISTS ledger_entries_wallet_bet_idx
	ON ledger_entries (wallet_id, bet_id);
