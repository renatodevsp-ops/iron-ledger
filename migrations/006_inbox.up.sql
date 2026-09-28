-- 006_inbox.sql
-- The inbox deduplicates redelivered queue messages. message_id is the
-- producer's own key rather than SQS's MessageId, so it survives a redrive and
-- a replay into another environment (contracts/wallet-ops.fifo.md).
-- processed_at IS NULL means the message was claimed but its transaction has
-- not committed, so the claim rolled back with it and the redelivery is clean.

CREATE TABLE IF NOT EXISTS inbox (
	id              BIGSERIAL   PRIMARY KEY,
	message_id      TEXT        NOT NULL,
	operation_id    UUID        REFERENCES operations (id),
	queue_url       TEXT        NOT NULL,
	message_group_id TEXT       NOT NULL,
	attempts        INT         NOT NULL DEFAULT 1,
	last_error      TEXT,
	first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	processed_at    TIMESTAMPTZ,

	CONSTRAINT inbox_message_id_uniq UNIQUE (message_id),
	CONSTRAINT inbox_attempts_chk    CHECK (attempts >= 0)
);

CREATE INDEX IF NOT EXISTS inbox_unprocessed_idx ON inbox (first_seen_at) WHERE processed_at IS NULL;
