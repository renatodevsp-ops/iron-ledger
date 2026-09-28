package domain

// WalletStatus is the lifecycle state of a wallet. A FROZEN wallet rejects
// every mutation; balance reads stay available.
type WalletStatus string

const (
	WalletActive WalletStatus = "ACTIVE"
	WalletFrozen WalletStatus = "FROZEN"
)

// Wallet is the aggregate that owns a balance. It is immutable: Debit and
// Credit return a new Wallet, so a rejected mutation cannot leave a partially
// updated value behind in memory.
type Wallet struct {
	ID        string
	TenantID  string
	PlayerID  string
	Currency  Currency
	Balance   Money
	Status    WalletStatus
	Version   int64
	UpdatedAt int64 // unix nanoseconds; diagnostics only
}

func NewWallet(id, tenantID, playerID string, currency Currency, balance int64) Wallet {
	return Wallet{
		ID:       id,
		TenantID: tenantID,
		PlayerID: playerID,
		Currency: currency,
		Balance:  Money{AmountMinor: balance, Currency: currency},
		Status:   WalletActive,
	}
}

// Debit removes amount from the wallet. It rejects a frozen wallet, a currency
// that differs from the wallet's, a non-positive amount and a balance that
// would go negative. The database CHECK is the final authority; this is the
// fail-fast layer.
func (w Wallet) Debit(amount Money) (Wallet, error) {
	if err := w.mutable(); err != nil {
		return w, err
	}
	if err := w.requireSameCurrency(amount); err != nil {
		return w, err
	}
	if !amount.IsPositive() {
		return w, NewError(ReasonInvalidAmount, "debit amount must be a positive integer in the currency minor unit").WithField("amountMinor")
	}
	if w.Balance.AmountMinor < amount.AmountMinor {
		return w, NewError(ReasonInsufficientFunds,
			"wallet balance %d would become %d", w.Balance.AmountMinor, w.Balance.AmountMinor-amount.AmountMinor).
			WithAvailableMinor(w.Balance.AmountMinor).
			WithWalletCurrency(w.Currency)
	}
	next := w
	next.Balance = Money{AmountMinor: w.Balance.AmountMinor - amount.AmountMinor, Currency: w.Currency}
	return next, nil
}

// Credit adds amount to the wallet under the same rules as Debit.
func (w Wallet) Credit(amount Money) (Wallet, error) {
	if err := w.mutable(); err != nil {
		return w, err
	}
	if err := w.requireSameCurrency(amount); err != nil {
		return w, err
	}
	if !amount.IsPositive() {
		return w, NewError(ReasonInvalidAmount, "credit amount must be a positive integer in the currency minor unit").WithField("amountMinor")
	}
	next := w
	next.Balance = Money{AmountMinor: w.Balance.AmountMinor + amount.AmountMinor, Currency: w.Currency}
	return next, nil
}

func (w Wallet) mutable() error {
	if w.Status != WalletActive {
		return NewError(ReasonWalletFrozen, "wallet %s is %s", w.ID, w.Status)
	}
	return nil
}

func (w Wallet) requireSameCurrency(amount Money) error {
	if amount.Currency != w.Currency {
		return NewError(ReasonCurrencyMismatch,
			"operation currency %s does not match wallet currency %s", amount.Currency, w.Currency).
			WithField("currency").
			WithWalletCurrency(w.Currency)
	}
	return nil
}
