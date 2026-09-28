# SQS Contract: `wallet-ops.fifo`

**Feature**: 001-distributed-betting-ledger | **Date**: 2026-09-28

The queue this service **consumes**. The same application use case backs this channel and the
HTTP API, so the financial guarantees in `spec.md` FR-003 hold identically on both.

---

## Queue topology

| Queue | Type | Purpose |
|-------|------|---------|
| `wallet-ops.fifo` | FIFO | Inbound wallet operations |
| `wallet-ops-dlq.fifo` | FIFO | Messages that exhausted retries or failed permanently |

Both must exist in both AWS and LocalStack, with these attributes. The `.fifo` suffix alone
is **not** sufficient — `FifoQueue=true` is required too.

```json
{
  "QueueName": "wallet-ops.fifo",
  "Attributes": {
    "FifoQueue": "true",
    "ContentBasedDeduplication": "false",
    "VisibilityTimeout": "60",
    "ReceiveMessageWaitTimeSeconds": "20",
    "MessageRetentionPeriod": "1209600",
    "RedrivePolicy": {
      "deadLetterTargetArn": "arn:aws:sqs:<region>:<account>:wallet-ops-dlq.fifo",
      "maxReceiveCount": 5
    }
  }
}
```

Rationale for the values:

- `ContentBasedDeduplication=false` + explicit `MessageDeduplicationId` — the dedup key is
  the caller's idempotency key, so two legitimately identical requests are not collapsed.
- `VisibilityTimeout=60` — comfortably longer than p95 processing latency (~300 ms) and
  longer than the SDK HTTP read timeout. A timeout too short delivers a duplicate while the
  first attempt is still working.
- `maxReceiveCount=5` — matches the 5 bounded retries for a not-yet-visible bet (D-2). The
  message reaches the DLQ when the retries are genuinely exhausted, not before.
- `ReceiveMessageWaitTimeSeconds=20` — long polling. The client's HTTP read timeout must be
  greater than 20s.

**Not enabled**: high-throughput FIFO (`DeduplicationScope=messageGroup` +
`FifoThroughputLimit=perMessageGroupId`). It raises the queue-wide TPS quota but does **not**
increase per-group throughput, so it does not solve a hot wallet.

---

## Message attributes (contractual)

| Attribute | Type | Required | Notes |
|-----------|------|----------|-------|
| `MessageGroupId` | — (SQS field) | yes | **`wallet_id` verbatim.** This is what guarantees per-wallet ordering and is the broker-side half of Constitution VII. |
| `MessageDeduplicationId` | — (SQS field) | yes | The operation's `idempotencyKey`. Collapses producer retries within the 5-minute dedup window — a convenience, **not** the correctness guarantee. |
| `contentType` | String | yes | `application/json` |
| `tenantId` | String | yes | Must match the realm-derived tenant the consumer resolves; a mismatch is a permanent failure. |
| `schemaVersion` | String | yes | `1`. A version this build does not understand is a permanent failure routed to the DLQ, never a parse attempt. |
| `occurredAt` | String | yes | RFC 3339. Business time recorded on the ledger entry. |

`tenantId` travels in the message body and in an attribute on purpose: the attribute is
matched first as a cheap guard before the body is parsed.

---

## Message body

```json
{
  "schemaVersion": 1,
  "messageId": "9a1c4e77-2b8d-4f61-9e03-7c5a2d9b1f48",
  "tenantId": "tenant-br",
  "walletId": "3f2a1b4c-5d6e-4f70-8a91-b2c3d4e5f607",
  "idempotencyKey": "order-20260928-000417-8812",
  "operation": {
    "type": "BET",
    "amountMinor": 3000,
    "currency": "BRL",
    "transactionId": "txn-8f21a0",
    "referenceId": "bet-9001",
    "betId": null,
    "operationId": null
  },
  "occurredAt": "2026-09-28T14:32:11.482Z"
}
```

`operation` is byte-for-byte the same object as the HTTP request body in
[`openapi.yaml`](./openapi.yaml). One schema, two transports — a divergence here is exactly
how the two channels stop being equivalent (FR-003).

**`messageId` is the inbox key.** It is the *producer's* unique id, not SQS's `MessageId`:
- it survives a queue redrive and a replay into another environment;
- it is stable across the `DeleteMessage`/redelivery cycle, so the inbox genuinely
  deduplicates a redelivery rather than merely avoiding a within-window broker dedup;
- SQS's own `MessageId` is retained separately for troubleshooting.

**Money is an integer plus a currency code.** `amountMinor` is `int64`. No float, no decimal
string, no JSON `number` with a fractional part. A body whose amount does not parse is a
permanent failure.

---

## Producer requirements

1. One message per operation. One operation per message — no batching of multiple wallet
   operations into one message.
2. `MessageGroupId = wallet_id`, unmodulated. Do not shard the group id unless the escape
   hatch in `plan.md` "Deferred" has been deliberately adopted, and then the bucket count is
   fixed for the wallet's lifetime.
3. `MessageDeduplicationId = idempotencyKey`. Without an explicit dedup id and with content
   dedup disabled, `SendMessage` **fails**.
4. `MessageGroupId` is at most 128 characters and may contain only alphanumerics and
   ``!"#$%&'()*+,-./:;<=>?@[\]^_`{|}~``. A raw UUID fits; a URL-encoded one may not — normalize.
5. Publish BET before its settlement for the same wallet. FIFO enforces it; do not rely on
   arrival order across *different* groups.
6. Producers are responsible for their own `idempotencyKey` uniqueness per wallet.

---

## Consumer contract

### Per-message lifecycle

```text
ReceiveMessage (long poll, up to 10 messages)
  └─ for each message, in the order returned:
       1. BEGIN
       2. INSERT INTO inbox (message_id, ...) ON CONFLICT DO NOTHING
            rows == 0 → already processed: COMMIT, DeleteMessage, continue
       3. apply the operation          ← the SAME use case the HTTP handler calls
       4. INSERT INTO outbox (...)     ← same transaction
       5. COMMIT
       6. DeleteMessage                ← only after step 5 returns without error
```

Non-negotiables:

- **`DeleteMessage` only after commit.** Deleting first loses the message on rollback; not
  deleting after commit is safe because step 2 absorbs the redelivery.
- **Never ack a 10-message batch atomically.** One poison message would then poison all ten
  and, after `maxReceiveCount`, land all ten in the DLQ. `DeleteMessageBatch` is acceptable as
  a throughput optimisation *after* each message has committed individually; it must never
  wrap the effects.
- **Same-group messages are applied in the order received**, and each completes before the
  group's next message becomes available. Concurrency therefore happens *across* groups.
- A message already claimed by the inbox is a success from the consumer's perspective —
  delete it immediately so a duplicate never burns retries toward the DLQ.

### Failure classification

| Condition | Class | Action |
|-----------|-------|--------|
| Malformed JSON, unknown `schemaVersion`, invalid `tenantId` | **permanent** | Record the reason, then let `maxReceiveCount` route it to the DLQ. Never retry. |
| `amountMinor` not a positive integer, or currency not `BRL`/`USD` | **permanent** | Same. |
| Currency mismatch against the wallet | **permanent** | Same. Record the rejection. |
| `INSUFFICIENT_FUNDS`, `WALLET_FROZEN`, `BET_ALREADY_SETTLED` | **permanent** | Record the rejection with the message id, then DLQ. These are business outcomes, not delivery failures. |
| Bet referenced but not yet visible (**D-2**) | **transient** | Retry with progressive visibility backoff. 5 attempts total, then DLQ with the reason recorded. |
| SQLSTATE `40001`, `40P01`, lock timeout, connection reset, context deadline | **transient** | Retry inside the transaction helper with bounded backoff. |
| Dependency unavailable | **transient** | Do not delete. Let the visibility timeout expire. |

`maxReceiveCount` counts **receipts**, not failures — a visibility-timeout expiry also counts.
An operation that times out at 60s and succeeds at 61s burns two attempts.

### DLQ handling

- The DLQ is FIFO, so a redriven message preserves its group.
- The DLQ must be alerted on: `ApproximateNumberOfMessages >= 1` means an operation did not
  reach the ledger.
- Redrive is a manual operation via `StartMessageMoveTask`, throttled. Before redriving,
  resolve the recorded reason: a message that failed because a bet was not visible may now
  succeed; one that failed on validation never will.
- **A DLQ'd message leaves a hole in that group's total order.** Later messages in the group
  proceed. Recovery is by replaying the recorded operation, which the idempotency record makes
  safe.

---

## Events published on the outbox queue

Events go to a **separate** queue (`wallet-events.fifo`); this contract only covers the
inbound `wallet-ops.fifo`. Event payloads reuse the `Money` shape from `openapi.yaml`, and each
carries `eventUid`, which is the `MessageDeduplicationId` so producers and consumers can dedup.

| Event | Meaning |
|-------|---------|
| `wallet.bet_placed` | A BET was applied; the stake was debited. |
| `wallet.bet_settled` | A WIN or LOSS settled a bet. Only WIN moves money; both are announced. |
| `wallet.bet_reversed` | A ROLLBACK wrote compensating entries. |
| `wallet.refunded` | A REFUND credited back a previous debit. |

Delivery is at-least-once. Consumers MUST be idempotent — the outbox guarantees only that no
event is published before its transaction commits, not that it is delivered exactly once.

---

## LocalStack development notes

- Image version MUST be pinned. FIFO ordering fidelity in LocalStack has regressed before
  (group-head messages going undelivered after their predecessor was deleted), so local
  ordering tests are not evidence of production ordering.
- Credentials must look real — `test`/`test` — because SQS requires SigV4 signing.
- `SQS_ENDPOINT_STRATEGY=standard` is the default. LocalStack binds IPv4 only, so
  `*.localhost.localstack.cloud` must resolve to `127.0.0.1`.
- `MessageRetentionPeriod` is not enforced by default; do not write tests that depend on expiry.
- Purge and delete/recreate rate limits are disabled locally but **enabled in AWS**. Tests must
  not depend on them.
- In tests, wait for a message with the `GET /_aws/sqs/messages?QueueUrl=...` peek endpoint.
  Polling with a real `ReceiveMessage` consumes the message under test.
