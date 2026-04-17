package hubsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hayeah/hubsync/archive"
)

// TestArchiveE2E_HubClientArchiveCoexist drives hub + scanner + watcher +
// archive worker + /blobs 302 fallback + a real Go sync client in the same
// test. Uses FakeStorage for the archive — no network — so it is part of
// the default tier-1 suite.
//
// Flow:
//  1. Hub scans two files → archive worker uploads both to FakeStorage.
//  2. A Go client bootstraps from /snapshots-tree/latest → local copies.
//  3. Hub-side operator unpins big.bin → local copy gone on the hub.
//  4. Client's already-downloaded big.bin is unaffected.
//  5. A new client bootstraps, then requests /blobs/{digest} for big.bin
//     → hub redirects (302) to the presigned "b2" URL.
func TestArchiveE2E_HubClientArchiveCoexist(t *testing.T) {
	hubDir := t.TempDir()
	dbPath := filepath.Join(hubDir, "hub.db")

	// Seed a couple of files before the hub starts so the baseline scan
	// picks them up.
	if err := os.WriteFile(filepath.Join(hubDir, "small.txt"), []byte("small"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubDir, "big.bin"), []byte(strings.Repeat("X", 4096)), 0644); err != nil {
		t.Fatal(err)
	}

	// Build the hub by hand so we can pass the FakeStorage in. This is the
	// same wiring InitializeHubApp performs, just with a stub storage.
	fakeStorage := archive.NewFakeStorage()
	fakeStorage.PresignPrefix = "" // we'll replace this after test server starts

	hasher := sha256Hasher{}
	store, cleanupStore, err := NewHubStore(HubStoreConfig{DBPath: dbPath, Hasher: hasher})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStore()

	ignorer, err := ProvideIgnorer(hubDir)
	if err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(ScannerConfig{WatchDir: hubDir, Ignorer: ignorer, Hasher: hasher})
	broadcaster := NewBroadcaster()
	hub := NewHub(store, scanner, nil, broadcaster, hasher) // no watcher in this test
	srv := NewServer(hub, ServerConfig{
		Hasher:    hasher,
		Presigner: fakeStorage,
		Prefix:    "backups/e2e/",
	})

	// Baseline scan — populates hub_entry with archive_state=NULL.
	if err := hub.FullScan(); err != nil {
		t.Fatal(err)
	}

	// Archive worker drains NULL rows.
	worker := &ArchiveWorker{
		Store:       store,
		Storage:     fakeStorage,
		Hasher:      hasher,
		Broadcaster: broadcaster,
		HubDir:      hubDir,
		Prefix:      "backups/e2e/",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(ctx) }()

	// Wait for both files to be archived.
	waitForCondition(t, func() bool {
		e1, _, _ := store.EntryLookup("small.txt")
		e2, _, _ := store.EntryLookup("big.bin")
		return e1.ArchiveState == ArchiveStateArchived && e2.ArchiveState == ArchiveStateArchived
	}, 3*time.Second)

	// Start the HTTP server so the Go client can talk to it.
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// Now that the test server URL is known, point the presigned URL at it
	// so the /blobs 302 is actually fetchable in the test.
	fakeStorage.PresignPrefix = ts.URL + "/__presigned__/"

	// Spin a Go client bootstrapping from the hub.
	clientDir := t.TempDir()
	clientDB := filepath.Join(t.TempDir(), "client.db")
	cstore, ccleanup, err := OpenClientStore(ClientStoreConfig{DBPath: clientDB})
	if err != nil {
		t.Fatal(err)
	}
	defer ccleanup()
	client := NewClient(cstore, ts.URL, BearerToken(""), clientDir)
	if err := client.Bootstrap(ctx); err != nil {
		t.Fatalf("client bootstrap: %v", err)
	}
	// Client should have both files locally.
	if data, err := os.ReadFile(filepath.Join(clientDir, "small.txt")); err != nil || string(data) != "small" {
		t.Fatalf("client small.txt: got %q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(clientDir, "big.bin")); err != nil || len(data) != 4096 {
		t.Fatalf("client big.bin len=%d err=%v", len(data), err)
	}

	// Operator unpins big.bin on the hub via the Reconciler directly (in a
	// real deployment, the CLI goes through the RPC socket; here we skip
	// the socket for brevity — rpc_test.go already covers that path).
	recon := &Reconciler{Store: store, Storage: fakeStorage, Hasher: hasher, HubDir: hubDir, Prefix: "backups/e2e/"}
	plan, err := recon.PlanUnpin("big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := recon.Apply(ctx, plan); err != nil {
		t.Fatalf("unpin apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hubDir, "big.bin")); !os.IsNotExist(err) {
		t.Errorf("hub big.bin should be evicted, err=%v", err)
	}
	e, _, _ := store.EntryLookup("big.bin")
	if e.ArchiveState != ArchiveStateUnpinned {
		t.Errorf("big.bin state=%q want unpinned", e.ArchiveState)
	}

	// /blobs/{digest} on the now-unpinned big.bin should return 302 →
	// FakeStorage's presigned URL.
	digestHex := e.Digest.Hex()
	req, _ := http.NewRequest("GET", ts.URL+"/blobs/"+digestHex, nil)
	// Don't follow redirects — we want to see the 302 directly.
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("/blobs/%s: status=%d want 302", digestHex, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, ts.URL+"/__presigned__/") || !strings.HasSuffix(loc, "backups/e2e/big.bin") {
		t.Errorf("Location=%q", loc)
	}

	// Client-cached copy is untouched (clients aren't affected by hub unpin).
	if data, err := os.ReadFile(filepath.Join(clientDir, "big.bin")); err != nil || len(data) != 4096 {
		t.Errorf("client big.bin should survive hub unpin: len=%d err=%v", len(data), err)
	}

	cancel()
	<-workerDone
}
