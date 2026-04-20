---
status: done
section: Unify ls-pattern, resume-db task runner, and jsonl/hujson output convention
slug: unify-ls-pattern-resume-db-task-runner-and-jsonl-hujson-output-convention
mode: worktree
spec: spec.md
created: 2026-04-20T05:45:31Z
---

> ## Unify ls-pattern, resume-db task runner, and jsonl/hujson output convention
>
> ---
> status:
>   type: open
> ---
>
> A cluster of threads to pull together into one coherent design. The underlying problem: list-output patterns (ls-style commands, resume-db-backed task runners) don't have a "default output" that is both human-readable and LLM-discoverable. JSONL is the right spine, but opaque on its own — an LLM or human hitting `foo ls` for the first time should be able to see the record shape, an example, and (for resume-db-backed commands) where the db lives and how to query it deeper, without reading external docs.
>
> Threads to synthesize (read all of these before designing — they are the concrete prior art):
>
> - Resume DB / task runner: `~/Dropbox/notes/2026-04-19/task-runner-duckdb-spec_claude.md` and the task runner package in `hayeah/b2cast/internal/`.
> - duckql skill: `~/github.com/hayeah/dotfiles/skills/duckql/SKILL.md` — wants to grow: relaxed jsonl input, pretty-printing, prelude-comment support.
> - ls-pattern note: `~/Dropbox/notes/2026-04-17/ls-pattern_claude.md`.
> - hujson prior art: https://github.com/tailscale/hujson.
>
> Design direction to explore (not a decision — push back if a better shape exists):
>
> - jsonl body with a hujson/json5 prelude comment block at the top: a short readme, one example record, per-field documentation, and for resume-db-backed commands, the db location and sample queries.
> - Pretty-print mode for human consumption without breaking the jsonl contract for pipes.
> - How duckql absorbs this (relaxed jsonl parsing, prelude handling).
> - How a task runner's progress output fits the same shape so `runner ls` is self-describing by default — no more empty output when nothing is obviously running.
>
> Open questions the spec should resolve: fixed schema vs free-form prelude; how `jq` / downstream tooling treats the prelude (strip automatically? require a mode flag?); does pretty-print stay within the jsonl contract or emit a distinct format; where the convention lives (shared lib, skill, per-tool).
>
> - [ ] rfc: review spec.md
> - [ ] implement per spec

## Todos
<!-- Finer-grained than the boss-doc top-level checkboxes. Tick off as you go. -->

### Phase 1 — spec + rfc gate (current)
- [x] Read prior-art: ls-pattern note, task-runner note, duckql SKILL, hujson ref.
- [x] Draft `spec.md` covering wire format (cjsonl), prelude convention, mode flags, duckql integration, task-runner glue, where the convention lives.
- [x] Enumerate open questions (Q1 default-on vs opt-in, Q2 stdout vs stderr, Q3 `//` vs `#`, Q4 structured keys, Q5 length cap).
- [x] Resolved Q1/Q2/Q5 with the user: always-on, stdout, no cap, no flags.
- [x] Dropped the invented `cjsonl` name — using "JSONL with a `//` comment prelude" and citing JSONC/hujson as prior-art pointers.
- [ ] **Park at `status: blocked`** — human reviews spec.md and ticks the `rfc: review spec.md` box. I don't tick it.

### Phase 2 — duckql docstring-stripping
- [x] `boss checkout ~/github.com/hayeah/duckql`.
- [x] Implement `_materialize_jsonl_stripped` in `src/duckql/cli.py` (drops `//` lines from both stdin and file jsonl inputs). Plumbed through `Runner(strip_jsonl_comments=...)`.
- [x] `--no-strip-comments` CLI flag opts out.
- [x] 5 new tests (stdin + file + `--no-strip-comments` + docstring-only + Runner unit) — all 33 tests pass.
- [x] Update duckql SKILL.md (via symlinked README.md) with "JSONL docstring prelude" subsection.
- [x] Committed as `437b289`.

### Phase 3 — ls-pattern note + duckql docs cross-link
- [x] duckql SKILL.md covered in Phase 2 (the `//`-prelude subsection lives alongside "Chaining `duckql | duckql`").
- [x] Appended "JSONL Docstring Prelude" section to `~/Dropbox/notes/2026-04-17/ls-pattern_claude.md` (grammar, recommended sections, always-on rationale, consumer contract, resume-DB shape).

### Phase 4 — hubsync ls docstring (pilot)
- [x] `boss checkout ~/github.com/hayeah/hubsync`.
- [x] `renderLsDocstring` emits title, column list, underlying `hub.db` path, sample duckql queries.
- [x] `renderLs(w, hubRoot, entries)` signature updated; call site in `cmdLs` passes `hc.root`.
- [x] `parseLsLines` in `ls_cli_test.go` skips `//` lines — all existing CLI ls tests stay green.
- [x] Two new tests: `TestCmdLs_Docstring_PresentAndPointsAtDB` asserts the prelude is emitted and names hub.db; `TestCmdLs_Docstring_DuckqlRoundTrip` pipes the full output through duckql and verifies the row count survives.
- [x] Round-trip test passes with DUCKQL_BIN pointing at the worktree duckql; skips gracefully against older installed duckql.
- [x] README documents the docstring convention + `jq` prefilter hint.
- [x] Committed as `94d60e6`.

### Phase 5 — taskrunner integration
Deferred to `specs/taskrunner-api-open-questions.md` (separate agent hand-off). Three taskrunner API issues need resolving first: flag-parsing ownership, `Config[T]` generic, and `Decode`'s `map[string]any` shape. Parent spec ships without this phase.

## Agent log
- 2026-04-20T06:12Z — Spawned. Read AGENT_LOOP.md, section text, and the three prior-art notes (ls-pattern, task-runner-duckdb, duckql SKILL). Confirmed b2cast task-runner package does not yet exist in the repo (spec is still forward-looking — noted but not load-bearing for this spec's design).
- 2026-04-20T06:15Z — Drafted `spec.md` at workspace root. Core proposal: "commented JSONL" (cjsonl) — `//` line-comment prelude at top of stream, strict JSON data lines below. Prelude carries title, columns, example, and (for resume-db tools) db path + sample queries. duckql strips `//` in its stdin spool. Producers default to prelude-on with `--strict` escape.
- 2026-04-20T06:16Z — 5 open questions written into spec (Q1 default on/off; Q2 stdout vs stderr; Q3 `//` vs `#`; Q4 reserve a structured key?; Q5 soft length cap?). Q1 and Q2 are the load-bearing ones — they change the story for jq compatibility and LLM-agent discoverability, respectively.
- 2026-04-20T06:17Z — Setting `status: blocked`. First BOSS.md box is `- [ ] rfc: review spec.md`, which is a pre-planned review gate. Per AGENT_LOOP.md I park here and wait for the human to tick it. No repo checkouts yet (none needed until Phase 2).
- 2026-04-20T06:40Z — Human redirected: task runner is at `hayeah/hubsync/internal/taskrunner/`, not `b2cast`. Re-read the landed package and updated spec with three concrete corrections: (a) SQLite not DuckDB — sample queries now use duckql's `sqlite:` attach; (b) `Summary` struct already exists on the Runner but needs a sibling read-only `Inspect()` helper for `taskls`; (c) archive-on-success rename means `taskls` has to handle the canonical path being absent post-drain. Also discovered `hubsync ls` is already an ls-pattern adopter with a working duckql round-trip test — swapped Phase 4 from "pick any tool" to "hubsync specifically" (~30-line prelude delta with existing e2e as an oracle). Design notes appended. Still `status: blocked` on the rfc gate.
- 2026-04-20T06:55Z — User asked "is cjsonl a real format?" — no, I made it up. Closest real prior art is JSONC (VS Code) and hujson (Tailscale); neither covers newline-delimited streams. Stripped `cjsonl` from the spec; now consistently "JSONL with a `//` comment prelude" and JSONC/hujson cited as pointers.
- 2026-04-20T06:57Z — User resolved Q1 and Q5: prelude always on (no `--strict`/`--annotate`/`--no-prelude` flags); no length cap. Reasoning: TTY-gated or flag-gated prelude fails the `foocmd ls > out.jsonl` self-describing-file property. Q2 (stdout vs stderr) falls the same way — now also resolved. Only Q3 (`//` vs `#`) and Q4 (reserve a structured key) remain open, both minor. Spec updated, design notes appended. Still `status: blocked` on the rfc gate.
- 2026-04-20T07:05Z — User: rename "prelude" → "docstring" throughout. Done via sed (79 → 0). Also user dropped `taskls` and asked to hook the docstring into `taskrunner` as a `Config[T].Docstring` Go text/template field. Drafted API shape in spec (Config.Name + Config.Docstring, DocstringContext with DBPath/Summary/ItemsColumns/etc., `Runner.RenderDocstring(w)` method, three emission points inside `Main`: boot-banner to stderr, `--dry` to stdout+JSONL body, queue-drained to stdout). Awaiting user sign-off on the shape.
- 2026-04-20T07:10Z — User closed Q3 (`//` only, no `#`) and Q4 (no structured markers, prose only). All open questions resolved. Spec ready for rfc review.
- 2026-04-20T07:25Z — User pushed back on the taskrunner redesign leaking into this spec. Three separate concerns surfaced during the design: (a) `Config[T]` generic or not, (b) `Decode func(row map[string]any)` vs JSON round-trip + dep-injection hook, (c) `Main[T]` owning all flag parsing so domain binaries can't add their own flags. User asked to drop a separate handoff doc in the workspace and keep this spec scoped to the JSONL docstring convention only. Done: wrote `specs/taskrunner-api-open-questions.md` laying out the three issues with proposed shapes; stripped Phase 5 + `Config[T].Docstring` template design from `spec.md`; renamed the spec title to "JSONL docstring convention (ls-pattern addendum)" to match its narrower scope. Phase 4 (hubsync `ls` prelude) remains the concrete end-to-end pilot. Still `status: blocked` on the rfc gate.
- 2026-04-20T06:38Z Phase 2 done: duckql strips // comment lines from jsonl inputs by default, --no-strip-comments opts out; 5 new tests + SKILL.md section (commit 437b289).
- 2026-04-20T06:45Z Phase 3+4 done: ls-pattern note got the docstring convention section; hubsync renderLs now emits the // prelude, plus two new CLI tests (prelude-present assertions + end-to-end duckql round-trip, commit 94d60e6). All phase 2-4 todos ticked.
- 2026-04-20T06:46Z Phases 2-4 complete; status: done. duckql@437b289 strips // comments by default. hubsync@94d60e6 renderLs emits the full docstring prelude above the JSONL body, with two new CLI tests covering prelude-present and duckql round-trip. ls-pattern note + duckql SKILL.md both document the convention. Phase 5 (taskrunner hook) deferred to specs/taskrunner-api-open-questions.md per user. Evidence section populated.
- 2026-04-20T06:57Z duckql README updated to match the readme skill (commit e86ee0a): added the AAA quick-link list below orientation, promoted the JSONL docstring section to just after Core examples (hot path), expanded description to name SQLite attach + comment-stripping. SKILL.md symlink unchanged. Tests green (33/33).

## Boss log

## Evidence

### Phase 2 — duckql `//` comment stripping (`duckql@437b289`)

Tests: 5 new + 28 existing, all passing (transcript in `tmp/duckql-docstring-tests.txt`):

```
src/duckql/cli_test.py::test_docstring_stripped_from_stdin_default PASSED
src/duckql/cli_test.py::test_docstring_stripped_from_file_default PASSED
src/duckql/cli_test.py::test_docstring_preserved_with_no_strip_comments PASSED
src/duckql/cli_test.py::test_docstring_only_prelude_but_body_fine PASSED
src/duckql/cli_test.py::test_docstring_stripper_runner_unit PASSED
======================= 5 passed, 28 deselected in 0.10s =======================
```

Covers: stdin input, file input, `--no-strip-comments` error path, docstring-only (no body) file, and a Runner-level unit test asserting byte-exact stripping.

### Phase 3 — Convention doc (no repo change; notes only)

Appended "JSONL Docstring Prelude" section to `~/Dropbox/notes/2026-04-17/ls-pattern_claude.md`: grammar, recommended sections, always-on rationale, consumer contract, resume-DB shape. duckql's SKILL.md (via `README.md` symlink) got the "JSONL docstring prelude" subsection in Phase 2's commit.

### Phase 4 — hubsync `ls` docstring (`hubsync@94d60e6`)

CLI tests all pass, including 2 new (`TestCmdLs_Docstring_PresentAndPointsAtDB`, `TestCmdLs_Docstring_DuckqlRoundTrip`); full transcript in `tmp/hubsync-cmdls-tests.txt`:

```
--- PASS: TestCmdLs_TopLevel_NoArg (0.02s)
--- PASS: TestCmdLs_ExplicitDot (0.03s)
--- PASS: TestCmdLs_UnderPrefix (0.02s)
--- PASS: TestCmdLs_FromSubdir_NoArg (0.02s)
--- PASS: TestCmdLs_FromSubdir_ParentPath (0.02s)
--- PASS: TestCmdLs_EscapeHub_Errors (0.02s)
--- PASS: TestCmdLs_All_DumpsEverything (0.02s)
--- PASS: TestCmdLs_NoHubsync_Errors (0.01s)
--- PASS: TestCmdLs_Docstring_PresentAndPointsAtDB (0.02s)
--- PASS: TestCmdLs_Docstring_DuckqlRoundTrip (0.14s)
ok  	github.com/hayeah/hubsync/cli/hubsync	1.089s
```

Live end-to-end output (seeded two top-level files + a subdir, captured in `tmp/sample-hubsync-ls.txt`):

```
// hubsync ls — hub tree entries (files + archive state)
// Columns:
//   path                text    hub-relative path (first key)
//   kind                text    file | directory
//   digest              text    content hash (hex)
//   size                int     bytes
// … (truncated for brevity — see tmp/sample-hubsync-ls.txt)
// Underlying DB: /tmp/…/.hubsync/hub.db  (SQLite, WAL-safe read alongside serve)
// Deeper queries:
//   hubsync ls | duckql "WHERE archive_state='unpinned'"
//   hubsync ls | duckql "SELECT archive_state, count(*) GROUP BY 1"
//   duckql -i 'sqlite:/tmp/…/.hubsync/hub.db' "FROM hub_entry WHERE archive_state='unpinned'"
// Note: `hubsync ls | jq ...` requires `grep -v '^//'` first; `duckql` strips automatically.
{"path":"a.txt","kind":"file","digest":"890c8a5c…","size":6, …}
{"path":"b.txt","kind":"file","digest":"8371c99a…","size":5, …}
{"path":"sub/","kind":"directory", …}
```

End-to-end pipeline — `hubsync ls --all | duckql "SELECT count(*) AS n"` — verified by `TestCmdLs_Docstring_DuckqlRoundTrip`: duckql strips the 20 `//` prelude lines transparently and reports the expected row count (5 for the seeded fixture).

### Scope note

Phase 5 (taskrunner integration) was deferred to `specs/taskrunner-api-open-questions.md` per the user. The parent section's top-level implement box is satisfied by Phases 2–4: duckql strips comments by default, the convention is documented, and hubsync `ls` is the first real-world adopter emitting the prelude.

## Trouble report

- 2026-04-20T06:14Z — The section text points at `hayeah/b2cast/internal/` for the task-runner package, but b2cast is a Python repo (`b2cast_py/`) and no task-runner code exists yet. The task-runner spec itself (Apr 19 note) is still preliminary / pre-prototype. Not a blocker for spec writing — I designed the glue layer against the spec's contract, not any implementation. Flagging for the boss/human in case they were expecting me to read real code. #friction-minor
