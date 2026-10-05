---
name: state view slice
description: Knows how to build state view slices (Go, terraskye/eventsourcing, Elasticsearch)
---
# STATE_VIEW Slice Skill

## Overview

A STATE_VIEW slice represents a read-only projection/view of data. It does NOT contain commands - only read models that are populated by events from other slices. This is the "Query" side of CQRS.


## Input Structure (from config.json)

```json
{
  "sliceType": "STATE_VIEW",
  "title": "slice: <Name>",
  "context": "<BoundedContext>",
  "commands": [],  // Always empty for STATE_VIEW
  "events": [],    // Always empty for STATE_VIEW
  "readmodels": [...],
  "screens": [...],
  "specifications": [...]
}
```

## Key Characteristics

1. **No commands** - STATE_VIEW slices only display data
2. **No events owned** - Events come from other STATE_CHANGE slices
3. **ReadModels reference external events** - Dependencies with INBOUND type point to events
4. **Screens display data** - May link to commands in other slices
5. **Elasticsearch storage** - Read models use ES with auto-generated mappings from struct tags
6. **Multi-tenancy** - Handled transparently by the repository layer (tenant extracted from context)

## Flow Pattern

```
Events (from STATE_CHANGE slices) → EventBus → Projector (OnEvent) → Repository[T] (Elasticsearch)  → QueryHandler → Screen
Events (from STATE_CHANGE slices) -> Projector (EventGroupProcessor) -> Repository[T] (Elasticsearch) -> QueryHandler -> Screen
```


```go
import (
	cqrs "github.com/terraskye/eventsourcing"
    "github.com/terraskye/ralph-loop/repository"
)
```

Key types used in STATE_VIEW slices:

| Type | Purpose |
|------|---------|
| `cqrs.Event` | Interface — `AggregateID() string`, `EventType() string` |
| `cqrs.Query` | Interface — `ID() []byte` (implement on query structs) |
| `cqrs.OnEvent[T]()` | Creates a typed event handler for projector wiring |
| `cqrs.NewEventGroupProcessor()` | Groups multiple `OnEvent` handlers; subscribe to EventBus |
| `cqrs.RegisterQueryHandler[T, R]()` | Registers a query handler on the QueryBus |
| `cqrs.NewQueryGateway[T, R]()` | Typed façade for dispatching queries from screens/controllers |
| `Repository[T]` | Global generic persistence abstraction backed by Elasticsearch |


### Global Repository[T]

All STATE_VIEW slices  have their own Repository interface that implements the  shared generic repository. 

```go
// Repository is the write+read interface — used by projectors.
type Repository[T any] interface {
    ReadRepository[T]
    Update(ctx context.Context, id string, model *T) error
    BulkUpdate(ctx context.Context, items []BulkItem[T]) error
    Delete(ctx context.Context, id string) error
    Purge(ctx context.Context) error
}

// ReadRepository is the read-only subset — used by query handlers.
type ReadRepository[T any] interface {
    Find(ctx context.Context, id string) (*T, error)
    FindAll(ctx context.Context, criteria *afreq.Criteria) (Connection[T], error)
    Iterator(ctx context.Context) (*cqrs.Iterator[T], error)
}

type Connection[T any] struct {
    Cursor string
    Nodes  []*T
}

type BulkItem[T any] struct {
    ID    string
    Model *T
}
```

- The **projector** receives `Repository[<Entity>]` (needs `Update`, `BulkUpdate`, and `Delete`).
- The **query handler** receives `ReadRepository[<Entity>]` (read-only; pass the same
  repo instance — Go satisfies the narrower interface automatically).

### BulkUpdate rule

**Always prefer `BulkUpdate` over repeated `Update` calls** when a projector handler touches
multiple entities. This includes:

- Looping over an event's slice field and writing each element (e.g. reorder, bulk-add).
- Iterating the repository with `Iterator` and updating every matching entity (e.g. language archive/restore, propagating a flag change).

Pattern for event-field loops:
```go
func (p *Projector) onThingsReordered(ctx context.Context, ev *events.ThingsReordered) error {
    items := make([]repository.BulkItem[ThingEntity], 0, len(ev.Things))
    for i, id := range ev.Things {
        key := ThingKey{...}
        entity, err := p.repo.Find(ctx, key.String())
        if err != nil {
            if errors.Is(err, repository.ErrNotFound) {
                entity = &ThingEntity{...}
            } else {
                return err
            }
        }
        entity.Index = int64(i)
        items = append(items, repository.BulkItem[ThingEntity]{ID: key.String(), Model: entity})
    }
    return p.repo.BulkUpdate(ctx, items)
}
```

Pattern for iterator + flag update (e.g. hidden, isDefault):
```go
func (p *Projector) updateHidden(ctx context.Context, locale string, hidden bool) error {
    iter, err := p.repo.Iterator(ctx)
    if err != nil {
        return err
    }
    var items []repository.BulkItem[ThingEntity]
    for iter.Next(ctx) {
        entity := iter.Value()
        if entity.Locale == locale {
            entity.Hidden = hidden
            key := ThingKey{...}
            e := entity
            items = append(items, repository.BulkItem[ThingEntity]{ID: key.String(), Model: &e})
        }
    }
    if err := iter.Err(); err != nil {
        return err
    }
    return p.repo.BulkUpdate(ctx, items)
}
```

Note: `BulkUpdate(ctx, nil)` and `BulkUpdate(ctx, []BulkItem{})` are both safe no-ops.

---

## Elasticsearch Struct Tags

Entity fields are annotated with `es:` tags that control which Elasticsearch analyzer
is applied. Choose the tag based on how the field will be searched — not just its Go type.

| Tag | Analyzer | When to use |
|-----|----------|-------------|
| `es:"fuzzy_3"` | 3-char ngram | Short text fuzzy search — names, codes, short identifiers |
| `es:"fuzzy_5"` | 5-char ngram | Longer text fuzzy search — descriptions, notes |
| `es:"prefix"` | Edge ngram | Autocomplete / prefix search |
| `es:"suffix"` | Reverse | Suffix matching |
| `es:"sayt"` | search_as_you_type | Search-as-you-type fields |

Multiple analyzers can be combined: `es:"fuzzy_3,prefix"`

Fields that are **not searched** (UUIDs, booleans, dates, amounts) carry no `es:` tag —
they are stored as-is and used as for filtering only.

### Tag Selection by Field Role

| Field role | Recommended tag |
|------------|----------------|
| Human name (person, company) | `es:"fuzzy_3,prefix"` |
| Reference code / case number | `es:"fuzzy_3"` |
| Free-text description / notes | `es:"fuzzy_5"` |
| Autocomplete input | `es:"sayt"` or `es:"prefix"` |
| UUID / ID (filter only) | _(no tag)_ |
| Boolean / date / amount | _(no tag)_ |


## ID Encoding Strategy

`Repository[T]` identifies records by a plain `string`.

| Scenario | Strategy |
|----------|----------|
| Single UUID key | `id.String()` — human-readable, unambiguous |
| Composite key (2+ UUIDs) | Key struct + SHA256 of concatenated raw bytes |

### Composite Key Pattern

```go
import (
    "crypto/sha256"
    "encoding/hex"
    "github.com/google/uuid"
)

type <ReadModelName>Key struct {
    <FirstField>  uuid.UUID
    <SecondField> uuid.UUID
}

// String hashes the concatenated raw UUID bytes (16 bytes each, fixed-width).
// No separator needed — fixed-width input is inherently unambiguous.
func (k <ReadModelName>Key) String() string {
    h := sha256.New()
    h.Write(k.<FirstField>[:])
    h.Write(k.<SecondField>[:])
    return hex.EncodeToString(h.Sum(nil))
}
```

The field write order in `String()` is the stable contract — never reorder the `h.Write` calls.

---


## Code Generation Steps

### Step 1: Identify Source Events

Look at the readmodel's dependencies with `type: "INBOUND"` and `elementType: "EVENT"`:

```json
{"dependencies": [
  { "type": "INBOUND", "title": "Law Firm Created", "elementType": "EVENT" },
  { "type": "INBOUND", "title": "Law Firm Updated", "elementType": "EVENT" }
]}
```

These events must already exist in the `<module>/events/` package (from STATE_CHANGE slices).
Each becomes an `OnEvent[T]` handler in the projector.

Event title to Go type name: Convert to PascalCase, e.g., "Law Firm created" -> `LawFirmCreated`.


### Step 2: Determine Persistence Strategy

| Scenario | Pattern |
|----------|---------|
| Single `idAttribute: true` field | Single Key — `id.String()` |
| Multiple `idAttribute: true` fields | Composite Key — key struct + SHA256 |
| `listElement: true` on readmodel | Composite Key |
| Parent/child relationship | Composite Key |

---

## Persistence Pattern A: Single Key

Use when the readmodel has exactly ONE `idAttribute: true` field.

### A.1: Entity, ReadModel & Query

**Location:** `<module>/slices/<readmodelname>/readmodel.go`

```go
package <readmodelname>

import "github.com/google/uuid"


const RepositoryIndexName = "<read-model-name>"

type  <ReadModelName>Repository interface {
    repository.Repository[<ReadModelName>Entity]
}

// <ReadModelName>Entity is the Elasticsearch projection document.
// Add es:"..." tags only on fields that will be searched.
type <ReadModelName>Entity struct {
    <IDFieldName> <Type>    `json:"<id_field>"`
    <TextField>   string    `json:"<text_field>" es:"<analyzer_tag>"`
    <FilterField> <Type>    `json:"<filter_field>"`
}

// <ReadModelName>ReadModel is the query result returned to callers.
type <ReadModelName>ReadModel struct {
    Data *<ReadModelName>Entity
}

// <ReadModelName>Query implements es.Query.
type <ReadModelName>Query struct {
    <IDFieldName> uuid.UUID
}

func (q <ReadModelName>Query) ID() []byte { return q.<IDFieldName>[:] }
```


### A.2: Projector

**Location:** `<module>/slices/<readmodelname>/projector.go`

```go
package <readmodelname>

import (
"context"

cqrs "github.com/terraskye/eventsourcing"
"<module>/events"
"<shared>/repository"
)

type Projector struct {
	repo <ReadModelName>Repository
}

func NewProjector(repo <ReadModelName>Repository) *cqrs.EventGroupProcessor {
	p := &Projector{
		repo: repo
	}
	return cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(p.on<CreatedEvent>),
		cqrs.OnEvent(p.on<UpdatedEvent>),
		cqrs.OnEvent(p.on<DeletedEvent>),
	)
}

// On<CreatedEvent> — full upsert; Update handles both insert and overwrite.
func (p *Projector) on<CreatedEvent>(ctx context.Context, ev *events.<CreatedEvent>) error {
	entity := &<ReadModelName>Entity{
		<IDFieldName>: ev.<IDFieldName>,
		<FieldName>:   ev.<EventFieldName>,
	}
	return p.repo.Update(ctx, ev.<IDFieldName>.String(), entity)
}

// On<UpdatedEvent> — fetch then patch, preserving unrelated fields.
func (p *Projector) on<UpdatedEvent>(ctx context.Context, ev *events.<UpdatedEvent>) error {
	entity, err := p.repo.Find(ctx,ev.<IDFieldName>.String())
	if err != nil {
		return err
	}
	entity.<FieldName> = ev.<EventFieldName>
	return p.repo.Update(ctx, ev.<IDFieldName>.String(), entity)
}

func (p *Projector) on<DeletedEvent>(ctx context.Context, ev *events.<DeletedEvent>) error {
	err := p.repo.Delete(ctx, ev.<IDFieldName>.String())
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			return err
		}
	}
	return nil
}
```


### A.3: Query Handler

**Location:** `<module>/slices/<readmodelname>/query.go`

Use a named struct so the handler stays readable as logic grows.
The constructor returns `cqrs.QueryHandler[Query, *Result]` and is registered on the bus in wiring.

```go
package <readmodelname>

import (
    "context"

    cqrs "github.com/terraskye/eventsourcing" 
    "<shared>/repository"
)

type QueryHandler struct {
    repo <ReadModelName>Repository
}

func NewQueryHandler(repo <ReadModelName>Repository) cqrs.QueryHandler[<ReadModelName>Query, *<ReadModelName>ReadModel] {
    return &QueryHandler{repo: repo}
}

func (h *QueryHandler) HandleQuery(ctx context.Context, qry <ReadModelName>Query) (*<ReadModelName>ReadModel, error) {
    entity, err := h.repo.Find(ctx, qry.<IDFieldName>.String())
    if err != nil {
        return nil, err
    }
    return &<ReadModelName>ReadModel{Data: entity}, nil
}
```


## Persistence Pattern B: Composite Key

Use when the readmodel has MULTIPLE `idAttribute: true` fields, or represents a
many-to-many / parent-child relationship.

### B.1: Entity, Key Struct, ReadModel & Query

**Location:** `<module>/slices/<readmodelname>/readmodel.go`

```go
package <readmodelname>

import (
    "crypto/sha256"
    "encoding/hex"
    "github.com/google/uuid"
    afreq "github.com/afosto/utils-go/query"
)

type <ReadModelName>Key struct {
    <FirstField>  uuid.UUID
    <SecondField> uuid.UUID
}

func (k <ReadModelName>Key) String() string {
    h := sha256.New()
    h.Write(k.<FirstField>[:])
    h.Write(k.<SecondField>[:])
    return hex.EncodeToString(h.Sum(nil))
}

type <ReadModelName>Entity struct {
    <FirstField>  uuid.UUID `json:"<first_field>"`
    <SecondField> uuid.UUID `json:"<second_field>"`
    <TextField>   string    `json:"<text_field>" es:"<analyzer_tag>"`
}

// Composite readmodels return a list.
type <ReadModelName>ReadModel struct {
    Data []*<ReadModelName>Entity
	Cursor string
}

// Query by the primary owner (first key field).
type <ReadModelName>Query struct {
    <FirstField> uuid.UUID
	Criteria *afreq.Criteria
}

func (q <ReadModelName>Query) ID() []byte {
	return append(q.<FirstField>[:], []byte(q.Criteria.String())...)
}

```


### B.2: Projector

**Location:** `<module>/slices/<readmodelname>/projector.go`

```go
package <readmodelname>

import (
    "context"

    cqrs "github.com/terraskye/eventsourcing"
    "<module>/events"
    "<shared>/repository"
)

type Projector struct {
    repo <ReadModelName>Repository
}

func NewProjector(repo <ReadModelName>Repository) *cqrs.EventGroupProcessor {
    p := &Projector{repo: repo}
    return cqrs.NewEventGroupProcessor(
        cqrs.OnEvent(p.on<AddedEvent>),
        cqrs.OnEvent(p.on<UpdatedEvent>),
        cqrs.OnEvent(p.on<RemovedEvent>),
    )
}

// On<UpdatedEvent> — fetch then patch, preserving unrelated fields.
func (p *Projector) on<UpdatedEvent>(ctx context.Context, ev *events.<UpdatedEvent>) error {

	key := <ReadModelName>Key{<FirstField>: ev.<FirstField>, <SecondField>: ev.<SecondField>}
	
	entity, err := p.repo.Find(ctx,key.String())
	if err != nil {
		return err
	}
	entity.<FieldName> = ev.<EventFieldName>
	return p.repo.Update(ctx, key.String(), entity)
}

func (p *Projector) on<AddedEvent>(ctx context.Context, ev *events.<AddedEvent>) error {
    key := <ReadModelName>Key{<FirstField>: ev.<FirstField>, <SecondField>: ev.<SecondField>}
    
	entity := &<ReadModelName>Entity{
        <FirstField>:  ev.<FirstField>,
        <SecondField>: ev.<SecondField>,
        <FieldName>:   ev.<EventFieldName>,
    }
	
    return p.repo.Update(ctx, key.String(), entity)
}

func (p *Projector) on<RemovedEvent>(ctx context.Context, ev *events.<RemovedEvent>) error {
    key := <ReadModelName>Key{<FirstField>: ev.<FirstField>, <SecondField>: ev.<SecondField>}
	
    return p.repo.Delete(ctx, key.String())
}
```


### B.3: Query Handler

```go
package <readmodelname>

import (
"context"
afreq "github.com/afosto/utils-go/query"
cqrs "github.com/terraskye/eventsourcing"
"<shared>/repository"
)
type QueryHandler struct {
	repo <ReadModelName>Repository
}

func NewQueryHandler(repo <ReadModelName>Repository) cqrs.QueryHandler[<ReadModelName>Query, *<ReadModelName>ReadModel] {
return &QueryHandler{repo: repo}
}

func (h *QueryHandler) HandleQuery(ctx context.Context, qry <ReadModelName>Query) (*<ReadModelName>ReadModel, error) {

	var cp *afreq.Criteria
	if qry.Criteria == nil {
		cp = afreq.NewCriteria()
	}else {
		cp = qry.Criteria.Clone()
	}

	// The filter field name must match the json tag of <FirstField> on <ReadModelName>Entity.
	// e.g. if the entity has `json:"case_id"` then use "case_id" here.
	cp.MustAddFilter(<readmodelname>Entity{}).<FirstField>JsonTag, afreq.OperatorEq, qry.<FirstField>.String())

	connection, err := h.repo.FindAll(ctx, cp)

	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			return nil, err
		}
	}

	return &<ReadModelName>ReadModel{Data: connection.Nodes, Cursor: connection.Cursor}, nil
}
```

---

## Field Mapping Rules

| Config Type | Go Type                              | ES tag guidance |
|-------------|--------------------------------------|-----------------|
| UUID | `uuid.UUID`                          | none — filter only |
| String (searchable) | `string`                             | pick tag by search role (see table above) |
| String (filter only) | `string`                             | none |
| Date | `time.Time`                          | none — filter only |
| Integer | `int`                                | none — filter only |
| Boolean | `bool`                               | none — filter only |
| Decimal | `float64`                            | none — filter only |
| Multiple cardinality | `[]*T`                               | composite key pattern |
| `optional: true` | pointer (`*T`)                       | same tag rules apply |
| `idAttribute: true` | contributes to ID string             | none |
| `mapping: "aggregateId"` | populated from event's `AggregateID` | none |

---


## Step 3 Specifications (Test Cases)

STATE_VIEW specs use Given/Then format (no When — no commands).

Use:
- `memory.Repository` — the in-memory `Repository[T]` implementation; satisfies both `Repository[T]` and `ReadRepository[T]`
- Standard `testing` package — no testify
- Table-driven tests — one `tests` slice, one `t.Run` loop


### Single Key Test

```go
package <readmodelname>_test

import (
    "context"
    "testing"

    "github.com/google/uuid"
    "<module>/<module>/<readmodelname>"
    "<module>/events"
    "<shared>/repository/memory"
)

func Test<ReadModelName>Projector(t *testing.T) {
    tests := []struct {
        name     string
        event    cqrs.Event
        id       uuid.UUID
        check    func(t *testing.T, entity *<readmodelname>.<ReadModelName>Entity)
    }{
        {
            name:  "<spec title>",
            id:    uuid.MustParse("<test-uuid>"),
            event: events.<SourceEvent>{
                AggregateID: uuid.MustParse("<test-uuid>"),
                <Field>:     <value>,
            },
            check: func(t *testing.T, entity *<readmodelname>.<ReadModelName>Entity) {
                if entity.<Field> != <expectedValue> {
                    t.Errorf("<Field>: got %v, want %v", entity.<Field>, <expectedValue>)
                }
            },
        },
        {
            name:  "<another spec title>",
            id:    uuid.MustParse("<test-uuid-2>"),
            event: events.<AnotherEvent>{
                AggregateID: uuid.MustParse("<test-uuid-2>"),
                <Field>:     <value2>,
            },
            check: func(t *testing.T, entity *<readmodelname>.<ReadModelName>Entity) {
                if entity.<Field> != <expectedValue2> {
                    t.Errorf("<Field>: got %v, want %v", entity.<Field>, <expectedValue2>)
                }
            },
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            ctx       :=afctx.SetTenant(t.Context(),"123")
            repo      := memory.NewRepository[<readmodelname>.<ReadModelName>Entity]()
            processor := <readmodelname>.NewProjector(repo)

            if err := processor.Handle(ctx, tt.event); err != nil {
                t.Fatalf("Handle: %v", err)
            }
			

            entity, err := repo.Find(ctx, tt.id.String())
            if err != nil {
                t.Fatalf("Find: %v", err)
            }
            if entity == nil {
                t.Fatal("expected entity, got nil")
            }
            tt.check(t, entity)
        })
    }
}
```

### Composite Key Test

For composite-key readmodels use the key struct's `String()` in `repo.Find`:

```go
func Test<ReadModelName>Projector(t *testing.T) {
    tests := []struct {
        name   string
        event  cqrs.Event
        key    <readmodelname>.<ReadModelName>Key
        check  func(t *testing.T, entity *<readmodelname>.<ReadModelName>Entity)
    }{
        {
            name: "<spec title>",
            key:  <readmodelname>.<ReadModelName>Key{
                <FirstField>:  uuid.MustParse("<uuid-1>"),
                <SecondField>: uuid.MustParse("<uuid-2>"),
            },
            event: events.<AddedEvent>{
                <FirstField>:  uuid.MustParse("<uuid-1>"),
                <SecondField>: uuid.MustParse("<uuid-2>"),
                <Field>:       <value>,
            },
            check: func(t *testing.T, entity *<readmodelname>.<ReadModelName>Entity) {
                if entity.<Field> != <expectedValue> {
                    t.Errorf("<Field>: got %v, want %v", entity.<Field>, <expectedValue>)
                }
            },
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            ctx       := afctx.SetTenant(t.Context(),"123")
            repo      := memory.NewRepository[<readmodelname>.<ReadModelName>Entity]()
            processor := <readmodelname>.NewProjector(repo)

            if err := processor.Handle(ctx, tt.event); err != nil {
                t.Fatalf("Handle: %v", err)
            }

            entity, err := repo.Find(ctx, tt.key.String())
            if err != nil {
                t.Fatalf("Find: %v", err)
            }
            if entity == nil {
                t.Fatal("expected entity, got nil")
            }
            tt.check(t, entity)
        })
    }
}
```

### Query Handler Test

Test the query handler in the same table-driven style, wiring projector and handler
against the same `memory.Repository`:

```go
func Test<ReadModelName>QueryHandler(t *testing.T) {
    tests := []struct {
        name  string
        given []cqrs.Event
        query <readmodelname>.<ReadModelName>Query
        check func(t *testing.T, result *<readmodelname>.<ReadModelName>ReadModel)
    }{
        {
            name: "returns read model after creation",
            given: []cqrs.Event{
                events.<CreatedEvent>{AggregateID: uuid.MustParse("<test-uuid>"), <Field>: <value>},
            },
            query: <readmodelname>.<ReadModelName>Query{AggregateID: uuid.MustParse("<test-uuid>")},
            check: func(t *testing.T, result *<readmodelname>.<ReadModelName>ReadModel) {
                if result == nil {
                    t.Fatal("expected result, got nil")
                }
                if result.Data.<Field> != <expectedValue> {
                    t.Errorf("<Field>: got %v, want %v", result.Data.<Field>, <expectedValue>)
                }
            },
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            ctx       := afctx.SetTenant(t.Context(),"123")
            repo      := memory.NewRepository[<readmodelname>.<ReadModelName>Entity]()
            processor := <readmodelname>.NewProjector(repo)

            for _, ev := range tt.given {
                if err := processor.Handle(ctx, ev); err != nil {
                    t.Fatalf("Handle: %v", err)
                }
            }

            handler := <readmodelname>.NewQueryHandler(repo)
            result, err := handler.HandleQuery(ctx, tt.query)
            if err != nil {
                t.Fatalf("HandleQuery: %v", err)
            }
            tt.check(t, result)
        })
    }
}
```


---

## Complete Example: "Law Firm Details for Auth"

**Input config:**
```json
{
  "sliceType": "STATE_VIEW",
  "title": "slice: Law Firm Details for Auth",
  "readmodels": [{
    "title": "Law Firm Details for Auth",
    "fields": [
      {"name": "aggregateId", "type": "UUID",   "idAttribute": true},
      {"name": "name",        "type": "String"}
    ],
    "dependencies": [
      {"type": "INBOUND", "title": "Law Firm Created", "elementType": "EVENT"},
      {"type": "INBOUND", "title": "Law Firm Updated", "elementType": "EVENT"}
    ]
  }]
}
```

Single `idAttribute` → Pattern A. No key struct needed.

`name` is a human-readable string that will be searched → `es:"fuzzy_3,prefix"`.

**Generated entity:**
```go
type LawFirmDetailsForAuthEntity struct {
    AggregateID uuid.UUID `json:"aggregate_id"`
    Name        string    `json:"name" es:"fuzzy_3,prefix"`
}
```

**Generated files:**
1. `lawfirm/slices/lawfirmdetailsforauth/readmodel.go`
2. `lawfirm/slices/lawfirmdetailsforauth/projector.go`
3. `lawfirm/slices/lawfirmdetailsforauth/query.go`

---

## Screen Dependencies

Screens in STATE_VIEW slices:
1. Retrieve data via `GenericQueryGateway.HandleQuery()`
2. May dispatch commands to other slices (STATE_CHANGE) via their own command bus —
   the STATE_VIEW slice itself never owns commands


   

**5. Wiring in `pim/setup.go`:**

All registrations are centralized in `pim/setup.go`. Do **not** touch `cmd/api.go` or `cmd/worker.go`.

**5a. Add the import** at the top of `pim/setup.go`:
```go
lawfirm_details_for_auth "github.com/afosto/pim-service/pim/slices/lawfirm_details_for_auth"
```

**5b. Add a repository field** to the `Setup` struct:
```go
type Setup struct {
    // ... existing fields ...
    lawFirmDetailsForAuthRepository lawfirm_details_for_auth.LawFirmDetailsForAuthRepository
}
```

**5c. Initialize the repository** in `New()`:
```go
s.lawFirmDetailsForAuthRepository = repository.NewTracingRepository(
    elasticsearchrepo.NewRepository[lawfirm_details_for_auth.LawFirmDetailsForAuthEntity](elasticClient,
        elasticsearchrepo.WithIndexName(lawfirm_details_for_auth.RepositoryIndexName),
        elasticsearchrepo.WithRefreshPolicy(elasticsearchrepo.RefreshPolicyWaitFor),
    ))
```

**5d. Register the query handler** in `RegisterQueryHandlers`:
```go
cqrs.RegisterQueryHandler(queryBus, tsotel.WithQueryTelemetry(lawfirm_details_for_auth.NewQueryHandler(s.lawFirmDetailsForAuthRepository), opts...))
```

**5e. Register the projector** in `RegisterProjectors`:
```go
reg(lawfirm_details_for_auth.RepositoryIndexName, lawfirm_details_for_auth.NewProjector(s.lawFirmDetailsForAuthRepository))
```
