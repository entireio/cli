package cliauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/oppassword"
	"github.com/entireio/cli/internal/entiredb/tokenstore"
)

// BreakGlassKeyringUser is the fixed pseudo-handle break-glass tokens are
// stored under. Break-glass tokens carry no user identity (their subject is
// a zero AccountID), so we use a constant slot per-(host) to keep the
// keyring layout flat. Multiple operators on the same machine share this
// slot, which matches reality — break-glass is one shared escape hatch.
const BreakGlassKeyringUser = "operator"

// BreakGlassKeyringService returns the keyring service name break-glass
// tokens are stashed under for the given cluster host. Sits alongside the
// cluster login token's service (entire:<host>) and the refresh token's
// (entire:<host>:refresh). Always cluster-keyed because the entiredb
// break-glass JWT is issued by entire-server, not entire-core — its
// validity is bounded by that cluster.
func BreakGlassKeyringService(host string) string {
	return tokenstore.ClusterKeyringService(host) + ":break-glass"
}

// ReadBreakGlassToken returns the JWT stashed by `entiredb admin break-glass`,
// or an error directing the operator to run that subcommand first.
func ReadBreakGlassToken(cfg Config) (string, error) {
	host := cfg.CredentialHost()
	tok, err := tokenstore.Get(BreakGlassKeyringService(host), BreakGlassKeyringUser)
	if err != nil {
		return "", fmt.Errorf("read break-glass token from keyring: %w (run 'entiredb admin break-glass' first)", err)
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", errors.New("break-glass token in keyring is empty (run 'entiredb admin break-glass' first)")
	}
	return tok, nil
}

func StoreBreakGlassToken(cfg Config, token string) error {
	host := cfg.CredentialHost()
	if err := tokenstore.Set(BreakGlassKeyringService(host), BreakGlassKeyringUser, token); err != nil {
		return fmt.Errorf("store break-glass token in keyring: %w", err)
	}
	return nil
}

// resolveBreakGlassBaseURL returns the cluster URL to break-glass against.
// The target must be explicit — break-glass is an operational tool that
// targets a specific cluster, and silently inheriting the default
// ENTIRE_BASE_URL (https://entire.io) would point at the SaaS host
// instead of an operator's cluster. Mirrors entire-core's --base-url
// plumbing (cmd/entire-core/cli/break_glass.go's resolveBaseURLWithSource)
// and the addNodeURLFlag rationale in admin.go.
//
// Precedence: --base-url flag, then ENTIRE_BASE_URL env var. Returns the
// trimmed URL and the parsed host. Error if neither is set.
func resolveBreakGlassBaseURL(flag string) (rawURL, host string, err error) {
	raw := strings.TrimSpace(flag)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("ENTIRE_BASE_URL"))
	}
	if raw == "" {
		return "", "", errors.New("--base-url or ENTIRE_BASE_URL must be set: break-glass needs an explicit cluster target (e.g. https://royalcanin.partial.to)")
	}
	parsed, perr := url.Parse(raw)
	if perr != nil {
		return "", "", fmt.Errorf("parse base URL %q: %w", raw, perr)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", fmt.Errorf("base URL %q must include http:// or https://", raw)
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("base URL %q missing host", raw)
	}
	return strings.TrimRight(raw, "/"), parsed.Host, nil
}

// breakGlassOpVault returns the 1Password vault holding the
// break-glass credential for a given cluster host. Production hosts
// under `*.entire.io` live in the `entire.io` vault; everything else
// (staging clusters under `*.partial.to`, localhost, raw IPs) lives in
// `partial.to`.
func breakGlassOpVault(host string) string {
	if strings.HasSuffix(host, ".entire.io") || host == "entire.io" {
		return "entire.io"
	}
	return "partial.to"
}

// resolveBreakGlassPreSharedKey returns the pre-shared operator key for
// the cluster at cfg.EntireBaseURL. Sources, in order:
//  1. ENTIRE_BREAK_GLASS_TOKEN env var — deliberate operator override.
//  2. 1Password: op://<vault>/entiredb-break-glass-tokens/<host>, where
//     <host> is cfg.EntireHost with any :port suffix stripped and
//     <vault> is `entire.io` for *.entire.io hosts or `partial.to`
//     otherwise. Single 1Password item per vault with one field per
//     cluster; keying by host means the CLI can't send a token meant
//     for a different cluster.
func resolveBreakGlassPreSharedKey(ctx context.Context, cfg Config) (string, error) {
	if tok := os.Getenv("ENTIRE_BREAK_GLASS_TOKEN"); tok != "" {
		Debugf("break-glass token source: ENTIRE_BREAK_GLASS_TOKEN env var (%d bytes)", len(tok))
		return tok, nil
	}

	host := cfg.EntireHost
	if host == "" {
		return "", errors.New("cannot derive break-glass cluster host; set ENTIRE_BREAK_GLASS_TOKEN explicitly or pass --base-url / ENTIRE_BASE_URL")
	}
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	Debugf("cluster host: %s (derived from base URL %s)", host, cfg.EntireBaseURL)

	opRef := "op://" + breakGlassOpVault(host) + "/entiredb-break-glass-tokens/" + host
	Debugf("break-glass token source: 1Password (ref=%s)", opRef)

	tok, err := oppassword.Read(ctx, opRef)
	if err != nil {
		Debugf("op read failed: %v", err)
		return "", err
	}
	Debugf("op read %s succeeded (%d bytes)", opRef, len(tok))
	return tok, nil
}

// NewBreakGlassCmd returns the `entiredb admin break-glass` subcommand. It
// exchanges the pre-shared operator key for a cluster-local ops-access JWT
// and stashes it. The next time the operator runs an admin command, the
// credential picker offers break-glass as slot 0 alongside any logged-in
// users.
func NewBreakGlassCmd(cfg Config) *cobra.Command {
	var baseURLFlag string
	cmd := &cobra.Command{
		Use:   "break-glass",
		Short: "Obtain a break-glass ops-access token (used when core is unreachable)",
		Long: `Exchanges the pre-shared break-glass key for a short-lived ops-access JWT
issued by this cluster directly, bypassing core. Use when core is down,
misconfigured, or not yet provisioned.

The target cluster must be specified explicitly via --base-url or
ENTIRE_BASE_URL — break-glass refuses to fall back to a default,
because silently targeting the wrong cluster is the failure mode this
command exists to avoid.

Pre-shared key sources (in order):
  1. ENTIRE_BREAK_GLASS_TOKEN environment variable (deliberate operator
     override)
  2. 1Password CLI: op read op://<vault>/entiredb-break-glass-tokens/<host>
     where <host> is the host of the resolved base URL (port stripped)
     and <vault> is "entire.io" for *.entire.io hosts and "partial.to"
     for everything else (staging, localhost, raw IPs). One 1Password
     item per vault, one field per cluster — the right token is fetched
     per target cluster.

The returned JWT is stored in the OS keyring under service
"entire:<host>:break-glass" (alongside the login JWT under "entire:<host>"),
and is offered as slot 0 in the credential picker the next time an admin
subcommand runs.`,
		Example: "  entiredb admin break-glass --base-url https://royalcanin.partial.to\n" +
			"  ENTIRE_BASE_URL=https://royalcanin.partial.to entiredb admin break-glass",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			baseURL, host, err := resolveBreakGlassBaseURL(baseURLFlag)
			if err != nil {
				return err
			}
			cfg.EntireBaseURL = baseURL
			cfg.EntireHost = host

			preShared, err := resolveBreakGlassPreSharedKey(ctx, cfg)
			if err != nil {
				return err
			}

			token, err := postBreakGlass(ctx, cfg.EntireBaseURL, preShared, cfg.SkipTLSVerify)
			if err != nil {
				return err
			}

			if err := StoreBreakGlassToken(cfg, token); err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "Break-glass token stored in keyring (%s/%s)\n",
				BreakGlassKeyringService(cfg.CredentialHost()), BreakGlassKeyringUser)
			return nil
		},
	}
	cmd.Flags().StringVar(&baseURLFlag, "base-url", "",
		"Cluster URL to break-glass against (e.g. https://royalcanin.partial.to). Overrides ENTIRE_BASE_URL.")
	return cmd
}

// postBreakGlass calls POST /api/auth/break-glass with the pre-shared key and
// returns the issued JWT. Errors carry the HTTP status so "feature disabled"
// (503) surfaces distinctly from "bad key" (401).
func postBreakGlass(ctx context.Context, baseURL, preShared string, skipTLSVerify bool) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/api/auth/break-glass",
		bytes.NewReader(nil))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+preShared)
	req.Header.Set("Content-Type", "application/json")

	resp, err := NewHTTPClient(skipTLSVerify).Do(req)
	if err != nil {
		return "", fmt.Errorf("break-glass request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body) //nolint:errcheck // read best-effort for error text

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("break-glass exchange: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("break-glass: empty token in response")
	}
	return out.Token, nil
}
