-- 001_extensions.sql
-- Extensions and helper functions used by later migrations.
-- Money is BIGINT minor units plus CHAR(3); no FLOAT/REAL/DOUBLE PRECISION and
-- no NUMERIC column is introduced anywhere in this schema (Constitution I).

-- gen_random_uuid() is provided by pgcrypto on older servers and by core from
-- PostgreSQL 13 onwards. The extension is created only when missing so the
-- migration is idempotent.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- touched_at is the single definition of "row was mutated", so updated_at is
-- set identically by every writer.
CREATE OR REPLACE FUNCTION touched_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
	NEW.updated_at := now();
	RETURN NEW;
END;
$$;
