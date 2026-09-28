-- 002_wallets.down.sql
DROP TRIGGER IF EXISTS wallets_touched_at ON wallets;
DROP TABLE IF EXISTS wallets;
