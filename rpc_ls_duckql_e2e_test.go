package hubsync

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// TestLsDuckqlRoundTrip takes the LsResponse JSONL output and pipes it
// through an actual `duckql` subprocess. This covers the section's required
// verification: `hubsync ls | duckql "WHERE archive_state='unpinned'"`.
//
// Skipped gracefully if duckql isn't on PATH (e.g. CI without the tool).
func TestLsDuckqlRoundTrip(t *testing.T) {
	duckqlBin, err := exec.LookPath("duckql")
	if err != nil {
		t.Skip("duckql not on PATH; skipping round-trip test")
	}

	env := newReconcilerEnv(t)
	env.writeLocal(t, "a.txt", "alpha")
	env.appendEntry(t, "a.txt", "alpha")
	env.writeLocal(t, "dir/b.txt", "beta")
	env.appendEntry(t, "dir/b.txt", "beta")

	client, _ := newTestRPCServer(t, env)
	ctx := context.Background()

	if _, err := client.Pin(ctx, PinRequest{Globs: []string{"**/*.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Unpin(ctx, PinRequest{Globs: []string{"dir/b.txt"}}); err != nil {
		t.Fatal(err)
	}

	ls, err := client.Ls(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var jsonlBuf bytes.Buffer
	enc := json.NewEncoder(&jsonlBuf)
	for _, e := range ls.Entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("hubsync ls (JSONL):\n%s", jsonlBuf.String())

	// Pipe through duckql -o jsonl:- (avoid YAML → simpler to parse).
	cmd := exec.Command(duckqlBin, "-o", "jsonl:-", "WHERE archive_state='unpinned'")
	cmd.Stdin = bytes.NewReader(jsonlBuf.Bytes())
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("duckql: %v\nstderr:\n%s", err, string(ee.Stderr))
		}
		t.Fatal(err)
	}
	t.Logf("duckql \"WHERE archive_state='unpinned'\" output:\n%s", string(out))

	// A second pattern from the spec: aggregate with duckql.
	cmd2 := exec.Command(duckqlBin, "-o", "jsonl:-",
		"SELECT archive_state, count(*) AS n GROUP BY 1 ORDER BY n DESC")
	cmd2.Stdin = bytes.NewReader(jsonlBuf.Bytes())
	out2, err := cmd2.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("duckql (agg): %v\nstderr:\n%s", err, string(ee.Stderr))
		}
		t.Fatal(err)
	}
	t.Logf("duckql aggregate output:\n%s", string(out2))

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 unpinned row, got %d:\n%s", len(lines), out)
	}
	var r struct {
		Path         string `json:"path"`
		ArchiveState string `json:"archive_state"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &r); err != nil {
		t.Fatalf("parse duckql output: %v: %s", err, lines[0])
	}
	if r.Path != "dir/b.txt" {
		t.Errorf("path=%q want dir/b.txt", r.Path)
	}
	if r.ArchiveState != "unpinned" {
		t.Errorf("archive_state=%q want unpinned", r.ArchiveState)
	}
}
