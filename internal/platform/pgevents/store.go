// Package pgevents is the PostgreSQL event store.
//
// It implements the same cqrs.EventStore contract the library's own Postgres
// store does, with one crucial difference: it joins the transaction carried by
// the context instead of opening its own. That is what allows the wallet's
// events, the wagering events, the ledger rows, the inbox row and the outbound
// events to share a single commit.
//
// Writers to a stream are serialised with a transaction-scoped advisory lock
// keyed on the stream id, so two processes appending to the same wallet queue
// briefly while two processes appending to different wallets do not. The
// optimistic revision check is still enforced against the database, so the
// lock is an optimisation, never the correctness argument.
package pgevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/platform/uow"
)

const pgUniqueViolation = "23505"

// Store is a transaction-aware cqrs.EventStore.
type Store struct {
	pool *pgxpool.Pool
}

// New builds a Store over a pool. The pool must outlive the store; Close does
// not close it.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var _ cqrs.EventStore = (*Store)(nil)

// Save appends events to the stream they share.
func (s *Store) Save(ctx context.Context, events []cqrs.Envelope, revision cqrs.StreamState) (cqrs.AppendResult, error) {
	if len(events) == 0 {
		return cqrs.AppendResult{Successful: true}, nil
	}

	streamID := events[0].StreamID
	for i, e := range events {
		if e.StreamID != streamID {
			return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf(
				"pgevents: batch to %q mixes streams: event %d targets %q", streamID, i, e.StreamID)
		}
	}

	tx, err := uow.TxFromContext(ctx)
	if err != nil {
		return cqrs.AppendResult{StreamID: streamID}, err
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, streamID); err != nil {
		return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf("pgevents: lock stream %q: %w", streamID, err)
	}

	currentVersion, err := streamVersion(ctx, tx, streamID)
	if err != nil {
		return cqrs.AppendResult{StreamID: streamID}, err
	}

	if err := checkRevision(streamID, currentVersion, revision); err != nil {
		return cqrs.AppendResult{Successful: false, StreamID: streamID, NextExpectedVersion: currentVersion}, err
	}

	recorder := uow.RecorderFromContext(ctx)
	for i := range events {
		e := events[i]
		if e.Event == nil {
			return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf("pgevents: event %d has no payload", i)
		}
		payload, err := json.Marshal(e.Event)
		if err != nil {
			return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf("pgevents: marshal event %d: %w", i, err)
		}
		metadata := e.Metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadataJSON, err := json.Marshal(metadata)
		if err != nil {
			return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf("pgevents: marshal metadata %d: %w", i, err)
		}
		occurredAt := e.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = time.Now().UTC()
		}
		eventID := e.EventID
		if eventID == uuid.Nil {
			eventID = uuid.New()
			events[i].EventID = eventID
		}

		const insert = `
			INSERT INTO events (event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`
		if _, err := tx.Exec(ctx, insert,
			eventID, streamID, int64(e.Version), e.Event.EventType(), payload, metadataJSON, occurredAt,
		); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
				actual, _ := streamVersion(ctx, tx, streamID)
				return cqrs.AppendResult{Successful: false, StreamID: streamID, NextExpectedVersion: actual},
					&cqrs.StreamRevisionConflictError{
						Stream:           streamID,
						ExpectedRevision: revision,
						ActualRevision:   cqrs.Revision(actual),
					}
			}
			return cqrs.AppendResult{StreamID: streamID}, fmt.Errorf("pgevents: insert event %d: %w", i, err)
		}
	}

	recorder.Record(events...)

	finalVersion := currentVersion + uint64(len(events))
	return cqrs.AppendResult{Successful: true, StreamID: streamID, NextExpectedVersion: finalVersion}, nil
}

func streamVersion(ctx context.Context, tx pgx.Tx, streamID string) (uint64, error) {
	var maxPos pgtype.Int8
	if err := tx.QueryRow(ctx, `SELECT MAX(stream_position) FROM events WHERE stream_id = $1`, streamID).Scan(&maxPos); err != nil {
		return 0, fmt.Errorf("pgevents: read stream version %q: %w", streamID, err)
	}
	if !maxPos.Valid {
		return 0, nil
	}
	return uint64(maxPos.Int64), nil
}

func checkRevision(streamID string, current uint64, revision cqrs.StreamState) error {
	switch rev := revision.(type) {
	case cqrs.Any:
		return nil
	case cqrs.NoStream:
		if current != 0 {
			return fmt.Errorf("stream %q already exists: %w", streamID, cqrs.ErrStreamExists)
		}
		return nil
	case cqrs.StreamExists:
		if current == 0 {
			return fmt.Errorf("stream %q does not exist: %w", streamID, cqrs.ErrStreamNotFound)
		}
		return nil
	case cqrs.Revision:
		if current != uint64(rev) {
			return &cqrs.StreamRevisionConflictError{
				Stream:           streamID,
				ExpectedRevision: rev,
				ActualRevision:   cqrs.Revision(current),
			}
		}
		return nil
	default:
		return fmt.Errorf("pgevents: unsupported revision %T on stream %q: %w", revision, streamID, cqrs.ErrInvalidRevision)
	}
}

// LoadStream returns every event of a stream, oldest first.
func (s *Store) LoadStream(ctx context.Context, id string) (*cqrs.Iterator[*cqrs.Envelope], error) {
	exists, err := s.exists(ctx, id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("pgevents: stream %q: %w", id, cqrs.ErrStreamNotFound)
	}
	return s.query(ctx, `SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
		FROM events WHERE stream_id = $1 ORDER BY stream_position ASC`, id)
}

// LoadStreamFrom returns the events of a stream that follow version.
func (s *Store) LoadStreamFrom(ctx context.Context, id string, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	switch version.(type) {
	case cqrs.NoStream:
		exists, err := s.exists(ctx, id)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("pgevents: stream %q: %w", id, cqrs.ErrStreamExists)
		}
		return cqrs.NewSliceIterator(ctx, []*cqrs.Envelope{}), nil
	case cqrs.StreamExists:
		exists, err := s.exists(ctx, id)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("pgevents: stream %q: %w", id, cqrs.ErrStreamNotFound)
		}
		return s.query(ctx, `SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
			FROM events WHERE stream_id = $1 ORDER BY stream_position ASC`, id)
	case cqrs.Any:
		return s.query(ctx, `SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
			FROM events WHERE stream_id = $1 ORDER BY stream_position ASC`, id)
	default:
		from := version.ToRawInt64()
		return s.query(ctx, `SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
			FROM events WHERE stream_id = $1 AND stream_position > $2 ORDER BY stream_position ASC`, id, from)
	}
}

// LoadFromAll returns every event across every stream in commit order,
// starting after version.
func (s *Store) LoadFromAll(ctx context.Context, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	from := version.ToRawInt64()
	return s.query(ctx, `SELECT id, event_id, stream_id, stream_position, event_type, payload, metadata, occurred_at
		FROM events WHERE id > $1 ORDER BY id ASC`, from)
}

// Close satisfies cqrs.EventStore. The pool is owned by the composition root,
// which closes it after every component has stopped using it.
func (s *Store) Close() error { return nil }

// querier is the read surface shared by the pool and an in-flight transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) reader(ctx context.Context) querier {
	if tx, err := uow.TxFromContext(ctx); err == nil {
		return tx
	}
	return s.pool
}

func (s *Store) exists(ctx context.Context, id string) (bool, error) {
	var exists bool
	if err := s.reader(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE stream_id = $1)`, id).Scan(&exists); err != nil {
		return false, fmt.Errorf("pgevents: check stream %q: %w", id, err)
	}
	return exists, nil
}

func (s *Store) query(ctx context.Context, sql string, args ...any) (*cqrs.Iterator[*cqrs.Envelope], error) {
	rows, err := s.reader(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("pgevents: query: %w", err)
	}
	return cqrs.NewIteratorFunc(ctx, func(context.Context) (*cqrs.Envelope, error) {
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		env, err := scanEnvelope(rows)
		if err != nil {
			return nil, err
		}
		return env, nil
	}, func() error {
		rows.Close()
		return nil
	}), nil
}

func scanEnvelope(rows pgx.Rows) (*cqrs.Envelope, error) {
	var (
		globalID       int64
		rawUUID        pgtype.UUID
		streamID       string
		streamPosition int64
		eventType      string
		payload        []byte
		metadata       []byte
		occurredAt     time.Time
	)
	if err := rows.Scan(&globalID, &rawUUID, &streamID, &streamPosition, &eventType, &payload, &metadata, &occurredAt); err != nil {
		return nil, fmt.Errorf("pgevents: scan envelope: %w", err)
	}

	ev, err := cqrs.NewEventByName(eventType)
	if err != nil {
		return nil, fmt.Errorf("pgevents: instantiate event %q: %w", eventType, err)
	}
	if err := json.Unmarshal(payload, ev); err != nil {
		return nil, fmt.Errorf("pgevents: decode event %q: %w", eventType, err)
	}

	meta := map[string]any{}
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &meta)
	}

	return &cqrs.Envelope{
		EventID:       uuid.UUID(rawUUID.Bytes),
		StreamID:      streamID,
		Event:         ev,
		Metadata:      meta,
		Version:       uint64(streamPosition),
		GlobalVersion: uint64(globalID),
		OccurredAt:    occurredAt.UTC(),
	}, nil
}
