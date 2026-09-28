package domain

// BetStatus enumerates the bet lifecycle states. The transitions below are the
// only legal ones; every other combination is rejected (data-model.md).
type BetStatus string

const (
	BetOpen        BetStatus = "OPEN"
	BetSettledWin  BetStatus = "SETTLED_WIN"
	BetSettledLoss BetStatus = "SETTLED_LOSS"
	BetVoided      BetStatus = "VOIDED"
	BetReversed    BetStatus = "REVERSED"
)

// Bet is the aggregate a wallet operation settles or corrects.
type Bet struct {
	ID                   string
	WalletID             string
	TenantID             string
	ExternalBetRef       string
	Stake                Money
	Status               BetStatus
	SettledByOperationID string
	CreatedAt            int64
	SettledAt            int64
}

func NewBet(id, walletID, tenantID, externalRef string, stake Money) Bet {
	return Bet{
		ID:             id,
		WalletID:       walletID,
		TenantID:       tenantID,
		ExternalBetRef: externalRef,
		Stake:          stake,
		Status:         BetOpen,
		CreatedAt:      0,
	}
}

// Settle applies a WIN or LOSS outcome. Only an OPEN bet can be settled, and a
// bet can be settled exactly once.
func (b Bet) Settle(outcome OperationType, operationID string) (Bet, error) {
	if b.Status != BetOpen {
		return b, NewError(ReasonBetAlreadySettled, "bet %s is already %s", b.ID, b.Status)
	}
	var next BetStatus
	switch outcome {
	case OperationWin:
		next = BetSettledWin
	case OperationLoss:
		next = BetSettledLoss
	default:
		return b, NewError(ReasonInvalidOperationState, "%s does not settle a bet", outcome)
	}
	if operationID == "" {
		return b, NewError(ReasonInvalidOperationState, "settlement requires the settling operation id")
	}
	out := b
	out.Status = next
	out.SettledByOperationID = operationID
	return out, nil
}

// Void is the REFUND path: an OPEN bet becomes VOIDED.
func (b Bet) Void(operationID string) (Bet, error) {
	if b.Status != BetOpen {
		return b, NewError(ReasonInvalidOperationState, "bet %s is %s and cannot be refunded", b.ID, b.Status)
	}
	out := b
	out.Status = BetVoided
	out.SettledByOperationID = operationID
	return out, nil
}

// Reverse is the ROLLBACK path. It is reachable from SETTLED_WIN and
// SETTLED_LOSS, and also from OPEN and VOIDED when a bet registration itself is
// reversed (D-4: a settled bet can be reversed).
func (b Bet) Reverse(operationID string) (Bet, error) {
	switch b.Status {
	case BetSettledWin, BetSettledLoss, BetOpen, BetVoided:
	default:
		return b, NewError(ReasonInvalidOperationState, "bet %s is %s and cannot be reversed", b.ID, b.Status)
	}
	if operationID == "" {
		return b, NewError(ReasonInvalidOperationState, "reversal requires the reversing operation id")
	}
	out := b
	out.Status = BetReversed
	out.SettledByOperationID = operationID
	return out, nil
}

// MovesMoney reports whether settling this bet changes the wallet balance. A
// LOSS moves no money, which is why it appends no ledger entry by design.
func (b Bet) MovesMoney(outcome OperationType) bool { return outcome == OperationWin }

// IsSettled reports whether the bet already carries a terminal outcome.
func (b Bet) IsSettled() bool {
	switch b.Status {
	case BetSettledWin, BetSettledLoss, BetVoided, BetReversed:
		return true
	default:
		return false
	}
}
