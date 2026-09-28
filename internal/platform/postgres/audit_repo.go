package postgres

import (
	"context"
	"fmt"

	"github.com/ironledger/ironledger/internal/domain"
)

// AuditRepo implements domain.AuditWriter. Every request that reaches the
// service produces exactly one row, rejections included, which is what makes
// "100% das recusas sao justificadas por um motivo registrado" (SC-010)
// testable rather than aspirational.
type AuditRepo struct {
	q querier
}

func NewAuditRepo(q querier) *AuditRepo { return &AuditRepo{q: q} }

// Record writes one audit line. detail is stored as JSONB and must contain no
// sensitive context: no token, no secret, no player PII beyond the ids already
// stored on the wallet.
func (r *AuditRepo) Record(ctx context.Context, rec domain.AuditRecord) error {
	// The wallet reference is resolved with a LEFT JOIN rather than a plain
	// foreign key. A rejection can be precisely about a wallet that does not
	// exist, and an immediate FK would abort the transaction and lose the reason.
	// Probing in the same statement costs nothing, never fails, and keeps SC-010
	// true in the one case where the subject is missing.
	_, err := r.q.Exec(ctx, `
		INSERT INTO audit_log (
			tenant_id, wallet_id, occurred_at, actor, channel, operation_type,
			idempotency_key, transaction_id, message_id, outcome, reason_code, detail
		)
		SELECT $1, w.id, to_timestamp($3::float8 / 1e9), $4, $5, NULLIF($6, ''),
			NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10, NULLIF($11, ''), $12
		FROM (SELECT NULLIF($2, '')::UUID AS wid) k
		LEFT JOIN wallets w ON w.id = k.wid`,
		nullIfEmpty(rec.TenantID), nullableUUID(rec.WalletID), rec.OccurredUnixNano, rec.Actor, string(rec.Channel),
		nullIfEmpty(string(rec.OperationType)), nullIfEmpty(rec.IdempotencyKey),
		nullIfEmpty(rec.TransactionID), nullIfEmpty(rec.MessageID),
		string(rec.Outcome), nullIfEmpty(string(rec.ReasonCode)), nullableJSON(rec.Detail))
	if err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableUUID guards the ::UUID cast on the audit insert. A rejection can be
// about a wallet that does not exist, or about a wallet id that was not a UUID
// at all, and neither may be allowed to fail the audit write: recording the
// line with a null wallet_id is what keeps SC-010 true, where casting would
// turn a 400 into a 500 and lose the reason entirely.
func nullableUUID(s string) any {
	if !isUUID(s) {
		return nil
	}
	return s
}

// isUUID reports whether s has the canonical 8-4-4-4-12 hexadecimal form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// nullableJSON passes a nil slice through as SQL NULL so an accepted record
// without detail does not store a JSON null blob.
func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
