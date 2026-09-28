package postgres

import (
	"context"
	"fmt"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/jackc/pgx/v5"
)

// OutboxRepo implements domain.OutboxWriter plus the publisher-side reads. A
// row is inserted in the same transaction as the ledger entries it describes,
// and only the separate publisher sends it, after the commit has returned
// (Constitution Principle V).
type OutboxRepo struct {
	q querier
}

func NewOutboxRepo(q querier) *OutboxRepo { return &OutboxRepo{q: q} }

// Enqueue writes one outbox row. It is always called with the repositories of
// the same transaction as the ledger entries, so a rolled-back operation leaves
// no event behind and a committed one can never lose its event.
func (r *OutboxRepo) Enqueue(ctx context.Context, e domain.OutboxEvent) error {
	_, err := r.q.Exec(ctx, `
		INSERT INTO outbox (
			event_uid, aggregate_type, aggregate_id, event_type, payload,
			message_group_id, dedup_id, created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, to_timestamp($8::float8 / 1e9)
		)`,
		e.EventUID, e.AggregateType, e.AggregateID, e.EventType, e.Payload,
		e.MessageGroupID, e.DedupID, e.CreatedUnixNano)
	if err != nil {
		return fmt.Errorf("enqueue outbox event: %w", err)
	}
	return nil
}

// OutboxRow is a published-event candidate read by the publisher.
type OutboxRow struct {
	ID             int64
	EventUID       string
	AggregateType  string
	AggregateID    string
	EventType      string
	Payload        []byte
	MessageGroupID string
	DedupID        string
}

// PollBatch returns up to limit unpublished rows in id order, locking them with
// FOR UPDATE SKIP LOCKED so N publisher instances run without any coordination
// between them (Constitution VII). The caller must run it inside a transaction
// and call MarkPublished after each successful send.
func (r *OutboxRepo) PollBatch(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, event_uid::TEXT, aggregate_type, aggregate_id::TEXT, event_type, payload,
			message_group_id, dedup_id
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY id ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("poll outbox: %w", err)
	}
	defer rows.Close()

	var out []OutboxRow
	for rows.Next() {
		var row OutboxRow
		if err := rows.Scan(&row.ID, &row.EventUID, &row.AggregateType, &row.AggregateID,
			&row.EventType, &row.Payload, &row.MessageGroupID, &row.DedupID); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// MarkPublished records a successful send. A crash between the send and this
// update causes a re-send with the same dedup_id, which the FIFO dedup window
// collapses when it is inside five minutes and the consumer's inbox collapses
// when it is not.
func (r *OutboxRepo) MarkPublished(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
	}
	return nil
}

// UnpublishedDepth reports how many rows are waiting to be published. It backs
// the DLQ-depth style alert: a growing number means the publisher is behind or
// stuck.
func (r *OutboxRepo) UnpublishedDepth(ctx context.Context) (int64, error) {
	var n int64
	if err := r.q.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count unpublished outbox rows: %w", err)
	}
	return n, nil
}
