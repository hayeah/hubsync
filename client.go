package hubsync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hayeah/go-lstree"
	"google.golang.org/protobuf/proto"
)

// Client is a replica that syncs from a hub.
// In write mode, it also pushes local changes back.
type Client struct {
	Store   *ClientStore
	hubURL  string
	token   BearerToken
	syncDir string
	http    *http.Client

	// Write mode fields
	writeMode      bool
	scanInterval   time.Duration
	localEditPaths map[string]bool // paths with local edits (skip during sync)

	// OnEvent is called after each sync event is applied (for testing).
	OnEvent func(version int64, path string, op ChangeOp)
	// OnPush is called after a push completes (for testing).
	OnPush func(accepted int)
}

// NewClient creates a Client.
func NewClient(store *ClientStore, hubURL string, token BearerToken, syncDir string) *Client {
	return &Client{
		Store:        store,
		hubURL:       hubURL,
		token:        token,
		syncDir:      syncDir,
		scanInterval: 5 * time.Second,
		http: &http.Client{
			Timeout: 0, // no timeout for streaming
		},
	}
}

// SetWriteMode enables write mode with the given scan interval.
func (c *Client) SetWriteMode(interval time.Duration) {
	c.writeMode = true
	if interval > 0 {
		c.scanInterval = interval
	}
}

// Bootstrap fetches the tree DB and all blobs from the hub.
// Steps: download hub_tree DB from /snapshots-tree/latest, then fetch each blob.
func (c *Client) Bootstrap(ctx context.Context) error {
	// Fetch the tree DB
	if err := c.bootstrapTreeDB(ctx); err != nil {
		return err
	}

	// Fetch all blobs referenced by the tree
	if err := c.bootstrapBlobs(ctx); err != nil {
		return err
	}

	version, _ := c.Store.HubVersion()
	log.Printf("bootstrap complete, version=%d", version)
	return nil
}

// Catchup brings the local FS into sync with the hub's current state, then returns.
// Reconciles by: fetching the latest tree DB, fetching missing/changed blobs,
// and removing local files no longer in the tree. Idempotent — safe to re-run.
func (c *Client) Catchup(ctx context.Context) error {
	// Fetch the latest tree DB (replaces local hub_tree state)
	if err := c.bootstrapTreeDB(ctx); err != nil {
		return err
	}

	tree, err := c.Store.TreeSnapshot()
	if err != nil {
		return fmt.Errorf("load tree: %w", err)
	}

	// Fetch missing/changed blobs
	fetched, skipped, err := c.reconcileBlobs(ctx, tree)
	if err != nil {
		return err
	}

	// Remove local files not in the tree
	deleted, err := c.removeExtraneous(tree)
	if err != nil {
		return err
	}

	version, _ := c.Store.HubVersion()
	log.Printf("catchup complete, version=%d, fetched=%d, skipped=%d, deleted=%d",
		version, fetched, skipped, deleted)
	return nil
}

// reconcileBlobs fetches blobs for paths whose local content doesn't match hub_tree.
// Files already on disk with the correct digest are skipped.
func (c *Client) reconcileBlobs(ctx context.Context, tree map[string]HubTreeEntry) (fetched, skipped int, err error) {
	for path, entry := range tree {
		if ctx.Err() != nil {
			return fetched, skipped, ctx.Err()
		}
		if entry.Kind != FileKindFile {
			continue
		}

		fullPath := filepath.Join(c.syncDir, path)

		// Check if local file already matches
		if data, readErr := os.ReadFile(fullPath); readErr == nil {
			if ComputeDigest(data) == entry.Digest {
				skipped++
				continue
			}
		}

		// Fetch the blob
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return fetched, skipped, err
		}
		if err := c.fetchFullBlob(entry.Digest, fullPath, os.FileMode(entry.Mode)); err != nil {
			return fetched, skipped, fmt.Errorf("fetch %s: %w", path, err)
		}
		fetched++
	}
	return fetched, skipped, nil
}

// removeExtraneous deletes local files (under syncDir) that are not in the hub_tree.
// Skips .hubsync/ and respects .gitignore (these are not synced from the hub).
func (c *Client) removeExtraneous(tree map[string]HubTreeEntry) (int, error) {
	ignorer, err := ProvideIgnorer(c.syncDir)
	if err != nil {
		return 0, err
	}

	deleted := 0
	fsys := os.DirFS(c.syncDir)
	q := lstree.Query{}
	err = lstree.Walk(fsys, q, ignorer, func(e lstree.Entry) error {
		if e.IsDir {
			return nil
		}
		if _, inTree := tree[e.Path]; inTree {
			return nil
		}
		fullPath := filepath.Join(c.syncDir, e.Path)
		if err := os.Remove(fullPath); err != nil {
			return fmt.Errorf("remove %s: %w", e.Path, err)
		}
		deleted++
		return nil
	})
	if err != nil {
		return deleted, err
	}
	return deleted, nil
}

func (c *Client) bootstrapTreeDB(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.hubURL+"/snapshots-tree/latest", nil)
	if err != nil {
		return err
	}
	c.setAuth(req)

	// Follow redirects with auth
	c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		c.setAuth(req)
		return nil
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch tree db: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tree db response: %s", resp.Status)
	}

	// Write the DB to the sync dir
	dir := c.syncDir
	if err := os.MkdirAll(filepath.Join(dir, ".hubsync"), 0755); err != nil {
		return err
	}

	dbPath := filepath.Join(dir, ".hubsync", "client.db")
	dbData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read tree db: %w", err)
	}
	if err := os.WriteFile(dbPath, dbData, 0644); err != nil {
		return fmt.Errorf("write tree db: %w", err)
	}

	// Open and replace our store
	db, err := OpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("open tree db: %w", err)
	}

	newStore, err := NewClientStore(db)
	if err != nil {
		db.Close()
		return err
	}
	*c.Store = *newStore
	return nil
}

func (c *Client) bootstrapBlobs(ctx context.Context) error {
	tree, err := c.Store.TreeSnapshot()
	if err != nil {
		return fmt.Errorf("load tree: %w", err)
	}

	// Deduplicate by digest — multiple paths may share the same content
	type blobTarget struct {
		path string
		mode os.FileMode
	}
	byDigest := make(map[Digest][]blobTarget)
	for path, entry := range tree {
		if entry.Kind != FileKindFile {
			continue
		}
		byDigest[entry.Digest] = append(byDigest[entry.Digest], blobTarget{path, os.FileMode(entry.Mode)})
	}

	log.Printf("bootstrap: fetching %d unique blobs for %d files", len(byDigest), len(tree))

	for digest, targets := range byDigest {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Fetch the blob once
		firstTarget := targets[0]
		fullPath := filepath.Join(c.syncDir, firstTarget.path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return err
		}
		if err := c.fetchFullBlob(digest, fullPath, firstTarget.mode); err != nil {
			return fmt.Errorf("fetch blob %s for %s: %w", digest.Hex()[:12], firstTarget.path, err)
		}

		// Copy to other targets with same digest
		if len(targets) > 1 {
			data, err := os.ReadFile(fullPath)
			if err != nil {
				return err
			}
			for _, t := range targets[1:] {
				destPath := filepath.Join(c.syncDir, t.path)
				if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
					return err
				}
				if err := os.WriteFile(destPath, data, t.mode); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// Sync connects to the hub and applies changes. Blocks until ctx is cancelled
// or the connection drops. Reconnects on failure.
// In write mode, also runs a periodic push loop.
func (c *Client) Sync(ctx context.Context) error {
	if c.writeMode {
		return c.syncReadWrite(ctx)
	}
	return c.syncReadOnly(ctx)
}

func (c *Client) syncReadOnly(ctx context.Context) error {
	for {
		err := c.syncOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("sync connection lost: %v, reconnecting in 1s", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c *Client) syncReadWrite(ctx context.Context) error {
	// Start push loop in background
	go c.pushLoop(ctx)

	// Run sync stream with reconnect
	for {
		// Refresh local edit paths before connecting
		c.refreshLocalEditPaths()

		err := c.syncOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("sync connection lost: %v, reconnecting in 1s", err)

		// Push any local changes before reconnecting
		c.doPush()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c *Client) pushLoop(ctx context.Context) {
	// Do an initial push immediately
	c.doPush()

	ticker := time.NewTicker(c.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.doPush()
		}
	}
}

func (c *Client) doPush() {
	accepted, err := c.Push(ConflictPolicy_FAIL)
	if err != nil {
		log.Printf("push error: %v", err)
		return
	}
	if accepted > 0 {
		// Refresh local edit paths after push
		c.refreshLocalEditPaths()
	}
	if c.OnPush != nil {
		c.OnPush(accepted)
	}
}

// SyncOnce connects to the subscribe endpoint and processes events until error.
func (c *Client) syncOnce(ctx context.Context) error {
	version, err := c.Store.HubVersion()
	if err != nil {
		return fmt.Errorf("get hub version: %w", err)
	}

	url := fmt.Sprintf("%s/sync/subscribe?since=%d", c.hubURL, version)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("subscribe response %s: %s", resp.Status, body)
	}

	log.Printf("connected to hub, syncing from version %d", version)

	// Read length-prefixed protobuf stream
	for {
		var ev SyncEvent
		if err := ReadLengthPrefixed(resp.Body, &ev); err != nil {
			return fmt.Errorf("read event: %w", err)
		}

		if err := c.applyEvent(&ev); err != nil {
			return fmt.Errorf("apply event v%d %s: %w", ev.Version, ev.Path, err)
		}
	}
}

// applyEvent processes a single sync event.
func (c *Client) applyEvent(ev *SyncEvent) error {
	version := int64(ev.Version)
	path := ev.Path
	fullPath := filepath.Join(c.syncDir, path)
	skipFS := c.skipPath(path)

	switch e := ev.Event.(type) {
	case *SyncEvent_Change:
		change := e.Change
		digest, _ := ParseDigest(fmt.Sprintf("%x", change.Digest))

		if !skipFS {
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				return err
			}

			if len(change.Data) > 0 {
				// Small file: data is inlined
				if err := os.WriteFile(fullPath, change.Data, os.FileMode(change.Mode)); err != nil {
					return err
				}
			} else {
				// Large file: try delta if we have an old version, else full fetch
				if err := c.fetchLargeFile(digest, path, fullPath, os.FileMode(change.Mode)); err != nil {
					return err
				}
			}

			// Set mtime
			mtime := time.Unix(change.Mtime, 0)
			os.Chtimes(fullPath, mtime, mtime)
		}

		// Always update hub_tree (even if we skipped FS write)
		if err := c.Store.ApplyChange(version, path, OpCreate, FileKind(change.Kind), digest, int64(change.Size), change.Mode, change.Mtime); err != nil {
			return err
		}

		if c.OnEvent != nil {
			c.OnEvent(version, path, OpCreate)
		}

	case *SyncEvent_Delete:
		if !skipFS {
			os.Remove(fullPath)
			// Try to remove empty parent dirs
			dir := filepath.Dir(fullPath)
			for dir != c.syncDir {
				if err := os.Remove(dir); err != nil {
					break
				}
				dir = filepath.Dir(dir)
			}
		}

		if err := c.Store.ApplyChange(version, path, OpDelete, FileKindFile, Digest{}, 0, 0, 0); err != nil {
			return err
		}

		if c.OnEvent != nil {
			c.OnEvent(version, path, OpDelete)
		}
	}

	return nil
}

// fetchLargeFile fetches a large file using the best available strategy:
// 1. Dedup: if the digest already exists locally at another path, copy it
// 2. Delta: if we have an old version at the same path, use rsync delta
// 3. Full fetch: download the entire blob
func (c *Client) fetchLargeFile(digest Digest, relPath, destPath string, mode os.FileMode) error {
	// Try dedup: same digest at a different path
	existingPath, found, err := c.Store.LookupByDigest(digest, relPath)
	if err == nil && found {
		srcPath := filepath.Join(c.syncDir, existingPath)
		if data, err := os.ReadFile(srcPath); err == nil {
			return os.WriteFile(destPath, data, mode)
		}
	}

	// Try delta: if we have a local file at this path, send its signature
	if localData, err := os.ReadFile(destPath); err == nil && len(localData) > 0 {
		data, err := c.fetchDelta(digest, localData)
		if err == nil {
			return os.WriteFile(destPath, data, mode)
		}
		log.Printf("delta fetch failed for %s, falling back to full: %v", relPath, err)
	}

	// Full fetch
	return c.fetchFullBlob(digest, destPath, mode)
}

// fetchDelta sends block signatures of the local file to the hub and
// applies the returned delta to reconstruct the target file.
func (c *Client) fetchDelta(targetDigest Digest, localData []byte) ([]byte, error) {
	blockSize := OptimalBlockSize(int64(len(localData)))
	sigs := ComputeSignature(localData, blockSize)

	// Build protobuf request
	req := &DeltaRequest{
		TargetDigest: targetDigest[:],
		BlockSize:    uint32(blockSize),
	}
	for _, sig := range sigs {
		req.Signature = append(req.Signature, &BlockSignature{
			Index:      sig.Index,
			WeakHash:   sig.WeakHash,
			StrongHash: sig.StrongHash[:],
		})
	}

	reqData, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal delta request: %w", err)
	}

	// POST to hub
	httpReq, err := http.NewRequest("POST", c.hubURL+"/blobs/delta", bytes.NewReader(reqData))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	c.setAuth(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("delta request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("delta response %s: %s", resp.Status, body)
	}

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var deltaResp DeltaResponse
	if err := proto.Unmarshal(respData, &deltaResp); err != nil {
		return nil, fmt.Errorf("unmarshal delta response: %w", err)
	}

	// Apply delta
	ops := DeltaOpsFromProto(&deltaResp)
	result := ApplyDelta(localData, ops, blockSize)

	// Verify digest
	got := ComputeDigest(result)
	if got != targetDigest {
		return nil, fmt.Errorf("delta result digest mismatch: got %s, want %s", got.Hex(), targetDigest.Hex())
	}

	return result, nil
}

// fetchFullBlob downloads the entire blob from the hub.
func (c *Client) fetchFullBlob(digest Digest, destPath string, mode os.FileMode) error {
	url := fmt.Sprintf("%s/blobs/%s", c.hubURL, digest.Hex())
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("create blob request: %w", err)
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch blob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("blob response: %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return os.WriteFile(destPath, data, mode)
}

func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+string(c.token))
	}
}
