# AGENTS.md — testing process

## TL;DR

- `go test ./...` — unit + e2e tests. No network. Safe to run anywhere. Current baseline runtime ~10s.
- `HUBSYNC_TEST_BUCKET=hayeah-hubsync-test godotenv -f ~/.env.secret go test -v ./archive/` — adds the live-B2 integration tests.
- `go vet ./...` + `go build ./...` — type-check; expected to be clean.
- `wire gen ./...` — regenerate `wire_gen.go` after touching `providers.go` or any `ProvideXxx`. Keep the regen in the same commit.

## Test tiers

### Tier 1: unit + in-process e2e (no network)

```bash
go test ./...
```

What runs:

- `store_test.go` — HubStore schema, Append txn, TreeSnapshot, config cache.
- `scanner_test.go` — digest cache, diff (create/update/delete ordering).
- `client_store_test.go` — client hub_tree upserts + ApplyChange.
- `reconciler_test.go` — pin/unpin planner over all 5 starting states + full pin→unpin→re-pin cycle against `archive.FakeStorage`.
- `archive_worker_test.go` — baseline drain + broadcaster pickup + already-archived idempotency.
- `rpc_test.go` — pin / unpin / ls / status over unix socket (JSON). Uses `/tmp` sockets because macOS sun_path is 104 bytes.
- `e2e_test.go` — hub↔Go-client: subscribe stream, snapshot bootstrap, catchup, push conflicts, delta.
- `archive/fake_test.go` — FakeStorage upload/head/download/presign round-trip.

Skips (no creds): `archive/b2store_integration_test.go`.

### Tier 2: live B2 integration

```bash
HUBSYNC_TEST_BUCKET=hayeah-hubsync-test \
  godotenv -f ~/.env.secret \
  go test -v ./archive/
```

What runs:

- `archive/b2store_integration_test.go` — upload → HeadByKey (verify `hubsync_digest` / `hubsync_digest_algo` fileInfo round-trip) → download → presigned HTTP GET against the real bucket.

Gated on `B2_APPLICATION_KEY_ID` + `B2_APPLICATION_KEY` (from `~/.env.secret`) + `HUBSYNC_TEST_BUCKET`. Missing any of the three → `t.Skip(...)`, so default `go test ./...` still passes for contributors without creds.

Each test mints a unique object prefix (`hubsync-itest/<timestamp>-<rand>/`) so reruns never collide. Objects are left in the bucket; rely on a lifecycle rule if you want them cleaned up.

### Tier 3: end-to-end smoke (manual)

Exercises `hubsync serve` as a real process against live B2:

```bash
# 1. Build
go build -o /tmp/hubsync ./cli/hubsync

# 2. Stage
SMOKE=/tmp/hs-smoke-$$
mkdir -p $SMOKE/.hubsync
cat > $SMOKE/.hubsync/config.toml <<EOF
[hub]
hash = "xxh128"

[archive]
provider      = "b2"
bucket        = "hayeah-hubsync-test"
bucket_prefix = "hubsync-smoke/$$/"
EOF
dd if=/dev/urandom of=$SMOKE/small.bin bs=1k count=8
dd if=/dev/urandom of=$SMOKE/big.bin   bs=1M count=4

# 3. Run serve in the background
godotenv -f ~/.env.secret /tmp/hubsync serve -dir $SMOKE -listen 127.0.0.1:8919 &
SERVE=$!; sleep 4

# 4. Walk the happy path
/tmp/hubsync status   -dir $SMOKE         # expect 2 archived after a few sec
/tmp/hubsync unpin    -dir $SMOKE big.bin # row flips to 'u'; local file removed
/tmp/hubsync ls       -dir $SMOKE         # 'u' stays stable across scanner sweeps
DIGEST=$(sqlite3 $SMOKE/.hubsync/hub.db \
  "SELECT hex(digest) FROM hub_entry WHERE path='big.bin';")
curl -sLI "http://127.0.0.1:8919/blobs/$DIGEST" | head -1  # 302 Found
/tmp/hubsync pin      -dir $SMOKE big.bin # restore, 'a', local reappears

kill $SERVE; rm -rf $SMOKE
```

Success criteria:

- `status` shows both files archived after the initial scan drains.
- Unpin removes the local file but leaves the row in `ls` (flag `u`).
- The `u` flag survives subsequent scanner sweeps (watcher-suppression works).
- `/blobs/{digest}` on an unpinned file returns `302 Found` with a `Location:` pointing at a `Authorization=…` presigned B2 URL.
- Pin restores the exact bytes and flips the row back to `a`.

### One-shot archive (no serve)

`hubsync archive` is the no-daemon variant for "back up this tree to B2 and leave". Same config layout; run it instead of `serve`:

```bash
SMOKE=/tmp/hs-archive-smoke-$$
mkdir -p $SMOKE/.hubsync
cat > $SMOKE/.hubsync/config.toml <<EOF
[hub]
hash = "xxh128"

[archive]
provider      = "b2"
bucket        = "hayeah-hubsync-test"
bucket_prefix = "hubsync-archive-smoke/$$/"
EOF
dd if=/dev/urandom of=$SMOKE/small.bin bs=1k count=8
dd if=/dev/urandom of=$SMOKE/big.bin   bs=1M count=4

# Preview — no network, no creds needed
/tmp/hubsync archive --dry $SMOKE

# Real run — takes hub.lock for the duration
godotenv -f ~/.env.secret /tmp/hubsync archive $SMOKE

# Verify — ls / status work without serve (DB read-only fallback)
/tmp/hubsync status -dir $SMOKE
/tmp/hubsync ls     -dir $SMOKE | duckql "WHERE state='archived' SELECT count(*)"

rm -rf $SMOKE
```

Success criteria: `archive --dry` prints one JSONL row per file with `"state":""`; real `archive` prints per-file progress on stderr + a summary; re-running `archive` is a no-op (rows are already `archived`); `ls` works without any `serve` running.

## Credentials & secrets

- **Never read `~/.env.secret` from code or from an agent directly.** Use `godotenv` to overlay it onto a subcommand's env.
- The live-bucket tests only need the app key (`B2_APPLICATION_KEY_ID` + `B2_APPLICATION_KEY`). The master key (`B2_MASTER_KEY_ID` / `B2_MASTER_KEY`) is for admin tasks like creating new buckets or issuing new app keys.
- Bucket `hayeah-hubsync-test` was created with the master key on 2026-04-17; feel free to reuse or create your own.

## When tests fail

- **Unix socket "invalid argument"** on macOS — path exceeds the 104-byte `sun_path` limit. Use `shortSocketPath(t)` (it mints the socket under `/tmp/`) in new tests.
- **BLOB digest column fails to scan** — a Rust/iOS client built against the pre-BLOB schema. Wipe the client's `.hubsync/` and re-bootstrap.
- **"hub DB was initialized with hash=…"** — `config.toml` is asking for a different algorithm than what's recorded in `hub_config_cache`. Wipe `.hubsync/` to start over (algo switching is out of scope for v1).
- **Wire generated code drift** — `wire gen ./...` to regenerate, commit the result alongside any provider changes.
