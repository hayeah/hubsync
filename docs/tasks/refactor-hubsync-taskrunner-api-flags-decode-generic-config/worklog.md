---
status: done
section: Refactor hubsync taskrunner API: flags, Decode, generic Config
slug: refactor-hubsync-taskrunner-api-flags-decode-generic-config
mode: worktree
spec: spec.md
created: 2026-04-20T06:33:58Z
---

> ## Refactor hubsync taskrunner API: flags, Decode, generic Config
>
> ---
> status:
>   type: open
> ---
>
> Spin-out from the parent JSONL-docstring spec. Three API-shape questions about `~/github.com/hayeah/hubsync/internal/taskrunner/` need to be resolved together in one refactor pass so the parent spec's Phase 5 (taskrunner docstring template + JSONL prelude hooks) can land.
>
> Primary brief — read this first, it contains the full analysis, current code pointers, recommended shapes, and migration notes:
>
> - `/Users/me/Dropbox/boss/tasks/unify-ls-pattern-resume-db-task-runner-and-jsonl-hujson-output-convention/specs/taskrunner-api-open-questions.md`
> - Parent spec (for context on what Phase 5 needs): `../spec.md` in the same workspace.
>
> The three issues, in priority order:
>
> - **Issue 3 (blocker)** — `Main[T]` owns all flag parsing; user binaries can't add domain flags without abandoning `Main` entirely and re-implementing ~60 lines of context/signal/exec/exit boilerplate. Recommended direction: expose `BindFlags(fs *flag.FlagSet) *RunOptions` + `Run(ctx, cfg, opts) error`, keep `Main[T]` as sugar. Hard user constraint: taskrunner must NOT fully own the user binary's flag surface.
> - **Issue 2** — `Decode func(row map[string]any) (T, error)` is awkward; every consumer writes the same json.Marshal/Unmarshal round-trip and the real job is closure-bound dep injection. Recommended: rename to `Init func(T) error`, runner does the JSON round-trip.
> - **Issue 1** — Is `Config[T Task]` generic load-bearing? Net wash; recommend leave as-is unless Issue 3's refactor makes it natural to drop.
>
> Only consumer today is `hubsync/cmd/archive_worker.go` (verify); migration window is narrow. Do all three in one pass if Issues 1/2 end up in scope.
>
> Spec should land with: proposed final API surface (types + function signatures), a worked before/after call-site example for archive_worker, and a clear decision on each of the three issues with rationale. Implementation comes after the rfc gate.
>
> - [ ] rfc: review spec.md
> - [ ] implement per spec

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

### Phase 1 — rfc gate (blocked on human)

- [x] Check out hubsync repo; map current API + sole consumer (`cli/hubsync/archive.go`, `archive_task.go`, `archive_e2e_test.go`).
- [x] Draft initial `spec.md` (keep generic, rename Decode→Init, split flags per brief).
- [x] Pivot to non-generic `TaskFactory` interface after design review; rewrite `spec.md` around `List(ctx) iter.Seq2[Task, error]` + `Hydrate(ctx) (Task, error)`.
- [x] Confirm Go 1.25 in `go.mod` (iter.Seq2 available).
- [x] Wait for rfc tick on `rfc: review spec.md`. (ticked 2026-04-20T07:04Z per Boss log)

### Phase 2 — taskrunner package refactor (landed in 0b9a02b)

- [x] `task.go` — add `TaskFactory` interface (`List`, `Hydrate`).
- [x] `runner.go` — de-genericize `Config` / `Runner`; replace `Plan`+`Decode` with `Factory TaskFactory`; move `DBPath` → `RunOptions`.
- [x] `plan.go` — `inferItemSchema(sample any)`; cache on Runner; consume `iter.Seq2` in `runPlan` with yield-false short-circuit on errors.
- [x] `runner.go` — add `r.hydrate(ctx, row)` (Factory.Hydrate + json round-trip with `json.RawMessage` substitution for `isJSON` columns); swap into `processRow`.
- [x] `runner.go` — add `BindFlags`, `Run(ctx, cfg, opts) (RunResult, error)`, `RunResult`, `ErrFailedRemain`, `ErrSetup`.
- [x] `main.go` — rewrite `Main(cfg)` as ~15-line `BindFlags` + `Parse` + signal ctx + `Run` + exit-code shell.

### Phase 3 — consumer migration (landed in 0b9a02b)

- [x] `archive_task.go` — replace `PlanArchiveTasks` + `DecodeArchiveTask` with `ArchiveTaskFactory` struct + `List` + `Hydrate`; delete `asInt64`.
- [x] `cli/hubsync/archive.go` — switch to `BindFlags` + `Run`; preserve Usage; default path logic moves from `resume == ""` to `opts.DBPath == ""`.
- [x] `archive_e2e_test.go:221` — update call-site to non-generic `Config` / `Run`.
- [x] `runner_test.go` — rewrite `decoderFor` as `echoFactory` struct; existing tests ported.
- [x] `archive_test.go` (taskrunner internal) — ported to factory-based runner helper.

### Phase 4 — tests (landed in ec1fdab)

- [x] `runner_test.go` — rewrite `decoderFor` as an `echoFactory` struct implementing `TaskFactory`. Share state via struct fields. (landed in 0b9a02b)
- [x] Add `TestRun_BindFlagsAndExitCodesClean` + `TestRun_ErrFailedRemain` + `TestRun_ErrSetup` (all three Run error categories).
- [x] Add `TestHydrate_JSONColumn` (Task with `[]string` + nested-struct fields; guards `json.RawMessage` substitution for `isJSON` columns). **Bonus**: surfaced a latent column-name collision in `SELECT * FROM items JOIN tasks USING(id)` when the user Task has a field name matching a tasks-table column (e.g. `meta`). Fixed by switching to `SELECT items.*, tasks.attempts AS __tr_attempts` and stripping the sentinel before hand-off.
- [x] Add `TestList_ContextCancellation` (factory honors ctx.Err() mid-enumerate; runPlan returns ctx.Canceled).

### Phase 5 — verification

- [x] `go build ./...` clean.
- [x] `go test ./...` green (uses `archive.FakeStorage`; no live B2).
- [x] Smoke `hubsync archive --help` (flag surface matches pre-refactor) + captured before/after `wc -l` on consumer files.
- [x] Live-B2 integration run: `HUBSYNC_TEST_BUCKET=hayeah-hubsync-test godotenv -f ~/.env.secret go test -v ./archive/` — 11 tests pass against the real bucket incl. `TestB2StorageRoundTrip`.

## Agent log
- 2026-04-20T06:45Z — Read AGENT_LOOP, section text, parent brief (`taskrunner-api-open-questions.md`), parent spec. Checked out hubsync → worktree 001. Mapped real consumer: `cli/hubsync/archive.go` (not `archive_worker.go`, which is the non-taskrunner watch path — worth confirming since the brief mentions archive_worker.go by name).

- 2026-04-20T06:55Z — Drafted initial `spec.md` along the lines of the parent brief: Issue 3 = `BindFlags`+`Run`+`Main`-as-sugar; Issue 2 = rename `Decode`→`Init` + runner-owned JSON round-trip; Issue 1 = keep the generic. One call beyond the brief: moved `DBPath` from `Config` → `RunOptions` so `BindFlags` owns `--resume` cleanly.

- 2026-04-20T07:05Z — Design review: human asked to evaluate a non-generic, interface-focused API (`Task` interface with `Run`, `TaskFactory` that produces tasks, everything else reflection-based, json-marshaling assumed). Walked through the trade: the three uses of `T` today (typed emit, typed Init, compile-time Plan/Init-type agreement) all have cheap interface substitutes; the recurring cost of threading `[T]` through every future `Config` addition (starting with the parent spec's `Docstring`) is the bigger tax. Flipped my recommendation on Issue 1 from "keep" to "drop the generic."

- 2026-04-20T07:10Z — Design review, `TaskFactory` shape: human asked about iterator-style (`Next/Error/New`) instead of callback-style. Evaluated: iterator is workable and familiar (`sql.Rows` idiom), but `Next()` has a context gap — no ctx per call means long enumerations can't be cancelled mid-stream. Alternatives: `Next(ctx)` (ugly), `Start(ctx)` (stateful/fragile), eager-materialize (defeats streaming). Go 1.23 `iter.Seq2` is the sweet spot — ctx flows through, errors interleave with emits, runner short-circuits via yield-returning-false. Confirmed hubsync is on Go 1.25 (`go.mod`), so `iter.Seq2` is available.

- 2026-04-20T07:15Z — Design review, method naming: human proposed `Plan → List` and `New → Open(ctx)`, both ctx-aware.
  - `List` accepted: matches ls-pattern parent spec, plainer verb than `Plan`, standard CLI/API idiom.
  - `Open(ctx)` discussed: evocative, ctx-aware, but carries a "pairs with Close" expectation (os.Open, sql.Open, net.Dial convention). Task has no Close. Options were (a) add an optional `io.Closer` probe in the runner (real feature, scope-creep for this spec), (b) accept mild misdirection, (c) rename to sidestep.
  - Human picked **Hydrate(ctx)** (option c). Precise — "re-hydrate a Task from its persisted row" — and doesn't imply resource ownership discipline the API doesn't enforce.

- 2026-04-20T07:25Z — Rewrote `spec.md` end-to-end around the new shape:
  - `Task` interface (Run unchanged).
  - `TaskFactory` interface: `List(ctx) iter.Seq2[Task, error]` + `Hydrate(ctx) (Task, error)`.
  - Non-generic `Config{Factory TaskFactory}`, `Runner`, `Main`, `Run`.
  - `RunOptions` gains `DBPath`.
  - `BindFlags`, `Run`, `RunResult`, `ErrFailedRemain` as planned.
  - Schema inference runs once at Run preamble: `factory.Hydrate(ctx)` → sample Task → reflect on its type. Emit-type validation at bind time catches the one compile-time guarantee we lost from dropping the generic.
  - Added Phase 4 tests: `TestHydrate_JSONColumn` (regression gap for `isJSON` columns) and `TestList_ContextCancellation`.

- 2026-04-20T07:35Z — `status: blocked` pending rfc tick on `rfc: review spec.md`.

- 2026-04-20T07:50Z — Human directive: use `hayeah-hubsync-test` bucket for any live-B2 testing. Already the convention in `AGENTS.md:6,34` — live tests skip cleanly when `HUBSYNC_TEST_BUCKET` / `B2_APPLICATION_KEY_ID` / `B2_APPLICATION_KEY` aren't all set. Added to spec Verification section and Phase 5 todo. Saved as project memory so future sessions don't have to rediscover it.
- 2026-04-20T07:15Z refactor landed: non-generic Config, TaskFactory, BindFlags/Run, consumer migrated. go test ./... green. 0b9a02b
- 2026-04-20T07:18Z Phase 4 tests added + latent items/tasks column-collision fixed (SELECT items.*, tasks.attempts AS __tr_attempts). ec1fdab
- 2026-04-20T07:21Z Phase 5 verified: build clean, full suite + live-B2 all green. Consumer -48 LOC. status: done. 2 commits: 0b9a02b (refactor) ec1fdab (tests + collision fix).

## Boss log
- 2026-04-20T07:04Z rfc ticked — spec approved. proceed with implementation per spec.md.

## Evidence

### Commits

- `0b9a02b` — Refactor taskrunner API: non-generic Config with TaskFactory interface
- `ec1fdab` — Add Run/Hydrate/List tests; fix items/tasks column collision

### Build

```
$ go build ./...
(exit 0)
```

### Test suites

```
$ go test -count=1 ./...
ok  	github.com/hayeah/hubsync	10.318s
ok  	github.com/hayeah/hubsync/archive	0.433s
ok  	github.com/hayeah/hubsync/cli/hubsync	2.258s
ok  	github.com/hayeah/hubsync/internal/taskrunner	2.516s
```

### Phase 4 new tests (all pass)

```
$ go test -count=1 -run 'TestRun_|TestHydrate_|TestList_' -v ./internal/taskrunner/
=== RUN   TestRun_BindFlagsAndExitCodesClean
--- PASS: TestRun_BindFlagsAndExitCodesClean (0.10s)
=== RUN   TestRun_ErrFailedRemain
--- PASS: TestRun_ErrFailedRemain (0.10s)
=== RUN   TestRun_ErrSetup
--- PASS: TestRun_ErrSetup (0.00s)
=== RUN   TestHydrate_JSONColumn
--- PASS: TestHydrate_JSONColumn (0.10s)
=== RUN   TestList_ContextCancellation
--- PASS: TestList_ContextCancellation (0.00s)
PASS
```

### Live-B2 integration tier

```
$ HUBSYNC_TEST_BUCKET=hayeah-hubsync-test godotenv -f ~/.env.secret go test -v ./archive/
=== RUN   TestB2StorageRoundTrip
--- PASS: TestB2StorageRoundTrip (4.45s)
=== RUN   TestB2StorageHeadMissing
--- PASS: TestB2StorageHeadMissing (0.68s)
[... 9 additional fake-storage tests all pass ...]
PASS
ok  	github.com/hayeah/hubsync/archive	5.459s
```

### Consumer ergonomics (LOC)

```
                           before   after   delta
cli/hubsync/archive.go       153     123     -30
archive_task.go              136     118     -18
-------------------------------------------------
                             289     241     -48
```

### CLI surface unchanged

```
$ go run ./cli/hubsync archive --help
usage: hubsync archive [--resume <path>] [--dry] [--where "<sql>"] [--workers N]

Plans (if needed) + runs the archive task queue. The SQLite file at
--resume is the canonical artifact; every invocation is plan-if-empty
followed by a drain of the work queue, optionally filtered by --where.

Examples:
  hubsync archive                                       # full run
  hubsync archive --dry                                 # plan only
  hubsync archive --where "path LIKE '%.mov'"           # subset
  hubsync archive --where "attempts = 0"                # only never-tried
```

Flag names and semantics match pre-refactor — `BindFlags` binds the same eight flags with the same defaults.

### Public API summary

New surface in `internal/taskrunner`:

- `type Task interface { Run(ctx) (any, error) }` (unchanged)
- `type StatusReporter interface { Status() (any, error) }` (unchanged, probed via type assertion)
- `type TaskFactory interface { List(ctx) iter.Seq2[Task, error]; Hydrate(ctx) (Task, error) }` (new)
- `type Config struct { Factory TaskFactory }` (non-generic; was `Config[T Task]{DBPath, Plan, Decode}`)
- `type RunOptions struct { DBPath, Workers, Where, MaxAttempts, Stale, HeartbeatEvery, Dry, KeepDB }` (DBPath moved in)
- `type RunResult struct { Summary; ArchivedPath }` (new)
- `var ErrSetup, ErrFailedRemain` (new sentinels for errors.Is branching)
- `func BindFlags(fs *flag.FlagSet) *RunOptions` (new)
- `func Run(ctx, cfg, opts) (RunResult, error)` (new; the packaged path)
- `func New(ctx, cfg, opts) (*Runner, error)` (still exposed for tests/raw use)
- `func Main(cfg Config)` (~15-line shell, preserved for zero-custom-flag binaries)


## Trouble report

- **Consumer location correction**: the parent brief points at `hubsync/cmd/archive_worker.go` as the only taskrunner consumer. There's no `cmd/` directory; the file exists at the repo root as `archive_worker.go` but it's the *watch-mode* worker, which hand-rolls its own queue and **does not use taskrunner**. The actual taskrunner consumer is `cli/hubsync/archive.go`. Worth the correction in case the boss reviews the brief against the landed code.
- `cli/hubsync/archive.go` already doesn't use `taskrunner.Main` — it hand-rolls the whole wrapper (flag parsing + signal ctx + New + Execute + Summary + exit). That's the concrete ~60-line ergonomics tax the parent brief describes. Post-refactor, the file shrinks ~60 → ~30.
- `DecodeArchiveTask` in the landed code does manual `row["id"].(string)` field casts, not the JSON round-trip the parent brief assumes. Functionally equivalent — just a different flavor of boilerplate. Post-refactor the field-extraction goes away entirely (runner's JSON round-trip subsumes it); the dep-injection is all that remains.
- The runner's re-hydration path needs care with `isJSON` columns (slice/map/struct fields stored as TEXT-of-JSON). Naive `json.Marshal(row)` wraps the stored string as a JSON-string, which breaks on unmarshal into a structured field. Spec's `hydrate` substitutes `json.RawMessage` for isJSON columns. Today's test corpus has no JSON-typed column → the bug would land silently. Captured as `TestHydrate_JSONColumn` in Phase 4.
- Dropping the generic forfeits the compile-time "Plan and Hydrate agree on T" invariant. Replaced with runtime reflect check at bind time (`reflect.TypeOf(task) != schemaType`). Microscopic in practice but worth being explicit — the one real downgrade from the generic, flagged as Q6 in the spec for the human's rfc review.
- `iter.Seq2` requires Go 1.23+. Confirmed hubsync is on Go 1.25 per `go.mod`. Safe.
- **Column-name collision, surfaced by `TestHydrate_JSONColumn`**: the feed-query `SELECT * FROM items JOIN tasks USING(id)` returned both `items.meta` and `tasks.meta` (and similarly for any user Task field shadowing a tasks-table column: `status`, `attempts`, `started_at`, `ended_at`, `heartbeat_at`, `error`). Under `map[string]any{name: value}`, the later-scanned column silently overwrote the earlier one, so `tasks.meta` won over `items.meta` — the user's field came back as whatever the runner wrote into tasks.meta, not the persisted row body. Latent bug pre-existing; today's `ArchiveTask` doesn't shadow any reserved name so nobody tripped it. Fixed in `ec1fdab` by switching to `SELECT items.*, tasks.attempts AS __tr_attempts` and stripping the sentinel from the row map before hand-off. The `TestHydrate_JSONColumn` jsonCols struct uses `Meta` deliberately so this regression can't come back unnoticed.
- Added an internal sentinel `const attemptsColumn = "__tr_attempts"` to runner.go. User Tasks must not emit a JSON tag starting with `__tr_`. Documented inline; low risk since the prefix is intentionally ugly.
- No README update needed: the CLI flag surface for `hubsync archive` is unchanged (BindFlags rebinds the exact same eight flag names with the same defaults). The taskrunner is an internal package with no public user-facing docs today.
