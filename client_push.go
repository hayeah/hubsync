package hubsync

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hayeah/go-lstree"
	"google.golang.org/protobuf/proto"
)

// LocalChange represents a local file that differs from hub_tree.
type LocalChange struct {
	Path     string
	Digest   Digest
	Data     []byte
	HubEntry *HubTreeEntry // nil for new files
}

// LocalDeletion represents a file present in hub_tree but missing locally.
type LocalDeletion struct {
	Path     string
	HubEntry HubTreeEntry
}

// ScanLocal walks the sync directory and compares against hub_tree.
// Returns local changes and deletions.
func (c *Client) ScanLocal() ([]LocalChange, []LocalDeletion, error) {
	hubTree, err := c.Store.TreeSnapshot()
	if err != nil {
		return nil, nil, fmt.Errorf("load hub_tree: %w", err)
	}

	ignorer, err := ProvideIgnorer(c.syncDir)
	if err != nil {
		return nil, nil, fmt.Errorf("create ignorer: %w", err)
	}

	fsys := os.DirFS(c.syncDir)
	seen := make(map[string]bool)
	var changes []LocalChange

	q := lstree.Query{Stat: true}
	err = lstree.Walk(fsys, q, ignorer, func(e lstree.Entry) error {
		if e.IsDir {
			return nil
		}
		seen[e.Path] = true

		hubEntry, exists := hubTree[e.Path]

		// Quick check: if mtime+size match hub_tree, skip (digest cache)
		if exists && hubEntry.MTime == e.ModTime.Unix() && hubEntry.Size == e.Size {
			return nil
		}

		data, err := fs.ReadFile(fsys, e.Path)
		if err != nil {
			return fmt.Errorf("read %s: %w", e.Path, err)
		}
		localDigest := ComputeDigest(data)

		// If digest matches, in sync despite stat change
		if exists && localDigest == hubEntry.Digest {
			return nil
		}

		change := LocalChange{
			Path:   e.Path,
			Digest: localDigest,
			Data:   data,
		}
		if exists {
			entry := hubEntry
			change.HubEntry = &entry
		}
		changes = append(changes, change)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("scan: %w", err)
	}

	// Find deletions: entries in hub_tree not found on local FS
	var deletions []LocalDeletion
	for path, entry := range hubTree {
		if !seen[path] {
			deletions = append(deletions, LocalDeletion{
				Path:     path,
				HubEntry: entry,
			})
		}
	}

	return changes, deletions, nil
}

// BuildPushOps converts scan results into PushOps.
func BuildPushOps(changes []LocalChange, deletions []LocalDeletion) []*PushOp {
	var ops []*PushOp

	for _, c := range changes {
		op := &PushOp{
			Path:   c.Path,
			Digest: c.Digest[:],
			Data:   c.Data,
		}
		if c.HubEntry == nil {
			op.Op = OpKind_OP_CREATE
			op.BaseVersion = 0
		} else {
			op.Op = OpKind_OP_UPDATE
			op.BaseVersion = uint64(c.HubEntry.Version)
		}
		ops = append(ops, op)
	}

	for _, d := range deletions {
		ops = append(ops, &PushOp{
			Path:        d.Path,
			Op:          OpKind_OP_DELETE,
			BaseVersion: uint64(d.HubEntry.Version),
		})
	}

	return ops
}

// Push sends local changes to the hub and handles the response.
// Returns the number of accepted ops, or an error.
func (c *Client) Push(policy ConflictPolicy) (int, error) {
	changes, deletions, err := c.ScanLocal()
	if err != nil {
		return 0, fmt.Errorf("scan: %w", err)
	}

	ops := BuildPushOps(changes, deletions)
	if len(ops) == 0 {
		return 0, nil
	}

	log.Printf("pushing %d ops (%d changes, %d deletions)", len(ops), len(changes), len(deletions))

	req := &PushRequest{
		Ops:        ops,
		OnConflict: policy,
	}

	resp, err := c.sendPush(req)
	if err != nil {
		return 0, err
	}

	return c.handlePushResponse(resp, changes, deletions)
}

func (c *Client) sendPush(req *PushRequest) (*PushResponse, error) {
	reqData, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal push request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.hubURL+"/push", bytes.NewReader(reqData))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	c.setAuth(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("push request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("push response %s: %s", resp.Status, body)
	}

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var pushResp PushResponse
	if err := proto.Unmarshal(respData, &pushResp); err != nil {
		return nil, fmt.Errorf("unmarshal push response: %w", err)
	}

	return &pushResp, nil
}

func (c *Client) handlePushResponse(resp *PushResponse, changes []LocalChange, deletions []LocalDeletion) (int, error) {
	accepted := 0

	for _, result := range resp.Results {
		switch outcome := result.Outcome.(type) {
		case *PushResult_Accepted:
			version := int64(outcome.Accepted.Version)
			if err := c.updateHubTreeAfterPush(result.Path, version, changes, deletions); err != nil {
				log.Printf("push %s: accepted but failed to update hub_tree: %v", result.Path, err)
			} else {
				log.Printf("push %s: accepted at version %d", result.Path, version)
			}
			accepted++

		case *PushResult_Conflict:
			conflict := outcome.Conflict
			log.Printf("push %s: conflict — %s (hub version=%d)", result.Path, conflict.Reason, conflict.CurrentVersion)
			c.handleConflict(result.Path, conflict)
		}
	}

	return accepted, nil
}

func (c *Client) updateHubTreeAfterPush(path string, version int64, changes []LocalChange, deletions []LocalDeletion) error {
	// Find the change or deletion for this path
	for _, ch := range changes {
		if ch.Path == path {
			op := OpCreate
			if ch.HubEntry != nil {
				op = OpUpdate
			}
			return c.Store.ApplyChange(version, path, op, FileKindFile, ch.Digest, int64(len(ch.Data)), 0644, 0)
		}
	}
	for _, d := range deletions {
		if d.Path == path {
			return c.Store.ApplyChange(version, path, OpDelete, FileKindFile, Digest{}, 0, 0, 0)
		}
	}
	return nil
}

// handleConflict renames the local file to a conflicted copy and fetches the
// hub's current version. The conflicted copy preserves the client's edits so
// an agent or user can reconcile later.
func (c *Client) handleConflict(path string, conflict *PushConflict) {
	fullPath := filepath.Join(c.syncDir, path)

	// Rename local file to conflicted copy
	conflictPath := conflictedCopyPath(fullPath)
	if err := os.Rename(fullPath, conflictPath); err != nil {
		log.Printf("conflict %s: failed to rename to conflicted copy: %v", path, err)
		return
	}
	log.Printf("conflict %s: local version saved as %s", path, filepath.Base(conflictPath))

	// Fetch hub's current blob and write to original path
	if len(conflict.CurrentDigest) > 0 {
		digest, err := ParseDigest(fmt.Sprintf("%x", conflict.CurrentDigest))
		if err == nil {
			if err := c.fetchFullBlob(digest, fullPath, 0644); err != nil {
				log.Printf("conflict %s: failed to fetch hub version: %v", path, err)
			}
		}
	}
}

// conflictedCopyPath returns a path like "name (conflicted copy).ext".
// If the conflicted copy already exists, appends a number: "name (conflicted copy 2).ext".
func conflictedCopyPath(fullPath string) string {
	dir := filepath.Dir(fullPath)
	ext := filepath.Ext(fullPath)
	base := strings.TrimSuffix(filepath.Base(fullPath), ext)

	candidate := filepath.Join(dir, base+" (conflicted copy)"+ext)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate
	}

	for i := 2; ; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s (conflicted copy %d)%s", base, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// pendingPushPaths returns the set of paths that have local modifications
// (used to skip overwriting during sync stream processing in write mode).
func (c *Client) pendingPushPaths() map[string]bool {
	changes, deletions, err := c.ScanLocal()
	if err != nil {
		return nil
	}
	paths := make(map[string]bool)
	for _, ch := range changes {
		paths[ch.Path] = true
	}
	for _, d := range deletions {
		paths[d.Path] = true
	}
	return paths
}

// skipPath checks if a path should be skipped during sync event application
// because it has pending local edits.
func (c *Client) skipPath(path string) bool {
	if c.localEditPaths == nil {
		return false
	}

	// Check if path has local edits, but still update hub_tree
	return c.localEditPaths[path]
}

// refreshLocalEditPaths scans for locally modified paths.
func (c *Client) refreshLocalEditPaths() {
	if !c.writeMode {
		return
	}
	c.localEditPaths = c.pendingPushPaths()
	if len(c.localEditPaths) > 0 {
		paths := make([]string, 0, len(c.localEditPaths))
		for p := range c.localEditPaths {
			paths = append(paths, p)
		}
		log.Printf("local edits detected, will skip overwriting: %s", strings.Join(paths, ", "))
	}
}
