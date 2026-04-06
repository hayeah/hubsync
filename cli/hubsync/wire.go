//go:build wireinject

package main

import (
	"github.com/google/wire"
	"github.com/hayeah/hubsync"
)

func InitializeHubApp(cfg *hubsync.HubConfig) (*hubsync.HubApp, func(), error) {
	wire.Build(hubsync.HubProviderSet)
	return nil, nil, nil
}

func InitializeClientApp(cfg *hubsync.ClientConfig) (*hubsync.ClientApp, func(), error) {
	wire.Build(hubsync.ClientProviderSet)
	return nil, nil, nil
}
