---

description: "Task list for the Distributed Betting Ledger feature"
---

# Tasks: Distributed Betting Ledger

**Input**: Design documents from `/specs/001-distributed-betting-ledger/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Test tasks ARE included and are required by constitution v1.0.1 — "Toda alteracao
que envolva dinheiro, invariantes, idempotencia ou publicacao de eventos MUST vir acompanhada
de testes que demonstrem a invariante". Write every test first and confirm it fails before
implementing.

**Organization**: Tasks are grouped by user story so each story can be implemented, tested and
shipped independently.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## Path Conventions

Single Go module, repository root. Layout is fixed by `plan.md` → "Project Structure":
`cmd/`, `internal/domain/`, `internal/usecase/`, `internal/platform/`, `migrations/`,
`test/integration/`, `test/boundaries/`.

## Governing constraints (apply to every task)

These come from `data-model.md` and `research.md`. Quoting them in tasks so they are not left to
implementation-time discretion:

- Money is `int64` minor units + `CHAR(3)` currency. **No `FLOAT`/`REAL`/`DOUBLE PRECISION`
  column may exist.** Supported currencies: `BRL`, `USD`, both 2 decimals.
- `CHECK (wallets.balance_minor >= 0)` — the no-negative-balance guarantee.
- `CHECK (operations.amount > 0)`, `CHECK (ledger_entries.amount_minor > 0)`,
  `CHECK (ledger_entries.direction IN (-1, 1))`, `CHECK (ledger_entries.balance_after_minor >= 0)`.
- `UNIQUE (wallet_id, idempotency_key)` on `operations`.
- `UNIQUE (wallet_id, transaction_id)` on `operations`.
- `UNIQUE (wallet_id, sequence)` on `ledger_entries`.
- `UNIQUE (message_id)` on `inbox`.
- `ledger_entries` MUST reject `UPDATE` and `DELETE` via trigger **and** `REVOKE`.
- Coordination is per-wallet only: `SELECT ... FOR UPDATE` on the wallet row.
  **No global lock, no single advisory key, no leader election.**
- Events are published only after commit, via an outbox row in the same transaction.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Project initialization and basic structure

- [ ] T001 Create the directory tree from plan.md "Source Code": `cmd/ironledger/`, `internal/{domain,usecase,platform/{postgres,sqs,httpapi,keycloak,fxapp,logging,observability}}/`, `migrations/`, `test/{integration,boundaries}/`
- [x] T002 Initialize the Go module with `go 1.25` in `go.mod` (floor set by pgx v5.11 + aws-sdk-go-v2 — research.md D-1)
- [x] T003 [P] Add dependencies to `go.mod`: `github.com/jackc/pgx/v5`, `go.uber.org/fx`, `github.com/aws/aws-sdk-go-v2` + `/config` + `/credentials` + `/service/sqs`, `github.com/golang-jwt/jwt/v5`, `github.com/MicahParks/keyfunc/v3`, `github.com/stretchr/testify`, `github.com/testcontainers/testcontainers-go` + `/modules/postgres` + `/modules/localstack`
- [x] T004 [P] Add `golangci-lint` config in `.golangci.yml` (enable `govet`, `errcheck`, `staticcheck`, `unused`) and a `Makefile` with `test`, `test-race`, `lint`, `migrate-up`, `migrate-down` targets
- [x] T005 [P] Create `docker-compose.yml` with pinned `postgres:16`, a **pinned** `localstack` image tag (never `latest` — FIFO fidelity has regressed before, research.md D-9), and `keycloak` with the audience mapper configured on the `ironledger-api` client
- [ ] T006 [P] Create `internal/platform/fxapp/config.go` with a `Config` struct populated from env: `DATABASE_URL`, `SQS_ENDPOINT`, `SQS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `KEYCLOAK_ISSUER`, `KEYCLOAK_AUDIENCE`, `KEYCLOAK_ALLOWED_CLIENTS`, `WALLET_OPS_QUEUE`, `WALLET_OPS_DLQ`, `WALLET_EVENTS_QUEUE`
- [x] T007 [P] Create `internal/platform/logging/logger.go`: `slog` JSON handler with a `ReplaceAttr` that redacts `authorization`, `token`, `password`, `client_secret`
- [x] T008 Create `test/boundaries/import_test.go` — walk the parsed import graph of `internal/domain` and `internal/usecase` with `go/parser` and fail if it contains `go.uber.org/fx`, `net/http`, `github.com/jackc/pgx`, `github.com/aws/aws-sdk-go-v2`, `database/sql` (research.md D-12; enforces constitution Principle IV)
- [ ] T009 Create `test/integration/harness_test.go` — testcontainers harness starting Postgres and LocalStack, exposing helpers `newWallet(t, currency, balanceMinor)`, `mustApply(t, op)`, `ledgerEntries(t, walletID)`; pin the LocalStack image version in `harness_test.go`
- [x] T010 [P] Add `internal/domain/money_test.go` asserting exact `int64` round-trip of `1234567890123` minor units through JSON with zero precision loss (constitution Principle I)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core infrastructure that MUST be complete before ANY user story can be implemented

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

### Schema and migrations

- [x] T011 Create the migration runner in `internal/platform/postgres/migrate.go` with a `schema_migrations` table storing name + checksum, sequential `NNN_name.up.sql` / `.down.sql` files applied in order, and an advisory lock **scoped to the migration table only** (never a global lock on business data)
- [x] T012 Create `migrations/001_extensions.sql` — extensions and helper functions used by later migrations
- [x] T013 Create `migrations/002_wallets.sql` — `wallets` with `id UUID PK`, `tenant_id TEXT NOT NULL`, `player_id TEXT NOT NULL`, `currency CHAR(3) NOT NULL` with `CHECK (currency IN ('BRL','USD'))`, `balance_minor BIGINT NOT NULL` with `CHECK (balance_minor >= 0)`, `status TEXT NOT NULL` with `CHECK (status IN ('ACTIVE','FROZEN'))`, `version BIGINT NOT NULL`, timestamps; index on `(tenant_id, player_id)`
- [x] T014 Create `migrations/003_bets.sql` — `bets` with `id UUID PK`, `wallet_id UUID NOT NULL REFERENCES wallets`, `tenant_id TEXT NOT NULL`, `external_bet_ref TEXT NOT NULL`, `stake_minor BIGINT NOT NULL` with `CHECK (stake_minor > 0)`, `currency CHAR(3) NOT NULL`, `status TEXT NOT NULL` with `CHECK (status IN ('OPEN','SETTLED_WIN','SETTLED_LOSS','VOIDED','REVERSED'))`, `settled_by_operation_id UUID`, `created_at`/`settled_at`; `UNIQUE (tenant_id, external_bet_ref)`; plus `CREATE UNIQUE INDEX bets_one_settlement ON bets (id) WHERE settled_by_operation_id IS NOT NULL` to enforce one settlement per bet
- [x] T015 Create `migrations/004_ledger_entries.sql` — append-only ledger with all columns from data-model.md: `id BIGSERIAL PK`, `entry_uid UUID NOT NULL UNIQUE`, `wallet_id UUID NOT NULL REFERENCES wallets`, `tenant_id TEXT NOT NULL`, `operation_id UUID NOT NULL REFERENCES operations`, `bet_id UUID`, `entry_type TEXT NOT NULL` with `CHECK (entry_type IN ('BET_DEBIT','WIN_CREDIT','REFUND_CREDIT','ROLLBACK_CREDIT','ROLLBACK_DEBIT'))`, `direction SMALLINT NOT NULL` with `CHECK (direction IN (-1, 1))`, `amount_minor BIGINT NOT NULL` with `CHECK (amount_minor > 0)`, `currency CHAR(3) NOT NULL`, `balance_after_minor BIGINT NOT NULL` with `CHECK (balance_after_minor >= 0)`, `reverses_entry_id BIGINT REFERENCES ledger_entries`, `sequence BIGINT NOT NULL`, `occurred_at TIMESTAMPTZ NOT NULL`, `recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()`; `UNIQUE (wallet_id, sequence)`; index on `(wallet_id, sequence)` for the point-in-time balance read
- [x] T016 Create `migrations/005_operations.sql` — idempotency + operations table with `id UUID PK`, `wallet_id UUID NOT NULL REFERENCES wallets`, `tenant_id TEXT NOT NULL`, `idempotency_key TEXT NOT NULL`, `request_hash BYTEA NOT NULL`, `operation_type TEXT NOT NULL` with `CHECK (operation_type IN ('BET','WIN','LOSS','REFUND','ROLLBACK'))`, `transaction_id TEXT NOT NULL`, `bet_id UUID`, `amount BIGINT NOT NULL` with `CHECK (amount > 0)`, `currency CHAR(3) NOT NULL`, `status TEXT NOT NULL`, `result_code TEXT NOT NULL`, `result_body JSONB NOT NULL`, `channel TEXT NOT NULL` with `CHECK (channel IN ('API','SQS'))`, `actor TEXT NOT NULL`, `created_at TIMESTAMPTZ NOT NULL`; `UNIQUE (wallet_id, idempotency_key)` and `UNIQUE (wallet_id, transaction_id)`
- [x] T017 Create `migrations/006_inbox.sql` — `inbox` with `id BIGSERIAL PK`, `message_id TEXT NOT NULL UNIQUE`, `operation_id UUID`, `queue_url TEXT NOT NULL`, `message_group_id TEXT NOT NULL`, `attempts INT NOT NULL`, `last_error TEXT`, `first_seen_at`, `processed_at TIMESTAMPTZ` (`processed_at IS NULL` means not yet applied)
- [x] T018 Create `migrations/007_outbox.sql` — `outbox` with `id BIGSERIAL PK`, `event_uid UUID NOT NULL UNIQUE`, `aggregate_type TEXT NOT NULL`, `aggregate_id UUID NOT NULL`, `event_type TEXT NOT NULL` with `CHECK (event_type IN ('wallet.bet_placed','wallet.bet_settled','wallet.bet_reversed','wallet.refunded'))`, `payload JSONB NOT NULL`, `message_group_id TEXT NOT NULL`, `dedup_id TEXT NOT NULL`, `created_at TIMESTAMPTZ NOT NULL`, `published_at TIMESTAMPTZ`; index on `(published_at, id)` for polling
- [x] T019 Create `migrations/008_audit_log.sql` — append-only `audit_log` with `id BIGSERIAL PK`, `tenant_id TEXT NOT NULL`, `wallet_id UUID`, `occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()`, `actor TEXT NOT NULL`, `channel TEXT NOT NULL`, `operation_type TEXT`, `idempotency_key TEXT`, `transaction_id TEXT`, `message_id TEXT`, `outcome TEXT NOT NULL` with `CHECK (outcome IN ('ACCEPTED','REJECTED'))`, `reason_code TEXT`, `detail JSONB`
- [x] T020 Create `migrations/009_immutability_guards.sql` — the `reject_mutation()` trigger function and `BEFORE UPDATE OR DELETE ... FOR EACH ROW` triggers on `ledger_entries` **and** `audit_log` that raise an exception (research.md D-5: a trigger, **not** `CREATE RULE`, because `INSERT ... ON CONFLICT` cannot target a table with an `UPDATE`/`INSERT` rule)
- [x] T021 Create `migrations/010_roles_and_grants.sql` — create `ironledger_app`, `ironledger_reader`, `ironledger_migrator`; grant `SELECT, INSERT, UPDATE` on `wallets`/`bets` to `ironledger_app`, `SELECT, INSERT` on `operations`/`inbox`/`audit_log`, `SELECT, INSERT, UPDATE` on `outbox`, `SELECT, INSERT` on `ledger_entries`; then `REVOKE UPDATE, DELETE ON ledger_entries FROM PUBLIC` and from `ironledger_app`

### Pure domain core (stdlib imports only)

- [x] T022 Create `internal/domain/money.go` — `type Currency string` with `BRL`/`USD` only, `type Money struct { AmountMinor int64; Currency Currency }`; `Parse` rejects empty, non-numeric, negative, exponent notation (`1e3`) and more than 2 decimal places, **never rounding**; `Add`/`Sub` require equal currencies; `String()` formats via integer arithmetic only; no float32/float64 anywhere in the file
- [x] T023 Create `internal/domain/errors.go` — typed domain errors and the stable reason codes from `contracts/openapi.yaml`: `INSUFFICIENT_FUNDS`, `BET_NOT_FOUND`, `BET_ALREADY_SETTLED`, `CURRENCY_MISMATCH`, `WALLET_FROZEN`, `INVALID_AMOUNT`, `INVALID_CURRENCY`, `IDEMPOTENCY_KEY_CONFLICT`, `DUPLICATE_TRANSACTION_ID`, `REFERENCE_ALREADY_EXISTS`, `INVALID_OPERATION_STATE`, with each carrying the `availableMinor`/`walletCurrency` detail fields from the contract
- [x] T024 Create `internal/domain/ports.go` — the interfaces the domain depends on: `LedgerRepository` (LockWallet, AppendEntry, UpdateBalance, GetBet, SetBetStatus), `OperationsRepository` (ClaimIdempotency, GetByIdempotencyKey, FindByTransactionId, FindByExternalBetRef), `AuditWriter` (Record), `Clock`, `IDGenerator`. Declared as interfaces **in the domain**, never in `internal/platform`
- [x] T025 Create `internal/domain/wallet.go` — `Wallet` aggregate with `Balance` and `Status`; `Debit(m Money)` rejects when the balance would go below zero or the currency differs; `Credit(m Money)`; both return a new value rather than mutating
- [x] T026 Create `internal/domain/bet.go` — `Bet` aggregate and state machine per data-model.md: `OPEN → SETTLED_WIN | SETTLED_LOSS` via settlement, `OPEN → VOIDED` via REFUND, and `SETTLED_* → REVERSED` via ROLLBACK; reject any transition not listed
- [x] T027 Create `internal/domain/ledger.go` — `LedgerEntry` value type with `EntryType` (`BET_DEBIT`, `WIN_CREDIT`, `REFUND_CREDIT`, `ROLLBACK_CREDIT`, `ROLLBACK_DEBIT`), `Direction` (`-1`/`+1`, sign lives here not in the amount), `ReversesEntryID` for compensating entries, and helpers computing the next per-wallet `sequence`
- [x] T028 Create `internal/domain/operation.go` — `OperationType` (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`), `Channel` (`API`, `SQS`), and the `Operation` struct matching the request body in `contracts/openapi.yaml` (`betId` required for WIN/LOSS/REFUND/ROLLBACK, `operationId` required for ROLLBACK, `amountMinor` strictly positive)

### Platform adapters and composition

- [x] T029 Create `internal/platform/postgres/db.go` and `internal/platform/postgres/tx.go` — `pgxpool.New` construction, and a `withTx` helper at `READ COMMITTED` that runs `BEGIN`/`COMMIT` and retries with exponential backoff plus jitter on SQLSTATE `40001` and `40P01` (max 5 attempts), **never** retrying `23505` or `23514` (research.md D-7)
- [x] T030 Create `internal/platform/keycloak/verifier.go` and `internal/platform/keycloak/claims.go` — `keyfunc` JWKS cache with `RefreshUnknownKID` and a rate-limited refresh, `jwt.Parse` with `jwt.WithValidMethods([]string{"RS256"})`, `jwt.WithIssuer(cfg.Issuer)`, `jwt.WithAudience(cfg.Audience)`, `jwt.WithExpirationRequired()`, `jwt.WithLeeway(30*time.Second)`; then check `azp` against `KEYCLOAK_ALLOWED_CLIENTS` and extract `scope` and the realm-derived tenant
- [x] T031 Create `internal/platform/httpapi/server.go` and `internal/platform/httpapi/middleware.go` — `http.NewServeMux` with Go 1.22 method+wildcard patterns, `ReadHeaderTimeout` set, and middleware chain in order: request id → recovery → logging → tenant → auth
- [x] T032 Create `internal/platform/fxapp/app.go`, `modules.go` and `cmd/ironledger/main.go` — `fx.New` composition root; all connection and goroutine lifetimes registered via `fx.Lifecycle` hooks, never created inside a constructor body (research.md D-11); wire `fxevent.SlogLogger` into the app logger

**Checkpoint**: Foundation ready — user story implementation can now begin

---

## Phase 3: User Story 1 - Register bet and debit the wallet (Priority: P1) MVP

**Goal**: A player places a bet, the wallet is debited, an append-only entry is written, and a
repeat of the same request never debits twice.

**Independent Test**: Submit one BET to a freshly funded wallet over HTTP and assert the balance
and the single ledger entry; then submit an unaffordable BET and assert rejection with no entry.
Requires no other user story.

### Tests for User Story 1 (write FIRST, confirm they FAIL)

- [x] T033 [P] [US1] Test the BET happy path and the insufficient-funds rejection in `test/integration/bet_test.go` using `newWallet`/`mustApply` from the harness
- [x] T034 [P] [US1] Test idempotent replay (same `Idempotency-Key` 3× → one entry, identical response body, `X-Idempotent-Replay: true`) and the conflict case (same key, different body → `409 IDEMPOTENCY_KEY_CONFLICT`, stored result NOT replayed) in `test/integration/bet_test.go`
- [x] T035 [P] [US1] Test idempotency survives a full process restart in `test/integration/idempotency_restart_test.go` — commit, tear down the harness connection, reconnect, resend the same key, assert one entry
- [x] T036 [P] [US1] Test the `CHECK (amount_minor > 0)` and `CHECK (balance_minor >= 0)` rejections and the BRL/USD currency-mismatch rejection in `test/integration/bet_test.go`

### Implementation for User Story 1

- [x] T037 [US1] Implement `LockWallet` (first statement of the transaction, `SELECT ... FROM wallets WHERE id = $1 FOR UPDATE`) and `UpdateBalance` (`UPDATE wallets SET balance_minor = balance_minor + $2`) in `internal/platform/postgres/ledger_repo.go` (depends on T029)
- [x] T038 [US1] Implement `AppendEntry` inserting a `BET_DEBIT` row with `direction = -1`, the next `sequence` and `balance_after_minor` in `internal/platform/postgres/ledger_repo.go`
- [x] T039 [US1] Implement `ClaimIdempotency` using `INSERT INTO operations (...) ON CONFLICT (wallet_id, idempotency_key) DO NOTHING` and branching on `result.RowsAffected()`, plus `GetByIdempotencyKey` and `FindByTransactionId` in `internal/platform/postgres/operations_repo.go` (research.md D-6)
- [x] T040 [US1] Implement the BET branch of `internal/usecase/apply_operation.go`: claim the key → lock the wallet → validate currency and balance → append the entry → update the balance → store the result body — all inside one transaction
- [x] T041 [US1] Implement `internal/usecase/idempotency.go` — on a successful claim proceed; on a conflict, compare `request_hash` and either replay the stored `result_body` verbatim or return `IDEMPOTENCY_KEY_CONFLICT`
- [x] T042 [US1] Implement request/response mapping in `internal/platform/httpapi/dto.go` — `amountMinor` as `int64`, currency as a string, stable `reason_code`→HTTP-status mapping, and a `replace` handler that emits `application/json`
- [x] T043 [US1] Implement `POST /v1/wallets/{walletId}/operations` for the BET case in `internal/platform/httpapi/handlers_operations.go`, including `Idempotency-Key` validation (`400 MISSING_IDEMPOTENCY_KEY` when absent) and the `X-Idempotent-Replay` header
- [x] T044 [US1] Implement `Record` writing an `ACCEPTED` or `REJECTED` row with `reason_code` in `internal/platform/postgres/audit_repo.go`, in the **same transaction** as the outcome (a rejection writes audit only, never an `operations` row)
- [x] T045 [US1] Implement `internal/usecase/get_balance.go` and `GET /v1/wallets/{walletId}` in `internal/platform/httpapi/handlers_wallet.go`
- [x] T046 [US1] Register the routes in `internal/platform/httpapi/server.go` and validate scenarios V1–V3 from `quickstart.md` end to end

**Checkpoint**: User Story 1 is fully functional — BET debits exactly once, negative balances
are impossible, and the balance reconciles with the ledger

---

## Phase 4: User Story 2 - Settle the outcome as WIN or LOSS (Priority: P2)

**Goal**: An open bet is settled — WIN credits the prize, LOSS closes the bet with no money
movement — and a bet can only be settled once.

**Independent Test**: Place a bet via the API, settle two different bets as WIN and LOSS, and
assert the resulting balances and the `bet_id` references on the entries.

### Tests for User Story 2 (write FIRST)

- [ ] T047 [P] [US2] Test WIN crediting the prize and LOSS moving no money in `test/integration/settlement_test.go`
- [ ] T048 [P] [US2] Test that two concurrent settlements of the same bet produce exactly one success and one `BET_ALREADY_SETTLED` in `test/integration/settlement_test.go` (proves the `bets_one_settlement` partial unique index, not application code)
- [ ] T049 [P] [US2] Test that a repeated WIN with the same idempotency key credits the prize exactly once in `test/integration/settlement_test.go`

### Implementation for User Story 2

- [ ] T050 [US2] Implement `GetBet` (loading the bet `FOR UPDATE`) and `SetBetStatus` in `internal/platform/postgres/ledger_repo.go`
- [ ] T051 [US2] Implement the WIN and LOSS branches in `internal/usecase/apply_operation.go` — WIN appends a `WIN_CREDIT` with `direction = +1`; LOSS appends **no** entry and only transitions the bet
- [ ] T052 [US2] Implement `internal/domain/operation.go` validation requiring `betId` for WIN and LOSS and rejecting `amountMinor <= 0`
- [ ] T053 [US2] Extend `POST /v1/wallets/{walletId}/operations` for WIN and LOSS in `internal/platform/httpapi/handlers_operations.go`, including `BET_NOT_FOUND` (transient per D-2) and `BET_ALREADY_SETTLED`
- [ ] T054 [P] [US2] Add the `wallet.bet_settled` outbox row in `internal/usecase/apply_operation.go`, written in the same transaction as the settlement
- [ ] T055 [P] [US2] Create `internal/platform/postgres/outbox_repo.go` with `Enqueue` for use in this and later stories
- [ ] T056 [US2] Validate scenarios for settlement from `quickstart.md` V7 (WIN half) end to end

**Checkpoint**: User Stories 1 and 2 both work independently; the bet lifecycle is complete
from placement to settlement

---

## Phase 5: User Story 3 - Correct a recorded operation (Priority: P3)

**Goal**: A wrong operation is fixed with compensating entries — REFUND for a debit, ROLLBACK
for any operation including an already-settled bet — without touching the original entries.

**Independent Test**: Place a bet, apply a REFUND, then apply a ROLLBACK on a separate operation,
and verify the originals are byte-identical, the compensating entries exist and reference them,
and the balance reflects exactly those corrections.

### Tests for User Story 3 (write FIRST)

- [ ] T057 [P] [US3] Test REFUND crediting back a previous debit while leaving the original entry untouched in `test/integration/refund_test.go`
- [ ] T058 [P] [US3] Test that a repeated REFUND with the same key does not credit twice in `test/integration/refund_test.go`
- [ ] T059 [P] [US3] Test ROLLBACK of a settled WIN writing compensating entries and setting `ReversesEntryID`, and that the WIN entry is unchanged in `test/integration/rollback_test.go`
- [ ] T060 [P] [US3] Test that a ROLLBACK which would require a negative balance is rejected with `INSUFFICIENT_FUNDS` and leaves the balance untouched in `test/integration/rollback_test.go` (D-4)
- [ ] T061 [P] [US3] Test that a direct `UPDATE` and `DELETE` on `ledger_entries` as `ironledger_app` both fail, proving append-only enforcement in `test/integration/append_only_test.go`

### Implementation for User Story 3

- [ ] T062 [US3] Implement the REFUND branch in `internal/usecase/apply_operation.go` — validate the referenced debit exists and is not already refunded, append a `REFUND_CREDIT` with `direction = +1`, transition the bet to `VOIDED`
- [ ] T063 [US3] Implement the ROLLBACK branch in `internal/usecase/apply_operation.go` — load the target operation and its entries, compute the compensating direction, validate the resulting balance stays `>= 0` before writing, append one or two entries with `ReversesEntryID` set, transition the bet to `REVERSED`
- [ ] T064 [P] [US3] Add the `wallet.refunded` and `wallet.bet_reversed` outbox rows in `internal/usecase/apply_operation.go`
- [ ] T065 [P] [US3] Extend `internal/domain/ledger.go` with `CompensatingEntryFor` returning the reversal entry type and direction for a given original entry type
- [ ] T066 [US3] Extend `POST /v1/wallets/{walletId}/operations` for REFUND and ROLLBACK in `internal/platform/httpapi/handlers_operations.go`, including the `operationId` requirement for ROLLBACK
- [ ] T067 [US3] Add the `wallet.refunded` / `wallet.bet_reversed` event types to the `outbox` payload builders in `internal/usecase/apply_operation.go`
- [ ] T068 [US3] Validate the full correction cycle from `quickstart.md` V6 and V7 end to end

**Checkpoint**: All three financial stories work independently; every correction is auditable
and the ledger is provably append-only

---

## Phase 6: User Story 4 - Process operations from the queue with the same guarantees (Priority: P4)

**Goal**: The SQS FIFO consumer applies operations with identical results to the HTTP channel,
deduplicates redelivery, and isolates poison messages.

**Independent Test**: Publish BET, WIN, LOSS, REFUND and ROLLBACK messages for one wallet and
apply the same five over HTTP to a parallel wallet, then assert identical balances and ledger
shapes.

### Tests for User Story 4 (write FIRST)

- [ ] T069 [P] [US4] Test API/queue equivalence for all five operation types in `test/integration/channel_equivalence_test.go` (SC-005, FR-003)
- [ ] T070 [P] [US4] Test that the same `messageId` delivered three times produces exactly one effect in `test/integration/channel_equivalence_test.go`
- [ ] T071 [P] [US4] Test that a rolled-back transaction leaves no outbox row and no published event, and that a committed one publishes only after the commit in `test/integration/outbox_test.go` (constitution Principle V)
- [ ] T072 [P] [US4] Test that a malformed or unknown-`schemaVersion` message is isolated without blocking later messages in `test/integration/consumer_failure_test.go`
- [ ] T073 [P] [US4] Test that a WIN whose bet is not yet visible is retried with backoff and then isolated with the reason recorded in `test/integration/consumer_failure_test.go` (D-2)
- [ ] T074 [P] [US4] Contract test for FIFO group semantics in `test/integration/sqs_fifo_contract_test.go` — only one in-flight message per `MessageGroupId`, correct head-of-group promotion after `DeleteMessage` (research.md D-9: LocalStack FIFO fidelity is unverified, so this must be asserted explicitly rather than assumed)

### Implementation for User Story 4

- [ ] T075 [US4] Implement `internal/platform/sqs/client.go` — `sqs.Client` via `sqs.NewFromConfig` with `Options.BaseEndpoint` for LocalStack (never the deprecated `EndpointResolver`) and `credentials.NewStaticCredentialsProvider("test","test","")` in tests
- [ ] T076 [US4] Implement `internal/platform/sqs/envelope.go` — decode and validate the message body from `contracts/wallet-ops.fifo.md`, rejecting unknown `schemaVersion`, bad `tenantId`, non-integer or non-positive `amountMinor`, and a currency outside `BRL`/`USD`
- [ ] T077 [US4] Implement `Claim` (`INSERT INTO inbox (message_id, ...) ON CONFLICT (message_id) DO NOTHING`, branching on `RowsAffected()`) and `MarkProcessed` in `internal/platform/sqs/inbox_repo.go`
- [ ] T078 [US4] Implement `internal/platform/sqs/consumer.go` — long-poll loop (`WaitTimeSeconds = 20`, `MaxNumberOfMessages = 10`, client HTTP read timeout above 20s), per-message transaction, `DeleteMessage` **only after commit**, never acking a batch atomically, and same-group messages applied in the order returned
- [ ] T079 [US4] Implement the failure classifier in `internal/platform/sqs/consumer.go` — permanent (malformed, unknown schema, validation, `INSUFFICIENT_FUNDS`, `WALLET_FROZEN`, `BET_ALREADY_SETTLED`, `CURRENCY_MISMATCH`) vs transient (`BET_NOT_FOUND`, SQLSTATE `40001`/`40P01`, lock timeout, connection reset, context deadline), with `ChangeMessageVisibility` backoff for transients and letting `maxReceiveCount = 5` route permanents to the DLQ
- [ ] T080 [US4] Implement `Poll` using `SELECT ... WHERE published_at IS NULL ORDER BY id FOR UPDATE SKIP LOCKED` and `MarkPublished` in `internal/platform/postgres/outbox_repo.go`
- [ ] T081 [US4] Implement `internal/platform/sqs/publisher.go` — send to `wallet-events.fifo` with `MessageGroupId = wallet_id` and `MessageDeduplicationId = event_uid`
- [ ] T082 [US4] Create the queue bootstrap (idempotent `CreateQueue` with `FifoQueue=true`, the redrive policy and the FIFO DLQ) in `internal/platform/sqs/bootstrap.go`, and register the consumer and publisher as `fx.Lifecycle` components in `internal/platform/fxapp/modules.go`

**Checkpoint**: The queue channel is fully functional and provably equivalent to the API

---

## Phase 7: User Story 5 - Stay correct across multiple instances and failures (Priority: P5)

**Goal**: The balance stays exactly correct with many instances running and with crashes
between processing steps, with per-wallet coordination only.

**Independent Test**: Fire tens of thousands of concurrent operations at one wallet from
multiple instances, killing and restarting processes mid-flight, then assert the final balance
equals the ledger sum exactly.

### Tests for User Story 5 (write FIRST)

- [ ] T083 [P] [US5] Test that 100 concurrent bets of 8000 against a balance of 10000 leave exactly one success and a final balance of 2000, and that no read ever observes a negative balance, in `test/integration/no_negative_balance_test.go`
- [ ] T084 [P] [US5] Test 10,000 concurrent operations on one wallet against an exactly known expected total in `test/integration/ledger_concurrency_test.go` (SC-006)
- [ ] T085 [P] [US5] Test that two distinct wallets make progress concurrently, proving the absence of a global lock, in `test/integration/ledger_concurrency_test.go` (Principle VII, SC-009)
- [ ] T086 [P] [US5] Test that two instances receiving the same key produce exactly one effect in `test/integration/ledger_concurrency_test.go`
- [ ] T087 [P] [US5] Test crash-after-commit-then-retry: kill the process between the commit and the HTTP response, restart, resend, assert one entry and the replayed original response in `test/integration/idempotency_restart_test.go`
- [ ] T088 [P] [US5] Test that a crash between `SendMessage` and `MarkPublished` re-publishes with the same `dedup_id` and no duplicate effect in `test/integration/outbox_test.go`

### Implementation for User Story 5

- [ ] T089 [US5] Add the `LockWallet` `FOR UPDATE` call as the mandatory first statement of every mutating transaction in `internal/usecase/apply_operation.go`, and add a startup assertion in `internal/domain/ports.go` consumers that no repository method may mutate a balance without it
- [ ] T090 [P] [US5] Tune the retry policy in `internal/platform/postgres/tx.go` — 5 attempts, exponential backoff with full jitter, `40001`/`40P01` only
- [ ] T091 [P] [US5] Add per-wallet observability in `internal/platform/observability/metrics.go` — counters for lock wait time, retry count, rejection reason and DLQ depth, so a hot wallet or a contended lock is visible
- [ ] T092 [P] [US5] Run the whole integration suite with `-race` in `test/integration/` and fix every reported race
- [ ] T093 [P] [US5] Load-test to the plan's targets (≥500 ops/sec aggregate, p95 < 300 ms via API, p95 < 5 s enqueue-to-commit) with multiple instances in `test/integration/load_test.go`
- [ ] T094 [US5] Document the hot-wallet mitigation escape hatch (`MessageGroupId = wallet_id:bucket`, bucket count fixed for the wallet's lifetime) in `docs/runbook.md`, without implementing it (YAGNI)
- [ ] T095 [US5] Validate scenarios V3, V5 and V8 from `quickstart.md` under repeated restarts

**Checkpoint**: Correctness holds under concurrency and failure; the service scales horizontally

---

## Phase 8: User Story 6 - Audit and reconcile the financial history (Priority: P6)

**Goal**: An auditor can read a wallet's complete history, see why each operation was refused,
and confirm the reported balance is the sum of the valid entries.

**Independent Test**: After any sequence of operations, read the history, sum it by hand and
compare with the reported balance — including on a wallet with contested operations.

### Tests for User Story 6 (write FIRST)

- [ ] T096 [P] [US6] Test that after 10,000 random operations including failures and restarts, `wallets.balance_minor` equals `SUM(direction * amount_minor)` exactly in `test/integration/reconciliation_test.go` (SC-001)
- [ ] T097 [P] [US6] Test that every rejected request has exactly one `audit_log` row with a populated `reason_code`, and that no rejection wrote an `operations` row, in `test/integration/reconciliation_test.go`
- [ ] T098 [P] [US6] Test that any wallet's balance at a past instant is readable from a single indexed read using `balance_after_minor` and `sequence`, in `test/integration/reconciliation_test.go`

### Implementation for User Story 6

- [ ] T099 [US6] Implement `internal/usecase/list_ledger.go` — cursor-paginated chronological read using `(wallet_id, sequence)` and optional `asOf` filtering, exposing `balanceAfterMinor` and `reversesEntryId` on every entry
- [ ] T100 [US6] Implement `GET /v1/wallets/{walletId}/ledger` with `limit`, `cursor` and `asOf` in `internal/platform/httpapi/handlers_ledger.go`, returning `nextCursor`
- [ ] T101 [US6] Implement `internal/usecase/list_audit.go` and `GET /v1/wallets/{walletId}/audit` with an `outcome` filter in `internal/platform/httpapi/handlers_audit.go`, returning accepted **and** rejected records
- [ ] T102 [US6] Implement `internal/usecase/reconcile.go` — recompute `SUM(direction * amount_minor)` per wallet and diff against the materialized balance, reporting `storedBalanceMinor`, `ledgerBalanceMinor` and `firstDivergentSequence` for each divergence
- [ ] T103 [US6] Implement `POST /v1/reconciliation` in `internal/platform/httpapi/handlers_reconcile.go`
- [ ] T104 [P] [US6] Implement a scheduled reconciliation report in `internal/platform/observability/reconcile_report.go`, emitting an alert when `divergences` is non-empty
- [ ] T105 [US6] Add the `ironledger_reader` role's read-only access check in `test/integration/reconciliation_test.go` and validate scenario V10 from `quickstart.md`

**Checkpoint**: The full feature is auditable and self-verifying

---

## Phase 9: Polish & Cross-Cutting Concerns

**Purpose**: Improvements that affect multiple user stories

- [ ] T106 [P] Add `docs/runbook.md` — operational procedures for DLQ replay, reconciliation divergence response, and Keycloak audience-mapper setup
- [ ] T107 [P] Add `docs/architecture.md` — the layering, the three correctness mechanisms (durable idempotency, per-wallet row lock, transactional outbox) and how each constitution principle maps to code and tests
- [ ] T108 Run the full validation gate: `go vet ./...`, `golangci-lint run`, `gofmt -l .`, `go test -race ./...`
- [ ] T109 Run every scenario in `quickstart.md` (V1–V10) against a clean environment and record the observed output
- [ ] T110 [P] Security hardening: confirm no money field anywhere is `float32`/`float64`, that `slog` redaction covers `authorization`/`token`/`client_secret`, and that the app role holds no `UPDATE`/`DELETE` on `ledger_entries`
- [ ] T111 [P] Verify the domain purity test fails when a forbidden import is deliberately introduced into `internal/domain`, then confirm it passes again
- [ ] T112 [P] Add CI wiring in `.github/workflows/ci.yml` running lint, the domain suites (no Docker) and the integration suite (Docker available)
- [ ] T113 Measure against the plan's performance targets and tune the pgx pool size, statement timeouts and consumer concurrency in `internal/platform/fxapp/config.go`
- [ ] T114 Remove the temporary Sync Impact Report comment from `.specify/memory/constitution.md` before committing the amendment
- [ ] T115 Final constitution compliance review: confirm every requirement in plan.md "Post-Design Constitution Re-check" still passes

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — can start immediately
- **Foundational (Phase 2)**: Depends on Setup — **BLOCKS all user stories**
- **User Stories (Phases 3–8)**: All depend on Foundational
  - Stories can then proceed in parallel if staffed
  - Or sequentially in priority order P1 → P2 → P3 → P4 → P5 → P6
- **Polish (Phase 9)**: Depends on all desired user stories being complete

### User Story Dependencies

- **User Story 1 (P1)**: No dependencies on other stories — this is the MVP
- **User Story 2 (P2)**: Depends on US1 only for the `bets` table population, not for correctness; independently testable
- **User Story 3 (P3)**: Depends on US1 and US2 entries existing to reverse; independently testable once they do
- **User Story 4 (P4)**: Requires the outbox from US2/US3 to publish; the consumer itself is testable after US1
- **User Story 5 (P5)**: Requires US1–US4 to have real operations to stress; the concurrency tests themselves are written in parallel
- **User Story 6 (P6)**: Requires US1–US3 to have history to reconcile

### Within Each User Story

- Tests MUST be written and confirmed to FAIL before implementation (constitution workflow)
- Models before services, services before endpoints
- Core implementation before integration
- Story complete before moving to the next priority

### Parallel Opportunities

- All Setup tasks marked [P]
- All migration tasks in Phase 2 are sequential by number (T012 → T021) but T022–T028 domain tasks can run in parallel with them
- T030, T031 are independent of T029
- Once Foundational completes, **all six user stories can start in parallel** by different developers
- All test tasks within a story marked [P]
- All `[P]` tasks within US3, US4, US5, US6 and Polish

---

## Parallel Example: User Story 1

```text
Launch all tests for User Story 1 together:
  Task: "T033 Test the BET happy path and the insufficient-funds rejection"
  Task: "T034 Test idempotent replay and the conflict case"
  Task: "T035 Test idempotency survives a full process restart"
  Task: "T036 Test the amount, balance and currency-mismatch rejections"

Launch all platform adapters together:
  Task: "T037 Implement LockWallet and UpdateBalance"
  Task: "T039 Implement ClaimIdempotency"
  Task: "T044 Implement Record"
```

## Parallel Example: User Story 4

```text
Launch all tests for User Story 4 together:
  Task: "T069 Test API/queue equivalence for all five operation types"
  Task: "T070 Test that the same messageId delivered three times produces one effect"
  Task: "T071 Test outbox rollback and post-commit publication"
  Task: "T072 Test poison-message isolation"
  Task: "T073 Test retry-then-isolate for a not-yet-visible bet"
  Task: "T074 Contract test for FIFO group semantics"

Launch independent adapters together:
  Task: "T075 Implement the SQS client"
  Task: "T077 Implement the inbox claim"
  Task: "T080 Implement outbox polling with SKIP LOCKED"
```

---

## Implementation Strategy

### MVP First (User Story 1 only)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational (CRITICAL — blocks all stories)
3. Complete Phase 3: User Story 1
4. **STOP and VALIDATE**: run scenarios V1–V3 from `quickstart.md`
5. Deploy/demo

The MVP already enforces the two properties that make the system trustworthy — the balance
can never go negative and a repeat can never double-apply — because both live in the schema
built during Phase 2, not in the US1 code.

### Incremental Delivery

1. Setup + Foundational → foundation ready
2. US1 → BET debits exactly once → **MVP**
3. US2 → WIN/LOSS completes the bet lifecycle
4. US3 → REFUND/ROLLBACK makes the system operationally correctable
5. US4 → the queue channel, with proven equivalence
6. US5 → concurrency and failure guarantees at scale
7. US6 → audit and reconciliation

Each story adds value without breaking the previous ones.

### Parallel Team Strategy

1. Team completes Setup + Foundational together
2. Then in parallel:
   - Developer A: User Stories 1 + 2 (the bet lifecycle)
   - Developer B: User Story 4 (the queue channel)
   - Developer C: User Story 3 (corrections)
3. US5 and US6 follow once there is real history to stress and reconcile

---

## Notes

- [P] = different files, no dependencies on incomplete tasks
- [Story] label maps each task to a user story for traceability
- Every test task names the invariant it proves; the mapping lives in `data-model.md` → "Invariant → enforcement → test map"
- Money is `int64` minor units + currency everywhere; a `float` in any form is a defect, not a style issue
- `ledger_entries` has no update or delete path in any task; corrections are new entries
- Events are published only after commit; the outbox row is written inside the same transaction
- Idempotency state lives only in PostgreSQL; no task may introduce a process-memory cache
- Commit after each task or logical group
- Stop at any checkpoint to validate the story independently

---

## Phase 10: Convergence

**Purpose**: Gaps between spec/plan/tasks intent and the current code, found by `/speckit.converge`.
Each item is work that was not anticipated by any existing task, or where a task is marked complete
but the behaviour it promises is not present. Existing task IDs are never reused or renumbered.

- [ ] T116 Reorder the HTTP middleware chain to `withRequestID -> recoverPanic -> logRequests -> mux` in `internal/platform/httpapi/server.go:123` so the access log correlates with the `X-Request-Id` the caller receives, and add a test asserting the logged `request_id` equals the response header (currently `logRequests` reads the pre-`withRequestID` request at `middleware.go:87` and always logs an empty id) per T031 (partial)
- [ ] T117 Make the database role separation actually enforceable and verified: give `ironledger_app`, `ironledger_reader` and `ironledger_migrator` a `LOGIN` grant or document the intended `SET ROLE` usage in `migrations/010_roles_and_grants.up.sql`, add an integration test that connects as `ironledger_app` and proves `UPDATE`/`DELETE` on `ledger_entries` and `audit_log` are refused and that `operations` holds no `UPDATE` privilege, and change the documented runtime DSN in `.env.example` from the container superuser to the application role, per T021 and T110 and Constitution Principles II and III (partial)
- [ ] T118 Make the tenant derivation in `internal/platform/keycloak/tenant.go` match the documented model: either derive the tenant from the verified `iss` only, or add an explicit `SharedRealm`/`AllowTenantClaim` configuration flag and refuse a `tenant_id` claim when it is not set, because the current code honours the claim unconditionally while its own comment claims a configuration gate that does not exist, per FR-029 and data-model.md (contradicts)
- [ ] T119 Replace the text-matching float guard in `test/boundaries/import_test.go:228-239` with a `go/ast` struct-field walk that inspects the real type of every field whose name is monetary, and prove the rewritten guard fails when a `float64` money field is planted in `internal/domain` and passes once removed, per Constitution Principle I and T110 (partial)
- [ ] T120 Propagate `cfg.Auth.JWKSURL` into the verifier: extend `keycloak.DefaultConfig` to accept the JWKS URL and pass it from `ProvideVerifier` in `internal/platform/fxapp/app.go:112-115`, which currently drops it so `jwksURL()` always derives from the realm URL, and add a test that a realm reachable under a different internal name than it issues in tokens still validates, per T006, T030 and FR-028 (partial)
- [ ] T121 Repair `specs/001-distributed-betting-ledger/quickstart.md` so its scenarios are runnable as written: use the `IRONLEDGER_*` environment variables the binary actually reads instead of `DATABASE_URL`/`SQS_*`/`AWS_*`/`KEYCLOAK_*`, replace the non-existent `migrate up` and `serve` subcommand invocations at lines 36-37, 140 and 146 with the real start-up procedure, replace the `ironledger_migrator:dev` DSN at line 27 which cannot connect because the role has no `LOGIN`, and add an explicit funded-wallet seed before V1 so `$WALLET_ID` exists and the V2/V3 scenarios do not reuse a balance already spent by V1, per T046 and T109 (partial)
- [ ] T122 Honour `JWKSCacheTTL` in `internal/platform/keycloak/claims.go:keyFor`: the cached key is returned at line 107 before the `stale()` check at line 129, which is reachable only for an unknown kid, so a key removed from the realm stays trusted for the lifetime of the process; refresh on expiry and add a test covering key rotation, and remove the unreachable block at lines 124-128, per T030 and the documented contract in `internal/platform/keycloak/verifier.go:38` (partial)
- [ ] T123 Add the `migrate-up` and `migrate-down` targets required by T004 to the `Makefile`, wiring them to `postgres.Up` and `postgres.Down` which already exist but are currently unreachable from the Makefile, per T004 (partial)
- [ ] T124 Add `client_secret` to `redactedKeys` in `internal/platform/logging/logger.go:42-49`, which T007 and T110 both name explicitly but which the handler does not redact, and add a test in a new `internal/platform/logging/logger_test.go` proving that `authorization`, `token`, `password` and `client_secret` are all redacted, per T007 and T110 (partial)
- [ ] T125 Pin the LocalStack image in `docker-compose.yml:28` to an exact version instead of the floating `localstack/localstack:3` major tag, and add a Keycloak realm export that creates the `tenant-br` realm, the `ironledger-api` client and its audience mapper, which the compose file currently omits even though `verifier.go:28-30` names the mapper a deployment prerequisite and the `Makefile` `token` target assumes that realm and client, per T005 and research.md D-9 (partial)
- [ ] T126 Reconcile `go.mod` with the dependency set T003 declares: add `github.com/MicahParks/keyfunc/v3` and the `aws-sdk-go-v2` modules plus `testcontainers-go/modules/localstack`, or amend T003, T030 and research.md D-10 to record that the JWKS cache is hand-rolled in `internal/platform/keycloak` and that the AWS SDK and LocalStack module arrive with US4, per T003 and research.md D-10 (partial)
- [ ] T127 Wire `fxevent.SlogLogger` into the app logger as T032 requires, replacing the `fx.NopLogger` passed at `internal/platform/fxapp/app.go:216`, and build the startup version line in `Run` from the loaded configuration instead of re-reading `IRONLEDGER_LOG_LEVEL` from the environment at `app.go:205` so the process cannot log at a level that disagrees with the configured one, per T032 (partial)
