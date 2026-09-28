-- 009_immutability_guards.sql
-- Append-only enforcement. A trigger raises on any UPDATE or DELETE, so even
-- an owner or superuser session cannot rewrite history silently.
--
-- Why a trigger and not CREATE RULE ... DO INSTEAD NOTHING (research.md D-5):
-- PostgreSQL refuses INSERT ... ON CONFLICT on a table that has an INSERT or
-- UPDATE rule, and the idempotency and inbox claims depend on ON CONFLICT.
-- A silently swallowed write is also a worse failure mode than a loud error.

CREATE OR REPLACE FUNCTION reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
	RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP
		USING ERRCODE = 'restrict_violation';
END;
$$;

DROP TRIGGER IF EXISTS ledger_entries_no_mutation ON ledger_entries;
CREATE TRIGGER ledger_entries_no_mutation
	BEFORE UPDATE OR DELETE ON ledger_entries
	FOR EACH ROW EXECUTE FUNCTION reject_mutation();

DROP TRIGGER IF EXISTS audit_log_no_mutation ON audit_log;
CREATE TRIGGER audit_log_no_mutation
	BEFORE UPDATE OR DELETE ON audit_log
	FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- A correction is a new compensating entry referencing the original, so
-- nothing is ever edited in place. The trigger is the mechanism; the REVOKE in
-- 010 is the second line of defence.
