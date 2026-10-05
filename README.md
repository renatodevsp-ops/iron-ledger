# iron-ledger

A Go service that moves player wallets in response to operations reported by game
providers, correctly while several instances run at once and messages arrive
twice, out of order, or not at all.

- **Go 1.24**, **Uber Fx** composition, **PostgreSQL**, **AWS SQS**, **Keycloak**.
- Monetary values are `int64` minor units end to end — no float ever touches an
  amount.
- Idempotency, the ledger and the inbox are enforced by the database, not by a
  process's memory, so a restart changes nothing.
- Events are published only after the transaction that produced them has
  committed.

`ARCHITECTURE.md` explains the decisions and their costs. This file gets it
running.

---

## Prerequisites

| | Version | Needed for |
| --- | --- | --- |
| Docker + Compose | any recent | everything |
| Go | 1.27+ | running the binaries and the tests on the host |
| `curl`, `jq` | any | the examples below |

Nothing else. No local PostgreSQL, no local broker, no identity provider.

---

## Quick start

```sh
cp .env.example .env          # every value already matches the compose file
docker compose up --build -d
```

The compose stack starts PostgreSQL, LocalStack (SQS) and Keycloak, runs the
migrations once, then starts the API and the worker. The queues — including their
redrive policies — are provisioned by the application on first start.

| | |
| --- | --- |
| API | http://localhost:8080 |
| Keycloak | http://localhost:8081 (admin / admin, realm `ironledger`) |
| LocalStack | http://localhost:4566 |
| Worker health | http://localhost:8082/health/ready |

```sh
curl -s localhost:8080/health/ready | jq
```

```json
{
  "status": "ok",
  "instance": "api-1",
  "uptime": "12s",
  "checks": { "identity": "ok", "postgres": "ok", "sqs": "ok" }
}
```

Stop it, keeping the database:

```sh
docker compose down
```

Stop it and discard everything:

```sh
docker compose down -v
```

### Running the binaries on the host

```sh
make infra                   # dependencies only
make migrate-up              # schema
make build
DATABASE_URL=postgres://iron:iron@localhost:5432/ironledger?sslmode=disable \
SQS_ENDPOINT=http://localhost:4566 \
OIDC_ISSUER_URL=http://localhost:8081/realms/ironledger \
HTTP_ADDRESS=:8080 ./bin/api

# and, in another shell
DATABASE_URL=postgres://iron:iron@localhost:5432/ironledger?sslmode=disable \
SQS_ENDPOINT=http://localhost:4566 \
OIDC_ISSUER_URL=http://localhost:8081/realms/ironledger \
HTTP_ADDRESS=:8082 ./bin/worker
```

`./bin/api -healthcheck` probes the liveness endpoint and exits `0` or `1`, which
is what the container runtime uses.

---

## Getting a token

The realm ships with three service accounts. Their secrets are local development
credentials and are committed on purpose, so the flow below is reproducible from
a clean checkout.

```sh
alias token='curl -s -X POST http://localhost:8081/realms/ironledger/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=CLIENT -d client_secret=SECRET | jq -r .access_token'

INTERNAL=$(curl -s -X POST http://localhost:8081/realms/ironledger/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=iron-ledger-internal \
  -d client_secret=iron-ledger-internal-secret | jq -r .access_token)

PROVIDER_A=$(curl -s -X POST http://localhost:8081/realms/ironledger/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=provider-a \
  -d client_secret=provider-a-secret | jq -r .access_token)

PROVIDER_B=$(curl -s -X POST http://localhost:8081/realms/ironledger/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=provider-b \
  -d client_secret=provider-b-secret | jq -r .access_token)
```

| Client | May do |
| --- | --- |
| `iron-ledger-internal` | Open wallets, read balances, ledger, reconciliation, any transaction |
| `provider-a` | Submit operations as `provider-a`, read `provider-a`'s transactions |
| `provider-b` | The same for `provider-b` — used to prove providers are isolated |

---

## Examples

### Open a wallet

```sh
PLAYER=$(uuidgen)

curl -s -X POST localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNAL" \
  -H 'Content-Type: application/json' \
  -d "{
        \"playerId\": \"$PLAYER\",
        \"initialBalance\": { \"amount\": \"100.00\", \"currency\": \"BRL\" }
      }"
```

```json
{
  "id": "0192f291-27dd-7d3f-8071-5f8685deef37",
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "balance": { "amount": "100.00", "currency": "BRL" },
  "version": 1,
  "createdAt": "2026-09-08T12:00:00Z",
  "updatedAt": "2026-09-08T12:00:00Z"
}
```

The opening balance creates, in one commit: the wallet at version `1`, one
`OPENING` transaction in `PROCESSED`, one credit ledger entry, and the outbound
`WagerTransactionProcessed` and `WalletBalanceChanged` events. A zero balance
creates only the wallet.

### Place a bet

The `Idempotency-Key` header is **mandatory**. `{providerId}:{externalTransactionId}`
is the convention.

```sh
WALLET=0192f291-27dd-7d3f-8071-5f8685deef37

curl -s -X POST localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d "{
        \"providerId\": \"provider-a\",
        \"externalTransactionId\": \"transaction-123\",
        \"playerId\": \"$PLAYER\",
        \"walletId\": \"$WALLET\",
        \"roundId\": \"round-987\",
        \"gameId\": \"fortune-chimp\",
        \"kind\": \"BET\",
        \"money\": { \"amount\": \"25.00\", \"currency\": \"BRL\" }
      }"
```

```json
{
  "transactionId": "0192f298-345e-7e38-af88-e43f851a819d",
  "status": "PROCESSED",
  "balance": { "amount": "975.00", "currency": "BRL" },
  "idempotentReplay": false
}
```

Send it again, unchanged: same answer, `"idempotentReplay": true`, **no second
debit**. Send it again with a different amount: `409 IDEMPOTENCY_KEY_CONFLICT`.

### The other kinds

```sh
# A win credits the wallet.
kind=WIN  amount=10.00

# A loss records a bet that moved nothing — the amount must be exactly 0.00.
kind=LOSS amount=0.00

# A refund returns a processed bet in full. It must name it.
kind=REFUND   amount=25.00   extra=',"referenceExternalTransactionId":"transaction-123"'

# A rollback undoes a processed bet, win or refund.
kind=ROLLBACK amount=25.00   extra=',"referenceExternalTransactionId":"transaction-123"'
```

Add `"referenceExternalTransactionId"` for the two reversals. A reversal whose
reference has not arrived is answered **202** with
`"status": "PENDING_REFERENCE"` and resolved by a worker once the reference
shows up — or rejected with `REFERENCE_NOT_FOUND` once the retry budget is spent.

### Reads

```sh
curl -s -H "Authorization: Bearer $INTERNAL" localhost:8080/wallets/$WALLET
curl -s -H "Authorization: Bearer $INTERNAL" "localhost:8080/wallets/$WALLET/ledger?limit=50"
curl -s -H "Authorization: Bearer $INTERNAL" -X POST localhost:8080/wallets/$WALLET/reconciliation

# A provider reads only its own transactions.
curl -s -H "Authorization: Bearer $PROVIDER_A" \
  localhost:8080/providers/provider-a/wagering/transactions/transaction-123
curl -s -H "Authorization: Bearer $INTERNAL" \
  localhost:8080/wagering/transactions/0192f298-345e-7e38-af88-e43f851a819d
```

The ledger paginates with an opaque cursor: pass the returned `nextCursor` as
`?cursor=…`.

```json
{
  "walletId": "0192f291-…",
  "currency": "BRL",
  "storedBalance": { "amount": "975.00", "currency": "BRL" },
  "calculatedBalance": { "amount": "975.00", "currency": "BRL" },
  "difference": { "amount": "0.00", "currency": "BRL" },
  "consistent": true,
  "checkedEntries": 2
}
```

### The asynchronous path

`cmd/seed` puts a message on the wager queue in exactly the shape an upstream
provider's integration sends. The worker does the rest.

```sh
echo "{
  \"providerId\": \"provider-a\",
  \"externalTransactionId\": \"transaction-777\",
  \"idempotencyKey\": \"provider-a:transaction-777\",
  \"playerId\": \"$PLAYER\",
  \"walletId\": \"$WALLET\",
  \"roundId\": \"round-988\",
  \"gameId\": \"fortune-chimp\",
  \"kind\": \"BET\",
  \"money\": { \"amount\": \"30.00\", \"currency\": \"BRL\" }
}" | ./bin/seed -message-id msg-777
```

Run it again with the same `-message-id`: the broker drops the duplicate. Run it
again with a different id: the inbox recognises the operation and replays its
stored result. Either way the wallet moves once.

Watch it happen:

```sh
docker compose logs -f worker
```

### Proving the guarantees

```sh
# Two 80.00 bets against a 100.00 balance, at the same instant.
./scripts/demo-race.sh
```

One is processed, one is refused with `INSUFFICIENT_BALANCE`, the balance ends at
20.00, and the ledger holds exactly one debit.

---

## Configuration

Every variable, its default and what it does is documented in `.env.example`.
The ones worth knowing:

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://iron:iron@localhost:5432/ironledger?sslmode=disable` | Connection string |
| `DATABASE_MIGRATE_ON_BOOT` | `true` | Apply pending migrations at start-up |
| `SQS_ENDPOINT` | `http://localhost:4566` | Broker endpoint |
| `SQS_VISIBILITY_TIMEOUT` | `30s` | Must exceed the worst-case processing time |
| `SQS_MAX_ATTEMPTS` | `5` | `maxReceiveCount` of both redrive policies |
| `AUTH_ENABLED` | `true` | **Leave it on.** Off grants only the internal role |
| `OIDC_ISSUER_URL` | `http://localhost:8081/realms/ironledger` | Identity provider |
| `OIDC_AUDIENCE` | `iron-ledger-api` | Expected token audience |
| `AUTH_CLIENT_PROVIDERS` | `provider-a=provider-a,provider-b=provider-b` | Fallback provider mapping |
| `WAGERING_MAX_REFERENCE_ATTEMPTS` | `8` | Retry budget for a missing reference |
| `WAGERING_MAX_CONFLICT_RETRIES` | `8` | Optimistic-concurrency retry budget |
| `OUTBOX_MAX_ATTEMPTS` | `10` | Publication attempts before dropping |
| `HTTP_SHUTDOWN_BUDGET` | `20s` | Graceful drain window |

`.env.example` contains **no real secrets**. The client secrets belong to the
local Keycloak realm and the AWS credentials are the values LocalStack accepts.

---

## Migrations

Versioned SQL files under `migrations/`, applied by `golang-migrate`. Each has an
up and a down.

```sh
make migrate-up                  # apply everything pending
make migrate-down                # roll back the last one
make migrate-down N=3            # roll back three
make migrate-version             # version=2 dirty=false

# or directly
go run ./cmd/migrate up
go run ./cmd/migrate down 1
go run ./cmd/migrate version
```

| File | Contents |
| --- | --- |
| `0001_core.up.sql` | Event store, `wallets`, the append-only `wallet_ledger_entries` and the triggers that protect it |
| `0002_wagering_and_messaging.up.sql` | `wager_transactions` with its unique indexes and checks, `inbox_messages`, `outbox_messages` |

`docker compose` runs `migrate up` as its own job before the application starts.
`DATABASE_MIGRATE_ON_BOOT=true` applies pending migrations at start-up, which is
convenient locally; a deployment would run the migration job instead.

The down migrations drop everything in reverse order. `0001_core.down.sql` drops
the immutability triggers first, because the ledger refuses to be truncated.

---

## Tests

### Unit

No infrastructure, no build tag, fast:

```sh
go test ./...
go test -race ./...
```

They cover `Money` parsing, scale, limits, invalid input and currency
mismatch; the wallet's invariants and transitions; the wager transaction state
machine including every terminal state; the five external kinds and their
zero-amount policies; and the fingerprinting rules behind the idempotency
conflict.

### Layering

```sh
make layers
```

A separate check, because it is a different kind of test. It reads the real
import graph with `go list` and fails if a package crosses a boundary it is not
allowed to cross — a context importing the broker, an adapter importing a
context, `sharedkernel` reaching upward. A constraint nobody checks is a
constraint that decays, and `internal/domain/wagering` sitting next to
`internal/pgdb` in one flat directory is precisely the kind of thing that looks
fine until something imports the wrong one.

It runs with `-count=1` on purpose: the check shells out to `go list`, which Go's
test cache cannot see, so a cached pass would survive the very change it exists
to catch.

### Integration

Real PostgreSQL, real Keycloak, real SQS. Behind the `integration` build tag, so
they never run by accident against the wrong database:

```sh
make integration        # starts the dependencies, then runs the suite
make integration-race   # the same, with -race
```

Underneath:

```sh
docker compose up -d postgres localstack keycloak
docker compose exec -T postgres psql -U iron -d postgres \
  -c 'CREATE DATABASE ironledger_test OWNER iron'

TEST_DATABASE_URL=postgres://iron:iron@localhost:5432/ironledger_test?sslmode=disable \
SQS_ENDPOINT=http://localhost:4566 \
OIDC_ISSUER_URL=http://localhost:8081/realms/ironledger \
OIDC_AUDIENCE=iron-ledger-api \
LOG_LEVEL=error \
go test -count=1 -race -tags integration ./test/integration/...
```

The suite applies the migrations and then truncates, which requires briefly
disabling the ledger's TRUNCATE guard. The guard itself is asserted separately
with the guards on.

What it proves, by category:

| | |
| --- | --- |
| **Concurrency** | Two 80.00 bets against 100.00 → one processed, one refused, 20.00 final, one ledger debit. The same bet 50× in parallel → one debit. Distinct wallets advance in parallel. All of it repeated across **three independent instances**, each with its own pool and its own use cases |
| **Idempotency** | A replay returns the original result including the original balance, after the wallet has moved on. Key reuse with a different payload is a conflict. `25`, `25.0` and `25.00` are one operation. The same operation over HTTP and over SQS, in both directions |
| **References** | A reversal that arrives before its reference survives, is resumed by another instance, and settles when the reference arrives. One that never arrives is rejected with a stable code |
| **Schema** | The ledger refuses `UPDATE`, `DELETE` and `TRUNCATE`. An entry that does not add up is refused. A negative balance is refused. A second wallet per player and currency is refused. A second successful reversal of one operation is refused |
| **Messaging** | A redelivered message replays instead of re-applying. Two publishers share one outbox without publishing twice. A failed publication is retried under the same `eventId`. A publisher that dies mid-flight is recovered. The queues are FIFO with redrive policies |
| **Restart** | A bet survives a process being destroyed and rebuilt: the balance, the idempotency record and the pending reversal are all still there |
| **Authentication** | Missing, malformed, altered and expired credentials are refused. Providers are isolated from each other. Wallet operations are restricted to the internal service. A provider cannot act as another provider |
| **Composition** | The Fx application starts and stops cleanly, hooks run in order, and an unreachable dependency fails start-up rather than serving |

### Load

Optional, and not claimed as a result. With the stack running:

```sh
scripts/load.sh
```

It reports throughput and the p50/p95/p99 of each request, the distribution of
outcomes, the idempotency conflicts, and the outbox lag, so a regression in any of
those is visible rather than guessed at.

---

## Running several instances

The guarantees do not depend on there being one process. Two ways to see it:

```sh
# Three API replicas and three workers behind different ports.
API_PORT=8080 WORKER_PORT=8082 docker compose up -d --scale api=3 --scale worker=3
```

```sh
# Or on the host, each with its own INSTANCE_ID and database pool.
for i in 1 2 3; do
  INSTANCE_ID="api-$i" HTTP_ADDRESS=":808$i" ./bin/api &
done
```

Then run `scripts/demo-race.sh` and watch the balance settle correctly regardless
of which instance served which request.

---

## Simulating failures

```sh
# Kill an instance mid-flight; the client retries with the same key and the
# inbox absorbs it.
scripts/demo-failure.sh kill-mid-flight

# PostgreSQL unavailable: the API answers 503 and readiness fails; the client
# retries with the same key and nothing is duplicated.
scripts/demo-failure.sh database-down

# The broker unavailable: events accumulate in the outbox and drain when it
# returns, under the same eventId.
scripts/demo-failure.sh broker-down
```

Each script explains what it does and asserts the outcome, so it is a check
rather than a demo.

---

## Layout

```
cmd/
  api/            HTTP surface
  worker/         SQS consumer, outbox publisher, reference resolver
  migrate/        schema up/down/version
  seed/           puts a message on the queue, for the asynchronous path

internal/
  sharedkernel/   money, ids, error taxonomy, idempotency fingerprint
  platform/       config, logging, metrics, transaction manager, event store,
                  repository seam, HTTP vocabulary, migrations, streams
  wallet/         ── Wallet context ──
    domain/         the aggregate
    events/         its events and their integration contract
    slices/         openwallet, applywalletmovement (state change)
                    walletdetails, walletledgerentries, walletreconciliation
                    (state view)
  wagering/       ── Wagering context ──
    domain/         the aggregate and its state machine
    events/         its events and their integration contract
    slices/         registerwageroperation, awaitwagerreference,
                    settlewageroperation, failwageroperation (state change)
                    wagertransactiondetails, providerwagertransactionlookup
                    (state view)
  messaging/      ── Messaging context ── inbox, outbox, SQS adapter
  identity/       ── Identity context ── OIDC verification, authorisation
  app/            use cases, HTTP handlers, workers, pipeline, Fx composition

migrations/       versioned SQL, up and down
deploy/keycloak/  realm import: clients, roles, service accounts
test/integration/ the integration suite
scripts/          load and failure simulations
```

A state-change slice is one file holding its command, its state, `initialState`,
`evolve`, `decide` and `NewCommandHandler`, with the specifications beside it. A
state-view slice holds its read model, its projector and its query handler. Both
follow the project's vertical-slice conventions; `ARCHITECTURE.md` §5 explains the
one place the read models deliberately differ.

---

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `401` on every business call | No `Authorization: Bearer …`, or a token from a different realm. Check `/health/ready` reports `identity: ok`. |
| `503` everywhere | A dependency is down. `GET /health/ready` names which. |
| `409 IDEMPOTENCY_KEY_CONFLICT` | The key was used for a different payload. Send the original, or a new key. |
| `422` with `PENDING_REFERENCE` | Expected, not an error: the reversal is waiting. Poll `GET /wagering/transactions/:id`. |
| `422` with `REFERENCE_NOT_FOUND` | The reference never arrived within the retry budget. |
| Events not appearing on `wager-events.fifo` | Check `docker compose logs worker` and `SELECT * FROM outbox_messages WHERE published_at IS NULL;` |
| Keycloak says the realm exists but tokens fail | The container re-imported the realm and changed its keys. Fetch a fresh token. |

---

## Licence

Provided as a reference implementation for this exercise.
