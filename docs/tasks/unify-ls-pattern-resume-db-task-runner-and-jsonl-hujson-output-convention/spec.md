---
status: draft
slug: unify-ls-pattern-resume-db-task-runner-and-jsonl-hujson-output-convention
---

# JSONL docstring convention (ls-pattern addendum)

## Goal

Glue three in-flight threads into one coherent output convention:

1. **`ls` pattern** (`~/Dropbox/notes/2026-04-17/ls-pattern_claude.md`) — every `foocmd ls` emits JSONL, DuckDB handles filter/sort/aggregate. First real adopter: `hubsync ls` (see `~/github.com/hayeah/hubsync/cli/hubsync/main.go:385` + `rpc_ls_jsonl_test.go` + `rpc_ls_duckql_e2e_test.go`).
2. **Resume-DB task runner** at `~/github.com/hayeah/hubsync/internal/taskrunner/` (prior note `~/Dropbox/notes/2026-04-19/task-runner-duckdb-spec_claude.md` was drafted against DuckDB; the landed implementation is **SQLite**). Two-table plan+state (`items` + `tasks`, plus a `runs` history table), `Runner.Execute` with plan-if-empty, claim/heartbeat/stale-reclaim, exposes `Runner.Summary()` (pending/running/done/failed counts).
3. **duckql** (`~/github.com/hayeah/duckql`) — the standard consumer of JSONL on stdin. Already supports `sqlite:` attach, which is the natural hop from a task-runner DB into SQL.

The missing piece: JSONL is expressive and composable but **opaque on first contact**. A human or an LLM running `foocmd ls` for the first time sees a wall of objects with no hint of what the fields mean, what filters make sense, or (for resume-db tools) where the durable state lives. They shouldn't have to go read external docs.

**Proposal in one line**: every `foocmd ls`-style output grows a `//`-comment **docstring** at the top: short readme, example record, per-field docs, and — for resume-db-backed commands — the db path and sample queries. The body below the docstring is unchanged, standard JSONL. The docstring is **always present** (no flags, no TTY branching); `duckql` strips `//` lines transparently; strict-JSON consumers like `jq` gain a one-word prefilter (`grep -v '^//' | jq`).

Out of scope: rewriting the ls-pattern or task-runner specs themselves (they stand) — only the glue layer and a handful of additions to each.

## Architecture

### The wire format: JSONL with a `//` comment docstring

(Not a named format. We're not inventing `cjsonl` or registering anything — it's a convention on top of standard JSONL. Closest real prior art: **JSONC** (VS Code's "JSON with comments") and **hujson** (Tailscale); neither covers newline-delimited streams, but both establish `//` as the comment prefix readers already expect.)

The stream is a sequence of lines where:

- Lines beginning with `//` are **comments** — ignored by readers that know to skip them, syntax errors to strict JSON parsers.
- All other non-empty lines are **standard JSON objects** — each valid on its own against a strict parser.
- Blank lines are permitted anywhere and skipped.

Nothing else. No block comments, no trailing commas inside objects, no unquoted keys. Objects remain strict JSON so `grep -v '^//' | jq` works and existing JSONL writers don't need to change anything about their data lines.

```
// hubsync ls — hub tree entries (files + archive state)
// Columns:
//   path                text     hub-relative path (first key)
//   kind                text     file | dir
//   digest              text     content hash
//   size                int      bytes
//   archive_state       text     pinned | archived | unpinned
//   archive_file_id     text     remote handle (populated when archived)
//   updated_at          int      unix seconds
// Example:
//   {"path":"a.txt","kind":"file","digest":"…","size":5,"archive_state":"archived", …}
// Underlying DB: .hubsync/hub.db (SQLite, WAL-safe read alongside serve)
// Deeper queries:
//   hubsync ls | duckql "WHERE archive_state='unpinned'"
//   hubsync ls | duckql "SELECT archive_state, count(*) GROUP BY 1"
//   duckql -i 'sqlite:.hubsync/hub.db' "FROM hub_entry WHERE archive_state='unpinned'"

{"path":"a.txt","kind":"file","digest":"…","archive_state":"archived", …}
{"path":"dir/b.txt","kind":"file","digest":"…","archive_state":"unpinned", …}
...
```

Why not hujson / JSON5 proper? hujson allows comments *inside* JSON values (trailing commas, block comments around keys, etc.). That's strictly more than we need and would force every producer to depend on a hujson serializer. We only want **line comments at the top of the stream**, which is a trivial filter (`line.startswith("//")`) in any language.

### Docstring convention — where it goes and what's in it

Docstrings live at the **top of the stream** — a contiguous block of `//` lines before any data line. Scattering comments between data lines is permitted by the grammar but discouraged: many downstream tools (including duckql) skip comments cheaply only when they're before the first data line, and intermixed comments break `head -1 | jq` ergonomics.

Recommended sections, in order, each optional:

1. **Title line** — `// foocmd ls — list Foo resources`. One sentence.
2. **Columns** — `//   name  type  one-line description`. Fixed-column layout, not YAML; grep-friendly.
3. **Example** — one example record, pretty-printed inline or as-is.
4. **Resume DB** (if applicable) — `// DB: ./foo.db  tables: items, tasks`.
5. **Sample queries** — 3-5 `// foocmd ls | duckql "..."` lines.

These are *conventions*, not a wire schema. Tools MAY generate them from reflection (Go struct tags, Python dataclass fields); tools MAY hand-write them. Consumers MUST NOT assume presence or structure — they either render the docstring verbatim (humans do) or strip it (machines do). No registry, no JSON-Schema, no sidecar file.

Why free-form prose instead of an embedded hujson struct? The primary audience is a person or an LLM reading the output directly. Prose lines render legibly in any terminal, grep cleanly, and don't require a parser. A structured sidecar block (`// { "schema": {...} }`) would require every consumer to parse it or ignore it — more surface for no real benefit.

### No flags, no TTY branching for the docstring

The docstring is **always emitted** on non-TTY stdout. There's no `--strict`, `--no-docstring`, or `--annotate`. Reasoning:

- **File redirection must preserve the docstring.** `foocmd ls > snapshot.jsonl` should produce a self-describing file six months from now. That's only true if the docstring doesn't depend on isatty state — a TTY-only docstring would vanish the moment you redirected.
- **Stdout, not stderr, for the same reason.** If the docstring lived on stderr it'd be lost to `> file`, lost to some agent harnesses that drop stderr, and schizophrenic under `2>&1`. Stdout is the one surface every caller treats the same.
- **Fewer flags.** Every producer implementing this would have to agree on flag names, semantics, and precedence. One convention ("the docstring is always there, strippers strip it") is cheaper to teach than a matrix.

The one orthogonal toggle already present in the ls-pattern convention (TTY-auto-table vs JSONL) stays: on a TTY, render a table; on anything-else (pipe, file, `/dev/null`), emit JSONL-with-docstring. Both branches of that switch remain self-describing — the TTY table has column headers; the JSONL-with-docstring has the `//` prose.

**jq workaround** (for the minority case where a user pipes straight to `jq` without duckql in between):

```bash
foocmd ls | grep -v '^//' | jq .
# or equivalently, use duckql as the canonical consumer and never think about it:
foocmd ls | duckql "WHERE ..."
```

We are explicitly accepting the one-time breakage cost for existing `foocmd ls | jq` shell history in exchange for the discoverability win. The breakage is loud (jq errors on the first `//` line with a clear message) and the fix is one `grep`.

### duckql integration

duckql already spools stdin and reads it via DuckDB's `read_json(..., format='nd')`. DuckDB's reader **rejects comment lines** out of the box, so we add a cheap preprocessing step:

- When stdin format is jsonl (or sniffed to be jsonl), duckql reads stdin into its spool tempfile line-by-line and **drops every `//` comment line** before DuckDB sees the file. One linear scan, no parsing.
- Symmetric on file inputs: `-i foo.jsonl` also strips `//` lines, so `duckql -i snapshot.jsonl "..."` on a saved self-describing file Just Works.
- Add `--no-strip-comments` to opt out (paranoid / exact-byte preservation).
- **Pretty-printing** stays where it already is — duckql's YAML multi-doc output (default for pipe) and table render (default for TTY). The producer does not emit a "pretty" mode. Users who want pretty: `foocmd ls | duckql`.

Since the docstring is always present and producers offer no way to suppress it, duckql's auto-strip is the path of least friction for the common case. The `grep -v '^//' | jq` escape hatch exists for anyone reaching for jq directly.

### Task-runner integration — **deferred**

Out of scope for this spec. Three taskrunner API issues surfaced while designing the docstring hook (flag-parsing ownership, `Config`'s generic parameter, `Decode`'s `map[string]any` shape). They're captured in `specs/taskrunner-api-open-questions.md` for a separate agent hand-off.

This spec proceeds without the taskrunner changes. Once those API issues are resolved, a follow-up spec will add:

- A docstring hook on `Config` (Go `text/template` rendered against runtime values: db path, summary counts, items/tasks columns, sample queries, archived-path).
- Emission points inside the runner's run-loop: dry-run, drained-exit, boot-banner.

Until then, the docstring convention is exercised end-to-end via `hubsync ls` (Phase 4) — hand-written prose, no templating, zero changes to the taskrunner package. That's enough to prove the wire format, get duckql's auto-strip shipped, and give other producers a concrete pattern to copy.

### Where the convention lives

- **Skill doc**: extend the existing `duckql` SKILL.md with a "JSONL comment docstring" subsection, and extend `ls-pattern_claude.md` in place with the same convention. No dedicated library yet, no new skill.
- **Library helpers**: only if ≥3 tools adopt the docstring. Candidate shape:
  - Go: `hayeah-go/lsfmt.Docstring{Title, Columns, Example, DBPath, SampleQueries}` with a `Writer(io.Writer).WriteDocstring(...)` method.
  - Python: `hayeah.core.lsfmt.docstring(title=..., columns=[...], example=..., db_path=..., queries=[...])` returning the comment block as a string.
  - TypeScript: parallel. Each is ~30 LOC. Defer.
- **duckql** code change lives in `~/github.com/hayeah/duckql/src/duckql/cli.py` in the stdin-spool step.

## Steps

### Phase 1 — spec + rfc gate

- [x] Draft `spec.md` (this document).
- [ ] Human reviews; ticks the `rfc: review spec.md` box in BOSS.md. (Out of my hands; I park at `status: blocked` until then.)

### Phase 2 — duckql docstring-stripping

- [ ] Implement `_strip_jsonl_comments` in the stdin-spool path of `src/duckql/cli.py`. Drop lines matching `^//` (after optional leading whitespace). Keep `--no-strip-comments` for the paranoid case.
- [ ] Unit test: fixture file with a docstring + 3 data rows; assert DuckDB sees only the 3 rows.
- [ ] Unit test: `--no-strip-comments` fails as expected (DuckDB parse error on the docstring).
- [ ] Update duckql SKILL.md with a "Commented JSONL input" subsection.

### Phase 3 — shared convention doc

- [ ] Append the "JSONL comment docstring" convention to the existing ls-pattern note + duckql SKILL.md. Covers: wire grammar (`//`-line comments, strict JSON body), docstring sections (title/columns/example/db-path/sample queries), consumer contract (duckql auto-strips, jq users `grep -v '^//'`). No new skill file.
- [ ] Link the new doc from the ls-pattern note and from duckql's SKILL.md.

### Phase 4 — hubsync ls docstring (first producer adoption)

Concrete pilot is **hubsync** because `cli/hubsync/main.go:385` (`cmdLs`) + `cli/hubsync/render.go:42` (`renderLs`) already emits JSONL, and `rpc_ls_duckql_e2e_test.go` already proves the duckql round-trip. Adding a docstring is a ~30-line change on a known-working baseline.

- [ ] Extend `renderLs` (or a sibling `renderLsWithDocstring`) to emit a `//`-comment docstring derived from the `HubEntry` struct's JSON tags + a short hand-written example + the SQLite-attach sample queries.
- [ ] (No producer-side flag for strict mode — jq users prefilter with `grep -v '^//'`, per the resolved Q1/Q2. Call this out explicitly in the `hubsync ls --help` text so the workaround is discoverable.)
- [ ] Extend the existing e2e test (`rpc_ls_duckql_e2e_test.go`) to cover "docstring present → duckql strips → row count unchanged".
- [ ] Flip the test's current `enc.Encode` loop over to the new `renderLsWithDocstring` so we test what we ship.

### (Phase 5 removed — taskrunner integration deferred to `specs/taskrunner-api-open-questions.md`.)

Phases 2 and 3 can ship independently. Phase 4 is gated on Phase 2 (so consumers have duckql's auto-strip available, and we can flip the e2e test over).

## Verification

Once Phase 2 lands:

```bash
cd ~/github.com/hayeah/duckql
uv run pytest -k docstring  # new tests pass
# manual smoke:
printf '// title\n// example\n{"a":1}\n{"a":2}\n' | duckql "SELECT count(*) AS n"
# expected: n = 2 (docstring stripped)
printf '// title\n{"a":1}\n' | duckql --no-strip-comments
# expected: DuckDB parse error on the // line
```

Once Phase 4 lands:

```bash
hubsync ls | head -20                                   # docstring visible, readable
hubsync ls | duckql "WHERE archive_state='unpinned'"    # works end-to-end
hubsync ls | grep -v '^//' | jq .                       # jq path works via grep prefilter
hubsync ls > snapshot.jsonl && head snapshot.jsonl      # file is self-describing
cd ~/github.com/hayeah/hubsync && go test -run TestLsDuckqlRoundTrip ./...
# existing e2e still green after the docstring flip
```

## Open questions

- **Q1 (resolved 2026-04-20, user): Always on. No `--strict` / `--annotate` / `--no-docstring` flag.** File-redirection would lose a TTY-gated docstring and flag proliferation isn't worth it. Docstring is unconditional; jq users `grep -v '^//'`; the common path goes through duckql's auto-strip.

- **Q2 (resolved 2026-04-20, same reasoning): Stdout, inline.** Stderr-docstring also fails the file-redirection test. Stdout is the one guaranteed-preserved surface.

- **Q5 (resolved 2026-04-20, user): No length cap.** Docstring is a readme for the output; 200 lines is fine when it's warranted. Not documenting a cap at all.

- **Q3 (resolved 2026-04-20, user): `//` only.** No `#` alternative, no bikeshed. Matches JSONC/hujson, doesn't collide with JSON string content.

- **Q4 (resolved 2026-04-20, user): No structured markers.** Everything in the docstring is prose. No `// @columns:`, no `// {"schema":...}`, no machine-parseable keys. If a future tool needs to extract column specs from arbitrary producers, it scrapes heuristically or we revisit — but we don't pre-commit to a syntax now.

All open questions closed. Spec is ready for the rfc review.

## Design notes

- 2026-04-20T05:55Z — Picked "commented JSONL" over "adopt hujson wholesale" as the wire format.
  - Alternatives considered:
    - **Full hujson** (Tailscale's format — line comments, block comments, trailing commas, comments inside objects): strictly more expressive, but every producer would need a hujson-capable serializer. Our actual need is *only* top-of-stream comments — data lines stay strict JSON. No reason to pay the library-dependency cost.
    - **JSON5**: similar to hujson but with unquoted keys + single quotes. Same objection, plus less well-known outside web tooling.
    - **YAML multi-doc with `---` separators + `#` comments** (what duckql already emits for pretty): great for humans, but the `ls` pattern chose JSONL explicitly because JSONL → DuckDB is the most direct path. Producers staying in JSONL keeps the pipe shape identical.
    - **Sidecar schema file** (`foocmd ls --schema` emits the docstring separately): cleanly separates docs from data but defeats first-contact discoverability — the whole point is that running the *primary* command surfaces the docs.
    - **Picked: JSONL + `//` docstring, no new format name.** Grammar is a one-line filter; producers unchanged below the docstring; consumers strip `^//` in four lines of any language. The narrow scope is the feature. Not registering a new format name (`cjsonl` etc.) — it's a convention, not a standard, and we use existing prior-art terminology (JSONC, hujson) as pointers rather than claiming a new one.

- 2026-04-20T06:00Z — Chose stdout-inline for the docstring over stderr.
  - Why: the LLM-agent discoverability story is load-bearing. Agent harnesses have inconsistent stderr handling (some merge, some drop, some label it weirdly), and we can't rely on it. Stdout is the one surface every harness treats the same. Also: `foocmd ls > file.jsonl` producing a self-describing file is a nice invariant we'd lose if the docstring went to stderr.
  - Trade-off: breaks `foocmd ls | jq` until users adopt `--strict` or pipe through duckql. Mitigation: `--strict` flag exists on day one; duckql (our canonical consumer) strips automatically.
  - Flagged as Q2 because this is the most-reversible-after-adoption decision and I want the human to confirm before we ship.

- 2026-04-20T06:05Z — Resolved the "pretty-print mode" open question by punting pretty entirely to duckql.
  - Why: duckql already has YAML-multi-doc (pipe default) and box-drawn table (TTY default) output modes. Duplicating "pretty" on the producer side would mean two mutually-inconsistent renderings of the same data. Every producer also gets more complex. Keep producer = JSONL+docstring (pipe) or table-on-TTY; keep all other rendering at the duckql layer. This also preserves the "JSONL is the wire" invariant from the ls-pattern note.

- 2026-04-20T06:55Z — Resolved Q1/Q2/Q5 with the user: docstring is **always on**, no flags, no TTY branching, stdout inline, no length cap.
  - Load-bearing reason: **file-redirection must preserve the docstring**. A TTY-gated docstring or stderr-only docstring both vanish under `foocmd ls > out.jsonl`, which breaks the "self-describing artifact" property that motivates the whole design.
  - User also pushed back on flag proliferation (`--strict`, `--annotate`, `--no-docstring`, `--jsonl`). Killed all of them. One convention, one wire shape.
  - Consequence: strict-jq users have to `grep -v '^//' | jq` manually (or, more commonly, use duckql which strips transparently). Accepted as a one-time breakage against existing `foo ls | jq` shell history in exchange for the always-discoverability property. Breakage is loud (jq errors on the first `//` line) and the fix is four words.
  - Consequence: the duckql auto-strip in Phase 2 is now the primary user-facing ergonomic. Without it, every pipeline would need the manual grep. This raises Phase 2's priority — it should land first, before Phase 4 flips hubsync's emitter over.
  - No length cap on the docstring (Q5): "it's just a readme for the output." Producers that emit 200-line docstrings when 200 lines are warranted are fine. Removed the earlier "50 lines / 5 KB" soft-cap suggestion from the spec.
  - Also dropped the invented name `cjsonl`. It's not a real format and pretending it is just invites questions like "where is the spec registered?" Using "JSONL with a `//`-comment docstring" throughout; JSONC and hujson are cited as prior-art pointers.

- 2026-04-20T06:32Z — Re-read the landed taskrunner at `~/github.com/hayeah/hubsync/internal/taskrunner/`. Corrections to prior draft:
  - The Apr 19 spec doc is titled "task-runner-duckdb-spec" but the landed implementation is **SQLite**, not DuckDB. `schema.go` uses `sqlite_master` + `pragma_table_info`; `runner.go` uses `database/sql` with SQLite semantics. Sample queries in the docstring switch to `duckql -i 'sqlite:foo.db'` (already supported per duckql SKILL.md "Querying SQLite"). No `duckdb:` scheme anywhere in the convention.
  - `Runner.Summary()` (`runner.go:191-204`) captures `{Pending, Running, Done, Failed}` at end of `Execute`. That's the shape the docstring's "Totals:" line reports. For `taskls` (read-only, not running Execute), we need a sibling `taskrunner.Inspect(dbPath)` helper — small addition, pure SELECT. Added as Phase 5's first sub-task.
  - Archive-on-success rename (`--keep-db` off by default; see `main.go:29` and `Runner.ArchivedPath()` at `runner.go:151`) means `taskls` pointed at the canonical post-drain path will see ENOENT. Added the "canonical DB archived; showing <archived-path>" docstring line so this doesn't look like a bug.
  - Tables are `items`, `tasks`, **`runs`** (the third table is a per-invocation history row, `schema.go:24-30`). Docstring's "Tables:" line lists all three.

- 2026-04-20T06:40Z — Picked hubsync as the Phase 4 pilot over agentboss/devport/mdnote.
  - Why: `hubsync ls` is already an ls-pattern adopter (`cli/hubsync/main.go:385` + `cli/hubsync/render.go:42`) and already has an end-to-end duckql round-trip test (`rpc_ls_duckql_e2e_test.go`). Adding a docstring is a ~30-line delta on a known-working baseline, and the existing test gives us an oracle for "did we break JSONL-through-duckql?" The other candidates would each need their ls-pattern adoption written from scratch *and* the docstring on top — more surface, worse signal.
  - Trade-off: hubsync is also the first *consumer* of the taskrunner (presumably via `archive_worker.go` etc.), so both Phases 4 and 5 touch the same repo. Keeps the boss-loop single-worktree. Only risk is if hubsync's own release timeline wants to gate on the docstring — noting so we don't trap their CI. If so, land the duckql auto-strip in Phase 2 first and hubsync can flip on its own schedule.

- 2026-04-20T06:08Z — On the task-runner "empty ls" pain: fixed by making the docstring's summary line unconditional.
  - Even on a queue where every task is `done`, `taskls` will emit a docstring whose last line is `// Totals: 0 pending, 0 running, 0 failed, 452 done (452 total)` and zero data rows. The output is no longer "empty" — it's a self-describing nil.
  - This is why the summary belongs in the docstring rather than as a special first data row: a data row would force every downstream consumer to know about it. A comment is invisible to strict JSONL consumers and visible to humans, which is exactly right.
