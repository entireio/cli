// Package cliauth holds CLI configuration (Config), credential resolution
// (login JWTs, break-glass tokens, ops-token exchange), and the HTTP
// client construction shared across the entiredb, entire-core, and
// entire-repo command trees.
package cliauth

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/caarlos0/env/v11"

	"github.com/entireio/cli/internal/entiredb/tokenstore"
)

type Config struct {
	EntireScheme      string
	EntireBaseURL     string `env:"ENTIRE_BASE_URL"`
	EntireHost        string
	ConfigDir         string `env:"ENTIRE_CONFIG_DIR"`
	SkipTLSVerify     bool   `env:"ENTIRE_TLS_SKIP_VERIFY"`
	EntireCoreBaseURL string `env:"ENTIRE_CORE_AUTH_BASE_URL"`
}

// CredentialHost returns the host used for credential storage and lookup.
// Derived from the host component of ENTIRE_BASE_URL — pointing at a cluster
// entry domain (e.g. royalcanin.partial.to) buckets creds per-cluster;
// pointing at a specific node forks them. Operators who want to bypass the
// credential store entirely set ENTIRE_TOKEN.
func (c *Config) CredentialHost() string {
	return c.EntireHost
}

// CredentialIssuerKey is the identity-of-the-issuer string credentials
// are filed under: the entire-core base URL the JWT issuer publishes
// at. A single login is visible whichever cluster it ends up being used
// against. Returns "" when ENTIRE_CORE_AUTH_BASE_URL is unset; callers that
// reach that path should already have failed with a clearer error.
//
// Used as the keyring service-name suffix (combined with the right
// prefix via KeyringService) and as the contexts.json CoreURL filter.
func (c *Config) CredentialIssuerKey() string {
	return strings.TrimRight(c.EntireCoreBaseURL, "/")
}

// KeyringService returns the tokenstore service name for the configured
// issuer ("entire-core:<core-url>"). Kept for the few callsites that
// know the issuer up front (e.g. tests). Most production callers should
// pull KeychainService off the resolved context instead.
func (c *Config) KeyringService() string {
	return tokenstore.KeyringServiceForIssuerKey(c.CredentialIssuerKey())
}

func NewConfig() Config {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		// Fallback to current directory if home directory is not available
		homeDir = "."
	}
	defaultConfigDir := filepath.Join(homeDir, ".config", "entire")

	return Config{
		EntireScheme: "https",
		ConfigDir:    defaultConfigDir,
	}
}

func (c *Config) Load() error {
	if err := env.Parse(c); err != nil {
		return fmt.Errorf("failed to parse environment variables: %w", err)
	}

	// ENTIRE_BASE_URL is optional now that the data-plane CLIs take the
	// cluster on the positional. It remains load-bearing for `entiredb
	// admin` ops/backup-token exchange; commands that need it surface a
	// clear error when it's unset.
	if c.EntireBaseURL == "" {
		return nil
	}

	parsedURL, err := url.Parse(c.EntireBaseURL)
	if err != nil {
		return fmt.Errorf("invalid URL format for ENTIRE_BASE_URL: %w", err)
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("ENTIRE_BASE_URL must include http:// or https:// protocol (got: %q)", c.EntireBaseURL)
	}

	if parsedURL.Host == "" {
		return fmt.Errorf("ENTIRE_BASE_URL must include a valid host (got: %q)", c.EntireBaseURL)
	}

	c.EntireHost = parsedURL.Host
	c.EntireScheme = parsedURL.Scheme
	return nil
}

func LoadConfig() (Config, error) {
	cfg := NewConfig()
	if err := cfg.Load(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
