package hubsync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// ConfigFile is the on-disk .hubsync/config.toml.
type ConfigFile struct {
	Hub     HubSection     `toml:"hub"`
	Archive *ArchiveConfig `toml:"archive"`
}

// HubSection is the [hub] section of config.toml.
type HubSection struct {
	// Hash is the content-hash algorithm used for hub_entry.digest and
	// change_log.digest. One of "xxh128", "sha256". Set once at init;
	// changing it later is out of scope (spec: hash is set-once).
	Hash string `toml:"hash"`
}

// ArchiveConfig is the [archive] section of config.toml.
type ArchiveConfig struct {
	Provider         string        `toml:"provider"`
	Bucket           string        `toml:"bucket"`
	BucketPrefix     string        `toml:"bucket_prefix"`
	B2KeyID          string        `toml:"b2_key_id"`
	B2AppKey         string        `toml:"b2_app_key"`
	LargeFileBytes   int64         `toml:"large_file_threshold"`
	ArchiveWorkers   int           `toml:"archive_workers"`
	HashWorkers      int           `toml:"hash_workers"`
	DebounceBase     duration      `toml:"debounce_base"`
	DebouncePer100MB duration      `toml:"debounce_per_100mb"`
	DebounceMax      duration      `toml:"debounce_max"`
}

// duration is a TOML-unmarshaling wrapper around time.Duration.
type duration time.Duration

func (d *duration) UnmarshalText(text []byte) error {
	td, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = duration(td)
	return nil
}

// Duration converts to the stdlib type.
func (d duration) Duration() time.Duration { return time.Duration(d) }

// Supported hash algorithms.
const (
	HashXXH128 = "xxh128"
	HashSHA256 = "sha256"
)

// DefaultHash is the algorithm used when .hubsync/config.toml is absent or has
// no [hub] hash set on a fresh hub.
const DefaultHash = HashXXH128

// LoadConfigFile reads .hubsync/config.toml under hubDir. If the file is
// absent, returns a ConfigFile with DefaultHash. Fills in any missing defaults
// on the returned value.
//
// Selected [archive] string fields (bucket, bucket_prefix, b2_key_id,
// b2_app_key) are run through os.ExpandEnv so the template can reference
// process-env vars like ${HUBSYNC_BUCKET} or ${B2_APPLICATION_KEY_ID} without
// baking secrets into the committed config.
func LoadConfigFile(hubDir string) (*ConfigFile, error) {
	path := filepath.Join(hubDir, ".hubsync", "config.toml")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &ConfigFile{Hub: HubSection{Hash: DefaultHash}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cf ConfigFile
	if err := toml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cf.Hub.Hash == "" {
		cf.Hub.Hash = DefaultHash
	}
	if cf.Archive != nil {
		cf.Archive.Bucket = os.ExpandEnv(cf.Archive.Bucket)
		cf.Archive.BucketPrefix = os.ExpandEnv(cf.Archive.BucketPrefix)
		cf.Archive.B2KeyID = os.ExpandEnv(cf.Archive.B2KeyID)
		cf.Archive.B2AppKey = os.ExpandEnv(cf.Archive.B2AppKey)
		if cf.Archive.BucketPrefix == "" {
			cf.Archive.BucketPrefix = filepath.Base(hubDir) + "/"
		}
	}
	if err := cf.validate(); err != nil {
		return nil, err
	}
	return &cf, nil
}

func (c *ConfigFile) validate() error {
	switch c.Hub.Hash {
	case HashXXH128, HashSHA256:
	default:
		return fmt.Errorf("unknown [hub] hash %q (want %q or %q)", c.Hub.Hash, HashXXH128, HashSHA256)
	}
	if c.Archive != nil {
		if c.Archive.Provider != "" && c.Archive.Provider != "b2" {
			return fmt.Errorf("unknown [archive] provider %q", c.Archive.Provider)
		}
	}
	return nil
}
