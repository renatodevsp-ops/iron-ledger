-- 003_bets.sql
-- A bet is the object a settlement or a correction acts on.
-- settled_by_operation_id references operations, which is created in 005; the
-- foreign key is added there so the migration order stays a simple forward
-- sequence with no circular dependency.

CREATE TABLE IF NOT EXISTS bets (
	id                      UUID        PRIMARY KEY,
	wallet_id               UUID        NOT NULL REFERENCES wallets (id),
	tenant_id               TEXT        NOT NULL,
	external_bet_ref        TEXT        NOT NULL,
	stake_minor             BIGINT      NOT NULL,
	currency                CHAR(3)     NOT NULL,
	status                  TEXT        NOT NULL,
	settled_by_operation_id UUID,
	created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
	settled_at              TIMESTAMPTZ,

	CONSTRAINT bets_stake_chk    CHECK (stake_minor > 0),
	CONSTRAINT bets_currency_chk CHECK (currency IN ('BRL', 'USD')),
	CONSTRAINT bets_status_chk   CHECK (status IN ('OPEN', 'SETTLED_WIN', 'SETTLED_LOSS', 'VOIDED', 'REVERSED')),
	CONSTRAINT bets_tenant_ref_uniq UNIQUE (tenant_id, external_bet_ref)
);

-- One settlement per bet, enforced in the database (FR-020). The partial index
-- only covers settled bets, so an OPEN bet is unconstrained.
CREATE UNIQUE INDEX IF NOT EXISTS bets_one_settlement
	ON bets (id) WHERE settled_by_operation_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS bets_wallet_status_idx ON bets (wallet_id, status);
