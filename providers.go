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

// HubProviderSet provides all hub-side dependencies.
var HubProviderSet = wire.NewSet(
	ProvideHubStore,
	ProvideScanner,
	ProvideWatcher,
	NewBroadcaster,
	NewHub,
	ProvideServer,
	wire.Struct(new(HubApp), "*"),
)

// ClientProviderSet provides all client-side dependencies.
var ClientProviderSet = wire.NewSet(
	ProvideClientStore,
	ProvideClient,
	wire.Struct(new(ClientApp), "*"),
)

// ProvideHubStore opens the hub DB and creates a HubStore.
func ProvideHubStore(cfg *HubConfig) (*HubStore, func(), error) {
	db, err := OpenDB(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	store, err := NewHubStore(db)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return store, func() { db.Close() }, nil
}

// ProvideScanner creates a Scanner from HubConfig.
func ProvideScanner(cfg *HubConfig) (*Scanner, error) {
	ignorer, err := ProvideIgnorer(cfg.WatchDir)
	if err != nil {
		return nil, err
	}
	return NewScanner(cfg.WatchDir, ignorer), nil
}

// ProvideWatcher creates a Watcher from HubConfig.
func ProvideWatcher(cfg *HubConfig) (*Watcher, func(), error) {
	return NewWatcher(cfg.WatchDir)
}

// ProvideServer creates a Server from HubConfig.
func ProvideServer(hub *Hub, cfg *HubConfig) *Server {
	return NewServer(hub, BearerToken(cfg.Token), cfg.Listen)
}

// ProvideClientStore opens the client DB and creates a ClientStore.
func ProvideClientStore(cfg *ClientConfig) (*ClientStore, func(), error) {
	db, err := OpenDB(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	store, err := NewClientStore(db)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return store, func() { db.Close() }, nil
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
