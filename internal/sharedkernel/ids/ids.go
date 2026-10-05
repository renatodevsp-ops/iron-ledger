// Package ids mints the identifiers used across the platform.
//
// Every externally visible identifier is a UUIDv7: time-ordered, so the
// append-only tables index well, and globally unique, so a row can be
// created without a round trip to the database. Forging one on the read path
// is impossible in practice, but every reader still validates before use.
package ids

import (
	"crypto/rand"
	"fmt"

	"github.com/google/uuid"
)

// New returns a new UUIDv7.
func New() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		// uuid.NewV7 only fails when the system entropy source does, in
		// which case there is nothing sensible left to do.
		panic(fmt.Sprintf("ids: cannot mint uuidv7: %v", err))
	}
	return id
}

// Parse validates and returns a UUID.
func Parse(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ids: %q is not a uuid", s)
	}
	return id, nil
}

// NewToken returns a random hex string of n bytes, used for opaque cursors and
// correlation identifiers that do not need to be UUIDs.
func NewToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("ids: read entropy: %w", err)
	}
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}
