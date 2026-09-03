package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/swarm"
)

// config.toml loading (004 §7): a documented minimal subset parser —
// sections, key = value (bool/int/string), # comments on their own line
// or inline after a value (outside quoted strings, TOML semantics — the
// 004 §7 example config uses inline comments). No arrays, no nested
// tables beyond dotted section names (the config surface is local-node
// knobs only; protocol-affecting knobs do not exist). Anything the parser
// does not understand is a LOUD error, never silently ignored — a typo
// must not quietly disable seeding.
//
// ponytail: hand-rolled subset instead of a TOML dependency — the dep
// budget for this slice is anacrolix/torrent + transitive only.

const swarmSection = "swarm"

// swarmKeys are the known [swarm] keys. Keys outside [swarm] are ignored
// (other components own them: [references], [runtimes.*], [models.*]).
var swarmKeys = map[string]bool{
	"enabled": true, "seed": true, "upload_limit": true, "dht": true,
	"no_seed_verify": true, "webseed_addr": true,
}

// configPath resolves the config file location: $SHARDR_CONFIG if set,
// else $XDG_CONFIG_HOME/shardr/config.toml, else ~/.config/shardr/config.toml.
func configPath() (string, error) {
	if p := os.Getenv("SHARDR_CONFIG"); p != "" {
		return p, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "shardr", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "shardr", "config.toml"), nil
}

// loadSwarmConfig reads config.toml and returns the [swarm] config, or
// defaults when the file is absent. A present-but-malformed file, or an
// unknown [swarm] key, is a loud error (fail closed).
func loadSwarmConfig() (swarm.Config, error) {
	cfg := swarm.DefaultConfig()
	cfg.NoSeedVerify = false
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil // no config = documented defaults
	}
	if err != nil {
		return cfg, err
	}
	section := ""
	for ln, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section == swarmSection {
				continue
			}
			// Other sections belong to other components; keys inside them
			// are not ours to validate.
			continue
		}
		if section != swarmSection {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return cfg, fmt.Errorf("config %s:%d: expected key = value, got %q", path, ln+1, line)
		}
		key = strings.TrimSpace(key)
		val = stripComment(strings.TrimSpace(val))
		if !swarmKeys[key] {
			return cfg, fmt.Errorf("config %s:%d: unknown [swarm] key %q (known: enabled, seed, upload_limit, dht, no_seed_verify, webseed_addr)", path, ln+1, key)
		}
		unq := strings.Trim(val, `"`)
		switch key {
		case "enabled":
			cfg.Enabled, err = parseBool(path, ln, unq)
		case "seed":
			cfg.Seed, err = parseBool(path, ln, unq)
		case "dht":
			cfg.DHT, err = parseBool(path, ln, unq)
		case "no_seed_verify":
			cfg.NoSeedVerify, err = parseBool(path, ln, unq)
		case "upload_limit":
			cfg.UploadLimit, err = parseInt(path, ln, unq)
			if err == nil && cfg.UploadLimit < 0 {
				return cfg, fmt.Errorf("config %s:%d: upload_limit must be >= 0", path, ln+1)
			}
		case "webseed_addr":
			cfg.WebseedAddr = unq
		}
		if err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// stripComment removes a TOML inline comment (# to end of line) from a
// value, honoring double-quoted strings: a # inside quotes does not
// start a comment.
func stripComment(v string) string {
	inQuote := false
	for i, r := range v {
		switch r {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return strings.TrimSpace(v[:i])
			}
		}
	}
	return v
}

func parseBool(path string, ln int, v string) (bool, error) {
	switch v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("config %s:%d: want true/false, got %q", path, ln+1, v)
}

func parseInt(path string, ln int, v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config %s:%d: want integer, got %q", path, ln+1, v)
	}
	return n, nil
}
