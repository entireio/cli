package gitremote

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
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

// sshConfigTimeout bounds `ssh -G`, which reads config and never connects, so
// it normally answers in milliseconds. The bound is generous because expiry
// is not an error the user sees but a silent "no match" that gates the push,
// and a loaded machine should not be enough to cause it.
const sshConfigTimeout = 5 * time.Second

// ResolveRepository is Info.Repository for a raw remote URL, additionally
// resolving an SSH host alias (`git@github-work:o/r` with `Host github-work` /
// `HostName github.com` in ~/.ssh/config) through `ssh -G`, which evaluates
// the user's ssh config exactly as the push would, without connecting.
//
// The alias is resolved only when git would run plain `ssh` (see
// EffectiveSSHCommand): a custom GIT_SSH_COMMAND, core.sshCommand, or GIT_SSH
// may read a different config or not be OpenSSH at all, so its hostname is not
// knowable here and the URL stays unresolved. Every failure — unparseable URL,
// unknown forge, missing ssh, timeout — reports false.
//
// dir and env follow GetPushURLsInDir: pass the target worktree and
// gitrepo.EnvWithoutRepoOverrides() from a hook, or "" and nil to use the
// ambient repository and environment.
func ResolveRepository(ctx context.Context, dir string, env []string, rawURL string) (Repository, bool) {
	info, err := ParseURL(rawURL)
	if err != nil {
		return Repository{}, false
	}
	if r, ok := info.Repository(); ok {
		return r, true
	}
	if info.Protocol != ProtocolSSH || info.Forge != "" {
		return Repository{}, false
	}
	host, ok := resolveSSHHostname(ctx, dir, env, info.Host)
	if !ok {
		return Repository{}, false
	}
	resolved := *info
	resolved.Host, resolved.Forge = host, hostToForge[host]
	return resolved.Repository()
}

// resolveSSHHostname returns the HostName ssh would connect to for host.
func resolveSSHHostname(ctx context.Context, dir string, env []string, host string) (string, bool) {
	// A leading dash would be read as an option; git refuses such hosts too.
	if host == "" || strings.HasPrefix(host, "-") {
		return "", false
	}
	if EffectiveSSHCommand(ctx, dir, env) != "ssh" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, sshConfigTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-G", host)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "hostname "); ok {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
				return v, true
			}
		}
	}
	return "", false
}

// EffectiveSSHCommand resolves the ssh invocation git itself would use, in
// git's own precedence order: the GIT_SSH_COMMAND environment variable, then
// the core.sshCommand git config value (read in dir when non-empty), then the
// GIT_SSH environment variable, falling back to plain "ssh" when none are set.
// A nil env reads the process environment.
func EffectiveSSHCommand(ctx context.Context, dir string, env []string) string {
	if env == nil {
		env = os.Environ()
	}
	if v, ok := envLookup(env, "GIT_SSH_COMMAND"); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	if v := gitConfigSSHCommand(ctx, dir, env); v != "" {
		return v
	}
	if v, ok := envLookup(env, "GIT_SSH"); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return "ssh"
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
func gitConfigSSHCommand(ctx context.Context, dir string, env []string) string {
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "core.sshCommand")
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
