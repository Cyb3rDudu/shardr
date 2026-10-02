package main

import (
	"fmt"

	"github.com/Cyb3rDudu/shardr/internal/catalog"
	"github.com/Cyb3rDudu/shardr/internal/config"
	"github.com/Cyb3rDudu/shardr/internal/swarm"
)

// [swarm] extraction (004 §7) on the shared parser (internal/config).
// Keys outside [swarm] belong to other components and are not ours to
// validate ([references], [runtimes.*], [models.*] — the runner owns
// those). Unknown [swarm] keys are a loud error (fail closed).

const swarmSection = "swarm"
const catalogSection = "catalog"

var swarmKeys = map[string]bool{
	"enabled": true, "seed": true, "upload_limit": true, "dht": true,
	"no_seed_verify": true, "webseed_addr": true,
}

var catalogKeys = map[string]bool{
	"upload_limit": true,
	"url":          true,
}

// loadSwarmConfig reads config.toml and returns the [swarm] config, or
// defaults when the file is absent. A present-but-malformed file, or an
// unknown [swarm] key, is a loud error (fail closed).
func loadSwarmConfig() (swarm.Config, error) {
	cfg := swarm.DefaultConfig()
	cfg.NoSeedVerify = false
	f, err := config.Load()
	if err != nil {
		return cfg, err
	}
	sec, ok := f[swarmSection]
	if !ok {
		return cfg, nil
	}
	// Report unknown keys in file order for stable messages.
	for _, key := range sortedKeys(sec) {
		if !swarmKeys[key] {
			return cfg, fmt.Errorf("config %s: unknown [swarm] key %q (known: enabled, seed, upload_limit, dht, no_seed_verify, webseed_addr)", configPathForError(), key)
		}
	}
	for key, v := range sec {
		var err error
		switch key {
		case "enabled":
			cfg.Enabled, err = wantBool(key, v)
		case "seed":
			cfg.Seed, err = wantBool(key, v)
		case "dht":
			cfg.DHT, err = wantBool(key, v)
		case "no_seed_verify":
			cfg.NoSeedVerify, err = wantBool(key, v)
		case "upload_limit":
			cfg.UploadLimit, err = wantInt(key, v)
			if err == nil && cfg.UploadLimit < 0 {
				return cfg, fmt.Errorf("config: upload_limit must be >= 0")
			}
		case "webseed_addr":
			cfg.WebseedAddr = v.Str
		}
		if err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// unknownCatalogKey validates the [catalog] key set once — both
// resolvers call it, so any unknown key is loud whichever value the
// daemon reads first (fail closed on the section, not the key).
func unknownCatalogKey(sec map[string]config.Value) error {
	for _, key := range sortedKeys(sec) {
		if !catalogKeys[key] {
			return fmt.Errorf("config %s: unknown [catalog] key %q (known: upload_limit, url)", configPathForError(), key)
		}
	}
	return nil
}

// resolveCatalogUploadLimit resolves the good-citizen seeding budget
// (Epic #65 ruling 2): [catalog] upload_limit when set, else INHERIT
// [swarm] upload_limit (same semantics, 0 = unlimited). Unknown
// [catalog] keys are a loud error (fail closed, like [swarm]).
func resolveCatalogUploadLimit(f config.File, swarmUploadLimit int64) (int64, error) {
	sec, ok := f[catalogSection]
	if !ok {
		return swarmUploadLimit, nil
	}
	if err := unknownCatalogKey(sec); err != nil {
		return 0, err
	}
	v, ok := sec["upload_limit"]
	if !ok {
		return swarmUploadLimit, nil
	}
	if v.Kind != config.KindInt {
		return 0, fmt.Errorf("config: [catalog] upload_limit must be an integer")
	}
	if v.Int < 0 {
		return 0, fmt.Errorf("config: [catalog] upload_limit must be >= 0")
	}
	return v.Int, nil
}

// loadCatalogUploadLimit is the standalone form (config loaded here).
func loadCatalogUploadLimit(swarmUploadLimit int64) (int64, error) {
	f, err := config.Load()
	if err != nil {
		return 0, err
	}
	return resolveCatalogUploadLimit(f, swarmUploadLimit)
}

// resolveCatalogURL resolves [catalog] url (Epic #65 follow-up):
// "" when unset — the provider then applies its own default
// (DefaultPiratefaceURL; behavior unchanged). The value rule itself is
// single-sourced in catalog.ResolveConfigURL (shared with the CLI so
// the two cannot drift); this wrapper adds the daemon's fail-closed
// section check: any unknown [catalog] key is loud here, whichever
// resolver runs first.
func resolveCatalogURL(f config.File) (string, error) {
	sec, ok := f[catalogSection]
	if !ok {
		return "", nil
	}
	if err := unknownCatalogKey(sec); err != nil {
		return "", err
	}
	return catalog.ResolveConfigURL(f)
}

// loadCatalogURL is the standalone form (config loaded here).
func loadCatalogURL() (string, error) {
	f, err := config.Load()
	if err != nil {
		return "", err
	}
	return resolveCatalogURL(f)
}

func sortedKeys(sec map[string]config.Value) []string {
	keys := make([]string, 0, len(sec))
	for k := range sec {
		keys = append(keys, k)
	}
	// insertion order is lost by the map; sort for determinism
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

func wantBool(key string, v config.Value) (bool, error) {
	if v.Kind != config.KindBool {
		return false, fmt.Errorf("config: [swarm] %s must be true/false", key)
	}
	return v.Bool, nil
}

func wantInt(key string, v config.Value) (int64, error) {
	if v.Kind != config.KindInt {
		return 0, fmt.Errorf("config: [swarm] %s must be an integer", key)
	}
	return v.Int, nil
}

func configPathForError() string {
	if p, err := config.Path(); err == nil {
		return p
	}
	return "config.toml"
}
