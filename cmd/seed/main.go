// Command seed publishes a wager operation onto the wager queue.
//
// It exists so the asynchronous path can be exercised by hand: it puts a
// message on the broker in exactly the shape an upstream game provider's
// integration would, and the worker does the rest. It is a development and
// demonstration tool — the HTTP API is the supported way to submit an operation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/messaging/sqs"
	"github.com/ironledger/iron-ledger/internal/platform/config"
)

func main() {
	messageID := flag.String("message-id", "msg-"+uuid.NewString(),
		"identity of the message; the inbox deduplicates on it")
	queue := flag.String("queue", "", "queue URL; resolved from the broker when omitted")
	body := flag.String("body", "", "the `data` object; read from stdin when omitted")
	group := flag.String("group", "seed", "FIFO message group; one per wallet keeps a wallet's operations in order")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fatal("%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runtime, err := sqs.Provision(ctx, cfg.SQS)
	if err != nil {
		fatal("%v", err)
	}
	target := *queue
	if target == "" {
		target = runtime.WagerQueue
	}

	raw := []byte(*body)
	if len(raw) == 0 {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			fatal("read body: %v", err)
		}
	}
	if len(raw) == 0 {
		fatal("a message body is required, on -body or on stdin")
	}

	// The envelope mirrors what an upstream publisher sends. The consumer reads
	// messageId as the durable identity of the message, so re-running this
	// command with the same -message-id is a duplicate the inbox absorbs.
	envelope := struct {
		MessageID  string          `json:"messageId"`
		Type       string          `json:"type"`
		OccurredAt time.Time       `json:"occurredAt"`
		Data       json.RawMessage `json:"data"`
	}{
		MessageID:  *messageID,
		Type:       "WagerTransactionRequested",
		OccurredAt: time.Now().UTC(),
		Data:       json.RawMessage(raw),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		fatal("encode message: %v", err)
	}

	if err := runtime.Client.SendRaw(ctx, target, string(encoded), *group, *messageID); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("seeded %s into %s\n", *messageID, target)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "seed: "+format+"\n", args...)
	os.Exit(1)
}
