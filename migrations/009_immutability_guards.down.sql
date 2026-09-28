-- 009_immutability_guards.down.sql
DROP TRIGGER IF EXISTS ledger_entries_no_mutation ON ledger_entries;
DROP TRIGGER IF EXISTS audit_log_no_mutation ON audit_log;
DROP FUNCTION IF EXISTS reject_mutation();
