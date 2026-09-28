-- 007_outbox.sql
-- The transactional outbox. A row is inserted in the same transaction as the
-- ledger entries it describes, and the separate publisher sends it only after
-- the commit returns (Constitution Principle V). Rows are never deleted;
-- published_at is the only mutation.

CREATE TABLE IF NOT EXISTS outbox (
	id               BIGSERIAL   PRIMARY KEY,
	event_uid        UUID        NOT NULL,
	aggregate_type   TEXT        NOT NULL,
	aggregate_id     UUID        NOT NULL,
	event_type       TEXT        NOT NULL,
	payload          JSONB       NOT NULL,
	message_group_id TEXT        NOT NULL,
	dedup_id         TEXT        NOT NULL,
	created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
	published_at     TIMESTAMPTZ,

	CONSTRAINT outbox_event_uid_uniq UNIQUE (event_uid),
	CONSTRAINT outbox_aggregate_chk  CHECK (aggregate_type IN ('wallet', 'bet')),
	CONSTRAINT outbox_event_chk      CHECK (event_type IN ('wallet.bet_placed', 'wallet.bet_settled', 'wallet.bet_reversed', 'wallet.refunded'))
);

-- Publisher poll: unpublished rows in id order, with FOR UPDATE SKIP LOCKED so N
-- publisher instances need no coordination between them.
CREATE INDEX IF NOT EXISTS outbox_unpublished_idx ON outbox (published_at, id) WHERE published_at IS NULL;
