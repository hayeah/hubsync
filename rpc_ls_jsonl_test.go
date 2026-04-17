package hubsync

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// TestLsJSONLFormat brings up the RPC server, calls Ls, then marshals the
// result the way cmdLs does (json.NewEncoder per entry). Asserts the JSONL
// shape: one object per line, carrying the structured fields that duckql
// / downstream tools consume.
func TestLsJSONLFormat(t *testing.T) {
	env := newReconcilerEnv(t)
	env.writeLocal(t, "a.txt", "alpha")
	env.appendEntry(t, "a.txt", "alpha")
	env.writeLocal(t, "dir/b.txt", "beta")
	env.appendEntry(t, "dir/b.txt", "beta")

	client, _ := newTestRPCServer(t, env)
	ctx := context.Background()

	// Pin both, then unpin dir/b.txt — gives us a mix of archived + unpinned.
	if _, err := client.Pin(ctx, PinRequest{Globs: []string{"**/*.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Unpin(ctx, PinRequest{Globs: []string{"dir/b.txt"}}); err != nil {
		t.Fatal(err)
	}

	ls, err := client.Ls(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	// Mirror cmdLs: one JSON object per line.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range ls.Entries {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}

	// Parse each line back independently; this also proves "one row per line".
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL rows, got %d:\n%s", len(lines), buf.String())
	}

	type row struct {
		Path      string `json:"path"`
		Kind      string `json:"kind"`
		State     string `json:"state"`
		Size      int64  `json:"size"`
		MTime     int64  `json:"mtime"`
		DigestHex string `json:"digest_hex"`
	}

	var states []string
	for _, line := range lines {
		var r row
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("parse %q: %v", string(line), err)
		}
		if r.Path == "" {
			t.Errorf("row has empty path: %s", string(line))
		}
		if r.Kind != "file" {
			t.Errorf("row %q has kind=%q, want file", r.Path, r.Kind)
		}
		if r.DigestHex == "" {
			t.Errorf("row %q has empty digest_hex", r.Path)
		}
		if r.Size <= 0 {
			t.Errorf("row %q has size=%d, want >0", r.Path, r.Size)
		}
		states = append(states, r.State)
	}

	// One archived, one unpinned (lexicographic: a.txt first).
	want := []string{"archived", "unpinned"}
	if states[0] != want[0] || states[1] != want[1] {
		t.Errorf("states = %v, want %v (sorted by path a.txt, dir/b.txt)", states, want)
	}
}
