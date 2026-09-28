# Research: Distributed Betting Ledger

**Date**: 2026-09-28 | **Feature**: 001-distributed-betting-ledger | **Phase**: 0

Every `NEEDS CLARIFICATION` from the spec is resolved below. Decisions D-1..D-4 came from the
user; D-5..D-12 are technical decisions from research and carry a rationale plus the
alternatives that were rejected.

---

## D-1. Go toolchain floor

**Decision**: Go 1.25 or later. The constitution was amended (v1.0.1) to move the floor from
1.22.

**Rationale**: Verified against `https://go.dev/doc/devel/release` and the module `go.mod`
files on 2026-09-28:

- Go 1.22, 1.23 and 1.24 are all **end-of-life** — the policy keeps a major supported until two
  newer majors exist. Current stable is 1.27.1.
- `github.com/jackc/pgx/v5@v5.11.0` declares `go 1.25.0`.
- `aws-sdk-go-v2` (core, `config` and `service/sqs`) declares `go 1.24`. Its README states
  "the v2 SDK requires a minimum version of Go 1.24".

Holding a 1.22 floor would have forced `pgx v5.7.4`, `service/sqs v1.42.3` and `config
v1.31.0` permanently — unpatched versions of the two components that handle money and the
network. For a financial system, an EOL toolchain with frozen dependencies is the worse
trade. The user's decision: raise the floor and amend the constitution.

The constitution clause was written as "Go 1.22+ ... Sem construcoes ou bibliotecas que exijam
versao superior", which was ambiguous between "at least 1.22" and "not more than 1.22". The
amendment removes the ambiguity: the floor is the lowest supported Go that the mandatory
drivers allow, and it must be revalidated whenever a driver is upgraded, never lowered to
accommodate a stale dependency.

**Alternatives considered**: keep 1.22 with pinned old drivers (rejected: frozen, unpatched
dependencies); raise to 1.26 or 1.27 (rejected: 1.25 is the lowest floor that satisfies both
drivers, and a lower floor leaves more headroom).

---

## D-2. Out-of-order settlement (spec Q1)

**Decision**: A WIN, LOSS, REFUND or ROLLBACK referencing a bet that does not exist yet is a
**transient** failure. The message is retried with progressive backoff for 5 attempts; if the
bet still does not exist, the message is isolated for later handling and the rejection is
recorded with the message id and reason.

**Rationale**: SQS FIFO with `MessageGroupId = wallet_id` already guarantees that a bet's
settlement is delivered after its own registration, because both messages share a group and a
group is delivered strictly in order. The retry path exists for the cases FIFO does not cover:
operations injected by a different producer, manual redrive from the DLQ, or a message
produced before a preceding one by an upstream that violated ordering. Classifying the
condition as transient costs at most a bounded delay in those rare cases; classifying it as
permanent would silently discard legitimate operations whenever ordering is not as clean as
the broker contract promises.

Rejected: immediate permanent rejection (loses real operations); a persistent pending-operation
state machine (adds a reconciliation surface and a timeout policy for a condition that FIFO
already prevents in the normal path — YAGNI).

---

## D-3. Currency model (spec Q2)

**Decision**: Wallets carry one currency. BRL and USD are supported, both with 2 decimal
places. An operation whose currency differs from the wallet's is rejected. No conversion
exists in this release.

**Rationale**: The constitution requires `int64` minor units **plus** the ISO 4217 code on
every amount, so the currency field is persisted either way. Supporting BRL and USD
concurrently costs nothing extra at the data layer and avoids a later migration of every
monetary column. What is deliberately absent is conversion: an implicit rate lookup inside a
balance operation would be exactly the kind of hidden monetary computation the constitution
forbids, and it needs its own audited design (rate source, rate timestamp, rounding residual,
reconciliation across rates).

Rejected: hardcoding BRL only (leaves a data migration for the next currency); a full
conversion engine (large scope, and per the constitution it must be an explicit audited
operation with a recorded rate — that is a separate feature).

---

## D-4. ROLLBACK semantics (spec Q3)

**Decision**: ROLLBACK reverses a target operation by writing compensating ledger entries,
including when the bet is already settled as WIN or LOSS. The original entries are never
touched. If the reversal would require a negative balance, ROLLBACK is **rejected** with a
stable reason and the balance is left untouched.

**Rationale**: Reversing a settled bet is a real operational need — a provider that settles
the wrong outcome, a voided bet settled in error. Without it the only remedy is REFUND, which
debits the player rather than undoing the original credit, leaving the ledger telling a story
that never happened. Compensating entries preserve Principle II and the audit trail. Making
the reversal conditional on available balance keeps Principle III's "no negative balance"
absolute, with no exception clause anywhere in the design.

Rejected: refusing ROLLBACK on settled bets (no path to fix a wrong WIN; pushes operators into
misleading REFUNDs); allowing a temporary negative balance (directly violates the constitution
and would require an amendment plus a "negative balance allowed" state machine).

---

## D-5. How to enforce append-only in PostgreSQL

**Decision**: A `BEFORE UPDATE OR DELETE` trigger on `ledger_entries` that raises an
exception, **plus** `REVOKE UPDATE, DELETE ON ledger_entries FROM ironledger_app`, **plus**
`REVOKE` from the migration role. A dedicated `ironledger_reader` role keeps `SELECT`.

**Rationale**: Three independent mechanisms, because each one has a hole:

- `REVOKE` alone fails if the application ever connects as the table owner — owners retain
  inherent modification rights.
- Triggers alone fail if a future migration or an operator tool runs as an owner/superuser.
- Together, a mistake requires deliberately escalating privileges.

**Alternatives considered and rejected**: `CREATE RULE ledger_entries_no_update AS ON UPDATE
TO ledger_entries DO INSTEAD NOTHING`. This looks elegant, but **PostgreSQL cannot use
`INSERT ... ON CONFLICT` on a table that has an `INSERT` or `UPDATE` rule**, and this design
needs `ON CONFLICT DO NOTHING` on `operations` and `inbox`, not on `ledger_entries`. Silently
accepted upserts are also a worse failure mode than a loud exception: a lost write looks like a
successful write. Going with trigger + revoke.

Gotcha recorded: `CHECK` and `NOT NULL` constraints cannot be `DEFERRABLE`, so the
no-negative-balance constraint is always checked immediately at statement end — which is what
we want, since there is no legitimate intermediate state.

---

## D-6. Idempotency claim inside the transaction

**Decision**: `INSERT INTO operations (wallet_id, idempotency_key, request_hash, ...) VALUES
(...) ON CONFLICT (wallet_id, idempotency_key) DO NOTHING`, then branch on
`result.RowsAffected()`.

- `RowsAffected() == 1` → this request is the first; proceed to apply the effect.
- `RowsAffected() == 0` → already processed; read and return the stored original response.

Both branches live inside the same transaction as the ledger write.

**Rationale**: The claim and the effect commit atomically, so there is no window where a key
is marked processed but the effect was lost, nor one where the effect is applied but the key
is not recorded. This is what makes idempotency survive a full restart (Constitution VI) —
there is no in-memory state to lose.

Gotcha: with `DO NOTHING`, Postgres still requires `SELECT` privilege on the table for the
`RETURNING`/read-back path. The app role gets `SELECT, INSERT` on `operations` — `UPDATE` and
`DELETE` are never needed, so they are not granted.

Duplicate keys with a **different** request body (`request_hash` mismatch) are rejected with a
distinct reason code rather than replaying the stored response, since replaying it would be
wrong.

---

## D-7. Per-wallet concurrency

**Decision**: `SELECT ... FROM wallets WHERE id = $1 FOR UPDATE` as the first statement of
every mutating transaction, at `READ COMMITTED`. Bounded retry (5 attempts, exponential backoff
with jitter) on SQLSTATE `40001` and `40P01`. Money movement happens as a single
`UPDATE wallets SET balance_minor = balance_minor + $2` after validation against the freshly
locked row.

**Rationale**: The row lock serializes only the operations touching one wallet, so distinct
wallets never contend — exactly Constitution VII. `READ COMMITTED` with `FOR UPDATE` re-reads
the updated row on lock acquisition, giving read-modify-write safety without the retry storms
that `SERIALIZABLE` produces under contention. `UPDATE ... = balance_minor + $2` is atomic at
the row level even before the `CHECK` is evaluated.

Rejected: `SERIALIZABLE` alone (unnecessary abort/retry volume for a single-row contention
point); `pg_advisory_xact_lock` (a correct per-wallet lock, but it requires every writer to
remember to take it, holds a pool connection for the transaction's duration, and hides the
contention point from anyone reading the SQL — the row lock is self-documenting); `NOWAIT` /
`SKIP LOCKED` for wallet locking (`SKIP LOCKED` is for queue-style work; skipping a contended
wallet would silently drop a balance operation).

Gotcha: `40001` must be retried, but `23505` (unique violation) and `23514` (check violation)
must **not** be retried — they indicate a real business-rule rejection that should surface as
an error to the caller.

---

## D-8. FIFO queue, `MessageGroupId = wallet_id`

**Decision**: `wallet-ops.fifo` with `FifoQueue=true`, `MessageGroupId = wallet_id`,
`MessageDeduplicationId = operation idempotency key`. The DLQ (`wallet-ops-dlq.fifo`) is also
FIFO, `maxReceiveCount = 5`, `VisibilityTimeout = 60s`.

**Rationale**: A FIFO group gives per-wallet ordering enforced by the broker, which is a
distributed per-wallet serialization primitive with no infrastructure to run — no lock
service, no leader election. It satisfies Constitution VII directly. The inbox still provides
the correctness guarantee: FIFO ordering is not at-most-once delivery.

**Constraints and mitigations, recorded honestly**:

- SQS allows **one in-flight message per group, queue-wide**. Per-wallet throughput is
  therefore `1 / operation latency`. No number of consumers raises it.
- FIFO deduplication is only a **5-minute window on the enqueue side**. Beyond it, duplicates
  are possible — which is why the inbox, not the dedup ID, is the correctness mechanism.
- A message moved to the DLQ leaves a hole in that group's total order; later messages in the
  group proceed. The balance is reconcilable from the append-only ledger, so this is
  recoverable.
- A hot wallet is a hard bottleneck. Mitigation, **not implemented** (YAGNI): shard the group
  id as `wallet_id:bucket(payment_id, N)`, with `N` fixed for the wallet's lifetime. Recorded
  in plan.md "Deferred".
- High-throughput FIFO mode (`DeduplicationScope=messageGroup` +
  `FifoThroughputLimit=perMessageGroupId`) raises the queue-wide TPS quota but **does not**
  increase per-group throughput. Not enabled.

Gotchas that shaped the consumer:

- `ReceiveMessage` has **no `NextToken`** and no paginator. The consumer is a long-poll loop.
- The SQS service module is `github.com/aws/aws-sdk-go-v2/service/sqs` at **v1.x** — there is
  no `sqs/v2` import path.
- `.fifo` alone is not enough: `FifoQueue=true` must also be set as a queue attribute, in AWS
  and in LocalStack.
- The `.fifo` suffix counts toward the 80-character queue-name limit.
- A FIFO queue's DLQ must also be FIFO ("Dead-letter queue must be same type of queue").
- `DeleteMessage` only **after** the transaction commits. Deleting before risks losing the
  message; not deleting after is safe because the inbox absorbs the redelivery.
- Never ack a 10-message batch atomically — one poison message would poison all ten.
- `maxReceiveCount` counts **receipts**, not failures; a visibility-timeout expiry counts too.

---

## D-9. LocalStack parity

**Decision**: `Options.BaseEndpoint` (client level, highest precedence) for the endpoint
override, `config.WithBaseEndpoint` at load time, and
`credentials.NewStaticCredentialsProvider("test", "test", "")` in tests. Queue creation is
idempotent (`CreateQueue` with the same attributes is a no-op) so the app can ensure its own
queues at startup. No `if localstack` branches in application code.

**Rationale**: The constitution requires LocalStack to be sufficient for dev and test with no
code difference in production. Endpoint resolution is the only environment-specific value, and
the SDK models that as data rather than branching logic.

Gotchas: `EndpointResolver`/`EndpointResolverWithURL` are **deprecated** in endpoint resolution
v2 — `BaseEndpoint` is the supported path. LocalStack needs SigV4 (anonymous credentials fail
with `InvalidSecurity`). `SQS_ENDPOINT_STRATEGY=standard` is the default and yields
`sqs.<region>.localhost.localstack.cloud:4566/...`; LocalStack binds IPv4 only, so that hostname
must resolve to `127.0.0.1`. LocalStack does not enforce `MessageRetentionPeriod` by default.
Purge and delete/recreate rate limits are disabled locally but **enabled in AWS**, so tests must
not depend on them.

**Unverified**: whether current LocalStack 4.x correctly models FIFO group ordering and
visibility semantics. There is a documented 3.0.2→3.1.0 regression where the next message of a
group is never delivered after its head is deleted, plus a history of dedup/visibility bugs.
Mitigation: pin the LocalStack image version and add an explicit FIFO contract test rather than
trusting local ordering behaviour.

---

## D-10. Keycloak token validation

**Decision**: `github.com/golang-jwt/jwt/v5` + `github.com/MicahParks/keyfunc/v3` for JWKS
caching. Validate signature (alg pinned to `RS256` via `jwt.WithValidMethods`), `iss` against
the realm issuer, `aud` against this API, `exp` (required) with 30s leeway. Then check `azp`
against the allowed client and `scope` for the required permission.

**Rationale**: `coreos/go-oidc/v3` is an **ID-token** verifier. Its API models ID-token
semantics (audience == the client that logged in), which is the opposite of the access-token
case: for `client_credentials`, `aud` is *this* API and `azp` is the caller. Using go-oidc would
require `SkipClientIDCheck: true` and then re-implementing most of what it does, plus it hardcodes
a 5-minute `nbf` leeway with no configurability and never checks `typ` or `scope`.
`golang-jwt` + `keyfunc` is the standard shape for validating a machine token.

**The `aud` trap**: in a Keycloak **access** token, `aud` is the **resource server**, not the
requesting client. By default the `audience resolve` mapper populates it from clients the
subject holds a role in — frequently yielding `"aud": ["account"]`. Without an audience mapper
on this API's client, a perfectly valid token gets a 401. This is a deployment prerequisite and
is recorded in quickstart.md.

**Tenant claim**: Keycloak has no standard `tenant` claim. Chosen: **realm per tenant**, with
`iss` validated per tenant. Zero mapper work and clean key separation. Documented in
contracts/openapi.yaml as a prerequisite. (Alternatives — a custom `tenant_id` protocol mapper,
or one client per tenant — both kept a single JWKS but couple authz to identity shape.)

Other gotchas: pin `KC_HOSTNAME` so `iss` is stable across environments; never send
`scope=openid` for a client-credentials request; rate-limit JWKS refresh on unknown `kid` so a
stream of garbage `kid`s cannot force constant refetches (a DoS vector); keep retired keys
cached longer than the maximum token lifetime.

---

## D-11. Fx composition

**Decision**: One binary; `cmd/ironledger/main.go` calls `fx.New` with modules for config,
logging, postgres, sqs, keycloak, httpapi, consumer and publisher. Any connection or goroutine
lifetime is registered via `fx.Lifecycle` hooks, never created in a constructor body.

**Rationale**: Fx's start hooks run in dependency order (a constructor's start hook runs after
its dependencies'), so the pool is open before the HTTP server binds and the pool is closed after
it stops. This is the behaviour we need for clean shutdown.

Gotchas that become tasks: `fx.Provide` is lazy, so a constructor nothing depends on never runs
(reads as "missing dependency" at first); I/O inside a constructor runs during `fx.New` before
`Start` and can hang startup; the hook `ctx` deadline does **not** preempt a hook that ignores
`ctx`, so it must be threaded down; `fxevent.SlogLogger` bridges Fx's events into the same
slog sink as the app.

---

## D-12. Domain purity enforcement

**Decision**: `test/boundaries/import_test.go` walks the parsed import graph of
`internal/domain` (and `internal/usecase`) with `go/parser` + `go list` and fails if it contains
`go.uber.org/fx`, `net/http`, `github.com/jackc/pgx`, `github.com/aws/aws-sdk-go-v2`,
`database/sql`, or any ORM.

**Rationale**: Constitution Principle IV is a rule about imports. A prose rule erodes; a test
that fails the build does not. A test that greps source text would be brittle; parsing the
actual import graph is exact and cheap.

---

## Verification summary

| Item | Status |
|------|--------|
| Spec Q1 (out-of-order) | RESOLVED — D-2 |
| Spec Q2 (currency) | RESOLVED — D-3 |
| Spec Q3 (ROLLBACK) | RESOLVED — D-4 |
| Go toolchain floor | RESOLVED — D-1, constitution amended to v1.0.1 |
| Append-only enforcement | RESOLVED — D-5 |
| Idempotency mechanism | RESOLVED — D-6 |
| Concurrency mechanism | RESOLVED — D-7 |
| Queue design | RESOLVED — D-8 |
| LocalStack parity | RESOLVED — D-9 (LocalStack 4.x FIFO fidelity UNVERIFIED) |
| Auth verification | RESOLVED — D-10 |
| DI composition | RESOLVED — D-11 |
| Domain purity | RESOLVED — D-12 |
| Money storage type | RESOLVED — `BIGINT` minor units + `CHAR(3)` currency, `CHECK (amount_minor > 0)` |
| DB role separation | RESOLVED — app role gets `SELECT, INSERT, UPDATE` only on the tables it needs; no `UPDATE`/`DELETE` on `ledger_entries` |
| Hot-wallet sharding | DEFERRED by decision — documented escape hatch in plan.md |
| FX conversion | OUT OF SCOPE by decision — separate feature |
