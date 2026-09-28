-- 008_audit_log.sql
-- Every request that reaches the service writes exactly one row here,
-- rejections included, which is what makes "100% das recusas sao justificadas
-- por um motivo registrado" (SC-010) testable. Rejections do not write to
-- operations, so a legitimate retry after a fix is never blocked.
-- Append-only like the ledger: 009 installs the trigger.

CREATE TABLE IF NOT EXISTS audit_log (
	id              BIGSERIAL   PRIMARY KEY,
	tenant_id       TEXT        NOT NULL,
	wallet_id       UUID        REFERENCES wallets (id),
	occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	actor           TEXT        NOT NULL,
	channel         TEXT        NOT NULL,
	operation_type  TEXT,
	idempotency_key TEXT,
	transaction_id  TEXT,
	message_id      TEXT,
	outcome         TEXT        NOT NULL,
	reason_code     TEXT,
	detail          JSONB,

	CONSTRAINT audit_log_outcome_chk     CHECK (outcome IN ('ACCEPTED', 'REJECTED')),
	CONSTRAINT audit_log_channel_chk     CHECK (channel IN ('API', 'SQS')),
	CONSTRAINT audit_log_operation_chk   CHECK (operation_type IS NULL OR operation_type IN ('BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
	-- Every rejected record carries a reason; an accepted one must not.
	CONSTRAINT audit_log_reason_chk CHECK (
		(outcome = 'REJECTED' AND reason_code IS NOT NULL) OR
		(outcome = 'ACCEPTED')
	)
);

CREATE INDEX IF NOT EXISTS audit_log_wallet_idx ON audit_log (wallet_id, id);
CREATE INDEX IF NOT EXISTS audit_log_tenant_idx ON audit_log (tenant_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS audit_log_outcome_idx ON audit_log (outcome, occurred_at DESC);
