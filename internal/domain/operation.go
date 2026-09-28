package domain

// OperationType enumerates the five financial operations.
type OperationType string

const (
	OperationBET      OperationType = "BET"
	OperationWin      OperationType = "WIN"
	OperationLoss     OperationType = "LOSS"
	OperationRefund   OperationType = "REFUND"
	OperationRollback OperationType = "ROLLBACK"
)

// ValidOperationType reports whether t is one of the five released types.
func ValidOperationType(t OperationType) bool {
	switch t {
	case OperationBET, OperationWin, OperationLoss, OperationRefund, OperationRollback:
		return true
	default:
		return false
	}
}

// RequiresBetID reports whether the type must carry betId. ROLLBACK addresses
// the target through operationId instead, but it still needs the bet to
// transition, so it is included.
func RequiresBetID(t OperationType) bool {
	switch t {
	case OperationWin, OperationLoss, OperationRefund, OperationRollback:
		return true
	default:
		return false
	}
}

// Channel identifies the entry point a request arrived through. Both channels
// call the same use case, so the financial guarantees are identical (FR-003).
type Channel string

const (
	ChannelAPI Channel = "API"
	ChannelSQS Channel = "SQS"
)

// Operation is the validated, transport-independent request. It matches the
// body of POST /v1/wallets/{walletId}/operations and, byte for byte, the
// `operation` object of a wallet-ops.fifo message.
type Operation struct {
	Type          OperationType
	Amount        Money
	TransactionID string
	BetID         string
	OperationID   string // target operation, required for ROLLBACK
	ReferenceID   string // optional external bet reference, BET only
}

// Validate enforces the contract's field rules before anything touches the
// database. Every monetary check here is integer-only.
func (o Operation) Validate() error {
	if !ValidOperationType(o.Type) {
		return NewError(ReasonUnknownOperationType, "unknown operation type %q", string(o.Type)).WithField("type")
	}
	if err := ValidateCurrency(o.Amount.Currency); err != nil {
		return err
	}
	if !o.Amount.IsPositive() {
		return NewError(ReasonInvalidAmount,
			"amountMinor must be a positive integer; precision finer than the currency minor unit is not accepted").
			WithField("amountMinor")
	}
	if o.TransactionID == "" {
		return NewError(ReasonMissingField, "transactionId is required").WithField("transactionId")
	}
	if len(o.TransactionID) > 128 {
		return NewError(ReasonInvalidAmount, "transactionId must be at most 128 characters").WithField("transactionId")
	}
	if RequiresBetID(o.Type) && o.BetID == "" {
		return NewError(ReasonMissingField, "betId is required for %s", o.Type).WithField("betId")
	}
	if o.Type == OperationRollback && o.OperationID == "" {
		return NewError(ReasonMissingField, "operationId is required for ROLLBACK").WithField("operationId")
	}
	if len(o.ReferenceID) > 128 {
		return NewError(ReasonInvalidAmount, "referenceId must be at most 128 characters").WithField("referenceId")
	}
	return nil
}
