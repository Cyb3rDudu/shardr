package catalog

import (
	"fmt"
	"net/url"

	"github.com/Cyb3rDudu/shardr/internal/config"
)

// ResolveConfigURL extracts and validates `[catalog] url` from a parsed
// config file (Epic #65 follow-up). It is the single source of the
// validation rule so the daemon (cmd/shardhive) and the CLI
// (internal/cli) cannot drift apart: a set value must be an ABSOLUTE
// https:// URL with a host and no path/query/fragment/userinfo (scheme
// + host, optional port); "" (unset) keeps the provider default.
func ResolveConfigURL(f config.File) (string, error) {
	sec, ok := f["catalog"]
	if !ok {
		return "", nil
	}
	v, ok := sec["url"]
	if !ok {
		return "", nil
	}
	if v.Kind != config.KindString {
		return "", fmt.Errorf("config: [catalog] url must be a quoted string")
	}
	if err := ValidateBaseURL(v.Str); err != nil {
		return "", err
	}
	return v.Str, nil
}

// ValidateBaseURL enforces the [catalog] url shape: absolute https://,
// host present, nothing but scheme + host (optional port).
func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("config: [catalog] url must be an absolute https:// URL with host and without path/query/fragment (scheme + host, optional port; unset = default %q), got %q", DefaultPiratefaceURL, raw)
	}
	return nil
}
