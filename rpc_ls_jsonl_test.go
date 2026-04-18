package hubsync

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// TestLsJSONLFormat brings up the RPC server, calls Ls, then marshals the
// result the way cmdLs does (json.NewEncoder per entry). Asserts the JSONL
// shape: one object per line, one field per hub_entry column.
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

	ls, err := client.Ls(ctx)
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

	// Parse into a tagged struct that matches the thin-projection wire
	// shape (JSON keys aligned with hub_entry SQL columns).
	type row struct {
		Path              string `json:"path"`
		Kind              string `json:"kind"`
		Digest            string `json:"digest"`
		Size              int64  `json:"size"`
		Mode              uint32 `json:"mode"`
		MTime             int64  `json:"mtime"`
		Version           int64  `json:"version"`
		ArchiveState      string `json:"archive_state"`
		ArchiveFileID     string `json:"archive_file_id"`
		ArchiveSHA1       string `json:"archive_sha1"`
		ArchiveUploadedAt int64  `json:"archive_uploaded_at"`
		UpdatedAt         int64  `json:"updated_at"`
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
		if r.Digest == "" {
			t.Errorf("row %q has empty digest", r.Path)
		}
		if r.Size <= 0 {
			t.Errorf("row %q has size=%d, want >0", r.Path, r.Size)
		}
		if r.MTime <= 0 {
			t.Errorf("row %q has mtime=%d, want unix-seconds >0", r.Path, r.MTime)
		}
		if r.Version <= 0 {
			t.Errorf("row %q has version=%d, want >0", r.Path, r.Version)
		}
		if r.UpdatedAt <= 0 {
			t.Errorf("row %q has updated_at=%d, want unix-seconds >0", r.Path, r.UpdatedAt)
		}
		if r.ArchiveFileID == "" {
			t.Errorf("row %q has empty archive_file_id (archived rows should carry a handle)", r.Path)
		}
		states = append(states, r.ArchiveState)
	}

	// One archived, one unpinned (lexicographic: a.txt first).
	want := []string{"archived", "unpinned"}
	if states[0] != want[0] || states[1] != want[1] {
		t.Errorf("archive_states = %v, want %v (sorted by path a.txt, dir/b.txt)", states, want)
	}
}
