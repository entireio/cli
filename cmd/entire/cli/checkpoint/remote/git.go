package remote

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// CheckpointTokenEnvVar is the environment variable for providing an access token
// used to authenticate git push/fetch operations for checkpoint branches.
// The token is injected as an HTTP Basic Authorization header per RFC 7617:
// the credentials string "x-access-token:<token>" is base64-encoded and sent as
// "Authorization: Basic <base64>". This matches GitHub's token auth for Git HTTPS.
// SSH remotes ignore the token (with a warning).
const CheckpointTokenEnvVar = "ENTIRE_CHECKPOINT_TOKEN"

var sshTokenWarningOnce sync.Once //nolint:gochecknoglobals // intentional per-process gate

// FetchOptions configures a git fetch operation.
type FetchOptions struct {
	Remote    string   // remote name or URL (required)
	RefSpecs  []string // one or more refspecs / object hashes
	Shallow   bool     // adds --depth=1
	NoTags    bool     // adds --no-tags
	NoFilter  bool     // when true, skips --filter=blob:none even if filtered fetches are enabled
	Dir       string   // working directory (empty = CWD)
	ExtraArgs []string // additional flags before remote (e.g., "--no-write-fetch-head")
}

// Fetch runs git fetch with checkpoint token injection and optional
// filtered fetches (--filter=blob:none when settings enable it).
// GIT_TERMINAL_PROMPT=0 is always set.
//
// Callers that pass a remote name (e.g., "origin") and want filtered fetches to
// resolve the name to a URL (to avoid persisting promisor settings) should call
// ResolveFetchTarget first and pass the resolved target as opts.Remote.
func Fetch(ctx context.Context, opts FetchOptions) ([]byte, error) {
	args := []string{"fetch"}
	if opts.NoTags {
		args = append(args, "--no-tags")
	}
	if opts.Shallow {
		args = append(args, "--depth=1")
	}
	args = append(args, opts.ExtraArgs...)
	if !opts.NoFilter && settings.IsFilteredFetchesEnabled(ctx) {
		args = append(args, "--filter=blob:none")
	}
	args = append(args, opts.Remote)
	args = append(args, opts.RefSpecs...)

	cmd := newCommand(ctx, args...)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	disableTerminalPrompt(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git fetch: %w", err)
	}
	return out, nil
}

// FetchBlobs fetches specific objects (typically blobs) by hash from a remote.
// Unlike Fetch, this never applies --filter=blob:none (which would be
// contradictory — the point is to download specific blobs) and always uses
// --no-write-fetch-head to avoid polluting FETCH_HEAD.
//
// The remote should be a URL (not a remote name) to avoid persisting promisor
// settings onto the named remote. Use FetchURL to obtain the URL.
func FetchBlobs(ctx context.Context, remote string, hashes []string) error {
	args := []string{"fetch", "--no-tags", "--no-write-fetch-head", remote}
	args = append(args, hashes...)

	cmd := newCommand(ctx, args...)
	disableTerminalPrompt(cmd)
	_, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch blobs: %w", err)
	}
	return nil
}

// PushResult holds raw porcelain output from git push.
type PushResult struct {
	Output string
}

// Push runs git push --no-verify --porcelain with token injection.
// GIT_TERMINAL_PROMPT=0 is always set.
func Push(ctx context.Context, remote, refSpec string) (PushResult, error) {
	pushTarget, err := resolvePushCommandTarget(ctx, remote)
	if err != nil {
		return PushResult{}, fmt.Errorf("resolve push target: %w", err)
	}

	cmd := newCommand(ctx, "push", "--no-verify", "--porcelain", pushTarget, refSpec)
	disableTerminalPrompt(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return PushResult{Output: string(output)}, fmt.Errorf("git push: %w", err)
	}
	return PushResult{Output: string(output)}, nil
}

// LsRemote runs git ls-remote with token injection.
// GIT_TERMINAL_PROMPT=0 is always set. Returns stdout only.
func LsRemote(ctx context.Context, remote string, patterns ...string) ([]byte, error) {
	return lsRemote(ctx, "", remote, patterns...)
}

// LsRemoteInDir is like LsRemote but runs in a specific directory.
func LsRemoteInDir(ctx context.Context, dir, remote string, patterns ...string) ([]byte, error) {
	return lsRemote(ctx, dir, remote, patterns...)
}

func lsRemote(ctx context.Context, dir, remote string, patterns ...string) ([]byte, error) {
	args := append([]string{"ls-remote", remote}, patterns...)
	cmd := newCommand(ctx, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	disableTerminalPrompt(cmd)
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git ls-remote: %w", err)
	}
	return out, nil
}

// IsURL returns true if the target looks like a URL rather than a git remote name.
func IsURL(target string) bool {
	return strings.Contains(target, "://") || strings.Contains(target, "@")
}

// ResolveFetchTarget returns the git fetch target to use. When filtered
// fetches are enabled, configured remotes are resolved to their URL so git does
// not persist promisor settings onto the remote name.
func ResolveFetchTarget(ctx context.Context, target string) (string, error) {
	if IsURL(target) || isLocalPath(target) || !settings.IsFilteredFetchesEnabled(ctx) {
		return target, nil
	}
	url, err := GetRemoteURL(ctx, target)
	if err != nil {
		return "", fmt.Errorf("get remote URL: %w", err)
	}
	return url, nil
}

// newCommand creates an exec.Cmd for a git operation that may need
// checkpoint token authentication. If ENTIRE_CHECKPOINT_TOKEN is set:
//   - if the target in args is (or resolves to) an SSH remote, the target is
//     rewritten in the args to the equivalent HTTPS URL so git uses HTTP
//     transport and our injected Authorization header applies;
//   - a Basic auth token is then injected via GIT_CONFIG_COUNT/GIT_CONFIG_KEY_*/
//     GIT_CONFIG_VALUE_* environment variables.
//
// If rewriting fails (unparseable URL, missing owner/repo) the command runs
// unmodified and a one-shot warning is printed.
// For empty/unset tokens, the command is returned unmodified.
//
// The remote is extracted from args by skipping the git subcommand and any flags
// (arguments starting with "-"). For example, in
// ["push", "--no-verify", "origin", "main"], the remote is "origin".
func newCommand(ctx context.Context, args ...string) *exec.Cmd {
	token := strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar))

	mkCmd := func(finalArgs []string) *exec.Cmd {
		c := exec.CommandContext(ctx, "git", finalArgs...)
		c.Stdin = nil // Disconnect stdin to prevent hanging in hook context
		return c
	}

	if token == "" {
		return mkCmd(args)
	}

	if !isValidToken(token) {
		fmt.Fprintf(os.Stderr, "[entire] Warning: %s contains invalid characters (CR, LF, or other control chars) — token ignored\n", CheckpointTokenEnvVar)
		return mkCmd(args)
	}

	target := extractRemoteFromArgs(args)
	if target == "" {
		return mkCmd(args)
	}

	newTarget, protocol := resolveTargetForTokenAuth(ctx, target)
	if newTarget != target {
		args = replaceFirstPositional(args, newTarget)
	}

	cmd := mkCmd(args)

	switch protocol {
	case ProtocolSSH:
		sshTokenWarningOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "[entire] Warning: %s is set but remote uses SSH — token ignored for SSH remotes\n", CheckpointTokenEnvVar)
		})
		return cmd
	case ProtocolHTTPS:
		cmd.Env = appendCheckpointTokenEnv(os.Environ(), token)
		return cmd
	default:
		// Unknown protocol (e.g., local path, or resolution failed) — don't inject
		return cmd
	}
}

// resolveTargetForTokenAuth resolves a git target (remote name or URL) to its
// effective protocol, rewriting SSH targets to the equivalent HTTPS URL so
// token-based auth can be applied. Returns the (possibly rewritten) target and
// its final protocol. Protocol is "" when resolution fails (local path,
// nonexistent remote, unparseable URL).
//
// This is only meaningful when ENTIRE_CHECKPOINT_TOKEN is set; callers gate on
// that themselves.
func resolveTargetForTokenAuth(ctx context.Context, target string) (string, string) {
	if target == "" || isLocalPath(target) {
		return target, ""
	}

	rawURL := target
	if !IsURL(target) {
		var err error
		rawURL, err = GetRemoteURL(ctx, target)
		if err != nil {
			return target, ""
		}
	}

	info, err := ParseURL(rawURL)
	if err != nil {
		return target, ""
	}

	if info.Protocol == ProtocolSSH {
		if httpsURL, ok := deriveTokenOriginURL(rawURL); ok {
			return httpsURL, ProtocolHTTPS
		}
		return target, ProtocolSSH
	}

	return target, info.Protocol
}

// replaceFirstPositional returns a copy of args with the first non-flag
// argument after args[0] (the git subcommand) replaced by newTarget. Callers
// use this to rewrite a remote name/URL after resolution without mutating the
// original slice.
func replaceFirstPositional(args []string, newTarget string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 1; i < len(out); i++ {
		if !strings.HasPrefix(out[i], "-") {
			out[i] = newTarget
			return out
		}
	}
	return out
}

// extractRemoteFromArgs finds the remote URL or name from git command args.
// It skips the subcommand (first arg) and any flags (args starting with "-"),
// returning the first positional argument, which is the remote for push/fetch/ls-remote.
func extractRemoteFromArgs(args []string) string {
	if len(args) < 2 {
		return ""
	}
	// Skip subcommand (e.g., "push", "fetch", "ls-remote").
	for _, arg := range args[1:] {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

// appendCheckpointTokenEnv appends GIT_CONFIG_COUNT-based env vars to inject
// an Authorization header into git HTTP requests. The token is sent as a Basic
// credential with the format "x-access-token:<token>" (base64-encoded), which
// is compatible with GitHub's token authentication.
//
// Existing GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* entries are preserved; the new
// http.extraHeader entry is appended at the next free index and
// GIT_CONFIG_COUNT is updated accordingly. This keeps caller-injected git
// config (e.g., safe.directory, custom CA settings) intact.
func appendCheckpointTokenEnv(baseEnv []string, token string) []string {
	existingCount := 0
	for _, e := range baseEnv {
		rest, ok := strings.CutPrefix(e, "GIT_CONFIG_COUNT=")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(rest); err == nil && n > 0 {
			existingCount = n
		}
	}

	// Strip the old GIT_CONFIG_COUNT entry (we'll emit a new one) but keep
	// GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* entries in place.
	filtered := make([]string, 0, len(baseEnv)+3)
	for _, e := range baseEnv {
		if strings.HasPrefix(e, "GIT_CONFIG_COUNT=") {
			continue
		}
		filtered = append(filtered, e)
	}

	idx := existingCount
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return append(filtered,
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", existingCount+1),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=http.extraHeader", idx),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=Authorization: Basic %s", idx, encoded),
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

// resolvePushCommandTarget returns the target to pass to git push. When a
// dedicated checkpoint_remote is configured, the checkpoint URL is returned so
// the push is routed to the separate checkpoint repo. Otherwise the remote
// name is returned unchanged so git uses its own config, updates the
// refs/remotes/<name>/<branch> tracking ref, and subsequent calls can use that
// tracking ref to skip redundant pushes.
//
// SSH→HTTPS coercion for token auth is handled by newCommand, which rewrites
// the command args or injects per-host config, rather than being baked into
// the target here.
func resolvePushCommandTarget(ctx context.Context, target string) (string, error) {
	if target == "" || IsURL(target) || isLocalPath(target) {
		return target, nil
	}

	pushTarget, enabled, err := PushURL(ctx, target)
	if err != nil {
		return "", err
	}
	if !enabled || pushTarget == "" {
		return target, nil
	}
	return pushTarget, nil
}

func isLocalPath(target string) bool {
	return filepath.IsAbs(target) || strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../")
}

// disableTerminalPrompt sets GIT_TERMINAL_PROMPT=0 on the command,
// initializing cmd.Env from os.Environ() if nil.
func disableTerminalPrompt(cmd *exec.Cmd) {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
}
