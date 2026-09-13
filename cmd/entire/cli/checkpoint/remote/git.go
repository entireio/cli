package remote

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// stampConfigTimeout bounds the local git-config reads/writes that mark a newly
// created checkpoint remote as skipped. They run detached from the fetch's
// context (see stampNewlyCreatedRemote), so a bound guards against a stuck
// config lock hanging the caller.
const stampConfigTimeout = 10 * time.Second

// CheckpointTokenEnvVar is the environment variable for providing an access token
// used to authenticate git push/fetch operations for checkpoint branches.
// The token is injected as an HTTP Basic Authorization header per RFC 7617:
// the credentials string "x-access-token:<token>" is base64-encoded and sent as
// "Authorization: Basic <base64>". This matches GitHub's token auth for Git HTTPS.
// SSH remotes ignore the token (with a warning).
const CheckpointTokenEnvVar = "ENTIRE_CHECKPOINT_TOKEN"

var sshTokenWarningOnce sync.Once //nolint:gochecknoglobals // intentional per-process gate

// nonInteractiveSSHKey marks a context whose checkpoint git subprocesses must
// never block on an interactive SSH prompt (e.g. a key passphrase when no
// ssh-agent is running).
type nonInteractiveSSHKey struct{}

// WithNonInteractiveSSH marks ctx so every checkpoint git command spawned under
// it runs SSH with BatchMode=yes, failing fast instead of hanging on an
// interactive prompt. Set this at best-effort, non-interactive entry points such
// as the git pre-push hook: a blocked passphrase prompt there would hang the
// user's own `git push` until the checkpoint push budget kills it, with no way
// to type the passphrase. Foreground commands (resume, explain) leave it unset
// so they can still prompt.
//
// BatchMode tradeoffs (issue #1523):
//   - Passphrase-protected keys with no ssh-agent: fail fast (desired).
//   - Touch-only security keys (sk-, user-presence only): still work — touch is
//     not a terminal passphrase read.
//   - PIN-protected FIDO2 keys (verify-required): PIN entry goes through ssh's
//     passphrase reader, so BatchMode suppresses it and the push fails. Load the
//     key into ssh-agent beforehand, or set an explicit BatchMode=no via
//     GIT_SSH_COMMAND / core.sshCommand (respected; we do not override it).
func WithNonInteractiveSSH(ctx context.Context) context.Context {
	return context.WithValue(ctx, nonInteractiveSSHKey{}, true)
}

// IsNonInteractiveSSH reports whether ctx was marked with WithNonInteractiveSSH.
func IsNonInteractiveSSH(ctx context.Context) bool {
	return nonInteractiveSSHFromContext(ctx)
}

func nonInteractiveSSHFromContext(ctx context.Context) bool {
	v, ok := ctx.Value(nonInteractiveSSHKey{}).(bool)
	return ok && v
}

// LooksLikeSSHAuthFailure reports whether errText looks like an SSH
// authentication failure (passphrase/PIN unavailable under BatchMode, missing
// agent identity, publickey rejection, etc.). Used to print an actionable
// ssh-agent hint from the pre-push checkpoint path.
func LooksLikeSSHAuthFailure(errText string) bool {
	if errText == "" {
		return false
	}
	lower := strings.ToLower(errText)
	// Keep needles auth-specific. Do not match git's generic
	// "Could not read from remote repository" epilogue — that also appears on
	// network failures where an ssh-agent hint would be wrong. Real auth
	// failures always include a Permission denied / auth-methods line too.
	needles := []string{
		"permission denied (publickey)",
		"permission denied (keyboard-interactive",
		"permission denied (password)",
		"too many authentication failures",
		"no more authentication methods to try",
	}
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	// Generic publickey denial without the parenthetical form.
	if strings.Contains(lower, "permission denied") && strings.Contains(lower, "publickey") {
		return true
	}
	return false
}

// batchModeOptionRe matches an explicit BatchMode ssh option (e.g.
// "-o BatchMode=yes" or "BatchMode=no"), case-insensitively. Anchored with \b
// so it doesn't false-positive on unrelated text that merely contains
// "BatchMode" as a substring of a longer token.
var batchModeOptionRe = regexp.MustCompile(`(?i)\bBatchMode\s*=\s*\S+`)

// hasExplicitBatchMode reports whether sshCmd already sets a BatchMode option,
// with any value. A user-supplied BatchMode=no is a deliberate choice and must
// be respected, not silently overridden to yes.
func hasExplicitBatchMode(sshCmd string) bool {
	return batchModeOptionRe.MatchString(sshCmd)
}

// envLookup returns the value of the last occurrence of key in env (matching
// exec.Cmd's last-wins semantics for duplicate entries) and whether it was
// found.
func envLookup(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], prefix); ok {
			return v, true
		}
	}
	return "", false
}

// gitConfigSSHCommand looks up core.sshCommand via `git config`, run with env
// so the lookup honors any HOME/GIT_CONFIG_* overrides present in env (e.g. in
// tests). Returns "" if unset or the lookup fails.
func gitConfigSSHCommand(ctx context.Context, env []string) string {
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "core.sshCommand")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// effectiveSSHCommand resolves the ssh invocation git itself would use, in
// git's own precedence order: the GIT_SSH_COMMAND environment variable, then
// the core.sshCommand git config value, then the GIT_SSH environment
// variable, falling back to plain "ssh" when none are set.
func effectiveSSHCommand(ctx context.Context, env []string) string {
	if v, ok := envLookup(env, "GIT_SSH_COMMAND"); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	if v := gitConfigSSHCommand(ctx, env); v != "" {
		return v
	}
	if v, ok := envLookup(env, "GIT_SSH"); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return "ssh"
}

// withBatchModeSSH returns env with GIT_SSH_COMMAND set so ssh runs with
// BatchMode=yes. The base ssh invocation is resolved via effectiveSSHCommand
// (env GIT_SSH_COMMAND > core.sshCommand > GIT_SSH > plain "ssh") so a custom
// ssh command configured via core.sshCommand isn't silently discarded. The
// flag is only appended when BatchMode isn't already explicitly set — an
// existing BatchMode=no is a deliberate user choice and is left untouched —
// so the result is idempotent.
func withBatchModeSSH(ctx context.Context, env []string) []string {
	const key = "GIT_SSH_COMMAND="
	base := effectiveSSHCommand(ctx, env)
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, key) {
			continue
		}
		out = append(out, e)
	}
	if !hasExplicitBatchMode(base) {
		base += " -o BatchMode=yes"
	}
	return append(out, key+base)
}

// applyNonInteractiveSSH sets BatchMode SSH on cmd when ctx is marked
// non-interactive (see WithNonInteractiveSSH). No-op otherwise, so foreground
// commands keep their interactive prompt behavior.
func applyNonInteractiveSSH(ctx context.Context, cmd *exec.Cmd) {
	if !nonInteractiveSSHFromContext(ctx) {
		return
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = withBatchModeSSH(ctx, cmd.Env)
}

// FetchOptions configures a git fetch operation.
type FetchOptions struct {
	Remote   string   // remote name or URL (required)
	RefSpecs []string // one or more refspecs / object hashes
	NoTags   bool     // adds --no-tags
	NoFilter bool     // when true, skips --filter=blob:none even if filtered fetches are enabled
	// Shallow adds --depth=1 to fetch only the tip commit and its tree. Use
	// for tip-only probes (e.g. resolving the latest checkpoint metadata)
	// where ancestry isn't needed. Creates .git/shallow state — callers that
	// later require full history should opt into Unshallow on a follow-up
	// fetch.
	Shallow bool
	// Unshallow adds --unshallow when the repository is currently shallow,
	// triggering git to download the rest of the history for the fetched ref.
	// Set this on metadata-repair / reconcile paths that need complete
	// checkpoint ancestry. Do not set on generic branch fetches — it would
	// silently convert a deliberately-shallow user clone into a full one.
	Unshallow bool
	// Depth adds --depth=<Depth>, fetching the refspec to this absolute depth.
	// Unlike Unshallow (which is repo-global) this is ref-scoped: it fully
	// fetches the named branch — healing a prior shallow boundary on it — while
	// leaving an independently-shallow source-tree clone untouched, and it does
	// not introduce shallowness on a full repo when the value exceeds the
	// branch's length. Use a value above the branch's realistic length but below
	// math.MaxInt32 (2147483647), which git special-cases as a global unshallow.
	// Ignored when zero or when Shallow is set.
	Depth     int
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
	args := []string{"fetch", "--no-auto-gc"}
	if opts.NoTags {
		args = append(args, "--no-tags")
	}
	args = append(args, opts.ExtraArgs...)
	switch {
	case opts.Shallow:
		args = append(args, "--depth=1")
	case opts.Depth > 0:
		args = append(args, fmt.Sprintf("--depth=%d", opts.Depth))
	case opts.Unshallow && isShallowRepository(ctx, opts.Dir):
		args = append(args, "--unshallow")
	}
	filtered := !opts.NoFilter && settings.IsFilteredFetchesEnabled(ctx)
	if filtered {
		args = append(args, "--filter=blob:none")
	}
	args = append(args, opts.Remote)
	args = append(args, opts.RefSpecs...)

	// A filtered fetch from a URL makes git record a URL-keyed remote section
	// (remote.<url>.*) so it can lazy-fetch filtered-out objects later. That
	// section also turns the URL into a phantom remote that `git fetch --all`
	// and `git remote update` keep dialing. When this fetch is the one creating
	// the section, stamp skipFetchAll so bulk fetches skip our adhoc remote.
	// Remotes that already existed are left untouched so we never rewrite the
	// user's config.
	var stampURL string
	var stampCandidate, existedBefore bool
	if filtered && IsURL(opts.Remote) {
		stampCandidate = true
		stampURL = opts.Remote
		if token := strings.TrimSpace(os.Getenv(CheckpointTokenEnvVar)); token != "" && isValidToken(token) {
			// With a checkpoint token, newCommand rewrites SSH targets to HTTPS
			// and git records the section under the rewritten URL.
			stampURL, _ = resolveTargetForTokenAuth(ctx, stampURL)
		}
		existedBefore = gitRemoteSectionExists(ctx, opts.Dir, stampURL)
	}

	cmd := newCommand(ctx, args...)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	disableTerminalPrompt(cmd)
	out, err := cmd.CombinedOutput()

	if stampCandidate && !existedBefore {
		stampNewlyCreatedRemote(ctx, opts.Dir, stampURL)
	}

	if err != nil {
		return out, fmt.Errorf("git fetch: %w", err)
	}
	return out, nil
}

// stampNewlyCreatedRemote stamps a URL-keyed remote section that this fetch just
// created. Git writes remote.<url>.promisor eagerly during connection setup, so
// a filtered fetch that later fails still leaves the phantom remote behind;
// stamping here — rather than only on fetch success — keeps it from lingering
// unstamped forever (the section then exists on the next attempt, so it never
// looks "new" again). Re-checking existence keeps us from inventing a section
// when the fetch died before git wrote anything.
//
// The git-config commands run on a context detached from the fetch's deadline:
// a filtered fetch that timed out leaves ctx already past its deadline, and
// inheriting it would make these local commands fail immediately and leave the
// phantom unstamped — the very miss this stamping exists to prevent.
func stampNewlyCreatedRemote(ctx context.Context, dir, url string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stampConfigTimeout)
	defer cancel()
	if gitRemoteSectionExists(ctx, dir, url) {
		markRemoteSkipped(ctx, dir, url)
	}
}

// markRemoteSkipped stamps skipFetchAll on a URL-keyed remote section so
// `git fetch --all` and `git remote update` skip it. Called only for remotes
// this fetch just created, so an adhoc checkpoint URL never lingers as a phantom
// remote that bulk fetches keep dialing.
// Best-effort: the git config write is not worth failing the fetch over, so
// failures only log.
func markRemoteSkipped(ctx context.Context, dir, url string) {
	fullKey := "remote." + url + ".skipFetchAll"
	cmd := exec.CommandContext(ctx, "git", "config", "--local", fullKey, "true")
	if dir != "" {
		cmd.Dir = dir
	}
	if out, cfgErr := cmd.CombinedOutput(); cfgErr != nil {
		redactedURL := RedactURL(url)
		// The output can echo the key, which embeds the URL — and a URL can
		// carry credentials. Redact before logging.
		msg := strings.TrimSpace(strings.ReplaceAll(string(out), url, redactedURL))
		logging.Warn(ctx, "failed to mark remote config entry as skipped for bulk fetches",
			slog.String("url", redactedURL),
			slog.String("output", msg),
			slog.String("error", cfgErr.Error()),
		)
	}
}

// gitRemoteSectionExists reports whether a remote.<url>.* config section already
// exists in the local git config. Used to tell whether a filtered URL fetch is
// about to create a new URL-keyed remote, so we only stamp remotes we create and
// never rewrite ones the user already has.
func gitRemoteSectionExists(ctx context.Context, dir, url string) bool {
	cmd := exec.CommandContext(ctx, "git", "config", "--local", "--list", "--name-only")
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	// Each name is "remote.<url>.<key>". Git config keys carry no dots, so the
	// final dotted component is the key and everything between "remote." and it
	// is the subsection (the URL, whose case git preserves). Compare the
	// subsection exactly so a longer URL that shares a prefix (e.g. a
	// ".../repo.git" section vs a ".../repo" fetch) is not a false match.
	for line := range strings.SplitSeq(string(out), "\n") {
		rest, ok := strings.CutPrefix(line, "remote.")
		if !ok {
			continue
		}
		lastDot := strings.LastIndexByte(rest, '.')
		if lastDot < 0 {
			continue
		}
		if rest[:lastDot] == url {
			return true
		}
	}
	return false
}

// FetchBlobs fetches specific objects (typically blobs) by hash from a remote.
// Uses `git fetch-pack` rather than `git fetch` because the high-level
// porcelain enforces partial-clone integrity checks that reject blob-only
// responses with "did not send all necessary objects". Plumbing skips those
// checks — it just downloads the requested objects into .git/objects/pack
// and exits — which is exactly what we want when grabbing individual blobs
// by SHA. Works against GitHub for any reachable object, including blobs.
//
// The remote should be a URL (not a remote name) to avoid persisting promisor
// settings onto the named remote. Use FetchURL to obtain the URL.
func FetchBlobs(ctx context.Context, remote string, hashes []string) error {
	args := []string{"fetch-pack", remote}
	args = append(args, hashes...)

	cmd := newCommand(ctx, args...)
	disableTerminalPrompt(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		redactedURL := RedactURL(remote)
		msg := strings.TrimSpace(strings.ReplaceAll(string(output), remote, redactedURL))
		if msg != "" {
			return fmt.Errorf("git fetch-pack from %s: %s: %w", redactedURL, msg, err)
		}
		return fmt.Errorf("git fetch-pack from %s: %w", redactedURL, err)
	}
	return nil
}

// PushResult holds raw porcelain output from git push.
type PushResult struct {
	Output string
}

// PushOptions configures a git push operation.
type PushOptions struct {
	Remote    string
	RefSpecs  []string
	ExtraArgs []string // additional flags before remote
	Dir       string
}

// Push runs git push --no-verify --porcelain with token injection.
// GIT_TERMINAL_PROMPT=0 is always set.
func Push(ctx context.Context, remote, refSpec string) (PushResult, error) {
	return PushWithOptions(ctx, PushOptions{
		Remote:   remote,
		RefSpecs: []string{refSpec},
	})
}

// PushWithOptions runs git push --no-verify --porcelain with token injection.
// GIT_TERMINAL_PROMPT=0 is always set.
func PushWithOptions(ctx context.Context, opts PushOptions) (PushResult, error) {
	pushTarget, err := resolvePushCommandTarget(ctx, opts.Remote)
	if err != nil {
		return PushResult{}, fmt.Errorf("resolve push target: %w", err)
	}

	args := []string{"push", "--no-verify", "--porcelain"}
	args = append(args, opts.ExtraArgs...)
	args = append(args, pushTarget)
	args = append(args, opts.RefSpecs...)

	cmd := newCommand(ctx, args...)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	disableTerminalPrompt(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return PushResult{Output: string(output)}, fmt.Errorf("git push: %w", err)
	}
	return PushResult{Output: string(output)}, nil
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
		return out, fmt.Errorf("git ls-remote: %w", formatGitCommandError(ctx, err, remote))
	}
	return out, nil
}

// formatGitCommandError enriches an exec error from git Output() so callers see
// useful detail: context deadline expiry by name, and git's stderr (auth denied,
// repository not found, DNS) which ExitError otherwise hides behind "exit status N".
// When remote is a URL it may carry credentials that git echoes into stderr;
// those are redacted before the error is returned (same pattern as FetchBlobs).
func formatGitCommandError(ctx context.Context, err error, remote string) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("deadline exceeded: %w", err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			if remote != "" {
				stderr = strings.ReplaceAll(stderr, remote, RedactURLOrPath(remote))
			}
			// Collapse whitespace so multi-line git stderr stays one log/attr value.
			stderr = strings.Join(strings.Fields(stderr), " ")
			return fmt.Errorf("%w (%s)", err, stderr)
		}
	}
	return err
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

// isShallowRepository returns true when the git repository at dir is shallow.
// An empty dir inherits the parent process's working directory, matching the
// semantics callers use when invoking Fetch with empty FetchOptions.Dir.
func isShallowRepository(ctx context.Context, dir string) bool {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-shallow-repository")
	cmd.Dir = dir
	disableTerminalPrompt(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
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
		// A transport-helper grandchild (e.g. git-remote-entire) can keep the
		// output pipe open after `git` is SIGKILLed, otherwise blocking
		// CombinedOutput indefinitely.
		execx.TerminateOnCancel(c)
		// Fail fast on interactive SSH prompts (e.g. a key passphrase with no
		// ssh-agent) when the caller marked ctx non-interactive. HTTPS token
		// auth rebuilds cmd.Env below (SSH is not used there), so this only
		// takes effect on the SSH/no-token paths that actually run ssh.
		applyNonInteractiveSSH(ctx, c)
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
