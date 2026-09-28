package usecase

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ironledger/ironledger/internal/domain"
)

// This file provides in-memory implementations of the domain ports so the
// application's decision logic can be tested without a database. The fake
// UnitOfWork really does snapshot and restore its state, so a test that
// exercises a rejection proves nothing was written, which is the property the
// real transaction gives us.

type fakeState struct {
	wallets    map[string]*domain.Wallet
	bets       map[string]*domain.Bet
	entries    []domain.LedgerEntry
	operations map[string]*domain.OperationRecord // keyed by walletID + "\x00" + idempotencyKey
	byTxn      map[string]*domain.OperationRecord // keyed by walletID + "\x00" + transactionID
	audit      []domain.AuditRecord
	outbox     []domain.OutboxEvent
	nextSeq    map[string]int64
}

func newFakeState() *fakeState {
	return &fakeState{
		wallets:    map[string]*domain.Wallet{},
		bets:       map[string]*domain.Bet{},
		operations: map[string]*domain.OperationRecord{},
		byTxn:      map[string]*domain.OperationRecord{},
		nextSeq:    map[string]int64{},
	}
}

func (s *fakeState) clone() *fakeState {
	out := &fakeState{
		wallets:    make(map[string]*domain.Wallet, len(s.wallets)),
		bets:       make(map[string]*domain.Bet, len(s.bets)),
		operations: make(map[string]*domain.OperationRecord, len(s.operations)),
		byTxn:      make(map[string]*domain.OperationRecord, len(s.byTxn)),
		nextSeq:    make(map[string]int64, len(s.nextSeq)),
		entries:    append([]domain.LedgerEntry(nil), s.entries...),
		audit:      append([]domain.AuditRecord(nil), s.audit...),
		outbox:     append([]domain.OutboxEvent(nil), s.outbox...),
	}
	for k, v := range s.wallets {
		c := *v
		out.wallets[k] = &c
	}
	for k, v := range s.bets {
		c := *v
		out.bets[k] = &c
	}
	for k, v := range s.operations {
		c := *v
		out.operations[k] = &c
	}
	for k, v := range s.byTxn {
		c := *v
		out.byTxn[k] = &c
	}
	for k, v := range s.nextSeq {
		out.nextSeq[k] = v
	}
	return out
}

func (s *fakeState) restore(from *fakeState) {
	*s = *from
}

type fakeLedger struct {
	state *fakeState
	calls *[]string
}

func (r *fakeLedger) record(name string) { *r.calls = append(*r.calls, name) }

func (r *fakeLedger) LockWallet(_ context.Context, walletID string) (*domain.Wallet, error) {
	r.record("LockWallet")
	w, ok := r.state.wallets[walletID]
	if !ok {
		return nil, domain.ErrWalletNotFound
	}
	c := *w
	return &c, nil
}

func (r *fakeLedger) FindWallet(_ context.Context, walletID string) (*domain.Wallet, error) {
	r.record("FindWallet")
	w, ok := r.state.wallets[walletID]
	if !ok {
		return nil, domain.ErrWalletNotFound
	}
	c := *w
	return &c, nil
}

func (r *fakeLedger) UpdateBalance(_ context.Context, walletID string, delta int64) error {
	r.record("UpdateBalance")
	w, ok := r.state.wallets[walletID]
	if !ok {
		return domain.ErrWalletNotFound
	}
	if w.Balance.AmountMinor+delta < 0 {
		return domain.ErrInsufficientFunds.WithAvailableMinor(w.Balance.AmountMinor)
	}
	w.Balance.AmountMinor += delta
	w.Version++
	return nil
}

func (r *fakeLedger) NextSequence(_ context.Context, walletID string) (int64, error) {
	r.record("NextSequence")
	return r.state.nextSeq[walletID] + 1, nil
}

func (r *fakeLedger) AppendEntry(_ context.Context, entry domain.LedgerEntry) error {
	r.record("AppendEntry")
	if _, exists := r.state.bets[entry.BetID]; !exists && entry.BetID != "" {
		return domain.ErrBetNotFound
	}
	r.state.nextSeq[entry.WalletID] = entry.Sequence
	r.state.entries = append(r.state.entries, entry)
	return nil
}

func (r *fakeLedger) CreateBet(_ context.Context, bet domain.Bet) error {
	r.record("CreateBet")
	if _, exists := r.state.bets[bet.ID]; exists {
		return domain.ErrReferenceAlreadyExists
	}
	c := bet
	r.state.bets[bet.ID] = &c
	return nil
}

func (r *fakeLedger) GetBet(_ context.Context, betID string) (*domain.Bet, error) {
	b, ok := r.state.bets[betID]
	if !ok {
		return nil, domain.ErrBetNotFound
	}
	c := *b
	return &c, nil
}

func (r *fakeLedger) LockBet(ctx context.Context, betID string) (*domain.Bet, error) {
	r.record("LockBet")
	return r.GetBet(ctx, betID)
}

func (r *fakeLedger) SetBetStatus(_ context.Context, bet domain.Bet) error {
	r.record("SetBetStatus")
	if _, ok := r.state.bets[bet.ID]; !ok {
		return domain.ErrBetNotFound
	}
	c := bet
	r.state.bets[bet.ID] = &c
	return nil
}

func (r *fakeLedger) FindBetByExternalRef(_ context.Context, tenantID, ref string) (*domain.Bet, error) {
	for _, b := range r.state.bets {
		if b.TenantID == tenantID && b.ExternalBetRef == ref {
			c := *b
			return &c, nil
		}
	}
	return nil, nil
}

func (r *fakeLedger) FindEntriesByOperation(_ context.Context, operationID string) ([]domain.LedgerEntry, error) {
	var out []domain.LedgerEntry
	for _, e := range r.state.entries {
		if e.OperationID == operationID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r *fakeLedger) CountEntriesByType(_ context.Context, walletID, betID string, t domain.EntryType) (int64, error) {
	var n int64
	for _, e := range r.state.entries {
		if e.WalletID == walletID && e.BetID == betID && e.EntryType == t {
			n++
		}
	}
	return n, nil
}

type fakeOperations struct {
	state *fakeState
}

func txnKey(walletID, transactionID string) string { return walletID + "\x00" + transactionID }

func (r *fakeOperations) ClaimIdempotency(_ context.Context, rec domain.OperationRecord) (bool, error) {
	key := txnKey(rec.WalletID, rec.IdempotencyKey)
	if _, exists := r.state.operations[key]; exists {
		return false, nil
	}
	c := rec
	c.ResultBody = append([]byte(nil), rec.ResultBody...)
	c.RequestHash = append([]byte(nil), rec.RequestHash...)
	r.state.operations[key] = &c
	r.state.byTxn[txnKey(rec.WalletID, rec.TransactionID)] = &c
	return true, nil
}

func (r *fakeOperations) GetByIdempotencyKey(_ context.Context, walletID, key string) (*domain.OperationRecord, error) {
	rec, ok := r.state.operations[txnKey(walletID, key)]
	if !ok {
		return nil, nil
	}
	c := *rec
	return &c, nil
}

func (r *fakeOperations) FindByTransactionID(_ context.Context, walletID, transactionID string) (*domain.OperationRecord, error) {
	rec, ok := r.state.byTxn[txnKey(walletID, transactionID)]
	if !ok {
		return nil, nil
	}
	c := *rec
	return &c, nil
}

func (r *fakeOperations) FindByID(_ context.Context, operationID string) (*domain.OperationRecord, error) {
	for _, rec := range r.state.operations {
		if rec.ID == operationID {
			c := *rec
			return &c, nil
		}
	}
	return nil, nil
}

type fakeAudit struct{ state *fakeState }

func (r *fakeAudit) Record(_ context.Context, rec domain.AuditRecord) error {
	r.state.audit = append(r.state.audit, rec)
	return nil
}

type fakeOutbox struct{ state *fakeState }

func (r *fakeOutbox) Enqueue(_ context.Context, e domain.OutboxEvent) error {
	r.state.outbox = append(r.state.outbox, e)
	return nil
}

// fakeUOW snapshots the state at the start of Do and restores it whenever the
// closure returns an error, so a rolled-back transaction leaves no trace.
type fakeUOW struct {
	mu    sync.Mutex
	state *fakeState
	calls *[]string
}

func (u *fakeUOW) Do(ctx context.Context, fn func(context.Context, domain.Tx) error) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	snapshot := u.state.clone()
	tx := domain.Tx{
		Ledger:     &fakeLedger{state: u.state, calls: u.calls},
		Operations: &fakeOperations{state: u.state},
		Audit:      &fakeAudit{state: u.state},
		Outbox:     &fakeOutbox{state: u.state},
	}
	if err := fn(ctx, tx); err != nil {
		u.state.restore(snapshot)
		return err
	}
	return nil
}

// fixedClock pins business time so result bodies are byte-comparable.
type fixedClock struct{ t int64 }

func (c fixedClock) Now() time.Time { return time.Unix(0, c.t).UTC() }

// seqIDs produces predictable, distinct UUID-shaped identifiers.
type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (s *seqIDs) NewUUID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", s.n)
}
