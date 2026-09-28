-- 002_wallets.sql
-- The wallet owns a materialized balance guarded by CHECK (balance_minor >= 0).
-- That CHECK, not the application, is the guarantee that no balance can be
-- negative (Constitution Principle III).

CREATE TABLE IF NOT EXISTS wallets (
	id            UUID        PRIMARY KEY,
	tenant_id     TEXT        NOT NULL,
	player_id     TEXT        NOT NULL,
	currency      CHAR(3)     NOT NULL,
	balance_minor BIGINT      NOT NULL,
	status        TEXT        NOT NULL,
	version       BIGINT      NOT NULL DEFAULT 1,
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

	CONSTRAINT wallets_currency_chk   CHECK (currency IN ('BRL', 'USD')),
	CONSTRAINT wallets_balance_chk    CHECK (balance_minor >= 0),
	CONSTRAINT wallets_status_chk     CHECK (status IN ('ACTIVE', 'FROZEN'))
);

CREATE INDEX IF NOT EXISTS wallets_tenant_player_idx ON wallets (tenant_id, player_id);

CREATE TRIGGER wallets_touched_at
	BEFORE UPDATE ON wallets
	FOR EACH ROW EXECUTE FUNCTION touched_at();
