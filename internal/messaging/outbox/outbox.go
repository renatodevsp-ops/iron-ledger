// Package outbox implements the transactional outbox: the only path by which
// an event leaves this platform.
//
// Events are rendered from the committed events of a transaction and stored in
// outbox_messages inside that same transaction, so an event that is visible is
// an event whose effects are durable — publication can never precede the commit
// that caused it. A separate worker then claims pending rows with
// FOR UPDATE SKIP LOCKED, publishes them, and marks them published. If it dies
// between those two steps the row stays claimed and another worker reclaims it
// after the visibility window, republishing the identical eventId.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/platform/integration"
	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
)

// TableName is the table this context owns.
const TableName = "outbox_messages"

// Message is one durable outbound event.
type Message struct {
	EventID       uuid.UUID
	AggregateType string
	AggregateID   string
	EventType     string
	CorrelationID string
	CausationID   string
	Payload       []byte
	OccurredAt    time.Time
	AvailableAt   time.Time
	Attempts      int
	ClaimedBy     string
	ClaimedAt     *time.Time
	PublishedAt   *time.Time
	LastError     string
}

// Repository is the outbox store.
type Repository interface {
	// Append stores events for later publication. It runs inside the business
	// transaction.
	Append(ctx context.Context, messages []Message) error
	// Claim takes ownership of pending rows, hiding them from other publishers
	// for the visibility window and recording the attempt.
	Claim(ctx context.Context, owner string, limit int, visibility time.Duration) ([]Message, error)
	// MarkPublished closes the rows. eventIds keep the published identity, so a
	// republication after a crash is the same event to every consumer.
	MarkPublished(ctx context.Context, eventIDs []uuid.UUID, at time.Time) error
	// Reschedule releases a row for a later attempt with exponential backoff.
	Reschedule(ctx context.Context, eventID uuid.UUID, nextAttemptAt time.Time, reason string) error
	// Pending reports how many rows are still waiting, for the lag gauge.
	Pending(ctx context.Context) (int64, error)
}

// Repo is the PostgreSQL implementation of Repository.
type Repo struct{ resolver pgdb.Resolver }

// NewRepo builds the repository.
func NewRepo(resolver pgdb.Resolver) *Repo { return &Repo{resolver: resolver} }

var _ Repository = (*Repo)(nil)

const columns = `event_id, aggregate_type, aggregate_id, event_type, correlation_id,
	causation_id, payload, occurred_at, available_at, attempts, claimed_by, claimed_at,
	published_at, coalesce(last_error, '')`

// Append stores the events. Nothing leaves this process here.
func (r *Repo) Append(ctx context.Context, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	const insert = `
		INSERT INTO ` + TableName + ` (
			event_id, aggregate_type, aggregate_id, event_type, correlation_id,
			causation_id, payload, occurred_at, available_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (event_id) DO NOTHING`
	for _, message := range messages {
		if _, err := r.resolver.Q(ctx).Exec(ctx, insert,
			message.EventID, message.AggregateType, message.AggregateID, message.EventType,
			nullIfEmpty(message.CorrelationID), nullIfEmpty(message.CausationID),
			message.Payload, message.OccurredAt.UTC(), message.AvailableAt.UTC(),
		); err != nil {
			return fmt.Errorf("outbox: append %s: %w", message.EventID, err)
		}
	}
	return nil
}

// Claim takes ownership of pending rows.
//
// SKIP LOCKED is what lets several publishers run against the same table
// without coordinating: each takes a disjoint batch. The visibility clause is
// the recovery path — a row whose owner died stays claimed, and once the window
// elapses any publisher may take it again.
func (r *Repo) Claim(ctx context.Context, owner string, limit int, visibility time.Duration) ([]Message, error) {
	const claim = `
		WITH candidates AS (
			SELECT o.event_id
			  FROM ` + TableName + ` AS o
			 WHERE published_at IS NULL
			   AND available_at <= $1
			   AND (claimed_at IS NULL OR claimed_at < $2::timestamptz - $3::interval)
			 ORDER BY occurred_at ASC, event_id ASC
			 LIMIT $4
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE ` + TableName + ` AS o
		   SET claimed_by = $5, claimed_at = $2, attempts = o.attempts + 1
		  FROM candidates
		 WHERE o.event_id = candidates.event_id
		RETURNING o.event_id, o.aggregate_type, o.aggregate_id, o.event_type,
			coalesce(o.correlation_id, ''), coalesce(o.causation_id, ''), o.payload,
			o.occurred_at, o.available_at, o.attempts, coalesce(o.claimed_by, ''),
			o.claimed_at, o.published_at, coalesce(o.last_error, '')`
	now := time.Now().UTC()
	rows, err := r.resolver.Q(ctx).Query(ctx, claim,
		now, now, visibility.String(), limit, owner,
	)
	if err != nil {
		return nil, fmt.Errorf("outbox: claim: %w", err)
	}
	defer rows.Close()

	var claimed []Message
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, *message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: claim: %w", err)
	}
	return claimed, nil
}

// MarkPublished closes the rows.
func (r *Repo) MarkPublished(ctx context.Context, eventIDs []uuid.UUID, at time.Time) error {
	if len(eventIDs) == 0 {
		return nil
	}
	const update = `
		UPDATE ` + TableName + ` SET published_at = $2, claimed_by = NULL, claimed_at = NULL, last_error = NULL
		WHERE event_id = ANY($1)`
	if _, err := r.resolver.Q(ctx).Exec(ctx, update, eventIDs, at.UTC()); err != nil {
		return fmt.Errorf("outbox: mark published: %w", err)
	}
	return nil
}

// Reschedule releases a row for a later attempt.
func (r *Repo) Reschedule(ctx context.Context, eventID uuid.UUID, nextAttemptAt time.Time, reason string) error {
	const update = `
		UPDATE ` + TableName + ` SET available_at = $2, claimed_by = NULL, claimed_at = NULL, last_error = $3
		WHERE event_id = $1`
	if _, err := r.resolver.Q(ctx).Exec(ctx, update, eventID, nextAttemptAt.UTC(), truncate(reason, 2000)); err != nil {
		return fmt.Errorf("outbox: reschedule %s: %w", eventID, err)
	}
	return nil
}

// Pending counts the rows still waiting for publication.
func (r *Repo) Pending(ctx context.Context) (int64, error) {
	var count int64
	const query = `SELECT count(*) FROM ` + TableName + ` WHERE published_at IS NULL`
	if err := r.resolver.Q(ctx).QueryRow(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("outbox: pending: %w", err)
	}
	return count, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMessage(row scanner) (*Message, error) {
	message := &Message{}
	err := row.Scan(
		&message.EventID, &message.AggregateType, &message.AggregateID, &message.EventType,
		&message.CorrelationID, &message.CausationID, &message.Payload, &message.OccurredAt,
		&message.AvailableAt, &message.Attempts, &message.ClaimedBy, &message.ClaimedAt,
		&message.PublishedAt, &message.LastError,
	)
	if err != nil {
		return nil, fmt.Errorf("outbox: scan message: %w", err)
	}
	return message, nil
}

// Envelope decodes the immutable payload snapshot stored with the row.
func (m Message) Envelope() (integration.Envelope, error) {
	envelope := integration.Envelope{}
	if err := json.Unmarshal(m.Payload, &envelope); err != nil {
		return integration.Envelope{}, fmt.Errorf("outbox: decode payload %s: %w", m.EventID, err)
	}
	return envelope, nil
}

// Transport renders the message as it travels over the broker. The envelope is
// reused verbatim from the stored snapshot, so retries are byte-identical.
func (m Message) Transport(messageID string, publishedAt time.Time) (integration.Message, error) {
	envelope, err := m.Envelope()
	if err != nil {
		return integration.Message{}, err
	}
	return integration.Message{
		MessageID:   messageID,
		PublishedAt: publishedAt.UTC(),
		Envelope:    envelope,
	}, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ErrNothingClaimed is returned by publishers that found no work.
var ErrNothingClaimed = errors.New("outbox: nothing claimed")
