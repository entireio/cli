package strategy

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// CheckpointTokenEnvVar is the environment variable for providing an access token
// used to authenticate git push/fetch operations for checkpoint branches.
// The token is injected as an HTTP Basic Authorization header per RFC 7617:
// the credentials string "x-access-token:<token>" is base64-encoded and sent as
// "Authorization: Basic <base64>". This matches GitHub's token auth for Git HTTPS.
// SSH remotes ignore the token (with a warning).
const CheckpointTokenEnvVar = "ENTIRE_CHECKPOINT_TOKEN"

var sshTokenWarningOnce sync.Once

// CheckpointGitCommand creates an exec.Cmd for a git operation that may need
// checkpoint token authentication. If ENTIRE_CHECKPOINT_TOKEN is set and the
// target resolves to an HTTPS remote, a Basic auth token is injected via
// GIT_CONFIG_COUNT/GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* environment variables.
//
// For SSH remotes, a warning is printed once to stderr and the token is not injected.
// For empty/unset tokens, the command is returned unmodified.
//
// The target parameter is used ONLY for protocol detection (SSH vs HTTPS) and does
// not affect the command executed. It can be:
//   - A URL (e.g., "https://github.com/org/repo.git")
//   - A remote name (e.g., "origin") — resolved via `git remote get-url`
//
// The actual remote must be specified again inside args, which contains the full
// git command arguments (e.g., "push", "--no-verify", remote, branch).
func CheckpointGitCommand(ctx context.Context, target string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = nil // Disconnect stdin to prevent hanging in hook context

	token := strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar))
	if token == "" {
		return cmd
	}

	if !isValidToken(token) {
		fmt.Fprintf(os.Stderr, "[entire] Warning: %s contains invalid characters (CR, LF, or other control chars) — token ignored\n", CheckpointTokenEnvVar)
		return cmd
	}

	protocol := resolveTargetProtocol(ctx, target)

	switch protocol {
	case protocolSSH:
		sshTokenWarningOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "[entire] Warning: %s is set but remote uses SSH — token ignored for SSH remotes\n", CheckpointTokenEnvVar)
		})
		return cmd
	case protocolHTTPS:
		cmd.Env = appendCheckpointTokenEnv(os.Environ(), token)
		return cmd
	default:
		// Unknown protocol (e.g., local path, or resolution failed) — don't inject
		return cmd
	}
}

// appendCheckpointTokenEnv appends GIT_CONFIG_COUNT-based env vars to inject
// an Authorization header into git HTTP requests. The token is sent as a Basic
// credential with the format "x-access-token:<token>" (base64-encoded), which
// is compatible with GitHub's token authentication.
// It filters out any pre-existing GIT_CONFIG_COUNT/KEY/VALUE entries to avoid
// conflicts, then appends the new ones.
//
// NOTE: This drops ALL existing GIT_CONFIG_* entries from the environment.
// If a parent process (e.g., CI) injects its own GIT_CONFIG_* vars, they will
// be lost. If that becomes an issue, read the existing count and append at the
// next index instead of replacing.
func appendCheckpointTokenEnv(baseEnv []string, token string) []string {
	filtered := make([]string, 0, len(baseEnv)+3)
	for _, e := range baseEnv {
		if strings.HasPrefix(e, "GIT_CONFIG_COUNT=") ||
			strings.HasPrefix(e, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(e, "GIT_CONFIG_VALUE_") {
			continue
		}
		filtered = append(filtered, e)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return append(filtered,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+encoded,
	)
}

// isValidToken returns false if the token contains control characters (bytes < 0x20
// or 0x7F). This prevents HTTP header injection via CR/LF or other control chars
// embedded in the token value.
func isValidToken(token string) bool {
	for _, b := range []byte(token) {
		if b < 0x20 || b == 0x7F {
			return false
		}
	}
	return true
}

// resolveTargetProtocol determines whether a push/fetch target uses SSH or HTTPS.
// Returns protocolSSH, protocolHTTPS, or "" if unknown.
func resolveTargetProtocol(ctx context.Context, target string) string {
	var rawURL string
	if isURL(target) {
		rawURL = target
	} else {
		// Remote name — resolve to URL
		var err error
		rawURL, err = getRemoteURL(ctx, target)
		if err != nil {
			return ""
		}
	}

	info, err := parseGitRemoteURL(rawURL)
	if err != nil {
		return ""
	}
	return info.protocol
}
