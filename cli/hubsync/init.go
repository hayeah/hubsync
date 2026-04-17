package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed template/config.toml
var defaultConfigTemplate []byte

// runInit scaffolds .hubsync/config.toml under dir. Returns the absolute path
// to the file that was written. Refuses to clobber an existing config.
func runInit(dir string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	hubsyncDir := filepath.Join(absDir, ".hubsync")
	if err := os.MkdirAll(hubsyncDir, 0755); err != nil {
		return "", fmt.Errorf("create %s: %w", hubsyncDir, err)
	}
	configPath := filepath.Join(hubsyncDir, "config.toml")
	if _, err := os.Stat(configPath); err == nil {
		return "", fmt.Errorf("%s already exists; edit it by hand or remove it first", configPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", configPath, err)
	}
	if err := os.WriteFile(configPath, defaultConfigTemplate, 0644); err != nil {
		return "", fmt.Errorf("write %s: %w", configPath, err)
	}
	return configPath, nil
}
