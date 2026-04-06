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

	"google.golang.org/protobuf/proto"
)

// Client is a read-only replica that syncs from a hub.
type Client struct {
	Store   *ClientStore
	hubURL  string
	token   BearerToken
	syncDir string
	http    *http.Client

	// OnEvent is called after each sync event is applied (for testing).
	OnEvent func(version int64, path string, op ChangeOp)
}

// NewClient creates a Client.
func NewClient(store *ClientStore, hubURL string, token BearerToken, syncDir string) *Client {
	return &Client{
		Store:   store,
		hubURL:  hubURL,
		token:   token,
		syncDir: syncDir,
		http: &http.Client{
			Timeout: 0, // no timeout for streaming
		},
	}
}

// Bootstrap downloads the latest snapshot and extracts it.
func (c *Client) Bootstrap(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.hubURL+"/snapshots/latest", nil)
	if err != nil {
		return err
	}
	c.setAuth(req)

	// Follow redirects manually to handle the 302
	c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		c.setAuth(req)
		return nil
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch snapshot: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("snapshot response: %s", resp.Status)
	}

	dir := c.syncDir
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	dbPath, err := ExtractSnapshot(resp.Body, dir)
	if err != nil {
		return fmt.Errorf("extract snapshot: %w", err)
	}

	if dbPath == "" {
		return fmt.Errorf("snapshot missing .hubsync/hub_tree.db")
	}

	// Open the extracted DB and replace our store's DB connection
	db, err := OpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("open snapshot db: %w", err)
	}

	newStore, err := NewClientStore(db)
	if err != nil {
		db.Close()
		return err
	}
	*c.Store = *newStore

	version, _ := c.Store.HubVersion()
	log.Printf("bootstrap complete, version=%d", version)
	return nil
}

// Sync connects to the hub and applies changes. Blocks until ctx is cancelled
// or the connection drops. Reconnects on failure.
func (c *Client) Sync(ctx context.Context) error {
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

	switch e := ev.Event.(type) {
	case *SyncEvent_Change:
		change := e.Change
		digest, _ := ParseDigest(fmt.Sprintf("%x", change.Digest))

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

		// Update store
		if err := c.Store.ApplyChange(version, path, OpCreate, FileKind(change.Kind), digest, int64(change.Size), change.Mode, change.Mtime); err != nil {
			return err
		}

		if c.OnEvent != nil {
			c.OnEvent(version, path, OpCreate)
		}

	case *SyncEvent_Delete:
		os.Remove(fullPath)
		// Try to remove empty parent dirs
		dir := filepath.Dir(fullPath)
		for dir != c.syncDir {
			if err := os.Remove(dir); err != nil {
				break
			}
			dir = filepath.Dir(dir)
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
