// Package repository is the persistence seam the STATE_VIEW slices read
// through.
//
// The read models of this platform live in PostgreSQL — the ledger, the
// wallet snapshot and the wager transaction index are financial records, and
// splitting them into a search engine would mean a second source of truth for
// money. The shape below is therefore the same generic read/write seam the
// vertical-slice conventions describe, implemented over pgx instead of a
// document store.
package repository

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned by Find when the record does not exist.
var ErrNotFound = errors.New("repository: not found")

// ErrInvalidCursor is returned when a cursor is not one this repository minted.
var ErrInvalidCursor = errors.New("repository: invalid cursor")

// Connection is one page of a read model plus the cursor of the next page.
// An empty Cursor means there is no further page.
type Connection[T any] struct {
	Cursor string
	Nodes  []*T
}

// ReadRepository is the query side a query handler depends on.
type ReadRepository[T any] interface {
	// Find returns the record with the given id, or ErrNotFound.
	Find(ctx context.Context, id string) (*T, error)
}

// Repository is the write side a projector depends on. Projectors upsert:
// Update creates the record when it is absent and overwrites it otherwise,
// because replaying a projection must converge.
type Repository[T any] interface {
	ReadRepository[T]
	Update(ctx context.Context, id string, model *T) error
}

// DefaultLimit caps a page when the caller asks for none.
const DefaultLimit = 50

// MaxLimit caps a page when the caller asks for too many.
const MaxLimit = 200

// NormalizeLimit clamps a requested page size.
func NormalizeLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultLimit
	case limit > MaxLimit:
		return MaxLimit
	default:
		return limit
	}
}

// EncodeCursor renders an opaque, order-stable cursor. The raw value is an
// opaque token to the client; only this package interprets it.
func EncodeCursor(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor reverses EncodeCursor.
func DecodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return string(decoded), nil
}

// EncodeUUIDCursor renders a UUID as an opaque cursor.
func EncodeUUIDCursor(id uuid.UUID) string { return EncodeCursor(id.String()) }

// DecodeUUIDCursor parses a cursor minted by EncodeUUIDCursor.
func DecodeUUIDCursor(cursor string) (uuid.UUID, error) {
	raw, err := DecodeCursor(cursor)
	if err != nil {
		return uuid.Nil, err
	}
	if raw == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return id, nil
}

// PageCursor is a position in an ordered collection.
//
// It carries both the instant and the identity, because neither alone is a
// total order: two rows can share a timestamp, and no identifier — however
// time-ordered it looks — is guaranteed to sort by creation across processes.
// Ordering by (created_at, id) and resuming after that pair is stable, skips
// nothing and repeats nothing.
type PageCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// IsZero reports whether the cursor points before the first row.
func (c PageCursor) IsZero() bool { return c.CreatedAt.IsZero() && c.ID == uuid.Nil }

// EncodePageCursor renders a position as an opaque cursor.
func EncodePageCursor(cursor PageCursor) string {
	return EncodeCursor(cursor.CreatedAt.UTC().Format(cursorLayout) + "|" + cursor.ID.String())
}

// DecodePageCursor parses a cursor minted by EncodePageCursor. An empty cursor
// yields the zero position, which reads from the beginning.
func DecodePageCursor(cursor string) (PageCursor, error) {
	if cursor == "" {
		return PageCursor{}, nil
	}
	raw, err := DecodeCursor(cursor)
	if err != nil {
		return PageCursor{}, err
	}
	instant, id, found := strings.Cut(raw, "|")
	if !found {
		return PageCursor{}, fmt.Errorf("%w: not a page cursor", ErrInvalidCursor)
	}
	createdAt, err := time.Parse(cursorLayout, instant)
	if err != nil {
		return PageCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return PageCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return PageCursor{CreatedAt: createdAt, ID: parsed}, nil
}

// cursorLayout is the timestamp layout a page cursor encodes.
const cursorLayout = "2006-01-02T15:04:05.000000000Z"
