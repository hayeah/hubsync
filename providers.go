package hubsync

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/wire"
	"github.com/hayeah/hubsync/archive"
)

// HubApp is the top-level hub container. ArchiveWorker and RPC are present
// whenever a config.toml exists; ArchiveWorker is nil when [archive] is
// absent from the config.
type HubApp struct {
	Server        *Server
	Hub           *Hub
	ArchiveWorker *ArchiveWorker // nil when [archive] is not configured
	RPC           *RPCServer
}

// ClientApp is the top-level client container.
type ClientApp struct {
	Client *Client
}

// HubProviderSet wires all hub-side dependencies. Each per-type dependency has
// a TypeNameConfig assembled from the root *HubConfig; constructors take only
// that config plus peer-type deps. See docs/wire.md (skills/golang/wire.md).
var HubProviderSet = wire.NewSet(
	ProvideHubHasher,
	ProvideConfigFile,
	ProvideArchiveStorage,
	ProvideReconciler,
	ProvideArchiveWorker,
	ProvideRPCServer,
	ProvideHubStoreConfig, NewHubStore,
	ProvideScannerConfig, NewScanner,
	ProvideWatcherConfig, NewWatcher,
	ProvideServerConfig, NewServer,
	NewBroadcaster,
	NewHub,
	wire.Struct(new(HubApp), "*"),
)

// ClientProviderSet wires all client-side dependencies.
var ClientProviderSet = wire.NewSet(
	ProvideClientStoreConfig, OpenClientStore,
	ProvideClient,
	wire.Struct(new(ClientApp), "*"),
)

// ProvideHubHasher resolves the hub's configured hash algorithm and returns a
// Hasher used throughout the hub stack.
func ProvideHubHasher(cfg *HubConfig) (Hasher, error) {
	algo, err := cfg.ResolveHashAlgo()
	if err != nil {
		return nil, err
	}
	return NewHasher(algo)
}

// ProvideHubStoreConfig assembles HubStoreConfig from the root HubConfig + Hasher.
func ProvideHubStoreConfig(cfg *HubConfig, h Hasher) HubStoreConfig {
	return HubStoreConfig{DBPath: cfg.DBPath, Hasher: h}
}

// ProvideScannerConfig assembles ScannerConfig (includes resolving an Ignorer).
func ProvideScannerConfig(cfg *HubConfig, h Hasher) (ScannerConfig, error) {
	ig, err := ProvideIgnorer(cfg.WatchDir)
	if err != nil {
		return ScannerConfig{}, err
	}
	return ScannerConfig{WatchDir: cfg.WatchDir, Ignorer: ig, Hasher: h}, nil
}

// ProvideWatcherConfig assembles WatcherConfig from the root HubConfig.
func ProvideWatcherConfig(cfg *HubConfig) WatcherConfig {
	return WatcherConfig{WatchDir: cfg.WatchDir}
}

// ProvideServerConfig assembles ServerConfig from the root HubConfig + Hasher.
// Includes the archive hooks so /blobs/{digest} can 302 to a presigned URL
// when the local file is absent and the row is 'unpinned'.
func ProvideServerConfig(cfg *HubConfig, cf *ConfigFile, h Hasher, stor archive.ArchiveStorage) ServerConfig {
	out := ServerConfig{Listen: cfg.Listen, Token: BearerToken(cfg.Token), Hasher: h}
	if stor != nil {
		out.Presigner = stor
		if cf != nil && cf.Archive != nil {
			out.Prefix = cf.Archive.BucketPrefix
		}
	}
	return out
}

// ProvideClientStoreConfig assembles ClientStoreConfig from the root ClientConfig.
func ProvideClientStoreConfig(cfg *ClientConfig) ClientStoreConfig {
	return ClientStoreConfig{DBPath: cfg.DBPath}
}

// ProvideConfigFile loads .hubsync/config.toml (or returns defaults).
func ProvideConfigFile(cfg *HubConfig) (*ConfigFile, error) {
	return LoadConfigFile(cfg.WatchDir)
}

// ProvideArchiveStorage constructs the ArchiveStorage when [archive] is
// configured. Falls back to nil (workers see nil and stay idle).
// Credentials: prefer the config.toml values, else env. Env lookups are done
// here so the rest of the stack doesn't read process env directly.
func ProvideArchiveStorage(cf *ConfigFile) (archive.ArchiveStorage, func(), error) {
	if cf == nil || cf.Archive == nil {
		return nil, func() {}, nil
	}
	a := cf.Archive
	if a.Provider != "" && a.Provider != "b2" {
		return nil, nil, fmt.Errorf("archive provider %q not supported", a.Provider)
	}
	keyID := a.B2KeyID
	if keyID == "" {
		keyID = os.Getenv("B2_APPLICATION_KEY_ID")
	}
	appKey := a.B2AppKey
	if appKey == "" {
		appKey = os.Getenv("B2_APPLICATION_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, cleanup, err := archive.NewB2Storage(ctx, archive.B2Config{
		KeyID:        keyID,
		AppKey:       appKey,
		Bucket:       a.Bucket,
		BucketPrefix: a.BucketPrefix,
	})
	if err != nil {
		return nil, nil, err
	}
	return store, cleanup, nil
}

// ProvideReconciler wires a Reconciler. Returns nil if archive isn't configured.
func ProvideReconciler(cfg *HubConfig, cf *ConfigFile, store *HubStore, stor archive.ArchiveStorage, h Hasher) *Reconciler {
	if stor == nil {
		return nil
	}
	prefix := ""
	if cf != nil && cf.Archive != nil {
		prefix = cf.Archive.BucketPrefix
	}
	return &Reconciler{
		Store:   store,
		Storage: stor,
		Hasher:  h,
		HubDir:  cfg.WatchDir,
		Prefix:  prefix,
	}
}

// ProvideArchiveWorker wires the archive worker. Returns nil when archive
// isn't configured.
func ProvideArchiveWorker(cfg *HubConfig, cf *ConfigFile, store *HubStore, stor archive.ArchiveStorage, h Hasher, bc *Broadcaster) *ArchiveWorker {
	if stor == nil {
		return nil
	}
	prefix := ""
	workers := 0
	if cf != nil && cf.Archive != nil {
		prefix = cf.Archive.BucketPrefix
		workers = cf.Archive.ArchiveWorkers
	}
	return &ArchiveWorker{
		Store:       store,
		Storage:     stor,
		Hasher:      h,
		Broadcaster: bc,
		HubDir:      cfg.WatchDir,
		Prefix:      prefix,
		Workers:     workers,
	}
}

// ProvideRPCServer wires the RPC server. Always returned so pin/unpin CLIs
// work (RPC is a no-op for hubs without a Reconciler — handlers error out).
func ProvideRPCServer(cfg *HubConfig, store *HubStore, r *Reconciler) *RPCServer {
	return &RPCServer{
		Reconciler: r,
		Store:      store,
		Token:      BearerToken(cfg.Token),
		Socket:     RPCSocketPath(cfg.WatchDir),
	}
}

// ProvideClient creates a Client from ClientConfig.
func ProvideClient(store *ClientStore, cfg *ClientConfig) *Client {
	c := NewClient(store, cfg.HubURL, BearerToken(cfg.Token), cfg.SyncDir)
	if cfg.Mode == "write" {
		var interval time.Duration
		if cfg.ScanInterval != "" {
			interval, _ = time.ParseDuration(cfg.ScanInterval)
		}
		c.SetWriteMode(interval)
	}
	return c
}
