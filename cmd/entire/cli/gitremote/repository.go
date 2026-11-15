package gitremote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Repository identifies one repository on a known forge, whichever URL
// reaches it: `git@github.com:o/r`, `https://github.com/o/r.git`, and the
// Entire mirror `entire://<cluster>/gh/o/r` all yield the same value. Owner
// and Repo are lowercased: the only known forge is GitHub, which resolves both
// case-insensitively.
type Repository struct {
	Forge, Owner, Repo string
}

// Repository returns the known-forge repository this URL names. ok is false
// when the forge is unknown — an unrecognized host, an Entire-native repo, or
// an SSH host alias (see ResolveRepository for the alias case) — and when the
// transport is neither a direct one (NamesGitHost) nor entire://. A remote
// helper scheme such as `bogus+ssh://github.com/o/r` parses with github.com as
// its host, but git hands the push to git-remote-bogus, which can send it
// anywhere; the host proves nothing about where the data goes.
func (i *Info) Repository() (Repository, bool) {
	if i.Protocol != ProtocolEntire && !NamesGitHost(i.Protocol) {
		return Repository{}, false
	}
	if _, known := i.UpstreamHost(); !known {
		return Repository{}, false
	}
	return Repository{Forge: i.Forge, Owner: strings.ToLower(i.Owner), Repo: strings.ToLower(i.Repo)}, true
}

// NamesGitHost reports whether a URL on this protocol is served by its host
// directly, so the host is where git sends the data: ssh, https, http, and
// git. Every other scheme fails closed — entire:// names a cluster rather than
// a git host, file:// names no host, and any other scheme is a remote helper
// git runs instead of connecting. git's git+ssh:// and ssh+git:// spellings
// arrive here as ProtocolSSH (see normalizeProtocol).
func NamesGitHost(protocol string) bool {
	switch protocol {
	case ProtocolSSH, ProtocolHTTPS, ProtocolHTTP, ProtocolGit:
		return true
	default:
		return false
	}
}

// sshConfigTimeout bounds the complete optional discovery attempt, including
// git config. Keep this generous for checkpoint push matching: a timeout skips
// synchronization. Session hooks supply their own shorter outer deadline.
const sshConfigTimeout = 5 * time.Second

// ResolveRepository resolves a known-forge identity for checkpoint matching.
// It preserves Repository's normalization and transport restrictions and fails
// closed on discovery errors. ResolveInfo documents the SSH support boundary.
func ResolveRepository(ctx context.Context, dir string, env []string, rawURL string) (Repository, bool) {
	info, err := ResolveInfo(ctx, dir, env, rawURL)
	if err != nil {
		return Repository{}, false
	}
	return info.Repository()
}

// discoveryOutput bounds cleanup too: Match exec descendants may retain pipes.
// Unix kills the process group; Windows currently kills only the direct child.
// Escaped descendants cannot extend the wait by execx's default ten seconds.
func discoveryOutput(cmd *exec.Cmd) ([]byte, error) {
	execx.TerminateOnCancel(cmd)
	cmd.WaitDelay = 25 * time.Millisecond
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// The command succeeded; only a descendant's inherited pipe remains.
		// Keep the captured output and leave that unrelated helper running.
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("SSH discovery subprocess: %w", err)
	}
	return out, nil
}

// EffectiveSSHCommand resolves the ssh invocation git itself would use, in
// git's own precedence order: the GIT_SSH_COMMAND environment variable, then
// the core.sshCommand git config value (read in dir when non-empty), then the
// GIT_SSH environment variable, falling back to plain "ssh" when none are set.
// A nil env reads the process environment.
//
// The result is a shell command, ready to extend and set as GIT_SSH_COMMAND.
// git runs GIT_SSH as a bare program path but GIT_SSH_COMMAND through sh, so a
// GIT_SSH path is shell-quoted when sh would otherwise mangle it, such as the
// backslashes in a Windows path.
func EffectiveSSHCommand(ctx context.Context, dir string, env []string) string {
	command, err := effectiveSSHCommand(ctx, dir, env)
	if err != nil {
		logging.Debug(ctx, "SSH command configuration unavailable; using transport fallback", "error", err.Error())
	}
	return command
}

// Discovery must distinguish an absent setting from a failed config lookup.
func effectiveSSHCommand(ctx context.Context, dir string, env []string) (string, error) {
	if env == nil {
		env = os.Environ()
	}
	if v, ok := envLookup(env, "GIT_SSH_COMMAND"); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed, nil
		}
	}
	v, err := gitConfigSSHCommand(ctx, dir, env)
	if v != "" {
		return v, err
	}
	if v, ok := envLookup(env, "GIT_SSH"); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return shellQuoteProgram(trimmed), err
		}
	}
	return "ssh", err
}

// shellQuoteProgram single-quotes path for sh unless every rune is safe bare.
// Unlike a display quoter, a backslash always needs quoting: git for Windows
// also runs GIT_SSH_COMMAND through sh.
func shellQuoteProgram(path string) string {
	if !strings.ContainsFunc(path, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		}
		return !strings.ContainsRune(`_@%+=:,./-`, r)
	}) {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
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
// tests). An unset key is a successful empty result; other failures are errors.
func gitConfigSSHCommand(ctx context.Context, dir string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "core.sshCommand")
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	out, err := discoveryOutput(cmd)
	if ctx.Err() != nil {
		return "", fmt.Errorf("read SSH command configuration: %w", ctx.Err())
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil // git config --get: key absent
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
