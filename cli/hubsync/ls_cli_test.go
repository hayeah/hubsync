package main

import (
	"encoding/json"
	"os"
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
	if code == 0 {
		t.Fatalf("expected non-zero exit for escape")
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
	if code == 0 {
		t.Fatalf("expected non-zero exit outside a hub")
	}
	if !strings.Contains(stderr, "no .hubsync/") {
		t.Errorf("stderr should say no .hubsync/: %s", stderr)
	}
}
