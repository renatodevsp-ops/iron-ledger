# Data Model: Distributed Betting Ledger

**Feature**: 001-distributed-betting-ledger | **Date**: 2026-09-28

PostgreSQL 16+. All monetary columns are `BIGINT` in the currency's minimum unit plus a
`CHAR(3)` ISO 4217 code. **No `FLOAT`, `REAL` or `DOUBLE PRECISION` column exists in this
schema, and no `numeric` column is used for money** — a `NUMERIC` column would be exact but
would also accept a float on the way in, and `BIGINT` makes the wrong value unrepresentable
rather than merely discouraged (Constitution I).

---

## ER overview

```text
tenants ──< wallets ──< bets ──< ledger_entries
                  │        │           │
                  │        │           └── reverses_entry_id (self-reference)
                  │        └── bet_id
                  ├──< operations        (idempotency; 1:1 with the effect it produced)
                  ├──< inbox             (message dedup; 1:1 with a SQS MessageId)
                  └──< audit_log

outbox  (independent table; same transaction as ledger_entries)
```

Tenants are modelled at the `tenant_id` column level rather than as a separate table: the
tenant identity comes from the validated Keycloak token (`iss`), and a table would add a
foreign key to enforce what the token already guarantees.

---

## Entities

### Money (Go value type, `internal/domain/money.go`)

| Field | Type | Rules |
|-------|------|-------|
| `AmountMinor` | `int64` | Minor units. BRL and USD both have 2 decimals, so 1 unit = 0.01. Negative only for signed ledger deltas, never for a requested operation amount. |
| `Currency` | `char(3)` / Go string | ISO 4217. Only `BRL` and `USD` are accepted. |

```go
type Money struct {
    AmountMinor int64
    Currency    Currency // "BRL" | "USD"
}
```

Invariants:
- `Parse` rejects an empty value, a non-numeric value, a negative value, exponent notation
  (`1e3`), and more than 2 decimal places. It never rounds.
- `Add`/`Sub` require equal currencies and return an error otherwise.
- `String()` renders `BRL 30.00` for humans. There is **no float formatting path**;
  formatting divides by 100 using integer arithmetic.
- JSON is `{"amountMinor": 3000, "currency": "BRL"}`. `encoding/json` never sees a `float64`
  on the money path.

### Wallet

| Field | Type | Null | Rules |
|-------|------|------|-------|
| `id` | `UUID` | no | PK |
| `tenant_id` | `TEXT` | no | From the validated token issuer |
| `player_id` | `TEXT` | no | External player reference |
| `currency` | `CHAR(3)` | no | `BRL` or `USD`; immutable after creation |
| `balance_minor` | `BIGINT` | no | `CHECK (balance_minor >= 0)`. Materialized, always reconcilable with `ledger_entries`. |
| `status` | `TEXT` | no | `ACTIVE` \| `FROZEN`. A non-ACTIVE wallet rejects all operations. |
| `version` | `BIGINT` | no | Incremented per mutation; diagnostic aid for reconciliation, not the concurrency mechanism (that is the row lock) |
| `created_at`, `updated_at` | `TIMESTAMPTZ` | no | |

Relations: one wallet to many bets, many ledger entries, many operations, many inbox rows.

State: `ACTIVE` ⇄ `FROZEN`. `FROZEN` rejects all mutations; balance reads stay available.

### Operation (also the idempotency record)

| Field | Type | Null | Rules |
|-------|------|------|-------|
| `id` | `UUID` | no | PK |
| `wallet_id` | `UUID` | no | FK → wallets |
| `tenant_id` | `TEXT` | no | |
| `idempotency_key` | `TEXT` | no | `UNIQUE (wallet_id, idempotency_key)` |
| `request_hash` | `BYTEA` (SHA-256) | no | Hash of the normalized request. A repeat with a different hash is a conflict, not a replay. |
| `operation_type` | `TEXT` | no | `BET` \| `WIN` \| `LOSS` \| `REFUND` \| `ROLLBACK` |
| `transaction_id` | `TEXT` | no | `UNIQUE (wallet_id, transaction_id)` — the user's explicit requirement. The upstream's business transaction reference. |
| `bet_id` | `UUID` | nullable | Required for WIN, LOSS, REFUND and ROLLBACK |
| `amount` | `BIGINT` | no | `CHECK (amount > 0)` |
| `currency` | `CHAR(3)` | no | Must equal the wallet's currency |
| `status` | `TEXT` | no | `COMPLETED` (only rows exist; a failed operation leaves no row) |
| `result_code` | `TEXT` | no | Stable result code, replayed verbatim on a duplicate request |
| `result_body` | `JSONB` | no | The original response, stored so a replay returns byte-identical results |
| `channel` | `TEXT` | no | `API` \| `SQS` |
| `actor` | `TEXT` | no | `azp` of the caller, or the SQS producer identity |
| `created_at` | `TIMESTAMPTZ` | no | |

**Why a failed operation leaves no row**: rejections are recorded in `audit_log`, not here.
This table's only purpose is the idempotency guarantee, and a row for a failed operation would
mean a retry after a legitimate fix (e.g. after funds are deposited) is permanently poisoned.

Relations: many to one Wallet; one to zero or one LedgerEntry set; one to many Inbox rows
(one operation can arrive via more than one message when the same key is republished).

### Bet

| Field | Type | Null | Rules |
|-------|------|------|-------|
| `id` | `UUID` | no | PK |
| `wallet_id` | `UUID` | no | FK → wallets |
| `tenant_id` | `TEXT` | no | |
| `external_bet_ref` | `TEXT` | no | `UNIQUE (tenant_id, external_bet_ref)` |
| `stake_minor` | `BIGINT` | no | `CHECK (stake_minor > 0)` |
| `currency` | `CHAR(3)` | no | Must equal the wallet's currency |
| `status` | `TEXT` | no | `OPEN` \| `SETTLED_WIN` \| `SETTLED_LOSS` \| `VOIDED` \| `REVERSED` |
| `settled_by_operation_id` | `UUID` | nullable | FK → operations |
| `created_at`, `settled_at` | `TIMESTAMPTZ` | nullable | |

**One settlement per bet, enforced in the database**: a partial unique index
`CREATE UNIQUE INDEX bets_one_settlement ON bets (id) WHERE settled_by_operation_id IS NOT
NULL;` combined with the status transition rule. A second WIN for the same bet hits a
constraint violation, not a race in application code (FR-020, Constitution III).

State transitions:

```text
            BET                    WIN / LOSS
  (none) ────────► OPEN ──────────────────────────► SETTLED_WIN
                     │                              SETTLED_LOSS
                     │
                     │ REFUND                     ROLLBACK (any state)
                     └────────────────────────────► VOIDED
                                                    REVERSED
```

`REVERSED` is reachable from `SETTLED_WIN` and `SETTLED_LOSS` via ROLLBACK (D-4). `VOIDED` is
reachable from `OPEN` via REFUND.

### LedgerEntry (append-only)

| Field | Type | Null | Rules |
|-------|------|------|-------|
| `id` | `BIGSERIAL` | no | PK. Monotonic, so a range scan is chronological |
| `entry_uid` | `UUID` | no | `UNIQUE`. The identifier exposed in API responses |
| `wallet_id` | `UUID` | no | FK → wallets |
| `tenant_id` | `TEXT` | no | |
| `operation_id` | `UUID` | no | FK → operations |
| `bet_id` | `UUID` | nullable | FK → bets |
| `entry_type` | `TEXT` | no | `BET_DEBIT` \| `WIN_CREDIT` \| `REFUND_CREDIT` \| `ROLLBACK_CREDIT` \| `ROLLBACK_DEBIT` |
| `direction` | `SMALLINT` | no | `-1` (debit) or `+1` (credit). CHECK `(direction IN (-1, 1))` |
| `amount_minor` | `BIGINT` | no | `CHECK (amount_minor > 0)`. Always positive; sign lives in `direction` |
| `currency` | `CHAR(3)` | no | Must equal the wallet's currency |
| `balance_after_minor` | `BIGINT` | no | `CHECK (balance_after_minor >= 0)`. Snapshot after this entry, so any historical balance is readable without replaying from zero |
| `reverses_entry_id` | `BIGINT` | nullable | Self-FK → ledger_entries.id. Set on compensating entries only |
| `sequence` | `BIGINT` | no | Per-wallet monotonic counter, `UNIQUE (wallet_id, sequence)` |
| `occurred_at` | `TIMESTAMPTZ` | no | Business time (from the request, or the message timestamp) |
| `recorded_at` | `TIMESTAMPTZ` | no | DB insert time, default `now()` |

**Immutability is enforced three ways** (D-5):
1. `CREATE TRIGGER ledger_entries_no_mutation BEFORE UPDATE OR DELETE ON ledger_entries FOR
   EACH ROW EXECUTE FUNCTION reject_mutation();` — raises `ledger_entry_immutable`.
2. `REVOKE UPDATE, DELETE ON ledger_entries FROM ironledger_app;`
3. The migration/owner role is separate, so the app never runs with owner rights.

**`sequence` and reconciliation**: `UNIQUE (wallet_id, sequence)` is what makes
`SELECT SUM(direction * amount_minor) FROM ledger_entries WHERE wallet_id = $1 AND sequence
<= $n` a deterministic, index-backed read of the balance at any point. Reconciliation is
`wallets.balance_minor = SUM(direction * amount_minor)` over all entries — one indexed
aggregate, exact by construction, no float (integer multiply in SQL `BIGINT` arithmetic).

**Volume note**: one BET is one `BET_DEBIT` entry; a WIN is one `WIN_CREDIT`; a ROLLBACK writes
one or two entries (credit the original debit, or debit the original credit). LOSS writes
nothing — it changes bet state only, which is why its entry list is empty by design.

### Inbox (message deduplication)

| Field | Type | Null | Rules |
|-------|------|------|-------|
| `id` | `BIGSERIAL` | no | PK |
| `message_id` | `TEXT` | no | `UNIQUE`. The producer's own operation key, not SQS's `MessageId` — it survives queue redrive and replay across environments |
| `operation_id` | `UUID` | nullable | FK → operations, set when the message produced an effect |
| `queue_url` | `TEXT` | no | |
| `message_group_id` | `TEXT` | no | `wallet_id` |
| `attempts` | `INT` | no | Increments per failed processing; drives retry vs. DLQ |
| `last_error` | `TEXT` | nullable | |
| `first_seen_at`, `processed_at` | `TIMESTAMPTZ` | nullable | `processed_at IS NULL` means not yet applied |

Claimed with `INSERT ... ON CONFLICT (message_id) DO NOTHING`; `RowsAffected() == 0` means
already processed, so the handler returns success and the message is deleted. The claim commits
with the effect, so a crash mid-transaction leaves no claim and the message is cleanly retried.

### Outbox (transactional event publishing)

| Field | Type | Null | Rules |
|-------|------|------|-------|
| `id` | `BIGSERIAL` | no | PK, monotonic |
| `event_uid` | `UUID` | no | `UNIQUE`. Exposed to consumers as the idempotency key |
| `aggregate_type` | `TEXT` | no | `wallet` \| `bet` |
| `aggregate_id` | `UUID` | no | |
| `event_type` | `TEXT` | no | `wallet.bet_placed`, `wallet.bet_settled`, `wallet.bet_reversed`, `wallet.refunded` |
| `payload` | `JSONB` | no | Event body. Monetary fields are `amountMinor` + `currency` integers |
| `message_group_id` | `TEXT` | no | `wallet_id` — the FIFO group |
| `dedup_id` | `TEXT` | no | `event_uid` — SQS `MessageDeduplicationId` |
| `created_at` | `TIMESTAMPTZ` | no | |
| `published_at` | `TIMESTAMPTZ` | nullable | `NULL` = not yet sent. Rows are never deleted. |

Publisher polls `SELECT ... WHERE published_at IS NULL ORDER BY id LIMIT $n FOR UPDATE SKIP
LOCKED`, sends, then sets `published_at`. `SKIP LOCKED` lets N publisher instances run without
coordination (Constitution VII). A crash between send and mark causes a re-send with the same
`dedup_id` — collapsed by the FIFO dedup window when it is within 5 minutes, and by the
consumer's inbox when it is not.

### AuditLog

| Field | Type | Rules |
|-------|------|-------|
| `id` | `BIGSERIAL` | PK |
| `tenant_id`, `wallet_id` | `TEXT`, `UUID` nullable | |
| `occurred_at` | `TIMESTAMPTZ` | Default `now()` |
| `actor`, `channel` | `TEXT` | `azp` (API) or producer identity (SQS) |
| `operation_type` | `TEXT` nullable | |
| `idempotency_key`, `transaction_id`, `message_id` | `TEXT` nullable | The three ways a caller can be correlated |
| `outcome` | `TEXT` | `ACCEPTED` \| `REJECTED` |
| `reason_code` | `TEXT` nullable | Populated for rejections |
| `detail` | `JSONB` | Non-sensitive context only |

Append-only like the ledger. Every rejection lands here, which is what makes "100% das recusas
são justificadas por um motivo registrado" (SC-010) testable. Rejections recorded here do not
write to `operations`, so a legitimate retry after a fix is not blocked.

---

## Roles

```sql
CREATE ROLE ironledger_app  NOINHERIT;  -- runtime
CREATE ROLE ironledger_reader NOINHERIT; -- read-only reporting
CREATE ROLE ironledger_migrator;         -- owns schema, used only by migrations

GRANT USAGE ON SCHEMA public TO ironledger_app, ironledger_reader;

GRANT SELECT, INSERT, UPDATE ON wallets, bets TO ironledger_app;
GRANT SELECT, INSERT ON operations, inbox, audit_log TO ironledger_app;
GRANT SELECT, INSERT, UPDATE ON outbox TO ironledger_app;   -- UPDATE only sets published_at
GRANT SELECT, INSERT ON ledger_entries TO ironledger_app;    -- deliberately no UPDATE/DELETE

REVOKE UPDATE, DELETE ON ledger_entries FROM PUBLIC;

GRANT SELECT ON wallets, bets, ledger_entries, operations, audit_log TO ironledger_reader;
```

The app holds no `UPDATE`/`DELETE` on `ledger_entries`, so the trigger is a second line of
defence rather than the only one (D-5).

---

## Invariant → enforcement → test map

| # | Invariant | Enforced by | Test |
|---|-----------|-------------|------|
| 1 | No negative balance (Principle III, SC-002) | `CHECK (balance_minor >= 0)` + wallet row lock | `test/integration/no_negative_balance_test.go` — 100 concurrent 80% bets on a 100 balance: exactly one succeeds, final balance 20 |
| 2 | Append-only (Principle II) | `BEFORE UPDATE OR DELETE` trigger + `REVOKE` + no code path | `test/integration/append_only_test.go` — direct `UPDATE`/`DELETE` as the app role must fail; correcting a BET produces a new entry with the original intact |
| 3 | Idempotent replay (Principle VI, SC-003) | `UNIQUE (wallet_id, idempotency_key)` + `ON CONFLICT DO NOTHING` | `test/integration/idempotency_restart_test.go` — same key 50× across 3 processes, then restart all; one entry, stored response replayed |
| 4 | Idempotency survives restart (SC-003) | Same table, no in-memory state anywhere | `test/integration/idempotency_restart_test.go` — kill the process after commit, restart, resend the same key |
| 5 | One settlement per bet (FR-020) | Partial unique index + `CHECK (status IN (...))` | `test/integration/ledger_concurrency_test.go` — two concurrent WINs for one bet: one succeeds, one rejected |
| 6 | No float anywhere (Principle I) | `BIGINT` columns + Go `int64` + `json` integer | `test/boundaries/import_test.go` (no float-typed money field), plus a test asserting JSON round-trip of `1234567890123` minor units is exact |
| 7 | Post-commit publication only (Principle V) | Outbox row shares the transaction; publisher is separate | `test/integration/sqs_equivalence_test.go` — a rolled-back transaction leaves no outbox row and no published event |
| 8 | Redelivery is a no-op (FR-022, SC-008) | `UNIQUE (message_id)` on inbox | `test/integration/sqs_equivalence_test.go` — same message delivered 3× → one effect |
| 9 | Balance reconciles with the ledger (SC-001, SC-007) | Append-only design + per-entry `balance_after_minor` | `test/integration/reconciliation_test.go` — 10k random ops incl. failures and restarts; `balance_minor = SUM(direction*amount_minor)` exactly |
| 10 | Per-wallet coordination, cross-wallet parallelism (Principle VII, SC-006, SC-009) | `FOR UPDATE` on the wallet row; `MessageGroupId = wallet_id` | `test/integration/ledger_concurrency_test.go` — 10k ops on one wallet (exact expected total) + a timing assertion that two distinct wallets proceed concurrently |
| 11 | Cursor of every rejection (FR-030, SC-010) | `audit_log` row in the same transaction | `test/integration/reconciliation_test.go` — every rejected request has exactly one audit row with a reason code |
| 12 | API and SQS are equivalent (FR-003, SC-005) | One use case, two adapters | `test/integration/sqs_equivalence_test.go` — same operation via both channels on parallel wallets → identical balances and identical ledger shapes |
| 13 | Domain purity (Principle IV) | Package layout + import graph test | `test/boundaries/import_test.go` |
| 14 | Currency mismatch rejected (FR-014, D-3) | Domain check + wallet currency column | `test/integration/sqs_equivalence_test.go` — USD wallet + BRL operation → rejected, no entry |
| 15 | ROLLBACK refuses negative balance (D-4) | Domain check + `CHECK` constraint | `test/integration/no_negative_balance_test.go` — ROLLBACK of a WIN that would go negative → rejected, balance unchanged |

---

## Validation rules summary

| Rule | Value | Where enforced |
|------|-------|-----------------|
| Currencies | `BRL`, `USD` | Go domain type + `CHECK (currency IN ('BRL','USD'))` |
| Decimals | 2 (all supported currencies) | Parser rejects more; `CHECK` implicit in `BIGINT` |
| Amount | `> 0` for every operation | `CHECK (amount > 0)` |
| Balance | `>= 0` | `CHECK (balance_minor >= 0)` |
| Operation types | `BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK` | `CHECK` enumerations on each table |
| Wallet status | `ACTIVE`, `FROZEN` | `CHECK` + domain |
| Bet status | `OPEN`, `SETTLED_WIN`, `SETTLED_LOSS`, `VOIDED`, `REVERSED` | `CHECK` + domain state machine |
| Entry direction | `-1`, `+1` | `CHECK (direction IN (-1,1))` |
