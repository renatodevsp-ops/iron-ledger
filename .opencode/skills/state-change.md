---
name: state change slice
description: Knows how to build state change slices (Go, terraskye/eventsourcing)
---
# STATE_CHANGE Slice Skill

## Overview

A STATE_CHANGE slice represents a business operation that modifies aggregate state through
commands and events. This is the write side of CQRS — it owns commands, events, and the
aggregate decision logic. It does NOT own read models or projectors; those live in
STATE_VIEW slices.


## Input Structure (from config.json)

```json
{
  "sliceType": "STATE_CHANGE",
  "title": "slice: <Name>",
  "context": "<BoundedContext>",
  "commands": [...],
  "events": [...],
  "readmodels": [...],
  "screens": [...],
  "processors": [...],
  "specifications": [...]
}
```

## Flow Pattern

```
Screen (UI) → Command → CommandBus → CommandHandler (evolve → decide) → EventStore → Event                                              
```

## Code Generation Steps

### Step 1: Create Command

**Location:** `<module>/slices/<commandname>/command.go`

**Template:**
```go
package <commandname>

import (
    "context"
    
    xerr "<rootpackage>/errors"
    events "<rootpackage>/<module>/events"
     "github.com/google/uuid"
    cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.Command = (*Command)(nil)

type Command struct {
    AttributeID   uuid.UUID
	// Add fields from slice command definition
	// Map types: UUID, uuid.UUID, Date → time.Time, Integer → int64, Boolean → bool , Decimal → float64
	// Handle cardinality: Single → regular type, Multiple → List<type>
}

func (c Command) AggregateID() string {
    return c.AttributeID.String()
}

```

**Field Mapping Rules:**
| Slice Type | go Type |
|------------|-------------|
| UUID | uuid.UUID |
| String | string |
| Date | time.Time |
| Integer | int64 |
| Boolean | bool |
| Decimal | float64 |
| Multiple cardinality | []T |
| optional: true | *T |

### Step 2: Create Event(s)

**Location:** `<module>/events/<EventName>.go`

**Template:**
```go
package events

import (
	uuid "github.com/google/uuid"
	cqrs "github.com/terraskye/eventsourcing"
)

// Compile-time interface check.
var _ cqrs.Event = (*<EventName>)(nil)

func init() {
	cqrs.RegisterEvent(&<EventName>{})
}

type <EventName> struct {
    <IDFieldName>   uuid.UUID
    // Add fields from slice event definition
    // Fields typically mirror command fields
}

func (e *<EventName>) AggregateID() string {
	return e. <IDFieldName>.String()
}
func (e *<EventName>) EventType() string {
	return cqrs.TypeName(e)
}
```

**Event title to Go type name:** PascalCase, e.g. "Clerk Assigned to Case" → `ClerkAssignedToCase`

**Field Mapping Rules:**

| Config Type | Go Type |
|-------------|---------|
| UUID | `uuid.UUID` |
| String | `string` |
| Date | `time.Time` |
| Integer | `int64` |
| Boolean | `bool` |
| Decimal | `float64` |
| Multiple cardinality | `[]T` |
| `optional: true` | `*T` |

---

### Step 3: Create Command + Handler

Everything for a command lives in one file — command struct, state, evolve, decide,
and the constructor that wires them into `cqrs.NewCommandHandler`.

**Location:** `<module>/slices/<commandname>/command.go`

**Template for existing aggregate:**
```go
package <commandname>

import (
"context"
"errors"

"github.com/google/uuid"
cqrs "github.com/terraskye/eventsourcing"
"<module>/events"
xerr"<rootpackage>/errors"
)

// ── Command ──────────────────────────────────────────────────────────────────

// Compile-time interface check.
var _ cqrs.Command = (*Command)(nil)

type Command struct {
	<IDFieldName> uuid.UUID
	// Add fields from slice command definition.
}

func (c Command) AggregateID() string {
	return c.<IDFieldName>.String()
}

// ── Aggregate state ───────────────────────────────────────────────────────────

// state holds only the fields needed to make decisions.
// It is rebuilt from event history on every command — never persisted directly.
type state struct {
	// e.g. exists bool, status string
}

func initialState() state {
	return state{}
}

// evolve applies a single historical event to the current state.
// It must be pure — no side effects, no errors.
func evolve(s state, envelope *cqrs.Envelope) state {
	switch ev := envelope.Event.(type) {
	case *events.<EventName>:
		// update s fields from ev
		_ = ev
	}
	return s
}

// ── Error codes ───────────────────────────────────────────────────────────────

// Error codes are NOT defined here — they live in the shared errors package.
// Append new codes to the iota block in errors/error_codes.go before implementing decide.
//
// Example entry in errors/error_codes.go:
//
//  const (
//      _ ErrorCode = 999 + iota
//      ErrLanguageAlreadyDefault
//      ErrLanguageNotExist
//      Err<RuleName>              // ← append here
//  )



// ── Decision ──────────────────────────────────────────────────────────────────

// decide validates business rules and returns the events to append.
// Use xerr.NewBusinessRuleError for all domain rule rejections — never return
// raw errors from decide. This ensures clients receive a stable code, a
// user-facing title, and an actionable message.
//
// Error message guidelines:
//   Title:   2–5 words, sentence case, no punctuation, describes what went wrong
//   Message: 1–2 sentences, neutral tone, explains why and what the user can do next
//
// Params: use $key placeholders in the message and supply a map[string]string for
// any context-specific values (names, identifiers) that make the message actionable.
// Prefer params over hardcoding values — the client can use them for interpolation
// or localization. Pass nil when the message needs no substitution.
func decide(s state, cmd Command) ([]cqrs.Event, error) {
	if s.<condition> {
		return nil, xerr.NewBusinessRuleError(
			xerr.Err<RuleName>,
			"<Short title>",
			"$<param> <explains why and what to do next.>",
			map[string]any{
				"<param>": <value from state or cmd>,
			},
		)
	}

	return []cqrs.Event{
		&events.<EventName>{
		<IDFieldName>: cmd.<IDFieldName>,
		// map remaining command fields to event fields
	},
	}, nil
}

// ── Constructor ───────────────────────────────────────────────────────────────

func NewCommandHandler(store cqrs.EventStore) cqrs.CommandHandler[Command] {
	return cqrs.NewCommandHandler(
		store,
		initialState, // function reference — cqrs.InitialState[T] = func() T
		evolve,
		decide,
		cqrs.WithStreamState(cqrs.Any{}),
		cqrs.WithStreamNamer(support.<Aggregate>StreamNamer), // always required — see Stream Naming section
	)
}
```

**`WithStreamState` options:**

| Option | When to use |
|--------|-------------|
| `cqrs.Any{}` | Upsert — create or update, no version check |
| `cqrs.NoStream{}` | Create only — fails if aggregate already exists |
| `cqrs.StreamExists{}` | Update only — fails if aggregate does not exist |
| `cqrs.Revision(n)` | Optimistic concurrency at a specific version |

---

## Stream Naming (required on every handler)

`cqrs.WithStreamNamer` is **required on every `NewCommandHandler`**. It tells the event store which stream to read history from and append to.

Stream namers are defined **once per aggregate type** in `pim/support/streams.go` and shared by all slices that operate on that aggregate.

### Choosing the right stream namer

Pick the stream namer for the **aggregate** your command targets — not the specific slice. All slices that modify the same aggregate share the same stream namer.

```go
func NewCommandHandler(store cqrs.EventStore) cqrs.CommandHandler[Command] {
    return cqrs.NewCommandHandler(
        store,
        initialState,
        evolve,
        decide,
        cqrs.WithStreamState(cqrs.StreamExists{}),
        cqrs.WithStreamNamer(support.<Aggregate>StreamNamer), // ← always required
    )
}
```

### Adding a stream namer for a new aggregate

If your command targets an **aggregate that doesn't yet exist**, add a new stream namer to `pim/support/streams.go`:

```go
// For aggregates with one stream per instance (most cases):
var <Aggregate>StreamNamer = func(ctx context.Context, cmd cqrs.Command) string {
    return fmt.Sprintf("%s-{bounded-context}-v1-<aggregates>-%s", afctx.MustGetTenant(ctx), cmd.AggregateID())
}

// For singleton aggregates (one stream per tenant, like Language):
var <Aggregate>StreamNamer = func(ctx context.Context, cmd cqrs.Command) string {
    return fmt.Sprintf("%s-{bounded-context}-v1-<aggregates>", afctx.MustGetTenant(ctx))
}
```

The format is `{tenant}-{bounded-context}-v1-{aggregate-plural}[-{aggregateID}]`. Use the plural, kebab-case aggregate name. Add the aggregate ID suffix unless the aggregate is a tenant-wide singleton.

---

## Specifications (Test Cases)

Tests live alongside the command in the **same package** (not `_test`) so they can
access unexported `initialState`, `evolve`, and `decide` directly. History is
replayed through `evolve` manually — no event store, no infrastructure.

**Location:** `<module>/slices/<commandname>/command_test.go`

Use:
- Standard `testing` package — no testify
- Table-driven tests — one `tests` slice, one `t.Run` loop
- `[]*cqrs.Envelope` for history — replay through `evolve` before calling `decide`
- JSON equality for event comparison — catches structural differences without field-by-field assertions
- `want: nil` on error cases — `decide` must return `nil` events alongside the error

```go
package <commandname>

import (
    "bytes"
    "encoding/json"
    "testing"

    cqrs "github.com/terraskye/eventsourcing"
    "<module>/events"
)

func Test_decide(t *testing.T) {
    type args struct {
        history []*cqrs.Envelope
        cmd     Command
    }

    tests := []struct {
        name    string
        args    args
        want    []cqrs.Event
        wantErr bool
    }{
        {
            name: "spec: <SliceName> - <happy flow title>",
            args: args{
                cmd: Command{
                    <IDFieldName>: uuid.MustParse("<test-uuid>"),
                    <Field>:       <value>,
                },
                history: []*cqrs.Envelope{
                    {Event: &events.<PriorEvent>{<fields>}},
                },
            },
            want:    []cqrs.Event{&events.<ExpectedEvent>{<fields>}},
            wantErr: false,
        },
        {
            name: "spec: <SliceName> - <rejection title>",
            args: args{
                cmd: Command{
                    <IDFieldName>: uuid.MustParse("<test-uuid>"),
                },
                history: []*cqrs.Envelope{
                    {Event: &events.<EventThatViolatesRule>{}},
                },
            },
            want:    nil,
            wantErr: true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            s := initialState()
            for _, envelope := range tt.args.history {
                s = evolve(s, envelope)
            }

            got, err := decide(s, tt.args.cmd)
            if (err != nil) != tt.wantErr {
                t.Errorf("decide() error = %v, wantErr %v", err, tt.wantErr)
                return
            }

            if len(got) != len(tt.want) {
                t.Errorf("decide() expected %d but got %d events", len(tt.want), len(got))
                return
            }

            wantData, _ := json.Marshal(tt.want)
            gotData, _  := json.Marshal(got)
            if !bytes.Equal(wantData, gotData) {
                t.Errorf("decide() expected %s but got %s", wantData, gotData)
            }
        })
    }
}
```

---

### Step 4: Wiring in `pim/setup.go`

All slice registrations are centralized in `pim/setup.go`. Do **not** touch `cmd/api.go` or `cmd/worker.go`.

**4a. Add the import** at the top of `pim/setup.go`:
```go
assign_clerk_to_case "github.com/afosto/pim-service/pim/slices/assign_clerk_to_case"
```

**4b. Register the command handler** inside `RegisterCommandHandlers`:
```go
func (s *Setup) RegisterCommandHandlers(commandBus *cqrs.CommandBus, eventStore cqrs.EventStore, opts []tsotel.Option) {
    // ... existing registrations ...
    cqrs.Register(commandBus, tsotel.WithCommandTelemetry(assign_clerk_to_case.NewCommandHandler(eventStore), opts...))
}
```

## Package Structure

```
<module>/
├── events/
│   └── <eventname>.go          # Shared events — one file per event
│
└── slices/
    └── <commandname>/
        ├── command.go           # Command, state, evolve, decide, NewCommandHandler
        └── command_test.go      # Table-driven specs
```

## Complete Example: "Assign Clerk to Case"

**Input config:**
```json
{
  "sliceType": "STATE_CHANGE",
  "title": "slice: Assign Clerk to Case",
  "commands": [{
    "title": "Assign Clerk to Case",
    "fields": [
      {"name": "caseId",     "type": "UUID",    "idAttribute": true},
      {"name": "clerkId",    "type": "UUID"},
      {"name": "hourlyRate", "type": "Decimal"},
      {"name": "caseName", "type": "String"}
    ]
  }],
  "events": [{
    "title": "Clerk Assigned to Case",
    "fields": [
      {"name": "caseId",     "type": "UUID",    "idAttribute": true},
      {"name": "clerkId",    "type": "UUID"},
      {"name": "hourlyRate", "type": "Decimal"},
      {"name": "caseName", "type": "String"}
    ]
  }],
  "specifications": [
    {
      "title": "assigns clerk to an open case",
      "given": [],
      "when": {"command": "Assign Clerk to Case"},
      "then": [{"event": "Clerk Assigned to Case"}]
    },
    {
      "title": "rejects if clerk already assigned",
      "given": [{"event": "Clerk Assigned to Case"}],
      "when": {"command": "Assign Clerk to Case"},
      "then": "error"
    }
  ]
}
```

**`cases/events/clerk_assigned_to_case.go`:**
```go
package events

import (
    "github.com/google/uuid"
    cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.Event = (*ClerkAssignedToCase)(nil)

func init() {
    cqrs.RegisterEvent(&ClerkAssignedToCase{})
}

type ClerkAssignedToCase struct {
    CaseID     uuid.UUID
    ClerkID    uuid.UUID
    HourlyRate float64
}

func (e *ClerkAssignedToCase) AggregateID() string { return e.CaseID.String() }
func (e *ClerkAssignedToCase) EventType() string   { return cqrs.TypeName(e) }
```

**`cases/slices/assignclerktoccase/command.go`:**
```go
package assignclerktocase

import (
    "github.com/google/uuid"
    cqrs "github.com/terraskye/eventsourcing"
    xerr "<rootpackage>/errors"
    "cases/events"
)

var _ cqrs.Command = (*Command)(nil)

type Command struct {
    CaseID     uuid.UUID
    ClerkID    uuid.UUID
    HourlyRate float64
}

func (c Command) AggregateID() string { return c.CaseID.String() }

// ── State ─────────────────────────────────────────────────────────────────────

type state struct {
    clerkAssigned bool
    caseName      string // used in error messages
}

func initialState() state { return state{} }

func evolve(s state, envelope *cqrs.Envelope) state {
    switch ev := envelope.Event.(type) {
    case *events.ClerkAssignedToCase:
        s.clerkAssigned = true
        s.caseName = ev.CaseName
    }
    return s
}

// Error codes live in the shared xerr package — append before implementing decide:
//
//  // xerr/errors.go
//  const (
//      _ ErrorCode = 999 + iota
//      ...
//      ErrClerkAlreadyAssigned
//  )

// ── Decision ──────────────────────────────────────────────────────────────────

func decide(s state, cmd Command) ([]cqrs.Event, error) {
    if s.clerkAssigned {
        return nil, xerr.NewBusinessRuleError(
            xerr.ErrClerkAlreadyAssigned,
            "Clerk already assigned",
            "$case already has a clerk assigned. Remove the current clerk before assigning a new one.",
            map[string]any{
                "case": s.caseName,
            },
        )
    }
    return []cqrs.Event{
        &events.ClerkAssignedToCase{
            CaseID:     cmd.CaseID,
            ClerkID:    cmd.ClerkID,
            HourlyRate: cmd.HourlyRate,
        },
    }, nil
}

func NewCommandHandler(store cqrs.EventStore) cqrs.CommandHandler[Command] {
    return cqrs.NewCommandHandler(
        store,
        initialState, // function reference — cqrs.InitialState[T] = func() T
        evolve,
        decide,
        cqrs.WithStreamState(cqrs.Any{}),
        cqrs.WithStreamNamer(support.CaseStreamNamer),
    )
}
```

**`cases/slices/assignclerktocase/command_test.go`:**
```go
package assignclerktocase

import (
    "bytes"
    "encoding/json"
    "testing"

    "github.com/google/uuid"
    cqrs "github.com/terraskye/eventsourcing"
    "cases/events"
)

var (
    caseID  = uuid.MustParse("00000000-0000-0000-0000-000000000001")
    clerkID = uuid.MustParse("00000000-0000-0000-0000-000000000002")
)

func Test_decide(t *testing.T) {
    type args struct {
        history []*cqrs.Envelope
        cmd     Command
    }

    tests := []struct {
        name    string
        args    args
        want    []cqrs.Event
        wantErr bool
    }{
        {
            name: "spec: Assign Clerk to Case - happy flow",
            args: args{
                cmd: Command{
                    CaseID:     caseID,
                    ClerkID:    clerkID,
                    HourlyRate: 125.50,
                },
                history: []*cqrs.Envelope{},
            },
            want:    []cqrs.Event{&events.ClerkAssignedToCase{CaseID: caseID, ClerkID: clerkID, HourlyRate: 125.50}},
            wantErr: false,
        },
        {
            name: "spec: Assign Clerk to Case - rejects if clerk already assigned",
            args: args{
                cmd: Command{CaseID: caseID, ClerkID: clerkID, HourlyRate: 125.50},
                history: []*cqrs.Envelope{
                    {Event: &events.ClerkAssignedToCase{CaseID: caseID, ClerkID: clerkID, HourlyRate: 125.50}},
                },
            },
            want:    nil,
            wantErr: true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            s := initialState()
            for _, envelope := range tt.args.history {
                s = evolve(s, envelope)
            }

            got, err := decide(s, tt.args.cmd)
            if (err != nil) != tt.wantErr {
                t.Errorf("decide() error = %v, wantErr %v", err, tt.wantErr)
                return
            }

            if len(got) != len(tt.want) {
                t.Errorf("decide() expected %d but got %d events", len(tt.want), len(got))
                return
            }

            wantData, _ := json.Marshal(tt.want)
            gotData, _  := json.Marshal(got)
            if !bytes.Equal(wantData, gotData) {
                t.Errorf("decide() expected %s but got %s", wantData, gotData)
            }
        })
    }
}
```

**Generated files:**
1. `cases/events/clerk_assigned_to_case.go`
2. `cases/slices/assignclerktocase/command.go`
3. `cases/slices/assignclerktocase/command_test.go`