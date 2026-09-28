// Package idgen provides the production identifier source. It exists as its
// own package because identifiers are not a PostgreSQL concern: a UUID minted
// for an outbox event never touches the database, and the SQS and HTTP adapters
// need the same generator.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Generator mints RFC 4122 version 4 UUIDs from crypto/rand.
//
// It is used for wallet, bet, operation, ledger entry and event identifiers.
// Those identifiers are not secrets and carry no authority on their own: every
// access check compares them against a row the caller may already address. A
// UUIDv7 would give better index locality, but it would also make identifiers
// guessable from their creation time, and a weak random source would be a
// correctness bug if one were ever introduced. crypto/rand removes the question.
type Generator struct{}

// New returns the production generator.
func New() *Generator { return &Generator{} }

// NewUUID returns a new UUIDv4 in the canonical 8-4-4-4-12 hexadecimal form.
func (Generator) NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A CSPRNG failure means the process cannot safely mint identities.
		// Panicking here is deliberate: continuing would hand out predictable
		// ids and silently corrupt the ledger's auditability.
		panic(fmt.Sprintf("idgen: crypto/rand unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
