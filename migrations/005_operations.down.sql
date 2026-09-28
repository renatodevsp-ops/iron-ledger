-- 005_operations.down.sql
-- bets and ledger_entries reference operations, and those foreign keys were
-- installed by this very migration. They have to go first: dropping a table that
-- is still referenced raises 2BP01 and the rollback would stop halfway with the
-- schema in a state no migration can fix.
ALTER TABLE IF EXISTS ledger_entries
	DROP CONSTRAINT IF EXISTS ledger_entries_operation_fk;
ALTER TABLE IF EXISTS bets
	DROP CONSTRAINT IF EXISTS bets_settled_by_operation_fk;

DROP TABLE IF EXISTS operations;
