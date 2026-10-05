//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Test_twoBetsOverOneBalance is the mandatory race: a wallet holding 100.00 BRL
// receives two distinct bets of 80.00 BRL at the same moment. Exactly one must
// be processed, one must be refused for insufficient funds, the final balance
// must be 20.00 and the ledger must hold a single debit.
func Test_twoBetsOverOneBalance(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	type outcome struct {
		status string
		code   xerr.Code
		err    error
	}
	outcomes := make([]outcome, 2)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	for i := range outcomes {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait()
			result, err := api.submit(t, operation{
				provider:   "provider-a",
				externalID: uuid.NewString(),
				playerID:   player,
				walletID:   walletID,
				kind:       string(domain.KindBet),
				amount:     "80.00",
			})
			o := outcome{err: err}
			if result != nil {
				o.status = result.Status
				o.code = result.FailureCode
			}
			outcomes[index] = o
		}(i)
	}

	start.Done()
	done.Wait()

	processed, insufficient := 0, 0
	for _, o := range outcomes {
		switch {
		case o.err != nil:
			t.Fatalf("unexpected failure: %v", o.err)
		case o.status == string(domain.StatusProcessed):
			processed++
		case o.status == string(domain.StatusRejected) && o.code == xerr.CodeInsufficientBalance:
			insufficient++
		default:
			t.Fatalf("unexpected outcome: status=%s code=%s", o.status, o.code)
		}
	}
	if processed != 1 || insufficient != 1 {
		t.Fatalf("processed = %d, insufficient = %d; want 1 and 1", processed, insufficient)
	}

	if balance := api.balance(t, walletID); balance.String() != "20.00" {
		t.Errorf("balance = %s, want 20.00", balance)
	}
	if debits := api.countDebits(t, walletID); debits != 1 {
		t.Errorf("ledger holds %d debits, want exactly 1", debits)
	}

	report := api.reconcile(t, walletID)
	if !report.Consistent {
		t.Errorf("reconciliation diverged: stored %s, calculated %s",
			report.StoredBalance, report.CalculatedBalance)
	}
}

// Test_fiftyConcurrentDuplicatesMoveTheWalletOnce sends the very same bet fifty
// times at the same moment and proves the wallet moved once.
func Test_fiftyConcurrentDuplicatesMoveTheWalletOnce(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "500.00")
	externalID := uuid.NewString()

	const deliveries = 50
	var wg sync.WaitGroup
	results := make([]*domainStatus, deliveries)
	errs := make([]error, deliveries)

	start := make(chan struct{})
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			result, err := api.submit(t, operation{
				provider:   "provider-a",
				externalID: externalID,
				playerID:   player,
				walletID:   walletID,
				kind:       string(domain.KindBet),
				amount:     "25.00",
			})
			if result != nil {
				results[index] = &domainStatus{status: result.Status, replay: result.IdempotentReplay}
			}
			errs[index] = err
		}(i)
	}
	close(start)
	wg.Wait()

	processed, replays := 0, 0
	for i := 0; i < deliveries; i++ {
		if errs[i] != nil {
			t.Fatalf("delivery %d failed: %v", i, errs[i])
		}
		switch {
		case results[i].status == string(domain.StatusProcessed) && !results[i].replay:
			processed++
		case results[i].status == string(domain.StatusProcessed) && results[i].replay:
			replays++
		default:
			t.Fatalf("delivery %d: status=%s replay=%t", i, results[i].status, results[i].replay)
		}
	}
	if processed != 1 {
		t.Errorf("%d deliveries claimed to be the first, want exactly 1", processed)
	}
	if replays != deliveries-1 {
		t.Errorf("%d deliveries were recognised as replays, want %d", replays, deliveries-1)
	}

	if balance := api.balance(t, walletID); balance.String() != "475.00" {
		t.Errorf("balance = %s, want 475.00 after one debit of 25.00", balance)
	}
	if debits := api.countDebits(t, walletID); debits != 1 {
		t.Errorf("ledger holds %d debits, want 1", debits)
	}
}

// Test_distinctWalletsAdvanceInParallel proves the coordination is per wallet:
// money in one wallet is never blocked by a long transaction in another.
func Test_distinctWalletsAdvanceInParallel(t *testing.T) {
	api := newInstance(t, "api")

	const wallets = 8
	const betsPerWallet = 6

	ids := make([]uuid.UUID, wallets)
	for i := range ids {
		player := uuid.New()
		ids[i] = api.openWallet(t, player, "100.00")
		_ = player
	}

	var wg sync.WaitGroup
	failures := make(chan error, wallets*betsPerWallet)

	for _, walletID := range ids {
		for b := 0; b < betsPerWallet; b++ {
			wg.Add(1)
			go func(walletID uuid.UUID, bet int) {
				defer wg.Done()
				view, err := api.wallets.Get(context.Background(), walletID)
				if err != nil {
					failures <- err
					return
				}
				result, err := api.submit(t, operation{
					provider:   "provider-a",
					externalID: uuid.NewString(),
					playerID:   uuid.MustParse(view.PlayerID),
					walletID:   walletID,
					kind:       string(domain.KindBet),
					amount:     "10.00",
				})
				if err != nil {
					failures <- err
					return
				}
				if result.Status != string(domain.StatusProcessed) {
					failures <- errUnexpected{status: result.Status}
				}
			}(walletID, b)
		}
	}
	wg.Wait()
	close(failures)

	for err := range failures {
		t.Fatalf("a parallel operation failed: %v", err)
	}

	for _, walletID := range ids {
		if balance := api.balance(t, walletID); balance.String() != "40.00" {
			t.Errorf("wallet %s balance = %s, want 40.00", walletID, balance)
		}
		if debits := api.countDebits(t, walletID); debits != betsPerWallet {
			t.Errorf("wallet %s holds %d debits, want %d", walletID, debits, betsPerWallet)
		}
		if report := api.reconcile(t, walletID); !report.Consistent {
			t.Errorf("wallet %s diverged", walletID)
		}
	}
}

// Test_threeInstancesShareTheSameWallet runs the mandatory race across three
// independent processes' worth of state — separate pools, separate event stores,
// separate use cases — which is the only way to show the guarantees are not an
// artefact of one process's memory.
func Test_threeInstancesShareTheSameWallet(t *testing.T) {
	instances := []*instance{
		newInstance(t, "instance-1"),
		newInstance(t, "instance-2"),
		newInstance(t, "instance-3"),
	}

	player := uuid.New()
	walletID := instances[0].openWallet(t, player, "100.00")

	type outcome struct {
		instance string
		status   string
		code     xerr.Code
	}
	outcomes := make([]outcome, 6)

	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i := 0; i < 6; i++ {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			instance := instances[index%len(instances)]
			start.Wait()
			result, err := instance.submit(t, operation{
				provider:   "provider-a",
				externalID: uuid.NewString(),
				playerID:   player,
				walletID:   walletID,
				kind:       string(domain.KindBet),
				amount:     "40.00",
			})
			o := outcome{instance: instance.name}
			if err != nil {
				o.status = "ERROR: " + err.Error()
			} else {
				o.status = result.Status
				o.code = result.FailureCode
			}
			outcomes[index] = o
		}(i)
	}
	start.Done()
	done.Wait()

	processed, insufficient := 0, 0
	for _, o := range outcomes {
		switch {
		case o.status == string(domain.StatusProcessed):
			processed++
		case o.status == string(domain.StatusRejected) && o.code == xerr.CodeInsufficientBalance:
			insufficient++
		default:
			t.Errorf("instance %s: unexpected outcome status=%s code=%s", o.instance, o.status, o.code)
		}
	}
	if processed != 2 || insufficient != 4 {
		t.Errorf("processed = %d, insufficient = %d; want 2 and 4 (100.00 funds two 40.00 bets)", processed, insufficient)
	}

	// Every instance must observe the same final state.
	for _, instance := range instances {
		if balance := instance.balance(t, walletID); balance.String() != "20.00" {
			t.Errorf("%s sees balance %s, want 20.00", instance.name, balance)
		}
		if report := instance.reconcile(t, walletID); !report.Consistent {
			t.Errorf("%s sees a diverged wallet", instance.name)
		}
	}
	if debits := instances[0].countDebits(t, walletID); debits != 2 {
		t.Errorf("ledger holds %d debits, want 2", debits)
	}
}

// domainStatus is a tiny local shape so the concurrency tests can record what
// each delivery answered without importing the application result type.
type domainStatus struct {
	status string
	replay bool
}

type errUnexpected struct{ status string }

func (e errUnexpected) Error() string { return "unexpected status " + e.status }
