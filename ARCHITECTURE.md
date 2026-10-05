# Architecture

This document explains the decisions behind iron-ledger: what they are, why they
were made, and what they cost. Anything that is an interpretation rather than a
requirement is called out as such, and the limitations are stated at the end
rather than left to be discovered.

## 1. The shape of the problem

A game provider reports an operation — a bet, a win, a loss, a refund, a
rollback — and the platform moves a player's wallet. Delivery is at-least-once
and unordered: the same message arrives twice, a reversal arrives before the
operation it reverses, two bets for the same wallet arrive at the same instant,
and the process dies somewhere between "committed" and "told the broker".

Every guarantee below follows from taking those five facts seriously rather
than assuming them away.

## 2. Bounded contexts

The codebase is organised in two layers, because there are two kinds of thing in
it and they are not the same kind of thing. `internal/domain` holds the bounded
contexts — the business model, which is where the money lives. Everything beside
it is infrastructure: the machinery that carries the model's decisions to a
database, a queue and a network.

```
internal/
  domain/          the business model: bounded contexts and nothing else
    wallet/        domain, events, slices
    wagering/      domain, events, slices
  sharedkernel/    value objects used by more than one context
    money/ ids/ idempotency/ xerr/
  platform/        infrastructure seams
    config/ pgdb/ uow/ pgevents/ repository/ logging/ metrics/ httpx/ migrations/
  messaging/       the inbox, the transactional outbox, the SQS adapter
  identity/        token verification and the authorisation rules
  app/             use cases, HTTP surface, workers, composition
```

The split matters because a bounded context and an adapter are not
interchangeable units. A context states what is true of the business; an adapter
decides where that truth is stored and how it gets there. Flattening them into
one directory makes `wallet` and `pgdb` look like peers, and the temptation is
then to import one from the other freely. Nesting the contexts under
`internal/domain` makes the boundary visible in the import path instead of in a
document nobody rereads.

Each context owns its domain, its events, its commands and its read models:

| Context | Owns | Package |
| --- | --- | --- |
| **Wallet** | The balance, the append-only ledger, reconciliation | `internal/domain/wallet` |
| **Wagering** | The operation lifecycle, idempotency, references and reversals | `internal/domain/wagering` |
| **Messaging** | The inbox, the transactional outbox, the SQS adapter | `internal/messaging` |
| **Identity** | Token verification and the authorisation rules | `internal/identity` |
| **Application** | Use cases, HTTP surface, workers, composition | `internal/app` |

The dependency direction follows from that, and nothing in it is circular:

```
app ────────────► domain/wagering ──► domain/wallet
  │                      │                  │
  ├──────────► messaging  │                  │
  ├──────────► identity   ▼                  ▼
  └──────────► platform ──► sharedkernel ◄────┘
```

`sharedkernel` is the floor: `money`, `ids`, `idempotency` and `xerr` depend on
nothing of ours. `platform` sits above it and below everything else, because it
holds the transaction manager and the event store that every context needs and
none of them own. A context may use `platform` and `sharedkernel`, never the
reverse. `app` is the only package that sees the whole graph, which is what
makes it the right place for composition.

Two contexts own money-adjacent facts and neither reaches into the other:

- The **Wallet** context owns `balance`. Nothing outside it may change it. It
  knows nothing about bets, wins or providers — it is handed a `Movement` and
  decides whether the balance may move.
- The **Wagering** context owns the *lifecycle* of an operation. It knows which
  operation is acceptable, which reference resolves it and what the movement
  ought to be. It never writes a balance; it asks the Wallet context to.

The **Messaging** context is the only place that knows SQS exists, and the
**Identity** context is the only place that knows what a JWT is. Everything above
them works with value objects and function calls.

### Where the boundaries were drawn, and why

The event stream is the boundary. The `Wallet` aggregate owns the
`ironledger-default-wallet-v1-wallets-{id}` stream; the `Wagering` aggregate
owns `ironledger-default-wagering-v1-wager-transactions-{id}`. One operation
legitimately appends to both, inside one transaction — and that is deliberate:
a bet and its balance change are two facts about two aggregates that must not
diverge. The transaction, not the aggregate, is the consistency boundary; the
aggregates are the places where the rules live.

The alternative — one aggregate owning both — would have made the wallet stream
the sole writer of the balance and put provider-specific vocabulary inside the
Wallet context. The cost of the split is that a business operation touches two
streams and therefore needs the transaction manager. That cost is paid once, in
`uow`, and it buys two contexts that neither imports the other.

### The layering is enforced, not just documented

The dependency direction above is checked by `test/architecture`, which reads the
real import graph with `go list` and fails on a boundary crossing. Run it with
`make layers`.

Two of the rules are worth singling out, because they are the ones that carry the
weight:

- **A context never imports an adapter.** `domain/wagering` does not know SQS
  exists and `domain/wallet` does not know what a JWT is. The temptation is real
  — a slice that needs to publish something reaches for the outbox, and it feels
  harmless because the outbox is in the same repository. It is the first step
  from a domain model to a distributed system that cannot be reasoned about
  locally.
- **`sharedkernel` depends on nothing of ours.** `money`, `ids`, `idempotency` and
  `xerr` are the floor. If one of them ever needs something from a context, every
  context transitively depends on every context, and the shared kernel has
  quietly become a second platform package.

Most upward violations are already caught by the compiler as import cycles,
because `app` depends on everything below it. The rule the compiler cannot catch
is an adapter importing a context: `messaging → domain/wallet/domain` is not a
cycle, so nothing stops it except this test.

## 3. Money

### Representation

`Money` holds an `int64` count of the currency's **minor units** and an ISO
4217 currency code, with a fixed scale of two decimals.

```
25.00 BRL  ->  Money{minor: 2500, currency: "BRL"}
```

No amount ever passes through `float32` or `float64`. Parsing goes
`string → int64`, arithmetic goes `int64 → int64`, persistence is a `BIGINT`
column, and the wire format is a decimal string: `{"amount":"25.00","currency":"BRL"}`.
`Money` implements `json.Marshaler` and `json.Unmarshaler`, so even a value that
was serialised and read back cannot become a float — a payload carrying
`{"amount": 25.00}` (a JSON number) is refused at the boundary, which the tests
assert.

**Usable range.** With two decimals, an `int64` covers
`[-92233720368547758.07, 92233720368547758.07]` — about 92 billion units.
Parsing rejects more than 17 integer digits, and every operation that could leave
the range (`Add`, `Sub`, `Neg`) returns `ErrOverflow` rather than wrapping.

### Parsing is strict on purpose

Accepted: `-? digits{1,17} ( "." digits{1,2} )?`

Refused: `""`, `"NaN"`, `"Infinity"`, `"1e5"`, `"1E+3"`, `"+1.00"`, `"1.234"`,
`"1."`, `".50"`, `" 1.00"`, `"1,000.00"`, `"R$ 25.00"`.

The temptation is to accept `1.234` and round it. That is exactly the wrong
instinct for money: a caller that sends an amount the platform cannot represent
has a bug, and silently rounding it turns that bug into a financial discrepancy
somewhere downstream. An input that cannot be represented is refused with a 400.

**Normalisation, and where it happens.** Equivalent spellings are accepted and
normalised *at parse time*, before anything is hashed: `"25"`, `"25.0"` and
`"25.00"` all become `Money{2500}`. Because normalisation precedes the
idempotency fingerprint, those three spellings are one request rather than three.
`"-0.00"` normalises to zero.

### Arithmetic requires a matching currency

`Add`, `Sub`, `Cmp` and `Neg` all refuse a mismatch with `ErrCurrencyMismatch`.
`Equal` is deliberately stricter still: two amounts in different currencies are
never equal, even when their numbers match, so a `25.00 USD` balance can never be
mistaken for a `25.00 BRL` one.

The wallet holds one currency, set at opening and never changed. A movement in
another currency is refused by the aggregate, so a cross-currency mistake fails at
the rule rather than in a ledger full of mixed currencies.

### The alternative, and why not

A decimal library (`shopspring/decimal`, `cockroachdb/apd`) was the other
candidate. An `int64` with a fixed scale was chosen because the domain is
single-currency per wallet with two decimals, which is the one case an integer is
unambiguously correct in — and because the fixed scale makes overflow and
rounding bugs compile-time impossible rather than a runtime discipline.

## 4. Aggregates and encapsulation

Each aggregate is a value type with unexported fields, a validating constructor,
and explicit transition methods. The zero value is not a domain object: a
`Wallet{}` has no currency, so every method that needs one rejects it, and the
tests assert that.

Creation and rehydration are separate on purpose:

- `domain.Open(...)` validates and builds a new aggregate.
- `domain.Rehydrate(snapshot)` validates and rebuilds one from persisted state.

`Rehydrate` runs the same validation as `Open` and performs **no movement, no
transition and no event**. Rebuilding state is not an event. It exists so a
projection corrupted by a bad migration is caught rather than carried forward.

Errors are values, classified by `errors.As` on `*xerr.Error`, and carry a
`Kind` (which maps to HTTP status and to retry behaviour) and a stable `Code`
(which is part of the external contract). A business rejection is always a
returned error; `panic` is reserved for programmer error — an unknown
currency at package-initialisation time — and the HTTP layer recovers any panic
into a 500 rather than dropping the connection.

## 5. The event store and the write side

Every business operation is **one PostgreSQL transaction** containing: the
idempotency record, the wagering events, the wallet events, the balance row, the
ledger entry, the inbox row and the outbound events. This is the single decision
the rest of the design leans on.

The event store is a `cqrs.EventStore` implementation over `pgx` with one
crucial difference from the off-the-shelf Postgres store: it **joins the
transaction carried by the context** instead of opening its own. That is what
makes "an event is published only after the commit that caused it" true by
construction rather than by convention.

### Slices

Commands are vertical slices following the project's event-sourcing conventions:
`command.go` holds the command, the state, `initialState`, `evolve`, `decide`
and `NewCommandHandler`, and `command_test.go` holds the specifications.

| Slice | Context | Command → Events |
| --- | --- | --- |
| `openwallet` | Wallet | `OpenWallet` → `WalletOpened` (+ `WalletBalanceChanged`) |
| `applywalletmovement` | Wallet | `ApplyWalletMovement` → `WalletBalanceChanged` |
| `registerwageroperation` | Wagering | `RegisterWagerOperation` → `WagerTransactionRegistered` |
| `awaitwagerreference` | Wagering | `AwaitWagerReference` → `WagerTransactionPendingReference` |
| `settlewageroperation` | Wagering | `SettleWagerOperation` → `WagerTransactionProcessed`/`WagerTransactionRejected` |
| `failwageroperation` | Wagering | `FailWagerOperation` → `WagerTransactionFailed` |

### Projections

Read models are PostgreSQL tables maintained by projectors that run **inside the
transaction that produced the event**, driven by the recorded envelopes:

| Slice | Table |
| --- | --- |
| `walletdetails` | `wallets` |
| `walletledgerentries` | `wallet_ledger_entries` |
| `wagertransactiondetails` | `wager_transactions` |
| `providerwagertransactionlookup` | reads `wager_transactions`, scoped by provider |
| `walletreconciliation` | reads `wallets` + `wallet_ledger_entries` |

The projection order matters — the wallet row must exist before a wager
transaction that references it — and is asserted by the integration tests.

### A deliberate deviation

The project's vertical-slice conventions define read models over a generic
`Repository[T]` backed by Elasticsearch. This implementation keeps the generic
shape (`ReadRepository[T]`, `Repository[T]`, `Connection[T]{Cursor, Nodes}`,
`cqrs.QueryHandler`, `cqrs.EventGroupProcessor` projectors) but stores them in
**PostgreSQL**.

The reason is that these read models *are* the financial record. The
`wallets.balance_minor` column is the balance the platform promises; the ledger
is the audit trail; the wager transaction index is what makes idempotency survive
a restart. Splitting them into a search index would create a second source of
truth for money and a consistency problem the domain does not have. The generic
seam is kept so a context can still be replayed or replaced without touching the
application layer — the use cases depend on narrow interfaces
(`usecase.WalletReader`, `usecase.TransactionReader`), not on the repositories.

Elasticsearch would be the right answer for a searchable catalogue. It is the
wrong answer for a balance.

## 6. Concurrency

Coordination is **per wallet**, at three layers, each independent of the others:

1. **A transaction-scoped advisory lock keyed on the wallet id**, taken by the
   transaction manager before any read. Writers of *different* wallets take
   different locks and never wait for each other — there is no global lock
   anywhere in the system.
2. **Optimistic concurrency on the event stream.** `UNIQUE (stream_id,
   stream_position)` means two writers cannot occupy the same position; the
   loser gets a `StreamRevisionConflictError` and the whole transaction is
   retried, bounded by `WAGERING_MAX_CONFLICT_RETRIES`.
3. **A conditional update on the projection.** The balance row moves only when
   it still carries the version the event was derived from, so a lost update is a
   zero-row update rather than a silent overwrite.

Below all of that sit the database constraints, which hold even if every layer
above is bypassed: `CHECK (balance_minor >= 0)`, `UNIQUE (wallet_id,
transaction_id)` on the ledger, and `CHECK` that every ledger entry adds up.

The advisory lock is an optimisation, not the correctness argument. The
`READ COMMITTED` snapshot taken *after* the lock is acquired sees the previous
writer's commit, so the winning balance is the one the loser re-reads; the
revision check and the constraints are what would catch a bug in that reasoning.

**Why optimistic and not pessimistic throughout?** A pessimistic row lock on
`wallets` would also work and would serialise writers just as tightly. It was
rejected because it holds a row lock for the whole transaction — including the
outbox writes — so a slow broker call in the same transaction would block every
other writer to that wallet. The advisory lock is held just as long, but it is a
transaction-scoped advisory lock rather than a row lock, so it never conflicts
with anything the database is doing with the table itself.

**`LOSS` and the version.** A LOSS moves nothing, so it writes no ledger entry
and does not increment the wallet version. The version tracks balance changes,
not command count.

## 7. Idempotency

### What is deduplicated

An external operation is identified by `(providerId, externalTransactionId)` and
deduplicated by `(providerId, idempotencyKey)`. Both are unique indexes, so
neither can be dodged by an application bug.

### The fingerprint

`payload_hash` is the lowercase hex SHA-256 of the canonical JSON encoding of
exactly the business fields:

```json
{
  "amount": "25.00",
  "currency": "BRL",
  "externalTransactionId": "transaction-123",
  "gameId": "fortune-chimp",
  "kind": "BET",
  "playerId": "0192f28f-…",
  "providerId": "provider-a",
  "referenceExternalTransactionId": "",
  "roundId": "round-987",
  "walletId": "0192f291-…"
}
```

`encoding/json` sorts object keys at every depth, which is the canonical form:
no insignificant whitespace, lexicographic keys, monetary values as fixed-scale
decimal strings. Amounts are normalised to two decimals **before** hashing, so
`25`, `25.0` and `25.00` are one request.

Excluded on purpose: the `Idempotency-Key` header, the broker `messageId`, the
HTTP method and path, the correlation id, and anything else the transport
attaches. Those identify the delivery, not the business intent. Excluding them is
what makes an operation sent over HTTP and the same operation sent over SQS one
operation, which the integration tests assert in both directions.

### The four outcomes

| Situation | Answer |
| --- | --- |
| New key, new operation | Processed; the result is persisted |
| Known key, same fingerprint | The stored result, with `idempotentReplay: true` |
| Known key, different fingerprint | `409 IDEMPOTENCY_KEY_CONFLICT` |
| Known `(provider, external id)` under a *different* key | `409 EXTERNAL_TRANSACTION_CONFLICT` |

The replayed result carries the **balance observed at processing time**, stored on
the transaction — not the wallet's current balance. A provider polling a
transaction days later sees the answer it was given the first time, which is the
only version of that answer that meant anything.

### Durability

There is no in-memory deduplication anywhere. The index is a table; the keys are
unique indexes. `Test_aRestartPreservesEverything` proves it: a bet is submitted,
the process is destroyed, a new one is built, and the replay returns the original
transaction and the original balance with no second debit.

## 8. Operations and references

| Kind | Movement | Condition | Terminal failure code |
| --- | --- | --- | --- |
| `BET` | Debit | amount > 0, balance sufficient | `INSUFFICIENT_BALANCE` |
| `WIN` | Credit | amount > 0 | — |
| `LOSS` | None | amount exactly `0.00` | `LOSS_AMOUNT_MUST_BE_ZERO` |
| `REFUND` | Credit | references a processed `BET`, same amount | see below |
| `ROLLBACK` | Opposite of the reference | references a processed `BET`/`WIN`/`REFUND`, same amount | see below |
| `OPENING` | Credit | internal only | `KIND_NOT_ACCEPTED` from a provider |

`LOSS` writing nothing is not an oversight: it records a bet that already happened
and changed no balance, so a value on it would mean money changed hands without a
ledger entry to prove it.

### Reversals

A reversal moves **exactly the amount the referenced operation moved**, and the
request must state that amount — a mismatch is `REFERENCE_AMOUNT_MISMATCH`
rather than a silent correction. The movement is the reference's opposite:

| Reference | `ROLLBACK` direction |
| --- | --- |
| `BET` (debit) | credit |
| `WIN` (credit) | debit |
| `REFUND` (credit) | debit |

**A reference may be reversed successfully at most once.** A partial unique index
on `reference_transaction_id WHERE status = 'PROCESSED' AND kind IN ('REFUND',
'ROLLBACK')` enforces it in the database; the application also checks first, so
the caller gets `REFERENCE_ALREADY_REVERSED` rather than a constraint violation.

That single rule resolves the `REFUND`/`ROLLBACK` combinations coherently:

- `BET → REFUND → ROLLBACK` is allowed. The rollback references the **refund**,
  not the bet, so the money goes back where it came from and the ledger shows
  three entries that add up.
- `BET → REFUND` and then `BET → ROLLBACK` is **refused**. The rollback
  references the same bet, and returning a debit that was already reversed would
  credit money that was never taken.
- `BET → ROLLBACK → REFUND` is **refused**, for the same reason.

**A reversal that would overdraw is refused** with
`REVERSAL_INSUFFICIENT_FUNDS` — deliberately a different code from
`INSUFFICIENT_BALANCE`, because a provider must be able to tell "this bet was too
big" from "we could not take back a payout because the player already spent it".

### A reference that has not arrived

Providers deliver independently, so a reversal routinely arrives first. Refusing
it would be wrong; guessing would move money that does not exist. So it is
persisted as `PENDING_REFERENCE` with a durable retry schedule
(`pending_attempts`, `next_attempt_at`) and answered **202**.

| Reference state | Behaviour |
| --- | --- |
| Not received | Wait. After `WAGERING_MAX_REFERENCE_ATTEMPTS` → `REJECTED REFERENCE_NOT_FOUND` |
| Received, still pending | Wait. After the budget → `REJECTED REFERENCE_NOT_PROCESSED` |
| Received, terminal but not processed | `REJECTED REFERENCE_NOT_PROCESSED` immediately |
| Received and processed | Settle |
| Different wallet | `REJECTED REFERENCE_KIND_MISMATCH` |
| Wrong kind for this reversal | `REJECTED REFERENCE_KIND_MISMATCH` |
| Already reversed | `REJECTED REFERENCE_ALREADY_REVERSED` |

A reference that exists but is itself pending is **waited for**, not rejected:
the two operations may simply have been delivered in the opposite order, and
rejecting would lose a legitimate refund. A reference that reached a terminal
state without success is rejected at once, because waiting cannot change it.

The retry schedule is exponential (`2s → 4s → … → 2m`) and lives in the database,
so any instance can continue it — and a restart loses nothing.

### The state machine

```
                 ┌──────────────────────────────┐
                 │                              │
   register ──► PENDING ──► PROCESSED (terminal)│
                 │   │                           │
                 │   └──► PENDING_REFERENCE ──► PROCESSED (terminal)
                 │              │                │
                 │              └──► REJECTED ───┴──► FAILED (terminal)
                 └──► REJECTED / FAILED
```

`PENDING_REFERENCE → PROCESSED` is the resolution path. A terminal state accepts
no further transition; a repeated request reads the persisted outcome instead.
`TestTerminalStatesAreTerminal` asserts it for every pair.

**Transient vs permanent.** The distinction is not the error type but its
`Kind`:

| Kind | Retry? | Where |
| --- | --- | --- |
| `KindValidation` | no — correct the request | HTTP 400, SQS message retired |
| `KindBusinessRule` | no — it is a decision | HTTP 422, SQS message retired |
| `KindNotFound` / `KindConflict` / `KindForbidden` | no | HTTP 404/409/403 |
| `KindUnavailable` | yes | HTTP 503, SQS retry |
| `KindInternal` (infrastructure) | yes | HTTP 500, SQS retry |

## 9. Transactional outbox

Events are rendered from the committed events of a transaction and written to
`outbox_messages` **in that transaction**. Nothing is published synchronously, so
an event cannot be observed before the effects it describes are durable.

The payload is an **immutable snapshot**: rendered once at commit time and stored
verbatim. A publisher that retries sends the stored bytes, not a re-render, so a
schema change in a later deploy cannot alter what a pending event says.

### The envelope

```json
{
  "eventId": "0192f2a0-…",
  "eventType": "WalletBalanceChanged",
  "aggregateType": "wallet",
  "aggregateId": "0192f291-…",
  "correlationId": "ironledger-host-7",
  "causationId": "0192f298-…",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "version": 1,
  "data": { "walletId": "…", "transactionId": "…", "direction": "DEBIT",
            "money": {"amount":"25.00","currency":"BRL"},
            "balanceBefore": {"amount":"100.00","currency":"BRL"},
            "balanceAfter": {"amount":"75.00","currency":"BRL"},
            "walletVersion": 2 }
}
```

The type and the version are defined by the event's own builder, not by the
transport. Timestamps are UTC RFC 3339 with millisecond precision; monetary values
are decimal strings.

### Publishing

A worker claims pending rows with
`FOR UPDATE SKIP LOCKED`, so several publishers run against one table without
coordinating — each takes a disjoint batch. It publishes, then marks the rows.

The recovery path is the interesting one:

- **Publisher dies after the broker accepted a send, before it recorded the
  fact.** The row stays claimed. After `OUTBOX_VISIBILITY_WINDOW` another
  publisher takes it and sends it again **with the same `eventId`**, and
  `MessageDeduplicationId` makes the FIFO queue drop it as the duplicate it is.
  Consumers see each event once.
- **Publisher fails and retries.** The row goes back with exponential backoff; the
  attempt count is recorded. After `OUTBOX_MAX_ATTEMPTS` the event is marked
  published and counted as dropped, because retrying forever would grow the
  backlog silently while blocking nothing.
- **A payload that cannot be decoded** is a permanent failure, handled the same
  way rather than retried forever.

`MessageGroupId` is the aggregate id, so all events of one aggregate stay in order
relative to each other. Groups are independent, so one slow wallet cannot hold up
another.

### Published events

| Event | Trigger | Published |
| --- | --- | --- |
| `WagerTransactionProcessed` | Successful settlement, including `LOSS` | yes |
| `WagerTransactionRejected` | Terminal business rejection | yes |
| `WagerTransactionPendingReference` | Registered a wait | yes |
| `WalletBalanceChanged` | An effective balance change | yes |
| `WagerTransactionRegistered` | An operation was accepted | no — internal bookkeeping |
| `WagerTransactionFailed` | A permanent infrastructure failure | no — our own attempts |
| `WalletOpened` | A wallet came into existence | no — a fact about our storage |

Adding a context to the publication contract means adding one `integration.Builder`
to the composition root. Nothing else has to know the event exists.

### Routing

Outbound events go to `wager-events.fifo` (`MessageGroupId = aggregateId`,
`MessageDeduplicationId = eventId`). Inbound operations arrive on
`wager-transactions.fifo` with the envelope documented in §10. Each has a
dead-letter queue. Consumers should route on `eventType` and use `eventId` as the
idempotency key of their own effect.

## 10. Inbox and the SQS consumer

### The contract

Inbound message:

```json
{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "…", "walletId": "…",
    "roundId": "round-987", "gameId": "fortune-chimp",
    "kind": "BET",
    "money": {"amount": "25.00", "currency": "BRL"}
  }
}
```

- `messageId` is the **durable identity** of the message. The inbox primary key is
  `(consumer_name, message_id)`, so two different consumers may handle the same
  message without either suppressing the other.
- `data.idempotencyKey` is the business deduplication key. The inbox is an
  *additional* guard, not a replacement: the financial guarantee is the unique
  index on the transaction index, which survives even a message that never
  reached the inbox.
- The consumer computes `sha256(body)` and stores it. A redelivery whose payload
  changed is visible in the inbox rather than silently applied.
- `MessageGroupId` is the wallet id, so operations of one wallet stay in order;
  the idempotency key is the natural `MessageDeduplicationId`.

### Order of operations

```
receive → run the business transaction (inbox claim + effect, one commit) → delete
```

- **Crash before the transaction commits.** Nothing happened; the broker
  redelivers.
- **Crash after the commit, before the delete.** The message is redelivered, the
  inbox claim no longer inserts, and the stored outcome is replayed. This is
  `Test_theInboxAbsorbsARedeliveredMessage`.
- **Confirmed business rejection.** Terminal — the message is deleted, not
  retried. It is already recorded as `REJECTED`.
- **Transient failure.** The message is left for redelivery. After
  `maxReceiveCount` (`SQS_MAX_ATTEMPTS`, 5) the broker's redrive policy sends it
  to `wager-transactions-dlq.fifo`.
- **Unparseable or unexpected message type.** Left on the queue so the redrive
  policy sends it to the dead-letter queue, where it can be inspected. Silently
  dropping it would destroy the evidence that something upstream is wrong.

A pending reference may complete its inbox row once the pendency itself is
durable; the reference worker then owns the rest of the story.

### Shutdown

On `SIGTERM` the consumer stops receiving, finishes the messages already in
flight within `HTTP_SHUTDOWN_BUDGET`, and **releases** anything it could not
finish (`ChangeMessageVisibility` to 0) so another instance takes over at once
rather than after the visibility timeout.

## 11. The ledger and reconciliation

Every effective balance change appends exactly one entry, in the same transaction
as the balance itself. Entries are immutable: `UPDATE`, `DELETE` and `TRUNCATE`
are all refused by triggers, and the tests assert each. A correction is a new pair
of entries, never an edit.

The database also refuses an entry that does not add up
(`balance_after = balance_before ± amount`), an entry with no value, and a second
entry for a `(wallet_id, transaction_id)` pair.

`POST /wallets/:id/reconciliation` rebuilds the balance from the ledger and
compares it with the stored balance, both read inside one read transaction so
they describe the same instant. It reports `difference` (stored minus
reconstructed), `consistent`, and `checkedEntries`. It **never writes**: a
reconciliation that could change the balance would destroy the evidence it exists
to produce. A divergence is logged at `error` and counted in
`ironledger_reconciliation_divergences_total`.

## 12. Authentication and authorisation

### The identity provider

Keycloak, in Docker Compose, over OAuth 2.0 `client_credentials`. The realm is
provisioned automatically from `deploy/keycloak/realm-ironledger.json` at
start-up (`--import-realm`), including three service accounts:

| Client | Secret | Identity |
| --- | --- | --- |
| `iron-ledger-internal` | `iron-ledger-internal-secret` | realm role `internal` |
| `provider-a` | `provider-a-secret` | claim `providerId: provider-a` |
| `provider-b` | `provider-b-secret` | claim `providerId: provider-b` |

The service stores no passwords and mints no tokens. Verification uses
`coreos/go-oidc`, which checks the signature against the issuer's JWKS, the
issuer, the audience and the expiry, refreshing keys on rotation so a Keycloak
restart does not require a restart of the application. `aud` is accepted as either
a string or an array, because providers differ and that should not be a
deployment accident.

### Why the internal role is a realm role, and the provider is a claim

An internal caller may do things a provider may not (open a wallet, read a
balance, reconcile), and a provider must be scoped to a provider identity. Those
are different shapes of permission, so they are expressed differently:

- **Internal** is a realm role (`internal`) on the service account, granted by
  `defaultRoles` on the client. Roles are the identity provider's native
  vocabulary and survive a token-shape change.
- **Provider** is a claim (`providerId`) set by a hardcoded-claim protocol mapper
  on each provider client. It is a statement *about* the client, which is what a
  provider identity is.
- **`AUTH_CLIENT_PROVIDERS`** is the fallback for identity providers that do not
  project a claim: it maps an OAuth `client_id` to the provider it may act as, and
  is re-read on every request, so removing a provider from configuration stops it
  immediately. The claim wins when both are present, because the claim is the IdP's
  own statement.

### The authorisation rules

| Caller | May |
| --- | --- |
| `internal` | Everything: open wallets, read balances, ledger, reconciliation, any transaction |
| `provider:<id>` | Submit operations **as that provider**, read **its own** transactions |
| `provider:<id>` | **Not** open or read wallets |
| `internal` | **Not** submit operations on a provider's behalf |

Three properties are worth stating explicitly, because each closes a specific hole:

1. **The provider identity comes from the credential, never the body.** A
   `providerId` in the request body is cross-checked against the token; a mismatch
   is `403`, not a silent substitution.
2. **Cross-provider reads are `404`, not `403`.** A provider asking for another
   provider's transaction by internal id gets "not found" — confirming it exists
   would leak data across the isolation boundary. A provider naming another
   provider *in the path* gets `403`, because the answer must not depend on
   whether that provider's transactions exist.
3. **The internal service is never also a provider.** The roles do not overlap: an
   `internal` credential submitting an operation is `403`.

Unauthenticated requests are refused in middleware, before any handler runs, with
a `WWW-Authenticate` challenge. Health probes sit outside the authentication chain
on purpose: a probe must answer even when the identity provider is down.

### Broker access

The broker is reached with static credentials scoped to the queue prefix the
platform owns, and the broker's own redrive policy is the last line of defence.
Domain validation is **not** delegated to the broker: a message that arrives
without the right shape is refused by the consumer, because a broker cannot know
what a valid bet is.

`AUTH_ENABLED=false` exists for local development. It grants only the internal
role, never a provider identity, so a misconfigured deployment cannot turn into an
open provider API.

## 13. HTTP contract

Every non-2xx response has the same shape:

```json
{
  "error": "Conflict",
  "code": "IDEMPOTENCY_KEY_CONFLICT",
  "title": "Idempotency key reused with a different payload",
  "message": "…",
  "params": { "key": "provider-a:transaction-123" },
  "correlationId": "ironledger-host-7",
  "retryable": false
}
```

`code` is stable and documented; `message` is for humans; `params` are the
substitutions. `correlationId` is echoed in the `X-Correlation-Id` response header
and appears in every log line of the operation.

| Status | Meaning | Body |
| --- | --- | --- |
| `200` | Settled | `OperationResult` |
| `201` | Wallet opened | `WalletView` |
| `202` | Durably accepted, waiting for a reference | `OperationResult` with `status: PENDING_REFERENCE` |
| `400` | Malformed or invalid | `Problem` |
| `401` | Missing, invalid or expired credential | `Problem` |
| `403` | Valid caller, not permitted | `Problem` |
| `404` | No such wallet or transaction | `Problem` |
| `409` | Idempotency or uniqueness conflict | `Problem` |
| `422` | Terminal business rejection | `OperationResult` with `status: REJECTED` and `failureCode` |
| `503` | Dependency down; safe to retry with the same key | `Problem` |

A business rejection is a successful HTTP exchange carrying a decision, so it uses
the result shape rather than the problem shape — a provider parses one body either
way.

`GET /wallets/:walletId/ledger` paginates with an opaque cursor over a
`(created_at, id)` order. Neither key alone is a total order — two entries can
share a timestamp, and no UUID is guaranteed to sort by creation across processes
— so the cursor carries both and the order is total and resumable.

## 14. Composition and lifecycle

Uber Fx owns the graph, in modules that mirror the layers:
`platform`, `storage`, `slices`, `messaging`, `application`, `workers`.

The domain imports nothing from Fx, HTTP, SQS or the database. Slices depend on
`cqrs.EventStore`, a repository interface and value objects; they would work with
a different composition root unchanged.

Every long-running component is an `fx.Lifecycle` hook, and shutdown is ordered by
construction because Fx runs stop hooks in reverse registration order:

```
SIGTERM
  ├─ workers stop fetching, finish in-flight work, release the rest
  ├─ the HTTP server stops accepting, drains within HTTP_SHUTDOWN_BUDGET
  └─ the pool closes last — after everything that used it has stopped
```

The pool is a `fx.Hook` `OnStop`, not a defer, because "after everything that used
it" is the whole point. A worker that returns an error is logged rather than
swallowed: a worker that died is an operational fact.

An invalid configuration, an unreachable database or an unreachable identity
provider fails **start-up**. Nothing accepts traffic it cannot honour.

Two binaries, one codebase:

- `cmd/api` — the HTTP surface, health probes and metrics. No consumers.
- `cmd/worker` — the SQS consumer, the outbox publisher and the reference
  resolver, plus the same health probes and metrics.

Both can run in any number of copies. They coordinate through the database, not
through each other.

## 15. Observability

**Logs** are JSON on stdout, one line per event, carrying `correlationId`,
`messageId`, `transactionId`, `walletId` and `providerId`. The cause of a failure
is logged; the payload is not. No credential and no full financial payload ever
reaches a log line.

**Metrics** on `/metrics`:

| Metric | Type | Meaning |
| --- | --- | --- |
| `ironledger_operation_results_total{kind,status}` | counter | Outcomes by kind and status |
| `ironledger_operation_latency_seconds{kind,source}` | histogram | From arrival to durable commit |
| `ironledger_idempotent_replays_total{kind,source}` | counter | Requests recognised as replays |
| `ironledger_idempotency_conflicts_total{kind,source}` | counter | Key reuse with a different payload |
| `ironledger_concurrency_conflicts_total{outcome}` | counter | Lost-update races, and the retries they cost |
| `ironledger_inbox_duplicates_total{consumer}` | counter | Messages already handled |
| `ironledger_inbox_completed_total{consumer,outcome}` | counter | Durably committed |
| `ironledger_inbox_failures_total{consumer,kind}` | counter | Failed and left for redelivery |
| `ironledger_dlq_messages_total{queue}` | counter | Exhausted and dead-lettered |
| `ironledger_outbox_published_total{eventType}` | counter | Events handed to the broker |
| `ironledger_outbox_retries_total{eventType}` | counter | Failed and rescheduled |
| `ironledger_outbox_lag_seconds` | histogram | Commit to publication |
| `ironledger_reconciliation_divergences_total{walletId}` | counter | Divergences found |
| `ironledger_dependency_up{dependency}` | gauge | PostgreSQL, SQS, identity |
| `ironledger_worker_running{worker}` | gauge | Each worker's liveness |

**Health** — `GET /health/live` answers from memory alone: a database outage must
not make the process look dead, because restarting it would not fix the outage.
`GET /health/ready` probes PostgreSQL, SQS and the identity provider, and returns
503 when any is unavailable so the instance leaves the load balancer.

## 16. Interpretations

Places where the specification left room, and what was chosen:

- **A missing wallet is a `404`, not a recorded rejection.** The wager
  transaction index is keyed by wallet and has a foreign key to it, so an
  operation naming a wallet that does not exist cannot be recorded. It is refused
  terminally instead; the same request always gets the same answer, so nothing is
  lost.
- **`WAGERING_MAX_REFERENCE_ATTEMPTS` counts waits, not time.** Eight attempts
  with the default backoff is roughly eight minutes; the two knobs
  (`MAX_REFERENCE_ATTEMPTS`, `REFERENCE_BACKOFF_MAX`) are enough to express any
  sensible TTL.
- **A pending reference is reported as `202`, not `200`.** It is durably accepted
  but not finished, and a provider must not treat it as settled.
- **Reversals must state the referenced amount.** The requirement says a reversal
  undoes the reference in full; accepting a different number would let a provider
  partially reverse an operation. A mismatch is refused rather than corrected.
- **The `internal` role is a realm role, the provider identity a claim.** Both are
  configured in Keycloak; the rationale is in §12.
- **The tenant segment of a stream id is `default`.** Stream names are
  `{namespace}-{tenant}-{bounded-context}-v1-{aggregate}-{id}`, following the
  project's convention. There is one tenant today; the segment exists so adding
  one does not renumber every stream.
- **`LOSS` does not increment the wallet version.** The version tracks balance
  changes, and a LOSS changes no balance.

## 17. Limitations

Honest accounting of what is not here:

- **No tracing.** OpenTelemetry is not wired. Correlation ids carry an operation
  across processes, but there are no spans. The `terraskye/eventsourcing`
  library ships `otel` wrappers that would be a short addition.
- **No load testing is included** in the repository. The commands to run one are
  in the README; the numbers are not claimed here.
- **No double-entry accounting.** The ledger is a single-sided movement log with
  balance snapshots. That is sufficient for the invariants this platform must
  hold, and it is not a general-purpose accounting system.
- **The outbox has no dead-letter queue of its own.** An event that exhausts its
  publication budget is marked published and counted as dropped rather than moved
  somewhere an operator will see it. The failure is logged at `error`; a real
  deployment should route it to a topic an operator owns.
- **`WagerTransactionRegistered` and `WagerTransactionFailed` are not published.**
  They are internal bookkeeping. A consumer that needs to know an operation was
  *accepted* has only the 202 and the read endpoint.
- **Currency support is bounded.** Any ISO 4217 code in the curated list is
  accepted, but the platform only ever settles BRL in its own tests, and there is
  no FX.
- **`AUTH_ENABLED=false` is a development switch.** It grants only the internal
  role. It should not exist in a production image's configuration.
- **Two event-store writers per stream are serialised by an advisory lock.** Under
  extreme contention on a single wallet that is a queue, not a throughput win. It
  is the right trade for a wallet, which is a serial resource anyway.
