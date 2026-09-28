// Package usecase holds the application services. It depends on the domain
// ports and the standard library only: no HTTP, no pgx, no SQS, no fx. Both
// channels call the same Apply, which is what makes the API and the queue
// equivalent (FR-003).
package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strconv"
	"strings"

	"github.com/ironledger/ironledger/internal/domain"
)

// maxIdempotencyKeyLength is the contract's bound.
const maxIdempotencyKeyLength = 255

// RequestHash is the SHA-256 of the normalized request. A repeat of the same key
// with a different hash is a conflict, not a replay: replaying the stored
// response for a different request would be wrong (research.md D-6).
//
// The normalization is a canonical string built from the validated fields only,
// so JSON key order and whitespace cannot make two identical requests hash
// differently. Monetary values are hashed as their exact integer minor units,
// never as text that could round.
func RequestHash(op domain.Operation) []byte {
	var b strings.Builder
	b.WriteString(string(op.Type))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(op.Amount.AmountMinor, 10))
	b.WriteByte('|')
	b.WriteString(string(op.Amount.Currency))
	b.WriteByte('|')
	b.WriteString(op.TransactionID)
	b.WriteByte('|')
	b.WriteString(op.BetID)
	b.WriteByte('|')
	b.WriteString(op.OperationID)
	b.WriteByte('|')
	b.WriteString(op.ReferenceID)
	sum := sha256.Sum256([]byte(b.String()))
	return sum[:]
}

// sameRequest reports whether a stored idempotency record describes the same
// request as the one being retried.
func sameRequest(storedHash []byte, incoming []byte) bool {
	return len(storedHash) == len(incoming) && bytes.Equal(storedHash, incoming)
}

// ValidateIdempotencyKey enforces the header's presence and length bound before
// anything else: a request without a key is rejected rather than processed
// unprotected, because processing it unprotected would make a later retry
// double-apply.
func ValidateIdempotencyKey(key string) error {
	if key == "" {
		return domain.ErrMissingIdempotencyKey
	}
	if len(key) > maxIdempotencyKeyLength {
		return domain.NewError(domain.ReasonMissingIdempotencyKey,
			"Idempotency-Key must be at most %d characters", maxIdempotencyKeyLength)
	}
	return nil
}

// loadStoredResult reads the stored original for a key inside the current
// transaction and decides between a replay and a conflict.
//
// The hash comparison is what makes the contract's promise precise: repeating
// the same key with the same body replays, repeating it with a different body
// is a conflict. Returning the stored response for a different request would
// make a caller believe a mutation happened that never happened.
func loadStoredResult(ctx context.Context, tx domain.Tx, walletID, key string, incomingHash []byte) (ApplyOutput, error) {
	stored, err := tx.Operations.GetByIdempotencyKey(ctx, walletID, key)
	if err != nil {
		return ApplyOutput{}, err
	}
	if stored == nil {
		return ApplyOutput{}, domain.NewError(domain.ReasonInternalError,
			"idempotency key %q is claimed but has no record", key)
	}
	if !sameRequest(stored.RequestHash, incomingHash) {
		return ApplyOutput{}, domain.NewError(domain.ReasonIdempotencyKeyConflict,
			"idempotency key %q was already used for a different request", key)
	}
	if len(stored.ResultBody) == 0 {
		return ApplyOutput{}, domain.NewError(domain.ReasonInternalError,
			"idempotency record %s has no stored result", stored.ID)
	}
	return ApplyOutput{
		Body:   append([]byte(nil), stored.ResultBody...),
		Replay: true,
	}, nil
}
