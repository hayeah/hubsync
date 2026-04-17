package hubsync

import "github.com/hayeah/go-lstree"

// HubConfig is the user-facing hub configuration (populated by the CLI from
// flags + .hubsync/config.toml). Per-type providers in providers.go derive
// narrower TypeNameConfig structs from this.
type HubConfig struct {
	DBPath   string
	WatchDir string
	Token    string
	Listen   string
	// HashAlgo is the content-hash algorithm ("xxh128" or "sha256"). Leave
	// empty to defer to .hubsync/config.toml or DefaultHash.
	HashAlgo string
}

// ClientConfig is the user-facing client configuration.
type ClientConfig struct {
	DBPath       string
	HubURL       string
	Token        string
	SyncDir      string
	Mode         string // "read" or "write" (default: "read")
	ScanInterval string // duration string for write mode scan interval (default: "5s")
}

// BearerToken returns the token for auth middleware.
func (c *HubConfig) BearerToken() BearerToken {
	return BearerToken(c.Token)
}

// ResolveHashAlgo returns the hub's configured hash algorithm, preferring the
// explicit HubConfig field, then .hubsync/config.toml, else DefaultHash.
func (c *HubConfig) ResolveHashAlgo() (string, error) {
	if c.HashAlgo != "" {
		return c.HashAlgo, nil
	}
	cf, err := LoadConfigFile(c.WatchDir)
	if err != nil {
		return "", err
	}
	return cf.Hub.Hash, nil
}

// HubStoreConfig bundles what NewHubStore needs: the DB path and the hasher
// (the hasher's algo name is recorded in hub_config_cache on first run).
type HubStoreConfig struct {
	DBPath string
	Hasher Hasher
}

// ScannerConfig bundles what NewScanner needs.
type ScannerConfig struct {
	WatchDir string
	Ignorer  lstree.Ignorer
	Hasher   Hasher
}

// WatcherConfig bundles what NewWatcher needs.
type WatcherConfig struct {
	WatchDir string
}

// ServerConfig bundles what NewServer needs.
type ServerConfig struct {
	Listen string
	Token  BearerToken
	Hasher Hasher
}

// ClientStoreConfig bundles what NewClientStore needs.
type ClientStoreConfig struct {
	DBPath string
}
