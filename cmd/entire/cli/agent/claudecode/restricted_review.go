package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/execx"
)

const flagRestricted = "--restricted"

// RestrictedCommandRunner supplies bounded subprocess IO and cancellation. The
// caller must honor the exact argv/environment, bound stdout, suppress stderr
// from errors, and terminate descendants on cancellation. The provider owns the
// Claude contract; the worker may share its generic process transport with Git.
type RestrictedCommandRunner func(context.Context, string, string, []string, []string, string) ([]byte, error)

// RestrictedReviewer executes structured read-only reviews. It intentionally
// does not use NewReviewer or GenerateText: neither has this permission boundary.
// No user apiKeyHelper is imported, and unrestricted fallback is forbidden.
type RestrictedReviewer struct {
	binary string
	run    RestrictedCommandRunner
}

// RestrictedReview specifies trusted launch parameters for an untrusted checkout.
// Prompt is sent over stdin. ReadDir contains the precomputed diff, not secrets.
type RestrictedReview struct {
	Directory    string
	ReadDir      string
	Prompt       string
	Model        string
	OutputSchema json.RawMessage
}

// NewRestrictedReviewer resolves Claude and checks capabilities without a model
// call. The transport is explicit so callers retain their output/deadline budget.
func NewRestrictedReviewer(ctx context.Context, run RestrictedCommandRunner) (*RestrictedReviewer, error) {
	if run == nil {
		return nil, errors.New("restricted review requires a process runner")
	}
	if _, _, err := agent.LookupOverride("CLAUDE_CONFIG_DIR"); err != nil {
		return nil, fmt.Errorf("claude configuration: %w", err)
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude Code is required: %w", err)
	}
	if !filepath.IsAbs(binary) {
		return nil, errors.New("claude must resolve to an absolute executable path")
	}
	return prepareRestrictedReviewer(ctx, binary, run)
}

func prepareRestrictedReviewer(ctx context.Context, binary string, run RestrictedCommandRunner) (*RestrictedReviewer, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	env, err := restrictedReviewEnv()
	if err != nil {
		return nil, err
	}
	out, err := run(probeCtx, binary, os.TempDir(), env, []string{flagRestricted, flagSettingSources, "", "--help"}, "")
	if err != nil {
		return nil, fmt.Errorf("check Claude capabilities: %w", err)
	}
	for _, flag := range []string{flagRestricted, flagStrictMCP, "--json-schema", flagSettingSources} {
		if !bytes.Contains(out, []byte(flag)) {
			return nil, fmt.Errorf("claude Code must support %s; update Claude before serving jobs (no unrestricted fallback)", flag)
		}
	}
	return &RestrictedReviewer{binary: binary, run: run}, nil
}

// Review returns the successful structured output, never raw Claude diagnostics.
func (r *RestrictedReviewer) Review(ctx context.Context, cfg RestrictedReview) (json.RawMessage, error) {
	// Explicitly disable settings sources as a second barrier alongside restricted
	// mode. Never import user auth helpers or repository-supplied environment.
	args := []string{"-p", flagOutputFormat, "json", "--json-schema", string(cfg.OutputSchema), flagRestricted, flagSettingSources, "", flagStrictMCP, "--mcp-config", `{"mcpServers":{}}`, "--settings", `{"disableAllHooks":true}`, "--no-session-persistence", flagTools, "Read,Grep,Glob", "--add-dir", cfg.ReadDir}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	env, err := restrictedReviewEnv()
	if err != nil {
		return nil, err
	}
	output, err := r.run(ctx, r.binary, cfg.Directory, env, args, cfg.Prompt)
	if err != nil {
		return nil, fmt.Errorf("claude review failed: %w", err)
	}
	return parseRestrictedReviewResponse(output)
}

// USER is required for Claude's macOS keychain login.
var restrictedReviewEnvKeys = map[string]bool{
	"HOME": true, "USER": true, "LOGNAME": true, "USERPROFILE": true,
	"SystemRoot": true, "WINDIR": true, "TEMP": true, "TMP": true, "TMPDIR": true,
	"LANG": true, "LC_ALL": true,
	"ANTHROPIC_API_KEY": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
	"HTTPS_PROXY": true, "https_proxy": true, "HTTP_PROXY": true, "http_proxy": true,
	"NO_PROXY": true, "no_proxy": true,
	"NODE_EXTRA_CA_CERTS": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
}

// Build from an allowlist rather than inheriting worker credentials, git config,
// loader options or caller-session markers. Restricted mode confines file tools.
func restrictedReviewEnv() ([]string, error) {
	config, configured, err := agent.LookupOverride("CLAUDE_CONFIG_DIR")
	if err != nil {
		return nil, fmt.Errorf("claude configuration: %w", err)
	}
	env := []string{"PATH=" + strings.Join(execx.PathScanDirs(), string(os.PathListSeparator))}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if restrictedReviewEnvKeys[key] {
			env = append(env, entry)
		}
	}
	if configured {
		env = append(env, "CLAUDE_CONFIG_DIR="+config)
	}
	return env, nil
}

func parseRestrictedReviewResponse(output []byte) (json.RawMessage, error) {
	var result responseEnvelope
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, errors.New("claude returned invalid JSON")
	}
	if result.IsError {
		// Reuse typed classification without exposing result text from an untrusted
		// review in worker logs or the failure API.
		return nil, classifyEnvelopeError("", result.APIErrorStatus, 0)
	}
	if result.Type != streamEventTypeResult || result.Subtype != "success" || len(result.PermissionDenials) != 0 || len(result.StructuredOutput) == 0 || bytes.Equal(bytes.TrimSpace(result.StructuredOutput), []byte("null")) {
		return nil, errors.New("claude returned no successful structured result")
	}
	return result.StructuredOutput, nil
}
