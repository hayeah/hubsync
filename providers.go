package hubsync

import (
	"time"

	"github.com/google/wire"
)

// HubApp is the top-level hub container.
type HubApp struct {
	Server *Server
	Hub    *Hub
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
func ProvideServerConfig(cfg *HubConfig, h Hasher) ServerConfig {
	return ServerConfig{Listen: cfg.Listen, Token: BearerToken(cfg.Token), Hasher: h}
}

// ProvideClientStoreConfig assembles ClientStoreConfig from the root ClientConfig.
func ProvideClientStoreConfig(cfg *ClientConfig) ClientStoreConfig {
	return ClientStoreConfig{DBPath: cfg.DBPath}
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
