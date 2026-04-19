package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayeah/hubsync"
	_ "github.com/marcboeker/go-duckdb/v2"
)

// binPath is set by TestMain — one `go build` per test run.
var binPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "hubsync-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)
	binPath = filepath.Join(tmp, "hubsync")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// runCLI runs the built hubsync binary in cwd with args + env additions,
// returning (stdout, stderr, exit). Pass "" for cwd to inherit the test
// runner's cwd.
func runCLI(t *testing.T, cwd string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return stdout.String(), stderr.String(), code
}

// seedHub scaffolds .hubsync/config.toml under dir with an [archive] section
// that has bogus credentials. Suitable for tests that don't actually touch
// B2 (e.g. --dry, missing-config, lock-held).
func seedHub(t *testing.T, dir string, withArchive bool) {
	t.Helper()
	hs := filepath.Join(dir, ".hubsync")
	if err := os.MkdirAll(hs, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := `[hub]
hash = "sha256"
`
	if withArchive {
		cfg += `
[archive]
provider      = "b2"
bucket        = "test"
bucket_prefix = "ci/"
b2_key_id     = "dummy"
b2_app_key    = "dummy"
`
	}
	if err := os.WriteFile(filepath.Join(hs, "config.toml"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCmdArchive_Dry_PopulatesItems(t *testing.T) {
	dir := t.TempDir()
	seedHub(t, dir, true) // [archive] present but creds bogus — --dry doesn't open B2
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("beta"), 0644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLI(t, dir, nil, "archive", "--dry")
	if code != 0 {
		t.Fatalf("archive --dry: code=%d stderr=%s", code, stderr)
	}

	dbPath := filepath.Join(dir, ".hubsync", "archive.duckdb")
	items := queryArchiveItems(t, dbPath)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d: %v", len(items), items)
	}
	wantPaths := map[string]bool{"a.txt": true, "sub/b.txt": true}
	for _, it := range items {
		if !wantPaths[it.Path] {
			t.Errorf("unexpected path: %s", it.Path)
		}
		if it.Size == 0 || it.DigestHex == "" {
			t.Errorf("unexpected row: %+v", it)
		}
	}

	// All tasks should be pending after --dry.
	pending := queryTaskCount(t, dbPath, "pending")
	if pending != 2 {
		t.Fatalf("pending=%d want 2", pending)
	}
	done := queryTaskCount(t, dbPath, "done")
	if done != 0 {
		t.Fatalf("done=%d want 0 after --dry", done)
	}
}

func TestCmdArchive_Dry_NoArchiveConfig_Succeeds(t *testing.T) {
	// --dry must work even without [archive] (user hasn't configured B2 yet
	// but wants to preview what would upload once they do).
	dir := t.TempDir()
	seedHub(t, dir, false)
	if err := os.WriteFile(filepath.Join(dir, "only.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCLI(t, dir, nil, "archive", "--dry")
	if code != 0 {
		t.Fatalf("archive --dry without [archive]: code=%d stderr=%s", code, stderr)
	}
	dbPath := filepath.Join(dir, ".hubsync", "archive.duckdb")
	items := queryArchiveItems(t, dbPath)
	if len(items) != 1 || items[0].Path != "only.txt" {
		t.Errorf("items=%v want [only.txt]", items)
	}
}

type archiveItem struct {
	Path      string
	Size      int64
	DigestHex string
}

// queryArchiveItems reads the items table from the task-runner DuckDB file.
func queryArchiveItems(t *testing.T, dbPath string) []archiveItem {
	t.Helper()
	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT path, size, digest FROM items ORDER BY path`)
	if err != nil {
		t.Fatalf("query items: %v", err)
	}
	defer rows.Close()
	var out []archiveItem
	for rows.Next() {
		var it archiveItem
		if err := rows.Scan(&it.Path, &it.Size, &it.DigestHex); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, it)
	}
	return out
}

// queryTaskCount returns the number of tasks rows in the given status.
func queryTaskCount(t *testing.T, dbPath, status string) int {
	t.Helper()
	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status = ?`, status).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

func TestCmdArchive_MissingHubsyncDir_Exits2(t *testing.T) {
	// hubContext walks up from cwd; running archive from a dir that
	// has no .hubsync anywhere in its ancestry should exit 2.
	dir := t.TempDir()
	_, stderr, code := runCLI(t, dir, nil, "archive", "--dry")
	if code != 2 {
		t.Fatalf("code=%d want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "no .hubsync") {
		t.Errorf("stderr should mention missing .hubsync: %s", stderr)
	}
}

func TestCmdArchive_LockHeld_Exits2(t *testing.T) {
	dir := t.TempDir()
	seedHub(t, dir, true)
	lock, err := hubsync.AcquireHubLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	_, stderr, code := runCLI(t, dir, nil, "archive", "--dry")
	if code != 2 {
		t.Fatalf("code=%d want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "locked") {
		t.Errorf("stderr should mention the lock: %s", stderr)
	}
}

// TestCmdArchive_Resume_SecondPassIsNoOp exercises the core invariant of
// the task-runner pattern: running `hubsync archive` twice with the same
// --resume path must not re-do the uploads. Uses --dry so no real B2 is
// involved; after the first pass all items are 'pending', second pass is
// still a no-op because --dry never claims / runs anything.
//
// The meaningful "second pass is no-op" check on live uploads lives in
// TestArchiveOneShot_EndToEnd (library-level) — it verifies via
// FakeStorage.VersionCount that no duplicate uploads land.
func TestCmdArchive_Resume_SecondPassIsNoOp(t *testing.T) {
	dir := t.TempDir()
	seedHub(t, dir, true)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("beta"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := runCLI(t, dir, nil, "archive", "--dry"); code != 0 {
		t.Fatalf("first archive: code=%d stderr=%s", code, stderr)
	}
	dbPath := filepath.Join(dir, ".hubsync", "archive.duckdb")
	firstItems := queryArchiveItems(t, dbPath)
	firstPending := queryTaskCount(t, dbPath, "pending")
	if len(firstItems) != 2 || firstPending != 2 {
		t.Fatalf("first pass unexpected: items=%d pending=%d", len(firstItems), firstPending)
	}

	// Second pass: items already populated, --dry → no plan, no run.
	if _, stderr, code := runCLI(t, dir, nil, "archive", "--dry"); code != 0 {
		t.Fatalf("second archive: code=%d stderr=%s", code, stderr)
	}
	if n := len(queryArchiveItems(t, dbPath)); n != 2 {
		t.Errorf("items count changed on second pass: %d", n)
	}
	if n := queryTaskCount(t, dbPath, "pending"); n != 2 {
		t.Errorf("pending count changed on second pass: %d", n)
	}
}

// TestCmdArchive_WhereSubset_Filters confirms --where is plumbed
// through. With --dry the predicate doesn't actually run anything, but
// we can verify the DB exists and items were populated (the --where
// applies only to the work-queue SELECT, not to planning).
func TestCmdArchive_WhereSubset_AppliesToWorkQueue(t *testing.T) {
	dir := t.TempDir()
	seedHub(t, dir, true)
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// --where with --dry is a no-op (still no run); still exercises flag parsing.
	_, stderr, code := runCLI(t, dir, nil, "archive", "--dry", "--where", "path = 'one.txt'")
	if code != 0 {
		t.Fatalf("archive --dry --where: code=%d stderr=%s", code, stderr)
	}
	// All three should be in items regardless of --where (plan always runs).
	dbPath := filepath.Join(dir, ".hubsync", "archive.duckdb")
	if n := len(queryArchiveItems(t, dbPath)); n != 3 {
		t.Errorf("expected 3 items after plan, got %d", n)
	}
}

func TestCmdLs_NoServe_ReadsDBDirectly(t *testing.T) {
	dir := t.TempDir()
	seedHub(t, dir, true)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0644); err != nil {
		t.Fatal(err)
	}
	// Populate hub_entry via `archive --dry` (does a FullScan).
	if _, _, code := runCLI(t, dir, nil, "archive", "--dry"); code != 0 {
		t.Fatalf("pre-populate scan failed")
	}

	// Now call `ls --all` — no serve, no lock — should open the DB read-only
	// and dump every row (new default `ls` lists only cwd's level).
	stdout, stderr, code := runCLI(t, dir, nil, "ls", "--all")
	if code != 0 {
		t.Fatalf("ls: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `"path":"a.txt"`) {
		t.Errorf("ls output missing row: %s", stdout)
	}
}

func TestCmdPin_NoCreds_CleanError(t *testing.T) {
	dir := t.TempDir()
	seedHub(t, dir, true) // bogus creds
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// Populate the DB so the glob has something to match.
	if _, _, code := runCLI(t, dir, nil, "archive", "--dry"); code != 0 {
		t.Fatalf("pre-populate scan failed")
	}

	// In-process pin should attempt to open the B2 client and fail. The
	// error should be readable — not a panic / ErrLocked misreporting.
	_, stderr, code := runCLI(t, dir, nil, "pin", "a.txt")
	if code == 0 {
		t.Fatalf("expected non-zero exit; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "archive") && !strings.Contains(stderr, "b2") && !strings.Contains(stderr, "KeyID") {
		t.Errorf("stderr should hint at the archive/B2 wiring failure: %s", stderr)
	}
}

// Smoke: usage with no args should exit non-zero and include 'archive' in
// the verb list.
func TestUsage_IncludesArchive(t *testing.T) {
	_, stderr, code := runCLI(t, "", nil, "help")
	if code == 0 {
		t.Errorf("expected non-zero exit for unknown verb")
	}
	if !strings.Contains(stderr, "hubsync archive") {
		t.Errorf("usage should mention hubsync archive:\n%s", stderr)
	}
}

