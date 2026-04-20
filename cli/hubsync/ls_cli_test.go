package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seededLsHub scaffolds .hubsync + a small tree and primes hub_entry via
// `archive --dry`. Returns the hub dir.
func seededLsHub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	seedHub(t, dir, true) // [archive] with bogus creds — --dry doesn't open B2
	files := map[string]string{
		"a.txt":           "alpha",
		"b.txt":           "beta",
		"sub/c.txt":       "gamma",
		"sub/d.txt":       "delta",
		"sub/deeper/e.txt": "epsilon",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, stderr, code := runCLI(t, dir, nil, "archive", "--dry"); code != 0 {
		t.Fatalf("prime scan: code=%d stderr=%s", code, stderr)
	}
	return dir
}

// parseLsLines parses JSONL ls output into {path: row}.
func parseLsLines(t *testing.T, out string) map[string]struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
} {
	t.Helper()
	m := map[string]struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	}{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		// Skip the ls-pattern `//` docstring prelude — data lines follow.
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "//") {
			continue
		}
		var r struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		m[r.Path] = r
	}
	return m
}

func TestCmdLs_TopLevel_NoArg(t *testing.T) {
	dir := seededLsHub(t)
	stdout, stderr, code := runCLI(t, dir, nil, "ls")
	if code != 0 {
		t.Fatalf("ls: code=%d stderr=%s", code, stderr)
	}
	rows := parseLsLines(t, stdout)
	// Expect files a.txt, b.txt and a synthetic dir "sub/".
	if _, ok := rows["a.txt"]; !ok {
		t.Errorf("missing a.txt: %v", rows)
	}
	if _, ok := rows["b.txt"]; !ok {
		t.Errorf("missing b.txt: %v", rows)
	}
	if r, ok := rows["sub/"]; !ok || r.Kind != "directory" {
		t.Errorf("want synthetic dir 'sub/' kind=directory, got %v", r)
	}
	// No deeper paths should leak.
	for path := range rows {
		if strings.Contains(strings.TrimSuffix(path, "/"), "/") {
			t.Errorf("unexpected deep row: %q", path)
		}
	}
}

func TestCmdLs_ExplicitDot(t *testing.T) {
	dir := seededLsHub(t)
	outDot, _, _ := runCLI(t, dir, nil, "ls", ".")
	outBare, _, _ := runCLI(t, dir, nil, "ls")
	if outDot != outBare {
		t.Errorf("`ls .` and bare `ls` should be equivalent.\ndot:\n%s\nbare:\n%s", outDot, outBare)
	}
}

func TestCmdLs_UnderPrefix(t *testing.T) {
	dir := seededLsHub(t)
	stdout, stderr, code := runCLI(t, dir, nil, "ls", "sub/")
	if code != 0 {
		t.Fatalf("ls sub/: code=%d stderr=%s", code, stderr)
	}
	rows := parseLsLines(t, stdout)
	if _, ok := rows["sub/c.txt"]; !ok {
		t.Errorf("missing sub/c.txt: %v", rows)
	}
	if _, ok := rows["sub/d.txt"]; !ok {
		t.Errorf("missing sub/d.txt: %v", rows)
	}
	if r, ok := rows["sub/deeper/"]; !ok || r.Kind != "directory" {
		t.Errorf("want synthetic dir 'sub/deeper/', got %v", r)
	}
	// sub/deeper/e.txt must be collapsed.
	if _, ok := rows["sub/deeper/e.txt"]; ok {
		t.Errorf("sub/deeper/e.txt should be collapsed into sub/deeper/")
	}
}

func TestCmdLs_FromSubdir_NoArg(t *testing.T) {
	dir := seededLsHub(t)
	sub := filepath.Join(dir, "sub")
	stdout, stderr, code := runCLI(t, sub, nil, "ls")
	if code != 0 {
		t.Fatalf("ls in sub: code=%d stderr=%s", code, stderr)
	}
	rows := parseLsLines(t, stdout)
	// Running from <hub>/sub/ with no arg should list sub/'s children.
	if _, ok := rows["sub/c.txt"]; !ok {
		t.Errorf("want sub/c.txt as if from hub root, got %v", rows)
	}
	if _, ok := rows["a.txt"]; ok {
		t.Errorf("top-level a.txt should NOT appear when cwd is sub/")
	}
}

func TestCmdLs_FromSubdir_ParentPath(t *testing.T) {
	dir := seededLsHub(t)
	sub := filepath.Join(dir, "sub")
	stdout, stderr, code := runCLI(t, sub, nil, "ls", "..")
	if code != 0 {
		t.Fatalf("ls ..: code=%d stderr=%s", code, stderr)
	}
	rows := parseLsLines(t, stdout)
	if _, ok := rows["a.txt"]; !ok {
		t.Errorf("`ls ..` from sub/ should see top-level rows: %v", rows)
	}
}

func TestCmdLs_EscapeHub_Errors(t *testing.T) {
	dir := seededLsHub(t)
	_, stderr, code := runCLI(t, dir, nil, "ls", "../../nope")
	if code != 2 {
		t.Fatalf("code=%d want 2 (startup failure); stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "outside the hub root") {
		t.Errorf("stderr should explain escape: %s", stderr)
	}
}

func TestCmdLs_All_DumpsEverything(t *testing.T) {
	dir := seededLsHub(t)
	stdout, stderr, code := runCLI(t, dir, nil, "ls", "--all")
	if code != 0 {
		t.Fatalf("ls --all: code=%d stderr=%s", code, stderr)
	}
	rows := parseLsLines(t, stdout)
	wantPaths := []string{"a.txt", "b.txt", "sub/c.txt", "sub/d.txt", "sub/deeper/e.txt"}
	for _, p := range wantPaths {
		if _, ok := rows[p]; !ok {
			t.Errorf("--all missing %q: %v", p, rows)
		}
	}
}

func TestCmdLs_NoHubsync_Errors(t *testing.T) {
	dir := t.TempDir()
	_, stderr, code := runCLI(t, dir, nil, "ls")
	if code != 2 {
		t.Fatalf("code=%d want 2 (startup failure); stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "no .hubsync/") {
		t.Errorf("stderr should say no .hubsync/: %s", stderr)
	}
}

// TestCmdLs_Docstring_PresentAndPointsAtDB verifies the `//`-comment
// prelude emitted above the JSONL body: it must include the underlying
// SQLite DB path and at least one `duckql` sample query, as called for
// by the ls-pattern docstring convention.
func TestCmdLs_Docstring_PresentAndPointsAtDB(t *testing.T) {
	dir := seededLsHub(t)
	stdout, stderr, code := runCLI(t, dir, nil, "ls", "--all")
	if code != 0 {
		t.Fatalf("ls --all: code=%d stderr=%s", code, stderr)
	}
	// Split comment-lines from data-lines.
	var comments, data []string
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "//") {
			comments = append(comments, line)
		} else if line != "" {
			data = append(data, line)
		}
	}
	if len(comments) == 0 {
		t.Fatalf("expected a `//` docstring prelude; got output:\n%s", stdout)
	}
	if len(data) == 0 {
		t.Fatalf("expected data rows after the prelude; got output:\n%s", stdout)
	}
	// Docstring must name the underlying DB path.
	dbPath := filepath.Join(dir, ".hubsync", "hub.db")
	joined := strings.Join(comments, "\n")
	if !strings.Contains(joined, dbPath) {
		t.Errorf("docstring should name the DB path %q\n%s", dbPath, joined)
	}
	if !strings.Contains(joined, "duckql") {
		t.Errorf("docstring should include at least one duckql sample query\n%s", joined)
	}
	// Data rows must parse as strict JSON (no stray `//` mixed in).
	for _, line := range data {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Errorf("data row is not strict JSON: %q: %v", line, err)
		}
	}
}

// TestCmdLs_Docstring_DuckqlRoundTrip proves the full pipeline:
// `hubsync ls` → `duckql "WHERE …"` works end-to-end with the prelude
// present; duckql strips the `//` lines and the row count is preserved.
// Respects $DUCKQL_BIN for cross-worktree testing; falls back to PATH.
// Skipped gracefully if `duckql` isn't available.
func TestCmdLs_Docstring_DuckqlRoundTrip(t *testing.T) {
	var (
		duckqlBin string
		duckqlArgs []string
	)
	if override := os.Getenv("DUCKQL_BIN"); override != "" {
		parts := strings.Fields(override)
		duckqlBin, duckqlArgs = parts[0], parts[1:]
	} else {
		found, err := exec.LookPath("duckql")
		if err != nil {
			t.Skip("duckql not on PATH (set $DUCKQL_BIN to override); skipping round-trip test")
		}
		duckqlBin = found
	}
	// Probe: skip if the duckql we're about to run doesn't know
	// --no-strip-comments (i.e. it predates the comment-stripping
	// feature). Running an old duckql would fail deterministically on
	// the `//` prelude and produce a misleading test failure.
	probeArgs := append([]string{}, duckqlArgs...)
	probeArgs = append(probeArgs, "--help")
	probe, probeErr := exec.Command(duckqlBin, probeArgs...).CombinedOutput()
	if probeErr != nil {
		t.Skipf("duckql --help failed, skipping: %v\n%s", probeErr, probe)
	}
	if !strings.Contains(string(probe), "--no-strip-comments") {
		t.Skip("installed duckql predates --no-strip-comments; set DUCKQL_BIN to a newer build")
	}
	dir := seededLsHub(t)
	stdout, stderr, code := runCLI(t, dir, nil, "ls", "--all")
	if code != 0 {
		t.Fatalf("ls --all: code=%d stderr=%s", code, stderr)
	}
	// Pipe the raw ls output (docstring + body) through duckql; the
	// downstream tool must see the 5 data rows despite the `//` prelude.
	args := append([]string{}, duckqlArgs...)
	args = append(args, "-o", "jsonl:-", "SELECT count(*) AS n")
	cmd := exec.Command(duckqlBin, args...)
	cmd.Stdin = bytes.NewReader([]byte(stdout))
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("duckql: %v\nstderr:\n%s", err, string(ee.Stderr))
		}
		t.Fatal(err)
	}
	var r struct {
		N int `json:"n"`
	}
	line := strings.TrimRight(string(out), "\n")
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("parse duckql output %q: %v", line, err)
	}
	// 5 files from seededLsHub.
	if r.N != 5 {
		t.Errorf("duckql count = %d, want 5 (docstring-strip broken?)", r.N)
	}
}
