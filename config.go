package hubsync

// HubConfig holds all configuration for the hub server.
type HubConfig struct {
	DBPath   string
	WatchDir string
	Token    string
	Listen   string
}

// ClientConfig holds all configuration for the client.
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
