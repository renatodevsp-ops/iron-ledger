// Package inbox implements at-most-once processing on top of at-least-once
// delivery.
//
// A message is claimed by inserting its identity into inbox_messages inside the
// same transaction that performs the business work. If the transaction
// commits, the message is done and the consumer deletes it; if the process dies
// before deleting, the broker redelivers it and the insert no longer inserts —
// the already-recorded outcome is replayed instead of the work. That is what
// makes "at-least-once delivery, exactly-once effect" true across process
// crashes rather than only in the happy path.
package inbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
)

// TableName is the table this context owns.
const TableName = "inbox_messages"

// Record is one claimed message.
type Record struct {
	ConsumerName string
	MessageID    string
	PayloadHash  string
	ReceivedAt   time.Time
	CompletedAt  *time.Time
	Outcome      string
}

// Repository is the inbox store.
type Repository interface {
	// Claim records the message identity. It reports whether this call was the
	// one that inserted it: false means the message was already claimed, and
	// the caller must replay the recorded outcome instead of the work.
	Claim(ctx context.Context, record Record) (claimed bool, err error)
	// Complete marks a claimed message as durably handled. It shares the
	// business transaction, so a crash before it commits leaves the message
	// unclaimed and the whole handling is redone.
	Complete(ctx context.Context, consumerName, messageID, outcome string, at time.Time) error
	// Lookup returns a previously claimed message.
	Lookup(ctx context.Context, consumerName, messageID string) (*Record, error)
}

// Repo is the PostgreSQL implementation of Repository.
type Repo struct{ resolver pgdb.Resolver }

// NewRepo builds the repository.
func NewRepo(resolver pgdb.Resolver) *Repo { return &Repo{resolver: resolver} }

var _ Repository = (*Repo)(nil)

// Claim inserts the message identity if it is new.
func (r *Repo) Claim(ctx context.Context, record Record) (bool, error) {
	const insert = `
		INSERT INTO ` + TableName + ` (consumer_name, message_id, payload_hash, received_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (consumer_name, message_id) DO NOTHING`
	tag, err := r.resolver.Q(ctx).Exec(ctx, insert,
		record.ConsumerName, record.MessageID, record.PayloadHash, record.ReceivedAt.UTC(),
	)
	if err != nil {
		return false, fmt.Errorf("inbox: claim %s/%s: %w", record.ConsumerName, record.MessageID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Complete marks the message as handled.
func (r *Repo) Complete(ctx context.Context, consumerName, messageID, outcome string, at time.Time) error {
	const update = `
		UPDATE ` + TableName + ` SET completed_at = $3, outcome = $4
		WHERE consumer_name = $1 AND message_id = $2`
	_, err := r.resolver.Q(ctx).Exec(ctx, update, consumerName, messageID, at.UTC(), outcome)
	if err != nil {
		return fmt.Errorf("inbox: complete %s/%s: %w", consumerName, messageID, err)
	}
	return nil
}

// Lookup returns a claimed message.
func (r *Repo) Lookup(ctx context.Context, consumerName, messageID string) (*Record, error) {
	const query = `
		SELECT consumer_name, message_id, payload_hash, received_at, completed_at, outcome
		FROM ` + TableName + ` WHERE consumer_name = $1 AND message_id = $2`
	record := &Record{}
	err := r.resolver.Q(ctx).QueryRow(ctx, query, consumerName, messageID).
		Scan(&record.ConsumerName, &record.MessageID, &record.PayloadHash,
			&record.ReceivedAt, &record.CompletedAt, &record.Outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotClaimed
	}
	if err != nil {
		return nil, fmt.Errorf("inbox: lookup %s/%s: %w", consumerName, messageID, err)
	}
	return record, nil
}

// ErrNotClaimed is returned by Lookup when the message was never claimed.
var ErrNotClaimed = errors.New("inbox: message not claimed")
