package integration

import (
	"context"
	"testing"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/ironledger/ironledger/internal/platform/idgen"
	"github.com/ironledger/ironledger/internal/platform/postgres"
	"github.com/ironledger/ironledger/internal/usecase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T035: idempotency survives a full restart of the process. The guarantee cannot
// depend on anything held in memory, so this test commits a request, throws away
// every connection and every object, then rebuilds the whole stack from the DSN
// and replays the same key. If the record were only in memory, or only in a
// connection-scoped transaction, this test would see a second debit.

// restart drops the shared pool and builds a brand new one, standing in for the
// process going away: no pool, no service, no cached state survives.
func restart(t *testing.T, dsn string) *usecase.Service {
	t.Helper()
	if sharedPool != nil {
		sharedPool.Close()
		sharedPool = nil
	}

	ctx := context.Background()
	pool, err := postgres.Connect(ctx, postgres.DefaultPoolConfig(dsn))
	require.NoError(t, err, "reconnect after restart")

	sharedPool = pool
	uow := postgres.NewUnitOfWork(pool, postgres.DefaultRetryPolicy(), nil)
	sharedSvc = usecase.New(uow, domain.SystemClock{}, idgen.New())
	return sharedSvc
}

func TestT035_IdempotencySurvivesProcessRestart(t *testing.T) {
	walletID := newWallet(t, 100_00, domain.BRL, domain.WalletActive)

	first, err := applyBet(t, walletID, "restart-key", 30_00, domain.BRL)
	require.NoError(t, err)
	assert.False(t, first.Replay)
	assert.Equal(t, int64(70_00), balanceOf(t, walletID))

	svc := restart(t, testDSN)

	out, err := svc.Apply(context.Background(), usecase.ApplyInput{
		WalletID:       walletID,
		TenantID:       testTenantID,
		IdempotencyKey: "restart-key",
		Operation: domain.Operation{
			Type:          domain.OperationBET,
			Amount:        domain.Money{AmountMinor: 30_00, Currency: domain.BRL},
			TransactionID: "txn-restart-key",
		},
		Channel: domain.ChannelAPI,
		Actor:   testActor,
	})
	require.NoError(t, err)
	assert.True(t, out.Replay, "after a restart the same key must replay, not apply again")
	assert.Equal(t, string(first.Body), string(out.Body),
		"the replayed body must be the original, byte for byte")

	assert.Equal(t, int64(70_00), balanceOf(t, walletID), "the restart must not have double-debited")
	assert.Equal(t, 1, countRows(t, `SELECT count(*)::int FROM ledger_entries WHERE wallet_id = $1`, walletID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*)::int FROM operations WHERE wallet_id = $1`, walletID))
}
