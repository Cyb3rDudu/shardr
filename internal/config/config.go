// Package config is the shared config.toml loader (004 §7): a
// documented minimal subset parser — sections, key = value
// (bool/int/string), # comments on their own line or inline after a
// value (outside quoted strings, TOML semantics — the 004 §7 example
// config uses inline comments). No arrays, no nested tables beyond
// dotted section names (the config surface is local-node knobs only;
// protocol-affecting knobs do not exist). Anything the parser does not
// understand is a LOUD error, never silently ignored — a typo must not
// quietly disable seeding.
//
// Section headers are kept VERBATIM (quoted segments included, e.g.
// [models."ns/name:quant".llama]); matching a section is the consumer's
// job. Unknown keys are also validated by the consumer — each component
// owns its own sections (004 §7: [swarm], [references], [runtimes.*],
// [models.*]).
//
// ponytail: hand-rolled subset instead of a TOML dependency — the dep
// budget is anacrolix/torrent + transitive only; both consumers
// (shardhive [swarm], shardr runner layers) share this one parser.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Kind tags the scalar type of a value.
type Kind int

const (
	KindString Kind = iota
	KindBool
	KindInt
)

// Value is one parsed config scalar.
type Value struct {
	Str  string
	Bool bool
	Int  int64
	Kind Kind
}

// File is the parsed config: section header (verbatim) → key → value.
type File map[string]map[string]Value

// Path resolves the config file location: $SHARDR_CONFIG if set, else
// $XDG_CONFIG_HOME/shardr/config.toml, else ~/.config/shardr/config.toml.
func Path() (string, error) {
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

// Load reads config.toml into a File, or an empty File when absent. A
// present-but-malformed file is a loud error (fail closed).
func Load() (File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return File{}, nil // no config = documented defaults
	}
	if err != nil {
		return nil, err
	}
	return Parse(path, string(data))
}

// Parse parses config bytes (subset grammar, loud on garbage). Exported
// for the runner's --config layer (same grammar as layer 2, 002 §2.1).
func Parse(path, data string) (File, error) {
	f := File{}
	section := ""
	for ln, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			// Section headers may carry inline comments (004 §7 example
			// uses them: [runtimes.llama] # overlay layer 2).
			line = stripComment(line)
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("config %s:%d: malformed section header %q", path, ln+1, raw)
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := f[section]; !ok {
				f[section] = map[string]Value{}
			}
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("config %s:%d: expected key = value, got %q", path, ln+1, line)
		}
		key = strings.TrimSpace(key)
		val = stripComment(strings.TrimSpace(val))
		v, err := parseValue(path, ln, val)
		if err != nil {
			return nil, err
		}
		if f[section] == nil {
			f[section] = map[string]Value{}
		}
		if _, dup := f[section][key]; dup {
			return nil, fmt.Errorf("config %s:%d: duplicate key %q in [%s]", path, ln+1, key, section)
		}
		f[section][key] = v
	}
	return f, nil
}

func parseValue(path string, ln int, val string) (Value, error) {
	if strings.HasPrefix(val, `"`) {
		if len(val) < 2 || !strings.HasSuffix(val, `"`) {
			return Value{}, fmt.Errorf("config %s:%d: unterminated string", path, ln+1)
		}
		return Value{Str: val[1 : len(val)-1], Kind: KindString}, nil
	}
	switch val {
	case "true":
		return Value{Bool: true, Kind: KindBool}, nil
	case "false":
		return Value{Bool: false, Kind: KindBool}, nil
	}
	if n, err := strconv.ParseInt(val, 10, 64); err == nil {
		return Value{Int: n, Kind: KindInt}, nil
	}
	return Value{}, fmt.Errorf("config %s:%d: want true/false, integer, or \"quoted string\", got %q", path, ln+1, val)
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
