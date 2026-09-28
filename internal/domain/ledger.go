package domain

// EntryType enumerates the append-only ledger entry kinds. LOSS has none,
// because settling a loss moves no money.
type EntryType string

const (
	EntryBetDebit       EntryType = "BET_DEBIT"
	EntryWinCredit      EntryType = "WIN_CREDIT"
	EntryRefundCredit   EntryType = "REFUND_CREDIT"
	EntryRollbackCredit EntryType = "ROLLBACK_CREDIT"
	EntryRollbackDebit  EntryType = "ROLLBACK_DEBIT"
)

// Direction carries the sign of an entry. It is -1 for a debit and +1 for a
// credit, so amount_minor is always strictly positive and the ledger sum is
// SUM(direction * amount_minor) with no sign juggling (data-model.md).
type Direction int16

const (
	DirectionDebit  Direction = -1
	DirectionCredit Direction = 1
)

// Valid reports whether d is one of the two permitted directions.
func (d Direction) Valid() bool { return d == DirectionDebit || d == DirectionCredit }

// SignedMinor returns the signed contribution of this entry to a wallet
// balance. Integer arithmetic only.
func (d Direction) SignedMinor(amountMinor int64) int64 {
	if d == DirectionDebit {
		return -amountMinor
	}
	return amountMinor
}

// LedgerEntry is one immutable line of the ledger. It carries
// balance_after_minor so any historical balance is a single indexed read rather
// than a replay from zero.
type LedgerEntry struct {
	EntryID            string // entry_uid, the identifier exposed by the API
	WalletID           string
	TenantID           string
	OperationID        string
	BetID              string
	EntryType          EntryType
	Direction          Direction
	AmountMinor        int64
	Currency           Currency
	BalanceAfterMinor  int64
	ReversesEntryID    string
	Sequence           int64
	OccurredAtUnixNano int64
}

// CompensatingEntryFor returns the entry type and direction that reverses the
// given original entry. A ROLLBACK never rewrites the original: it appends this
// entry instead, referencing the original through ReversesEntryID
// (Constitution Principle II).
func CompensatingEntryFor(original EntryType) (EntryType, Direction, error) {
	switch original {
	case EntryBetDebit:
		return EntryRollbackCredit, DirectionCredit, nil
	case EntryWinCredit, EntryRefundCredit:
		return EntryRollbackDebit, DirectionDebit, nil
	case EntryRollbackCredit, EntryRollbackDebit:
		return "", 0, NewError(ReasonInvalidOperationState, "%s is already a compensating entry", original)
	default:
		return "", 0, NewError(ReasonInvalidOperationState, "%s is not a known entry type", original)
	}
}

// NewEntry validates and builds a ledger entry, so an invalid combination can
// never reach the database.
func NewEntry(e LedgerEntry) (LedgerEntry, error) {
	if !e.Direction.Valid() {
		return e, NewError(ReasonInvalidOperationState, "direction must be -1 or +1")
	}
	if e.AmountMinor <= 0 {
		return e, NewError(ReasonInvalidAmount, "amount_minor must be > 0; the sign lives in direction").WithField("amountMinor")
	}
	if e.BalanceAfterMinor < 0 {
		return e, NewError(ReasonInsufficientFunds, "balance_after_minor must be >= 0")
	}
	if err := ValidateCurrency(e.Currency); err != nil {
		return e, err
	}
	return e, nil
}
