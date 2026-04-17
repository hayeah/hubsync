package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayeah/hubsync"
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

// runCLI runs the built hubsync binary with args + env additions, returning
// (stdout, stderr, exit).
func runCLI(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
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

func TestCmdArchive_Dry_EmitsJSONL(t *testing.T) {
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

	stdout, stderr, code := runCLI(t, nil, "archive", "--dry", dir)
	if code != 0 {
		t.Fatalf("archive --dry: code=%d stderr=%s", code, stderr)
	}

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL rows, got %d:\n%s", len(lines), stdout)
	}
	type row struct {
		Path      string `json:"path"`
		Kind      string `json:"kind"`
		State     string `json:"state"`
		Size      int64  `json:"size"`
		MTime     string `json:"mtime"`
		DigestHex string `json:"digest_hex"`
	}
	for _, line := range lines {
		var r row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if r.Kind != "file" || r.Size == 0 || r.DigestHex == "" {
			t.Errorf("unexpected row: %+v", r)
		}
		if r.State != "" {
			t.Errorf("state=%q want empty (NULL) for fresh dry-run row", r.State)
		}
		// Should match ls row shape exactly — no spurious fields.
	}

	// The row order should be lexicographic by path: a.txt, sub/b.txt.
	var first row
	_ = json.Unmarshal([]byte(lines[0]), &first)
	if first.Path != "a.txt" {
		t.Errorf("first row path=%q want a.txt", first.Path)
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
	stdout, stderr, code := runCLI(t, nil, "archive", "--dry", dir)
	if code != 0 {
		t.Fatalf("archive --dry without [archive]: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `"path":"only.txt"`) {
		t.Errorf("stdout missing only.txt row: %s", stdout)
	}
}

func TestCmdArchive_MissingHubsyncDir_Exits2(t *testing.T) {
	// locateHubDir walks up from cwd; pointing archive at a hub dir that
	// has no .hubsync anywhere in its ancestry should exit 2.
	dir := t.TempDir()
	_, stderr, code := runCLI(t, nil, "archive", "--dry", dir)
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

	_, stderr, code := runCLI(t, nil, "archive", "--dry", dir)
	if code != 2 {
		t.Fatalf("code=%d want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "locked") {
		t.Errorf("stderr should mention the lock: %s", stderr)
	}
}

func TestCmdLs_NoServe_ReadsDBDirectly(t *testing.T) {
	dir := t.TempDir()
	seedHub(t, dir, true)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0644); err != nil {
		t.Fatal(err)
	}
	// Populate hub_entry via `archive --dry` (does a FullScan).
	if _, _, code := runCLI(t, nil, "archive", "--dry", dir); code != 0 {
		t.Fatalf("pre-populate scan failed")
	}

	// Now call `ls` — no serve, no lock — should open the DB read-only.
	stdout, stderr, code := runCLI(t, nil, "ls", "-dir", dir)
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
	if _, _, code := runCLI(t, nil, "archive", "--dry", dir); code != 0 {
		t.Fatalf("pre-populate scan failed")
	}

	// In-process pin should attempt to open the B2 client and fail. The
	// error should be readable — not a panic / ErrLocked misreporting.
	_, stderr, code := runCLI(t, nil, "pin", "-dir", dir, "a.txt")
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
	_, stderr, code := runCLI(t, nil, "help")
	if code == 0 {
		t.Errorf("expected non-zero exit for unknown verb")
	}
	if !strings.Contains(stderr, "hubsync archive") {
		t.Errorf("usage should mention hubsync archive:\n%s", stderr)
	}
}

