-- 010_roles_and_grants.sql
-- Role separation. The runtime role holds the minimum it needs, so a bug in
-- the application still cannot rewrite the ledger. The migration role owns the
-- schema and is never used at runtime.
--
-- Enforcement is layered: the trigger from 009 blocks mutation for every role
-- including the owner, and this REVOKE removes the privilege from the app role
-- before any statement can reach the trigger.

DO $$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ironledger_app') THEN
		CREATE ROLE ironledger_app NOINHERIT;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ironledger_reader') THEN
		CREATE ROLE ironledger_reader NOINHERIT;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ironledger_migrator') THEN
		CREATE ROLE ironledger_migrator;
	END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO ironledger_app, ironledger_reader;

-- wallets and bets are read and written; version and balance move.
GRANT SELECT, INSERT, UPDATE ON wallets TO ironledger_app;
GRANT SELECT, INSERT, UPDATE ON bets TO ironledger_app;

-- The idempotency record is insert and read only: UPDATE and DELETE are never
-- needed and are deliberately not granted, so a completed operation cannot be
-- rewritten.
GRANT SELECT, INSERT ON operations TO ironledger_app;
GRANT SELECT, INSERT, UPDATE ON inbox TO ironledger_app;
GRANT SELECT, INSERT ON audit_log TO ironledger_app;
GRANT SELECT, INSERT ON ledger_entries TO ironledger_app;

-- The outbox UPDATE only ever sets published_at.
GRANT SELECT, INSERT, UPDATE ON outbox TO ironledger_app;

-- The reader role is read-only reporting; it can read history and never write.
GRANT SELECT ON wallets, bets, ledger_entries, operations, audit_log, inbox, outbox
	TO ironledger_reader;

-- Append-only, enforced by privilege as well as by the trigger.
REVOKE UPDATE, DELETE ON ledger_entries FROM PUBLIC;
REVOKE UPDATE, DELETE ON ledger_entries FROM ironledger_app;
REVOKE UPDATE, DELETE ON audit_log FROM PUBLIC;
REVOKE UPDATE, DELETE ON audit_log FROM ironledger_app;

-- Sequences are needed for BIGSERIAL inserts.
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ironledger_app;
