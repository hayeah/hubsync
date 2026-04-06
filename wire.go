//go:build wireinject

package hubsync

import (
	"github.com/google/wire"
)

// InitializeTestHubApp creates a HubApp for testing.
func InitializeTestHubApp(cfg *HubConfig) (*HubApp, func(), error) {
	wire.Build(HubProviderSet)
	return nil, nil, nil
}

// InitializeTestClientApp creates a ClientApp for testing.
func InitializeTestClientApp(cfg *ClientConfig) (*ClientApp, func(), error) {
	wire.Build(ClientProviderSet)
	return nil, nil, nil
}
