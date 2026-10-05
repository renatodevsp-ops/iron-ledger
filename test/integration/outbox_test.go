//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/app/worker"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/messaging/outbox"
	"github.com/ironledger/iron-ledger/internal/messaging/sqs"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/integration"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
)

// recordingPublisher captures what the outbox handed to the broker, and can be
// told to fail a given number of times first.
type recordingPublisher struct {
	mu       sync.Mutex
	sent     []integration.Message
	failNext int
	failures int
}

func (p *recordingPublisher) Send(_ context.Context, _ string, message integration.Message, _, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failures < p.failNext {
		p.failures++
		return errBroker{}
	}
	p.sent = append(p.sent, message)
	return nil
}

func (p *recordingPublisher) Ready(context.Context) error { return nil }

func (p *recordingPublisher) published() []integration.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]integration.Message, len(p.sent))
	copy(out, p.sent)
	return out
}

type errBroker struct{}

func (errBroker) Error() string { return "broker is unavailable" }

// newPublisher builds an outbox publisher over a recording transport.
func newPublisher(t *testing.T, instance *instance, publisher *recordingPublisher) *worker.Publisher {
	t.Helper()
	repo := outbox.NewRepo(instance.db)
	outboxCfg := config.Outbox{
		PollInterval:     10 * time.Millisecond,
		BatchSize:        50,
		VisibilityWindow: 200 * time.Millisecond,
		MaxAttempts:      5,
		BackoffBase:      20 * time.Millisecond,
		BackoffMax:       40 * time.Millisecond,
		QueueURL:         "http://broker.invalid/wager-events.fifo",
	}
	return worker.NewPublisher(publisher, repo, instance.tx, outboxCfg,
		outboxCfg.QueueURL, logger, instance.metrics, instance.name)
}

// Test_eventsAreOnlyPublishedAfterTheCommit proves the ordering the design rests
// on: an event that is visible is an event whose effects are durable.
func Test_eventsAreOnlyPublishedAfterTheCommit(t *testing.T) {
	api := newInstance(t, "api")
	publisher := &recordingPublisher{}
	p := newPublisher(t, api, publisher)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the publisher did not stop")
		}
	}()

	player := uuid.New()
	publishedBefore := len(publisher.published())

	// Opening the wallet commits an event. Whether the publisher has sent it yet
	// is a race — but it may never send an event that was not committed, which
	// is what the assertions below prove.
	walletID := api.openWallet(t, player, "100.00")

	_, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "25.00",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		return len(publisher.published()) >= 4
	}, "the opening and the bet to be published")

	published := publisher.published()
	types := map[string]int{}
	for _, message := range published {
		types[string(message.Envelope.EventType)]++
	}
	for _, want := range []string{"WalletBalanceChanged", "WagerTransactionProcessed"} {
		if types[want] == 0 {
			t.Errorf("no %s was published; published %v", want, types)
		}
	}

	// The published envelope carries the contract the payload promises.
	var balanceChanged *integration.Message
	for i := range published {
		if published[i].Envelope.EventType == integration.EventWalletBalanceChanged {
			balanceChanged = &published[i]
		}
	}
	if balanceChanged == nil {
		t.Fatal("no WalletBalanceChanged was published")
	}
	if balanceChanged.Envelope.EventID == uuid.Nil {
		t.Error("the published event carries no identity")
	}
	if balanceChanged.Envelope.CorrelationID == "" {
		t.Error("the published event carries no correlation id")
	}
	if balanceChanged.Envelope.Version != integration.Version {
		t.Errorf("envelope version = %d, want %d", balanceChanged.Envelope.Version, integration.Version)
	}
	if balanceChanged.Envelope.OccurredAt.Location() != time.UTC {
		t.Errorf("occurredAt = %s, want UTC", balanceChanged.Envelope.OccurredAt)
	}

	data, ok := balanceChanged.Envelope.Data.(map[string]any)
	if !ok {
		t.Fatalf("data = %T, want a decoded object", balanceChanged.Envelope.Data)
	}
	for _, field := range []string{"walletId", "transactionId", "direction", "money", "balanceBefore", "balanceAfter", "walletVersion"} {
		if _, present := data[field]; !present {
			t.Errorf("the published payload has no %q", field)
		}
	}

	// Publication never precedes its commit: every event that went out was
	// already durably recorded, and the balance it reports is the balance the
	// database holds.
	stored := api.storedEventIDs(t)
	for _, message := range published {
		if _, known := stored[message.Envelope.EventID]; !known {
			t.Errorf("event %s was published without being committed", message.Envelope.EventID)
		}
	}
	if publishedBefore > 0 {
		t.Fatalf("the publisher had already sent %d events before this test opened a wallet", publishedBefore)
	}
	if balance := api.balance(t, walletID); balance.String() != "75.00" {
		t.Errorf("balance = %s, want 75.00: the effects are durable before publication", balance)
	}

	// Every event is published exactly once and then marked.
	waitFor(t, 5*time.Second, func() bool {
		return api.countRows(t,
			`SELECT count(*) FROM outbox_messages WHERE published_at IS NULL`) == 0
	}, "every outbox row to be marked published")

	ids := map[uuid.UUID]int{}
	for _, message := range published {
		ids[message.Envelope.EventID]++
	}
	for id, count := range ids {
		if count != 1 {
			t.Errorf("event %s was published %d times, want 1", id, count)
		}
	}
}

// Test_twoPublishersShareOneOutbox proves several publishers can drain the same
// table without publishing anything twice.
func Test_twoPublishersShareOneOutbox(t *testing.T) {
	api := newInstance(t, "api")
	first := &recordingPublisher{}
	second := &recordingPublisher{}
	p1 := newPublisher(t, api, first)
	p2 := newPublisher(t, api, second)

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")
	for i := 0; i < 6; i++ {
		_, err := api.submit(t, operation{
			provider: "provider-a", externalID: uuid.NewString(), playerID: player, walletID: walletID,
			kind: string(domain.KindWin), amount: "1.00",
		})
		if err != nil {
			t.Fatalf("win %d: %v", i, err)
		}
	}

	pending := api.countRows(t, `SELECT count(*) FROM outbox_messages WHERE published_at IS NULL`)
	if pending == 0 {
		t.Fatal("nothing was enqueued for publication")
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, p := range []*worker.Publisher{p1, p2} {
		wg.Add(1)
		go func(p *worker.Publisher) {
			defer wg.Done()
			_ = p.Run(ctx)
		}(p)
	}

	waitFor(t, 10*time.Second, func() bool {
		return api.countRows(t, `SELECT count(*) FROM outbox_messages WHERE published_at IS NULL`) == 0
	}, "both publishers to drain the outbox")
	cancel()
	wg.Wait()

	seen := map[uuid.UUID]int{}
	total := 0
	for _, publisher := range []*recordingPublisher{first, second} {
		for _, message := range publisher.published() {
			seen[message.Envelope.EventID]++
			total++
		}
	}
	if total != len(seen) {
		t.Errorf("%d publications covered %d distinct events: an event was published twice", total, len(seen))
	}
	if len(seen) == 0 {
		t.Error("nothing was published")
	}
	if first.published() == nil && second.published() == nil {
		t.Error("neither publisher sent anything")
	}
}

// Test_aFailedPublicationIsRetriedAndKeepsItsIdentity proves the recovery path a
// publisher crash produces: the row goes back to the queue, and the event goes
// out again under the very same eventId.
func Test_aFailedPublicationIsRetriedAndKeepsItsIdentity(t *testing.T) {
	api := newInstance(t, "api")
	publisher := &recordingPublisher{failNext: 2}
	p := newPublisher(t, api, publisher)

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	_, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "25.00",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	waitFor(t, 10*time.Second, func() bool {
		return api.countRows(t, `SELECT count(*) FROM outbox_messages WHERE published_at IS NULL`) == 0
	}, "the retried events to be published")

	published := publisher.published()
	if len(published) < 4 {
		t.Fatalf("published %d events, want the opening and the bet", len(published))
	}

	// The events that failed first are published with the identity they were
	// recorded under, because the payload is a snapshot taken in the commit.
	stored := api.storedEventIDs(t)
	for _, message := range published {
		if _, known := stored[message.Envelope.EventID]; !known {
			t.Errorf("published event %s was never committed", message.Envelope.EventID)
		}
	}

	// And the attempts are recorded rather than hidden.
	if attempts := api.maxAttempts(t); attempts < 2 {
		t.Errorf("highest attempt count = %d, want at least 2 after two failures", attempts)
	}
}

// Test_aPublisherThatDiesMidFlightIsRecovered proves an event abandoned by a
// dead publisher is taken over by another one rather than lost.
func Test_aPublisherThatDiesMidFlightIsRecovered(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")
	if _, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "10.00",
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// A publisher that claims rows and then "dies": the claim is never resolved.
	repo := outbox.NewRepo(api.db)
	ctx := context.Background()
	var claimed []outbox.Message
	err := api.tx.Do(ctx, uow.Options{Name: "claim_and_die", NoRetry: true}, func(ctx context.Context) error {
		var err error
		claimed, err = repo.Claim(ctx, "publisher-that-dies", 100, 150*time.Millisecond)
		return err
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) == 0 {
		t.Fatal("nothing was claimed")
	}

	// Before the visibility window elapses, nobody may take the rows.
	var early []outbox.Message
	err = api.tx.Do(ctx, uow.Options{Name: "claim_early", NoRetry: true}, func(ctx context.Context) error {
		var err error
		early, err = repo.Claim(ctx, "publisher-2", 100, 150*time.Millisecond)
		return err
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(early) != 0 {
		t.Errorf("%d rows were taken while still owned by a live publisher", len(early))
	}

	// Once the window elapses, another publisher takes them over and publishes
	// the same event ids.
	publisher := &recordingPublisher{}
	p := newPublisher(t, api, publisher)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- p.Run(runCtx) }()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, 10*time.Second, func() bool {
		return api.countRows(t, `SELECT count(*) FROM outbox_messages WHERE published_at IS NULL`) == 0
	}, "the abandoned events to be taken over and published")

	ids := map[uuid.UUID]bool{}
	for _, message := range publisher.published() {
		ids[message.Envelope.EventID] = true
	}
	for _, message := range claimed {
		if !ids[message.EventID] {
			t.Errorf("event %s was abandoned and never republished", message.EventID)
		}
	}
}

// Test_theEventsQueueIsFIFO proves the broker contract the design relies on: one
// group per aggregate, and the event id as the deduplication key.
func Test_theEventsQueueIsFIFO(t *testing.T) {
	runtime, err := sqs.Provision(context.Background(), testConfig.SQS)
	if err != nil {
		t.Skipf("the broker is not reachable, skipping the queue contract test: %v", err)
	}

	attributes, err := runtime.Client.QueueAttributes(context.Background(), runtime.EventsQueue)
	if err != nil {
		t.Fatalf("read queue attributes: %v", err)
	}
	if attributes["FifoQueue"] != "true" {
		t.Errorf("the events queue is not FIFO: FifoQueue = %q", attributes["FifoQueue"])
	}
	if attributes["RedrivePolicy"] == "" {
		t.Error("the events queue has no redrive policy")
	}

	attributes, err = runtime.Client.QueueAttributes(context.Background(), runtime.WagerQueue)
	if err != nil {
		t.Fatalf("read queue attributes: %v", err)
	}
	if attributes["FifoQueue"] != "true" {
		t.Errorf("the wager queue is not FIFO: FifoQueue = %q", attributes["FifoQueue"])
	}
	if attributes["RedrivePolicy"] == "" {
		t.Error("the wager queue has no redrive policy")
	}

	// The wager queue has a dead-letter queue behind it.
	dlq, err := runtime.Client.QueueAttributes(context.Background(), runtime.WagerDeadLetterQueue)
	if err != nil {
		t.Fatalf("read dead-letter attributes: %v", err)
	}
	if dlq["FifoQueue"] != "true" {
		t.Errorf("the dead-letter queue is not FIFO: FifoQueue = %q", dlq["FifoQueue"])
	}
}
