---
status: draft
slug: refactor-hubsync-taskrunner-api-flags-decode-generic-config
parent-spec: /Users/me/Dropbox/boss/tasks/unify-ls-pattern-resume-db-task-runner-and-jsonl-hujson-output-convention/specs/taskrunner-api-open-questions.md
---

# taskrunner API refactor — non-generic, factory-based

## Goal

Reshape the `hubsync/internal/taskrunner` public API around two small interfaces (`Task`, `TaskFactory`) with a non-generic `Config`, so that (a) user binaries own their own flag surface, (b) the planning and rehydration hooks are coherent and cancellation-aware, and (c) `Config` can grow new fields (e.g. the parent spec's `Docstring`) without dragging a type parameter through every signature.

This unblocks Phase 5 of the parent JSONL-docstring spec by giving `Config` a clean, non-parametric home, and unblocks user binaries like `cli/hubsync/archive.go` from having to fork ~60 lines of `Main`-lookalike boilerplate whenever they want a domain flag.

**In scope**: the public API of `internal/taskrunner` — `Task`, new `TaskFactory` interface, `Config`, `RunOptions`, `Main`, `New`, plus a new `BindFlags` + `Run` pair. Full breaking-change pass: all consumers in the repo migrate in the same commit range.

**Out of scope**: the `Docstring` field itself (follow-up under the parent spec), the JSONL prelude emission hooks, duckql auto-strip. Internal mechanics (SQLite schema, claim loop, heartbeat, archive-on-success) are unchanged.

Go version in hubsync is 1.25 — `iter.Seq2` is available, so `List` is a range-over-func rather than a Next/Error pair.

## Decisions on the three issues

### Issue 1 — the `Config[T Task]` generic — **DROP**

Originally leaning "keep" in an earlier draft of this spec. Reversed after walking through the interface-based shape with the human. The rationale:

- The three places `T` buys anything today — typed `emit`, typed `Init`, compile-time "Plan and Init agree on T" — all have near-zero practical weight. Call-sites already use implicit interface conversion (`emit(&Chunk{...})` looks identical whether emit is `func(*Chunk)` or `func(Task)`); the Plan/Init-agreement check catches real bugs in zero current consumers.
- What `T` costs, conversely, is ongoing: every future `Config` field that isn't T-dependent still has to ride through `Config[T]`. The parent spec's `Docstring` hook is the first concrete example; `BindFlags` / `Run` / `Main` are the second.
- `reflect.TypeFor[T]()` → `reflect.TypeOf(factory.Hydrate(ctx))` is a one-call substitute for schema inference, evaluated once at `New(cfg, opts)` time and cached on `Runner`.

**Decision**: non-generic `Task` + `TaskFactory` interfaces. `Config` / `RunOptions` / `Runner` / `Main` / `Run` all lose the type parameter.

### Issue 2 — `Decode func(row map[string]any) (T, error)` — **SUBSUMED BY `TaskFactory.Hydrate`**

The brief's rename (Decode → Init) was aimed at the same problem: users were writing the same JSON round-trip boilerplate because the real job was dep injection. The non-generic refactor rolls that rename into a bigger shift — Plan + Init collapse into a single `TaskFactory` with two methods (`List` + `Hydrate`), both ctx-aware. The runner owns the items-row → JSON → `Task` round-trip; the user's `Hydrate` returns a fresh Task instance with transient deps already set.

### Issue 3 — flag ownership — **SPLIT (unchanged from earlier draft)**

- **`BindFlags(fs *flag.FlagSet) *RunOptions`** — binds the runner's flags onto a user-supplied FlagSet and returns the pointer the caller reads after `fs.Parse`. User adds their own flags on the same fs before parsing.
- **`Run(ctx, cfg, opts) (RunResult, error)`** — primary library entry point. Opens the DB, runs Execute, captures summary, returns. Does not install signal handlers, does not call `os.Exit`. Caller owns both.
- **`Main(cfg)` stays as sugar** for zero-custom-flag binaries. Wires `BindFlags` → `Parse` → `signal.NotifyContext` → `Run` → `os.Exit`.

The user's hard constraint ("taskrunner must NOT fully own the user binary's flag surface") is satisfied: `cli/hubsync/archive.go` calls `BindFlags` on its own FlagSet, adds `--bucket` / etc. alongside, and parses once.

## Architecture

### Final API surface

```go
package taskrunner

// Task is the unit of work. Concrete types both carry the row body
// (round-trip through encoding/json with an "id" field) and execute
// (Run). The runner never touches a Task beyond json marshal/unmarshal
// and invoking Run.
type Task interface {
    // Run executes one task. Result is JSON-marshaled into tasks.meta.result
    // on success; non-nil err becomes a 'failed' terminal write.
    //
    // Handlers MUST be idempotent — a stale-heartbeat running row is
    // re-claimed from scratch.
    Run(ctx context.Context) (result any, err error)
}

// StatusReporter is an optional add-on probed via type assertion: when
// Task implements it, the runner polls Status on each heartbeat tick and
// merges the snapshot into tasks.meta.status. Called concurrently with
// Run.
type StatusReporter interface {
    Status() (any, error)
}

// TaskFactory produces tasks. List enumerates work once (on the first
// Execute against an empty items table); Hydrate returns a fresh Task
// instance for every worker claim, with transient deps pre-injected.
//
// The factory itself is long-lived — it spans the Runner's lifetime.
// Implementations typically close over their dependency struct
// (storage client, DB handle, paths) once and re-use on every call.
type TaskFactory interface {
    // List enumerates work on first run. Called exactly when the items
    // table is empty (first Execute, or after a manual wipe). Returns
    // a range-over-func iterator: each yielded (Task, nil) is planned
    // into the items table; a yielded (_, err) aborts planning.
    //
    // The ctx is the caller's Execute ctx; honor it for cancellation
    // on long enumerations (directory walks, DB queries).
    List(ctx context.Context) iter.Seq2[Task, error]

    // Hydrate returns a fresh Task instance with transient deps
    // injected. Called once per successful claim. The runner
    // subsequently json.Unmarshal's the items row into the returned
    // Task (mutating its JSON-tagged fields); deps tagged `json:"-"`
    // survive the unmarshal.
    //
    // An error here becomes a terminal 'failed' write on the row with
    // "decode: <err>" — matching the current cfg.Decode-error path.
    Hydrate(ctx context.Context) (Task, error)
}

// Config is non-generic. Future fields (e.g. Docstring from the parent
// spec) land here without re-parametrizing.
type Config struct {
    Factory TaskFactory
    // future: Docstring *template.Template, etc.
}

// RunOptions tunes a single run. DBPath moved here from Config — it's a
// runtime knob (the state file path), not a domain configuration.
type RunOptions struct {
    DBPath         string        // SQLite state file. Empty → in-memory.
    Workers        int           // default runtime.NumCPU()
    Where          string        // SQL fragment AND-combined with work-queue predicate
    MaxAttempts    int           // default 3
    Stale          time.Duration // default 30s
    HeartbeatEvery time.Duration // default 5s
    Dry            bool          // plan-if-empty, don't run
    KeepDB         bool          // skip archive-on-success rename
}

// BindFlags binds the taskrunner's flags onto fs and returns the
// RunOptions the caller reads after fs.Parse. Callers may add their
// own flags to the same fs before parsing.
//
// Flags bound: --resume (→ DBPath), --where, --dry, --workers,
// --max-attempts, --stale, --heartbeat, --keep-db. Defaults match
// today's Main.
func BindFlags(fs *flag.FlagSet) *RunOptions

// Run is the library entry point: open the DB, infer schema from the
// factory's sample task, Execute, capture summary, close, and return.
// Does not install signal handlers; does not call os.Exit. Caller owns
// both.
//
// Error composition:
//   - setup failures (schema inference, DB open)   → returns err, zero RunResult
//   - Execute's ctx/worker error                    → returns err with populated RunResult
//   - Summary.Failed > 0 after clean drain          → returns ErrFailedRemain
//   - clean drain with zero failures                → returns nil
func Run(ctx context.Context, cfg Config, opts RunOptions) (RunResult, error)

// RunResult surfaces the summary + archived path without forcing
// callers to keep a Runner alive.
type RunResult struct {
    Summary      Summary
    ArchivedPath string
}

// ErrFailedRemain signals a clean-drain run where one or more tasks
// are in terminal 'failed' state. Use errors.Is in the caller (Main
// does).
var ErrFailedRemain = errors.New("taskrunner: failed row(s) remain")

// Main is the zero-custom-flag sugar: BindFlags + Parse + signal ctx +
// Run + os.Exit. Exit codes: 2 on setup failure, 1 on any other error
// (including ErrFailedRemain), 0 on clean drain.
func Main(cfg Config)
```

### Key shifts from today

- **`Config` / `Runner` / `Main` / `Run` are non-generic.** Stack traces lose the `Runner[*hubsync.ArchiveTask]` prefix; `Config` grows new fields without re-parametrizing.
- **`Config.Plan` + `Config.Decode` merge into `Config.Factory TaskFactory`.** One coherent unit: "here is how my work is enumerated and instantiated."
- **`List` uses `iter.Seq2[Task, error]`** — Go 1.23+ range-over-func. Context flows through naturally; errors interleave with task emits; runner can short-circuit on bind failure via yield-returning-false.
- **`Hydrate(ctx)` takes a context.** Allows future I/O at rehydrate time (per-task secret fetch, tmpfile allocation, logger mint). Today's hubsync consumer uses it only for dep injection, no I/O.
- **`DBPath` moves from `Config` → `RunOptions`.** `BindFlags` owns `--resume`; `Config` describes the domain; `opts` describes the run.
- **Schema inference runs once, at `Run`'s preamble.** Call `factory.Hydrate(ctx)` once with a scratch context, reflect on the returned Task's type, build the cached `itemSchema`. If Hydrate returns a non-pointer-to-struct or the struct lacks a `json:"id"` field, fail the Run at setup (mirrors today's `inferItemSchema` failure mode but detected earlier).
- **Emit-type validation at plan time.** Each `(Task, nil)` yielded by `List` is checked: `reflect.TypeOf(task) != schemaType` → terminal plan error. Catches "factory.Hydrate returns `*Chunk` but List yielded `*Segment`" as a clean first-test-run error. Today's generic catches this at compile time; the runtime check is the only real downgrade from dropping the generic, and the cost is microscopic.

### Task-instance lifecycle (unchanged semantically, reworded)

| phase | frequency | what runs |
|---|---|---|
| `List` | once per DB lifetime (first Execute when items empty) | user enumerates work; runner binds + INSERTs each yielded Task into `items`; seeds `tasks` |
| `Hydrate` | once per claim (worker picks up one row) | user returns a fresh Task with deps; runner json.Unmarshal's the items row into it |
| `Task.Run` | once per claim (after Hydrate) | user executes the task |

### Before/after at `cli/hubsync/archive.go`

**Before** (today, `cli/hubsync/archive.go:39-143` — ~60 lines of wiring):

```go
func cmdArchive(args []string) {
    fs := flag.NewFlagSet("archive", flag.ExitOnError)
    resume := fs.String("resume", "", "...")
    where := fs.String("where", "", "...")
    dry := fs.Bool("dry", false, "...")
    workers := fs.Int("workers", 0, "...")
    maxAttempts := fs.Int("max-attempts", 3, "...")
    stale := fs.Duration("stale", 30*time.Second, "...")
    heartbeat := fs.Duration("heartbeat", 5*time.Second, "...")
    keepDB := fs.Bool("keep-db", false, "...")
    fs.Usage = func() { /* ... */ }
    fs.Parse(args)
    // ... hub / stack setup ...
    cfg := taskrunner.Config[*hubsync.ArchiveTask]{
        DBPath: dbPath,
        Plan:   hubsync.PlanArchiveTasks(deps),
        Decode: hubsync.DecodeArchiveTask(deps),
    }
    runner, err := taskrunner.New(cfg)
    if err != nil { fatalf("taskrunner: %v", err) }
    defer runner.Close()
    ctx, cancel := signal.NotifyContext(...)
    defer cancel()
    opts := taskrunner.RunOptions{Workers: *workers, Where: *where, ...}
    if err := runner.Execute(ctx, opts); err != nil { ... }
    s := runner.Summary()
    fmt.Fprintf(os.Stderr, "archive: %d done, ...\n", s.Done, ...)
    if archived := runner.ArchivedPath(); archived != "" { ... }
    if s.Failed > 0 { os.Exit(1) }
}
```

**After** (~30 lines):

```go
func cmdArchive(args []string) {
    fs := flag.NewFlagSet("archive", flag.ExitOnError)
    opts := taskrunner.BindFlags(fs)
    // hubsync-specific flags would live here.
    fs.Usage = func() { /* unchanged */ }
    fs.Parse(args)

    // ... hub / stack setup ...
    if opts.DBPath == "" {
        opts.DBPath = filepath.Join(hubDir, ".hubsync", "archive.duckdb")
    }

    cfg := taskrunner.Config{
        Factory: hubsync.NewArchiveTaskFactory(deps),
    }

    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    result, err := taskrunner.Run(ctx, cfg, *opts)
    fmt.Fprintf(os.Stderr, "archive: %d done, %d pending, %d running, %d failed\n",
        result.Summary.Done, result.Summary.Pending, result.Summary.Running, result.Summary.Failed)
    if result.ArchivedPath != "" {
        fmt.Fprintf(os.Stderr, "archive: archived task DB -> %s\n", result.ArchivedPath)
    }
    if err != nil {
        if !errors.Is(err, taskrunner.ErrFailedRemain) {
            fmt.Fprintf(os.Stderr, "archive: %v\n", err)
        }
        os.Exit(1)
    }
}
```

And the factory that replaces `PlanArchiveTasks` + `DecodeArchiveTask`:

**Before** (`archive_task.go:54-101` — ~50 lines: Plan closure + DecodeArchiveTask closure + asInt64 helper):

```go
func PlanArchiveTasks(deps ArchiveTaskDeps) func(ctx context.Context, emit func(*ArchiveTask)) error {
    return func(ctx context.Context, emit func(*ArchiveTask)) error {
        rows, err := deps.Store.PendingArchiveRows()
        if err != nil { return fmt.Errorf("plan archive tasks: %w", err) }
        for _, r := range rows {
            emit(&ArchiveTask{ID: r.Path, Path: r.Path, /* 5 more fields + 5 deps */})
        }
        return nil
    }
}

func DecodeArchiveTask(deps ArchiveTaskDeps) func(row map[string]any) (*ArchiveTask, error) {
    return func(row map[string]any) (*ArchiveTask, error) {
        id, _ := row["id"].(string)
        path, _ := row["path"].(string)
        digestHex, _ := row["digest"].(string)
        size := asInt64(row["size"])
        mtime := asInt64(row["mtime"])
        t := &ArchiveTask{
            ID: id, Path: path, DigestHex: digestHex, Size: size, MTime: mtime,
            store: deps.Store, storage: deps.Storage, /* 3 more */
        }
        return t, nil
    }
}

func asInt64(v any) int64 { /* switch-case dead code */ }
```

**After** (~30 lines, one struct + three methods):

```go
type ArchiveTaskFactory struct { deps ArchiveTaskDeps }

func NewArchiveTaskFactory(deps ArchiveTaskDeps) *ArchiveTaskFactory {
    return &ArchiveTaskFactory{deps: deps}
}

func (f *ArchiveTaskFactory) List(ctx context.Context) iter.Seq2[taskrunner.Task, error] {
    return func(yield func(taskrunner.Task, error) bool) {
        rows, err := f.deps.Store.PendingArchiveRows()
        if err != nil { yield(nil, fmt.Errorf("plan archive tasks: %w", err)); return }
        for _, r := range rows {
            if ctx.Err() != nil { yield(nil, ctx.Err()); return }
            t := &ArchiveTask{
                ID: r.Path, Path: r.Path, Size: r.Size,
                DigestHex: hex.EncodeToString(r.Digest.Bytes()), MTime: r.MTime,
            }
            // deps injected via Hydrate at claim time, not here — keeps
            // List focused on "enumerate work", Hydrate on "wire deps"
            if !yield(t, nil) { return }
        }
    }
}

func (f *ArchiveTaskFactory) Hydrate(ctx context.Context) (taskrunner.Task, error) {
    return &ArchiveTask{
        store:   f.deps.Store,
        storage: f.deps.Storage,
        hasher:  f.deps.Hasher,
        hubDir:  f.deps.HubDir,
        prefix:  f.deps.Prefix,
    }, nil
}
```

`asInt64` goes away entirely — the runner's JSON round-trip handles numeric column widening natively.

Net LOC: `cli/hubsync/archive.go` ~60 → ~30, `archive_task.go`'s Plan+Decode ~50 → ~30. About 50 lines of mechanical boilerplate deleted, and the shape of "factory" maps cleanly onto the user's mental model.

### What the new `Run` does, in order

1. Validate `cfg.Factory != nil`.
2. Open `opts.DBPath` (SQLite WAL DSN) and ensure base tables. Fail → return wrapped error.
3. Call `cfg.Factory.Hydrate(ctx)` once with a scratch context → sample Task. Reflect on its type to build the cached `itemSchema`. Fail (non-pointer-struct, missing `json:"id"`, unsupported field type) → return wrapped error.
4. Construct a Runner bound to the cached schema and the factory.
5. Run `Execute` (plan-if-empty → drain → archive-on-success). Inside:
   - `runPlan` consumes `cfg.Factory.List(ctx)` via `for task, err := range seq`.
   - `processRow` calls `cfg.Factory.Hydrate(ctx)` + `json.Unmarshal(row, task)`.
6. Capture `Summary()` + `ArchivedPath()` into `RunResult`.
7. `r.Close()`.
8. Compose error per the rules above (setup / execute / `ErrFailedRemain` / nil).

Callers that want mid-run access to the raw Runner (tests calling `Execute` twice; tooling poking at `r.DB()`) still use `New(cfg, opts) (*Runner, error)` + `Execute` + `Close`. `Run` is the packaged-path for binaries.

### Internal changes

- **`plan.go`** — `inferItemSchema` loses its generic, becomes `inferItemSchema(sample any) (itemSchema, error)`. Called once from `New` with the sample returned by `factory.Hydrate(ctx)`; schema cached on `Runner`.
- **`plan.go`** — `runPlan` consumes `iter.Seq2[Task, error]` instead of calling a `Plan` func. Each yielded task is type-checked against the cached schema, bound, and INSERTed. First error (yielded or bind) aborts via `yield-false` return and propagates up.
- **`runner.go`** — `Config` and `Runner` lose the `[T]` parameter. `Config.Plan` + `Config.Decode` removed. `Config.Factory TaskFactory` added. `processRow` calls a new `r.hydrate(ctx, row) (Task, error)` that wraps Factory.Hydrate + json round-trip (with `json.RawMessage` substitution for `isJSON` columns so slice/map/struct fields round-trip structurally, not as JSON-inside-a-string).
- **`main.go`** — `Main` becomes ~15 lines around `BindFlags` + `Run`.
- **`task.go`** — `Task` interface unchanged. `StatusReporter` unchanged. `TaskFactory` added.

## Steps

### Phase 1 — spec + rfc gate

- [x] Draft `spec.md` (this document).
- [ ] Human reviews; ticks `rfc: review spec.md` in the boss doc.

### Phase 2 — taskrunner package refactor

- [ ] `internal/taskrunner/task.go` — add `TaskFactory` interface.
- [ ] `internal/taskrunner/runner.go` — non-generic `Config` / `Runner`; add `Factory TaskFactory`; move `DBPath` to `RunOptions`.
- [ ] `internal/taskrunner/plan.go` — `inferItemSchema(sample any)`; cache on Runner; consume `iter.Seq2` in `runPlan` with yield-false on errors.
- [ ] `internal/taskrunner/runner.go` — `hydrate(ctx, row)` method (Factory.Hydrate + json round-trip with RawMessage fix-up).
- [ ] `internal/taskrunner/runner.go` — add `BindFlags`, `Run`, `RunResult`, `ErrFailedRemain`.
- [ ] `internal/taskrunner/main.go` — rewrite `Main` as thin shell.

### Phase 3 — consumer migration

- [ ] `archive_task.go` — replace `PlanArchiveTasks` + `DecodeArchiveTask` with `ArchiveTaskFactory` + `NewArchiveTaskFactory` + `List` + `Hydrate`. Delete `asInt64`.
- [ ] `cli/hubsync/archive.go` — switch to `BindFlags` + `Run`. Default path logic moves from `resume == ""` to `opts.DBPath == ""`. Preserve Usage text.
- [ ] `archive_e2e_test.go:221` — update call-site to `taskrunner.New(taskrunner.Config{Factory: NewArchiveTaskFactory(deps)}, opts)` or `taskrunner.Run`.

### Phase 4 — tests

- [ ] `runner_test.go` — rewrite `decoderFor` helper as an `echoFactory` struct implementing `TaskFactory`. Share test state via struct fields, not closure.
- [ ] `runner_test.go` — add `TestRun_BindFlagsAndExitCodes`: fresh DB, call `BindFlags`, parse a synthetic args slice, call `Run`, assert `RunResult.Summary` and `errors.Is(err, ErrFailedRemain)` branches.
- [ ] `runner_test.go` — add `TestHydrate_JSONColumn`: Task with a `[]string` field (JSON-tagged), first Execute emits via List; second Execute re-hydrates via Hydrate + unmarshal; assert the slice survives byte-for-byte. Guards the `json.RawMessage` substitution logic.
- [ ] `runner_test.go` — add `TestList_ContextCancellation`: factory whose List checks `ctx.Err()` mid-enumerate; caller's ctx is cancelled; assert runPlan returns ctx.Canceled.
- [ ] `archive_test.go` / `archive_e2e_test.go` — `go test ./...`, fix call-site breakage.

### Phase 5 — verification

- [ ] `cd repos/github.com/hayeah/hubsync && go build ./...` clean.
- [ ] `go test ./internal/taskrunner/... ./...` green.
- [ ] Manual smoke: `hubsync archive --dry` on a scratch hub; `hubsync archive --help` confirms flag surface matches today.

## Verification

```bash
cd repos/github.com/hayeah/hubsync

# Build.
go build ./...

# Taskrunner unit tests.
go test -count=1 ./internal/taskrunner/...

# Broader suite — archive_e2e exercises the new Config shape via
# cli/hubsync/archive.go. Uses archive.FakeStorage, no live B2.
go test -count=1 ./...

# Confirm the old surface is gone:
rg 'taskrunner\.(Config\[|Decode:|Plan:|New\[)' .   # expect: no hits
rg 'Config\[.+\]\{'                                  # expect: no hits
rg 'DecodeArchiveTask|PlanArchiveTasks'              # expect: no hits

# Smoke the CLI.
go test -run TestArchive -count=1 -v .

# Live-B2 integration tier (AGENTS.md:6,34 — gated on HUBSYNC_TEST_BUCKET
# + B2 creds; skips cleanly when any of the three env vars are missing).
# Run this after the refactor to confirm nothing regressed end-to-end
# against the real bucket.
HUBSYNC_TEST_BUCKET=hayeah-hubsync-test \
  godotenv -f ~/.env.secret \
  go test -v ./archive/
```

Evidence in `## Evidence` will include:

- `go test ./...` tail with all green.
- Before/after `wc -l cli/hubsync/archive.go` + `archive_task.go` (the tangible ergonomics win).
- Short transcript of `hubsync archive --dry` and `--help`.

## Open questions

Decisions taken in conversation, flagged here for the human's rfc review:

- **Q1**: `Config` is non-generic; `TaskFactory` interface replaces `Plan` + `Decode` closures. Confirmed.
- **Q2**: `List(ctx) iter.Seq2[Task, error]` over `Next()/Error()/New()` iterator. Preferred because ctx flows naturally, errors interleave, yield-false gives the runner a short-circuit. Go 1.25 makes `iter.Seq2` available.
- **Q3**: Method name `Hydrate(ctx)` over `Open(ctx)` / `New(ctx)`. `Open` carried a "pairs with Close" expectation the API doesn't honor; `Hydrate` is precise ("re-hydrate a Task from its persisted row") without baggage.
- **Q4**: `DBPath` moved from `Config` to `RunOptions`. Keeps `Config` as "domain" and `RunOptions` as "runtime knobs including the state file path". Natural home for `BindFlags`'s `--resume`.
- **Q5**: `Run` returns `(RunResult, error)` rather than a `Run(...) error` + out-param. Flat two-value return, standard Go.

One remaining for the human:

- **Q6**: Does the runtime check "Factory.Hydrate returns pointer-to-struct with `json:\"id\"` field" at `Run` preamble (not at first emit) feel like enough coverage for the "Plan/Hydrate-type-drift" case that the generic used to catch at compile time? My read: yes — the check runs once, fails loudly at startup, and can't false-negative. But it's the one real downgrade from dropping the generic, so worth a nod before shipping.

## Design notes

- 2026-04-20T07:10Z — Flipped Issue 1 from "keep generic" to "drop it" after walking through the factory-based alternative with the human.
  - What flipped: the earlier draft recommended keeping `Config[T Task]` on the grounds that `Init func(T)` benefited from typed `T`. Re-examining, the benefit is mostly cosmetic — user call-sites write `&Chunk{}` identically whether the signature takes `*Chunk` or `Task` (implicit interface conversion). What's *not* cosmetic is the recurring cost of threading `[T]` through every future `Config` addition (starting with the parent spec's `Docstring` hook).
  - Alternatives considered:
    - **Keep generic, rename Decode→Init** (earlier draft): compile-time type agreement between Plan and Init. Real but narrow win; nobody mixes concrete types in one queue.
    - **Drop generic, factory struct-of-funcs** (`Config{Plan func, New func}`): same API shape, non-generic. Viable, but less evocative than a named `TaskFactory` interface.
    - **Drop generic, single TaskFactory interface with `Plan(ctx, emit)` + `New() Task`**: coherent, but callback-style emit is inverted control; `iter.Seq2` is cleaner for 1.23+.
    - **Drop generic, Next/Error/New iterator**: sql.Rows idiom, familiar. Loses on context propagation (Next has no ctx; per-call ctx is redundant; Start(ctx) is stateful/fragile).
    - **Picked: drop generic, TaskFactory with `List(ctx) iter.Seq2[Task, error]` + `Hydrate(ctx) (Task, error)`**: ctx in both, errors interleave with emits via Seq2, runner short-circuits via yield-false, closures still compose if users prefer that over a named factory struct.
  - Runtime downgrade: "Plan and Hydrate agree on T" is no longer a compile error, just a first-emit reflect check. Microscopic in practice.
  - Follow-on: every future `Config` field (Docstring, telemetry hooks, anything else) lives on a plain struct. The parent spec's Phase 5 is unblocked on exactly this.

- 2026-04-20T07:18Z — Picked `Hydrate(ctx)` over `Open(ctx)` and `New(ctx)` for the factory's per-claim method.
  - Alternatives:
    - **`Open(ctx)`**: evocative ("open a row into a runnable Task") and ctx-aware. Problem: Go convention pairs Open with Close (os.Open, sql.Open, net.Dial). Task has no Close today. Three sub-responses — accept the mild misdirection; add an `io.Closer` probe in the runner for optional per-task cleanup (same pattern as StatusReporter); rename to sidestep. The Closer probe is a real feature, but it's scope-creep for this spec.
    - **`New(ctx)`**: shortest, Go-idiomatic. But `New` as a method name (not package function) reads oddly, and "new" suggests no I/O — which contradicts why we added ctx in the first place.
    - **`Hydrate(ctx)`**: precise. "Re-hydrate a Task from its persisted row" is exactly what the method does. No Close baggage; ctx signals I/O is OK.
    - **Picked: `Hydrate`.** Leaves the `io.Closer` probe question for a later spec if per-task resource ownership ever matters.

- 2026-04-20T07:22Z — Moved `DBPath` from `Config` to `RunOptions`.
  - Why: `BindFlags` owns `--resume`. The flag maps naturally to a field on `RunOptions` (which is what `BindFlags` returns). Leaving `DBPath` on `Config` forced either an asymmetric `BindFlags(fs) (*RunOptions, *string)` signature or a post-parse copy step. Neither is clean.
  - Trade-off: `Config` becomes "pure domain description" (Factory + future Docstring); `RunOptions` becomes "everything the operator can tune at run time, including the state file location." That split feels right — the DB path is a runtime artifact, not a property of the task definition.
