# Implementation Plan: Distributed Betting Ledger

**Branch**: `001-distributed-betting-ledger` | **Date**: 2026-09-28 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/speckit.specify`

## Summary

Backend service in Go that moves player wallet balances for betting operations. Two entry
channels with identical guarantees: a REST API and an SQS FIFO consumer. Five operations —
BET, WIN, LOSS, REFUND, ROLLBACK — are applied to an append-only ledger with a
database-materialized balance. Correctness must hold with many instances running and with
crashes between processing steps, via three mechanisms working together:

1. A durable idempotency record claimed inside the same transaction as the financial effect
   (so a repeat never double-applies, and survives a full restart).
2. Pessimistic per-wallet row locking (`SELECT ... FOR UPDATE` on the wallet row) with
   `READ COMMITTED` and bounded retry on serialization/deadlock errors — per-wallet
   serialization, never global.
3. A transactional outbox written in the same transaction, published only after commit, plus
   a FIFO inbox that dedupes redelivered messages by their unique operation key.

The balance is maintained as a materialized column guarded by `CHECK (balance >= 0)`, and is
trivially reconcilable against the ledger because the ledger is append-only.

## Technical Context

**Language/Version**: Go 1.25+ (floor set by driver support; see research.md D-1)

**Primary Dependencies**: `go.uber.org/fx` (DI, composition root only) ·
`github.com/jackc/pgx/v5` + `pgxpool` (PostgreSQL, explicit SQL, no ORM) ·
`github.com/aws/aws-sdk-go-v2` + `service/sqs` (FIFO messaging; `BaseEndpoint` for LocalStack) ·
`github.com/golang-jwt/jwt/v5` + `github.com/MicahParks/keyfunc/v3` (Keycloak JWKS validation) ·
`log/slog` (structured logging) · `net/http` with Go 1.22+ pattern routing · `testify` for
assertions

**Storage**: PostgreSQL 16+ — wallets, bets, ledger_entries (append-only), operations
(idempotency), inbox, outbox, audit_log. Explicit SQL in versioned migrations, applied by a
migration runner; no ORM.

**Testing**: `go test` + `testcontainers-go` for PostgreSQL and LocalStack (integration and
concurrency suites) · `fxtest` for DI graph validation and lifecycle · table-driven domain unit
tests with no I/O

**Target Platform**: Linux server, containerised, horizontally scaled, multiple instances
behind a load balancer; no local disk state

**Project Type**: web-service (HTTP API + background consumer in one binary)

**Performance Goals**: ≥ 500 operations/sec sustained aggregate across instances ·
p95 < 300 ms for a committed operation end-to-end via API · p95 < 5 s from SQS enqueue to
ledger commit · retry-limited path (uncontended) stays under the 100 ms budget

**Constraints**: no global locks, no cross-instance coordination service · no float anywhere
in the money path · no event published before commit · idempotency state only in PostgreSQL ·
SQS per-group throughput is `1 / operation latency`, so a hot wallet is capped (mitigation:
`wallet_id:bucket` group ids, fixed per wallet for its lifetime)

**Scale/Scope**: 10⁶ wallets, 10⁷ ledger entries in year 1, 5 operations, 2 channels,
2 currencies (BRL, USD), 1 feature release

## Constitution Check

*GATE: evaluated before Phase 0 research and re-checked after Phase 1 design.*

Constitution: `.specify/memory/constitution.md` v1.0.1.

| Principle | Gate | How the design satisfies it |
|-----------|------|----------------------------|
| I. Zero-Float Money | PASS | `Money{amountMinor int64, currency char(3)}` in the domain; `BIGINT` columns plus `currency CHAR(3)` in Postgres; JSON as `{"amountMinor": 3000, "currency": "BRL"}`; explicit parse rejects exponent notation and excess precision. See data-model.md and contracts/openapi.yaml. |
| II. Append-Only Ledger | PASS | `ledger_entries` has no `UPDATE`/`DELETE` path anywhere in the code; enforcement is a `BEFORE UPDATE OR DELETE` trigger that raises, plus `REVOKE UPDATE, DELETE` from the app role. Corrections are new compensating entries referencing `reverses_entry_id`. Balance is derived and reconcilable, not the source of truth. |
| III. DB-Enforced Invariants | PASS | `CHECK (wallets.balance_minor >= 0)` is the no-negative-balance guarantee and is enforced by the database even under concurrency. `UNIQUE (wallet_id, idempotency_key)`, `UNIQUE (wallet_id, transaction_id)` per the user's explicit requirement, partial unique indexes for one-settlement-per-bet. Concurrency resolved by `SELECT ... FOR UPDATE` on the wallet row. Every invariant has a testcontainers integration test. |
| IV. Pure Domain Core | PASS | `internal/domain` imports only stdlib. Ports (`LedgerRepository`, `OutboxWriter`, `Clock`, `IdGenerator`) are interfaces declared by the domain. `fx` appears only in `internal/platform/*` and `cmd/ironledger`. Domain tests compile and run with no Docker, no network, no DB. Verified by an import-boundary test. |
| V. Post-Commit Publication | PASS | `outbox` rows are inserted with the ledger entries in the same transaction. A separate publisher polls with `FOR UPDATE SKIP LOCKED` and sends after commit. Delivery is at-least-once; the inbox is the dedup. Re-publishable after a crash: unpublished rows are simply never marked. |
| VI. Durable Idempotency | PASS | `operations` table with `UNIQUE (wallet_id, idempotency_key)`, claimed via `INSERT ... ON CONFLICT DO NOTHING` in the same transaction as the effect, storing the original response. Inbox `UNIQUE (message_id)` for the queue channel. Nothing idempotency-related lives in process memory. Restart-survival is an explicit test. |
| VII. Per-Wallet Coordination | PASS | Coordination is the wallet row lock. `MessageGroupId = wallet_id` gives broker-side per-wallet ordering. No advisory lock on a global key, no mutex table, no leader election. Cross-wallet parallelism is asserted by a concurrency test. |

**Stack section (v1.0.1)**

| Rule | Gate | Notes |
|------|------|-------|
| Go 1.25+ | PASS | Floor raised from 1.22 by amendment during this planning. Recorded in research.md D-1 and reflected in the constitution's sync impact report. |
| Fx confined to composition | PASS | Only `cmd/ironledger` and `internal/platform/*` import `go.uber.org/fx`. Enforced by test. |
| PostgreSQL as source of truth | PASS | All financial state is in Postgres; no other store holds authoritative money state. |
| SQS via LocalStack, production parity | PASS | `Options.BaseEndpoint` override, no conditional code paths; same queue attributes locally and in AWS. |
| Keycloak OAuth 2.0/OIDC | PASS | JWT signature, `iss`, `aud`, `exp` validated against the realm JWKS; `azp` and `scope` checked for authorization. No local identity flows. |
| FIFO queues, group by wallet | PASS | `MessageGroupId = wallet_id`; DLQ is also FIFO. |
| Multi-currency, no implicit conversion | PASS | BRL and USD wallets; operation currency must match wallet currency; no FX table, no conversion path in this release. |

**Workflow section**

| Rule | Gate | Notes |
|------|------|-------|
| Spec → plan → tasks before implementation | PASS | This plan and the subsequent `/speckit.tasks` satisfy it. |
| Tests demonstrating each invariant | PASS | Invariant-to-test mapping is enumerated in data-model.md and quickstart.md. |
| Versioned, idempotent migrations | PASS | Sequential `NNN_name.up.sql` / `.down.sql`, checksum-tracked in `schema_migrations`, applied under an advisory lock scoped to the migration table (not a global lock on business data). |
| Non-negotiable violations rejected in review | PASS | No violations to justify. |
| YAGNI | PASS | No conversion engine, no withdrawal/deposit, no multi-region, no sharding of hot wallets (only the documented escape hatch). |

**Complexity Tracking**: empty — no violations, so nothing to justify.

## Project Structure

### Documentation (this feature)

```text
specs/001-distributed-betting-ledger/
├── plan.md              # This file (/speckit.plan command output)
├── spec.md              # Feature specification (/speckit.specify)
├── research.md          # Phase 0 output: decisions D-1..D-12
├── data-model.md        # Phase 1 output: entities, DDL sketch, invariant→test map
├── quickstart.md        # Phase 1 output: runnable validation guide
├── contracts/
│   ├── openapi.yaml     # HTTP API contract (BET/WIN/LOSS/REFUND/ROLLBACK, balances, audit)
│   └── wallet-ops.fifo.json  # SQS message envelope + attribute contract
└── checklists/
    └── requirements.md  # Spec quality checklist
```

### Source Code (repository root)

```text
cmd/ironledger/
└── main.go                      # fx.New composition root; the ONLY place fx appears

internal/
├── domain/                      # PURE. stdlib imports only. No fx, no http, no pgx, no sqs.
│   ├── money.go                 # Money{AmountMinor int64, Currency}, parse/format, no float
│   ├── wallet.go                # Wallet aggregate: Balance, Credit, Debit, invariants
│   ├── operation.go             # OperationType (BET/WIN/LOSS/REFUND/ROLLBACK), Operation
│   ├── ledger.go                # LedgerEntry, reversal/compensation rules
│   ├── bet.go                   # Bet aggregate + state machine (OPEN→SETTLED/VOIDED)
│   ├── errors.go                # Typed domain errors → stable reason codes
│   ├── ports.go                 # Interfaces: LedgerRepository, OutboxWriter, Clock, IDGen
│   └── *_test.go                # Domain tests: no Docker, no network, no DB
│
├── usecase/                     # Application services. Depends on domain ports only.
│   ├── apply_operation.go       # One use case for all 5 types; both channels call it
│   ├── idempotency.go           # Key claiming + replay of the original result
│   ├── get_balance.go
│   └── list_ledger.go
│   └── *_test.go                # Use cases tested against in-memory fake ports
│
├── platform/
│   ├── postgres/                # Adapters implementing domain ports
│   │   ├── db.go                # pgxpool construction, tx helper, health check
│   │   ├── tx.go                # withTx: BEGIN/COMMIT + bounded retry on 40001/40P01
│   │   ├── sqlc-or-raw.go       # explicit SQL constants (no ORM)
│   │   ├── ledger_repo.go       # FOR UPDATE wallet lock, insert entries, balance update
│   │   ├── operations_repo.go   # idempotency claim via ON CONFLICT DO NOTHING
│   │   ├── inbox_repo.go        # message dedup claim
│   │   └── outbox_repo.go       # SKIP LOCKED polling + mark published
│   │
│   ├── sqs/
│   │   ├── client.go            # aws-sdk-go-v2 sqs.Client, BaseEndpoint for LocalStack
│   │   ├── consumer.go          # long-poll loop, per-message tx, delete after commit
│   │   ├── envelope.go          # message decode/validate
│   │   └── publisher.go         # outbox → SQS, FIFO group id = wallet_id
│   │
│   ├── httpapi/
│   │   ├── server.go            # net/http mux, Go 1.22 method+wildcard patterns
│   │   ├── handlers_*.go        # one handler per operation + balance + audit
│   │   ├── dto.go               # request/response mapping; int64 only, no float in JSON
│   │   └── middleware.go        # auth, request id, logging, recovery, tenant
│   │
│   ├── keycloak/
│   │   ├── verifier.go          # keyfunc JWKS cache + jwt v5 validation
│   │   └── claims.go            # azp/scope/tenant extraction
│   │
│   ├── fxapp/                   # fx modules wiring ports → adapters
│   │   ├── app.go
│   │   ├── modules.go
│   │   └── config.go            # env-driven config struct
│   │
│   ├── logging/                 # slog JSON handler + redaction
│   └── observability/           # metrics counters, reconciliation report writer
│
├── migrations/
│   ├── 001_extensions.sql
│   ├── 002_wallets.sql
│   ├── 003_bets.sql
│   ├── 004_ledger_entries.sql
│   ├── 005_operations.sql
│   ├── 006_inbox.sql
│   ├── 007_outbox.sql
│   ├── 008_audit_log.sql
│   ├── 009_immutability_guards.sql
│   └── 010_roles_and_grants.sql
│
└── test/
    ├── integration/             # testcontainers: postgres + localstack
    │   ├── harness_test.go
    │   ├── ledger_concurrency_test.go   # SC-006, Principle VII
    │   ├── idempotency_restart_test.go # SC-003, Principle VI
    │   ├── no_negative_balance_test.go # SC-002, Principle III
    │   ├── append_only_test.go          # Principle II
    │   ├── reconciliation_test.go       # SC-001, SC-007
    │   └── sqs_equivalence_test.go      # SC-005, User Story 4
    └── boundaries/
        └── import_test.go       # asserts internal/domain imports no fx/http/pgx/sqs
```

**Structure Decision**: single Go module, one binary that runs the HTTP server, the SQS
consumer and the outbox publisher as separate Fx-lifecycle-managed goroutine groups, so any
instance can be scaled to any role independently later. The `internal/domain` →
`internal/usecase` → `internal/platform` layering is what makes Constitution Principle IV
mechanically checkable; `test/boundaries/import_test.go` fails the build if the domain ever
imports `go.uber.org/fx`, `net/http`, `github.com/jackc/pgx`, or
`github.com/aws/aws-sdk-go-v2`. Migrations are plain SQL applied by a small runner rather than
an ORM, per the "explicit SQL and migrations" requirement.

## Post-Design Constitution Re-check

Re-evaluated after `research.md`, `data-model.md` and `contracts/` were written.

| Check | Result | Change since pre-design gate |
|-------|--------|------------------------------|
| I. Zero-Float Money | PASS | contracts/openapi.yaml uses `integer` + `currency` for every monetary field; no `number`/`format: double` anywhere. JSON encoding uses `int64`, never `float64`. |
| II. Append-Only Ledger | PASS | Final DDL uses a `BEFORE UPDATE OR DELETE` trigger plus `REVOKE`. The `CREATE RULE ... DO INSTEAD NOTHING` option was **rejected** because `INSERT ... ON CONFLICT` cannot target a table that has an `UPDATE`/`INSERT` rule, and the ledger needs `ON CONFLICT` for idempotency (research.md D-5). |
| III. DB-Enforced Invariants | PASS | `CHECK (balance_minor >= 0)`; `UNIQUE (wallet_id, idempotency_key)`; `UNIQUE (wallet_id, transaction_id)`; `CHECK (amount_minor > 0)`; partial unique index for one settlement per bet. Race test proved the constraint, not app code, is what prevents the negative balance. |
| IV. Pure Domain Core | PASS | No design change needed; the boundary test is a task, and the layering is unchanged. |
| V. Post-Commit Publication | PASS | Outbox insert shares the transaction; publisher is a separate lifecycle component using `FOR UPDATE SKIP LOCKED`. |
| VI. Durable Idempotency | PASS | `INSERT ... ON CONFLICT DO NOTHING` claim; note from research.md D-6 that the app role must hold `SELECT` on `operations` to detect the "already exists" case via `RowsAffected() == 0`. |
| VII. Per-Wallet Coordination | PASS | Per-wallet `FOR UPDATE` + `MessageGroupId = wallet_id`. Hot-wallet mitigation documented, not implemented (YAGNI), with the fixed-bucket precondition recorded. |

**Final gate result: PASS.** No violations, no unresolved clarifications, no Complexity Tracking
entries. Two amendments were required and completed during this phase: the Go floor (D-1) and
the explicit FIFO/multi-currency rules, both folded into constitution v1.0.1.

## Deferred (documented, not implemented)

| Item | Why deferred | Revisit when |
|------|---------------|---------------|
| Hot-wallet group sharding (`wallet_id:bucket`) | YAGNI: no measured hot wallet yet. Bucket count must be fixed per wallet for its lifetime, so it is expensive to retrofit. | A wallet exceeds the single-group throughput ceiling |
| Currency conversion engine | Out of scope for this release; constitution requires an explicit, audited operation with a recorded rate, which is a separate feature | Business requests multi-currency settlement |
| Withdrawal / deposit / bonus | Out of scope per spec Assumptions | New feature |
| Outbound event consumers | Outbox exists and is populated, but no downstream contract is defined in this release | First downstream consumer is specified |
