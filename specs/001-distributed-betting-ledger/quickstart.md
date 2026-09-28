# Quickstart: Validating the Distributed Betting Ledger

**Feature**: 001-distributed-betting-ledger | **Date**: 2026-09-28

Runnable scenarios that prove the feature works end to end. This is a **validation guide**,
not an implementation guide: no schema DDL, no handler code, no task breakdown. Design detail
lives in [`data-model.md`](./data-model.md), [`research.md`](./research.md) and
[`contracts/`](./contracts/).

---

## Prerequisites

| Tool | Version | Why |
|------|---------|-----|
| Go | 1.25+ | Floor set by pgx v5.11 (needs 1.25) and aws-sdk-go-v2 (needs 1.24). See research.md D-1 |
| Docker | any recent | testcontainers needs a working daemon |
| LocalStack | pinned image | FIFO ordering fidelity has regressed before; never `latest` |
| Keycloak | 24+ | Dev-only, started by the test harness |

---

## Local environment

```bash
docker compose up -d postgres localstack keycloak
export DATABASE_URL='postgres://ironledger_migrator:dev@localhost:5432/ironledger?sslmode=disable'
export SQS_ENDPOINT='http://localhost:4566'
export SQS_REGION='us-east-1'
export AWS_ACCESS_KEY_ID='test'
export AWS_SECRET_ACCESS_KEY='test'
export KEYCLOAK_ISSUER='http://localhost:8081/realms/tenant-br'
export KEYCLOAK_AUDIENCE='ironledger-api'
export KEYCLOAK_ALLOWED_CLIENTS='betting-provider,settlement-engine'

go run ./cmd/ironledger migrate up     # apply migrations
go run ./cmd/ironledger serve
```

Wait for `GET /health/ready` to return `200` before issuing traffic. It reports `503` while the
database or the queue is unreachable, so traffic is withdrawn automatically rather than failing
per request.

### Keycloak deployment prerequisite

Configure an **audience mapper** on the `ironledger-api` client:

```text
protocolMapper = oidc-audience-mapper
config."included.client.audience" = ironledger-api
config."access.token.claim"      = true
```

Without this, a valid token carries `aud: ["account"]` and every request is rejected `401`.
This is the single most common cause of "auth is broken" on this stack — see research.md D-10.

Obtain a token and keep it:

```bash
TOKEN=$(curl -s -X POST \
  "$KEYCLOAK_ISSUER/protocol/openid-connect/token" \
  -d grant_type=client_credentials \
  -d client_id=betting-provider \
  -d client_secret=dev-secret \
  | jq -r .access_token)
```

Do not send `scope=openid` — this is a machine flow and there is no ID token.

---

## V1 — A single BET debits the wallet (P1, SC-001)

```bash
WALLET_ID=$(uuidgen)
curl -s -X POST "http://localhost:8080/v1/wallets/$WALLET_ID/operations" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: seed-001" \
  -H 'Content-Type: application/json' \
  -d '{"type":"BET","amountMinor":3000,"currency":"BRL","transactionId":"txn-seed-1"}'
```

**Expected**: `200`, `balanceMinor: 0`, one `ledgerEntryIds` element of type `BET_DEBIT`,
`status: COMPLETED`.

**Negative variant** — a second bet the wallet cannot cover:

```bash
curl -s -X POST "http://localhost:8080/v1/wallets/$WALLET_ID/operations" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: seed-002" \
  -d '{"type":"BET","amountMinor":9000,"currency":"BRL","transactionId":"txn-seed-2"}'
```

**Expected**: `409 INSUFFICIENT_FUNDS`, `availableMinor: 0`. Read the ledger again and confirm
the entry count is unchanged — a rejected operation writes no entry but does write one
`audit_log` row (FR-030).

---

## V2 — Idempotent replay (P1, SC-003)

```bash
for i in 1 2 3; do
  curl -s -X POST "http://localhost:8080/v1/wallets/$WALLET_ID/operations" \
    -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: seed-003" \
    -d '{"type":"BET","amountMinor":1000,"currency":"BRL","transactionId":"txn-seed-3"}'
  echo
done
```

**Expected**: three `200` responses. The first has `X-Idempotent-Replay` absent; the second and
third have `X-Idempotent-Replay: true` and an **identical body** to the first. The ledger gained
exactly one entry.

**Conflict variant** — same key, different body:

```bash
curl -s -X POST "http://localhost:8080/v1/wallets/$WALLET_ID/operations" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: seed-003" \
  -d '{"type":"BET","amountMinor":2000,"currency":"BRL","transactionId":"txn-seed-3"}'
```

**Expected**: `409 IDEMPOTENCY_KEY_CONFLICT`. The stored result is *not* replayed, because
replaying it would be wrong.

---

## V3 — Restart survival (P5, SC-003, Principle VI)

```bash
# 1. apply an operation, note the response
curl -s -X POST "http://localhost:8080/v1/wallets/$WALLET_ID/operations" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: seed-004" \
  -d '{"type":"BET","amountMinor":500,"currency":"BRL","transactionId":"txn-seed-4"}'

# 2. hard-kill the process
pkill -9 -f cmd/ironledger

# 3. restart it
go run ./cmd/ironledger serve &

# 4. resend the identical request
curl -s -X POST "http://localhost:8080/v1/wallets/$WALLET_ID/operations" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: seed-004" \
  -d '{"type":"BET","amountMinor":500,"currency":"BRL","transactionId":"txn-seed-4"}'
```

**Expected**: `200` with `X-Idempotent-Replay: true` and the balance unchanged from step 1.
A `400 MISSING_IDEMPOTENCY_KEY` or a second ledger entry means idempotency state was held in
process memory — a constitution violation.

---

## V4 — Money never touches a float (Principle I)

```bash
curl -s "http://localhost:8080/v1/wallets/$WALLET_ID/ledger" \
  -H "Authorization: Bearer $TOKEN" | jq '.entries[0].amountMinor'
```

**Expected**: an integer, never a decimal. Then confirm the schema has no float column and the
domain has no float money field:

```bash
go test ./test/boundaries/ -run TestDomainImports
psql "$DATABASE_URL" -c "
  SELECT table_name, column_name, data_type FROM information_schema.columns
  WHERE data_type IN ('real','double precision','numeric','float4','float8');"
```

**Expected**: the boundary test passes; the query returns **zero rows**.

---

## V5 — No negative balance under concurrency (SC-002, SC-006, Principle III)

```bash
go test ./test/integration/ -run 'TestNoNegativeBalance|TestConcurrentBets' -v -count=1
```

The test funds a wallet with 10000 and fires 100 concurrent `BET`s of 8000 from independent
connections, then asserts:

- the balance is never observed below zero at any point;
- exactly one bet succeeds;
- the final balance is 2000;
- the number of `BET_DEBIT` entries is exactly 1.

Run it with the race detector: `-race`. A pass without the constraint being checked would mean
the application, not the database, was preventing the negative balance.

**Cross-wallet parallelism** (Principle VII, SC-009) is asserted in the same package: two
wallets must make progress concurrently. If a change ever introduces a global lock or a
single advisory key, this assertion fails.

---

## V6 — Append-only (Principle II)

```bash
go test ./test/integration/ -run TestAppendOnly -v -count=1
```

It attempts `UPDATE ledger_entries SET amount_minor = 1` and `DELETE FROM ledger_entries`
directly as the runtime role, and asserts both fail. It also applies a ROLLBACK and asserts the
original entry is byte-identical while a new compensating entry appears.

---

## V7 — ROLLBACK semantics (P3, D-4)

```bash
# WIN credits a prize
curl -s -X POST "http://localhost:8080/v1/wallets/$WALLET_ID/operations" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: rb-001" \
  -d '{"type":"WIN","betId":"<bet-uuid>","amountMinor":9000,"currency":"BRL","transactionId":"txn-rb-1"}'

# ROLLBACK reverses it with compensating entries
curl -s -X POST "http://localhost:8080/v1/wallets/$WALLET_ID/operations" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: rb-002" \
  -d '{"type":"ROLLBACK","operationId":"<win-operation-uuid>","amountMinor":9000,"currency":"BRL","transactionId":"txn-rb-2"}'
```

**Expected**: `200`, `ledgerEntryIds` has two entries (`ROLLBACK_DEBIT` and `ROLLBACK_CREDIT`),
`reversedOperationId` set, and the original WIN entry unchanged.

**Negative variant** — a ROLLBACK that would need a negative balance is `409 INSUFFICIENT_FUNDS`
with the balance untouched.

---

## V8 — SQS channel equivalence (P4, SC-005, FR-003)

```bash
go test ./test/integration/ -run TestChannelEquivalence -v -count=1
```

Publishes BET, WIN, LOSS, REFUND and ROLLBACK to `wallet-ops.fifo` on one wallet, applies the
same five to a parallel wallet over HTTP, then asserts identical balances and identical ledger
shapes. Contract details: [`contracts/wallet-ops.fifo.md`](./contracts/wallet-ops.fifo.md).

Manual spot check:

```bash
awslocal sqs send-message --queue-url "$QUEUE_URL" \
  --message-group-id "$WALLET_ID" \
  --message-deduplication-id manual-001 \
  --message-body '{"schemaVersion":1,"messageId":"m-001","tenantId":"tenant-br","walletId":"'$WALLET_ID'","idempotencyKey":"sqs-001","operation":{"type":"BET","amountMinor":2000,"currency":"BRL","transactionId":"txn-sqs-1"},"occurredAt":"2026-09-28T14:32:11Z"}'
```

**Expected**: the wallet is debited exactly as the API would have. A missing
`--message-group-id` fails the send; omitting `--message-deduplication-id` with content
dedup disabled also fails.

**Redelivery** — deliver the same `messageId` three times; exactly one effect.

---

## V9 — Post-commit publication (Principle V)

```bash
go test ./test/integration/ -run 'TestOutboxRolledBackWithTx|TestOutboxPublishedAfterCommit' -v
```

Asserts that a transaction which rolls back leaves no outbox row and no published event, and
that a committed one is published only after the commit is visible. This is the test that fails
if someone moves the `SendMessage` call inside the transaction.

---

## V10 — Reconciliation (SC-001, SC-007)

```bash
curl -s -X POST "http://localhost:8080/v1/reconciliation" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: recon-001"
```

**Expected**: `divergences: []`. After the V1-V8 scenarios have produced a mix of accepted and
rejected operations, this must still be empty — the ledger is the source of truth and the
balance is always derivable from it.

---

## Test commands

```bash
go test ./internal/domain/... ./internal/usecase/...   # fast; no Docker, no DB
go test ./test/boundaries/ -v                          # domain purity; no Docker
go test -race ./test/integration/ -v -count=1          # Postgres + LocalStack via testcontainers
go vet ./... && gofmt -l .                             # lint gate
```

The domain and use case suites MUST run without Docker. If they fail when Docker is absent,
something from `internal/platform` has leaked into the core — that is a constitution violation,
not a test problem.
