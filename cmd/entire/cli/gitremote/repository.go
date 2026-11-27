package gitremote

import (
	"context"
	"errors"
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

// SSHInvocation is the ssh client git itself would run for a remote operation.
type SSHInvocation struct {
	// Command is a shell command line, ready to extend and set as
	// GIT_SSH_COMMAND; a GIT_SSH program path is already shell-quoted.
	Command string
	// Program is the bare program path when Command came from GIT_SSH, which
	// git runs without a shell; "" otherwise.
	Program string
	// Variant is GIT_SSH_VARIANT, else ssh.variant, when either is set
	// (HasVariant); git detects the variant from the program name otherwise.
	Variant    string
	HasVariant bool
}

// ResolveSSH resolves the ssh invocation git itself would use, in git's own
// precedence order: the GIT_SSH_COMMAND environment variable, then the
// core.sshCommand git config value (read in dir when non-empty), then the
// GIT_SSH environment variable, falling back to plain "ssh" when none are set.
// A nil env reads the process environment.
//
// ok is false when git config could not be read (as opposed to holding
// nothing): the invocation is then resolved as if the config were empty, and a
// caller that would replace the user's command must not trust it.
func ResolveSSH(ctx context.Context, dir string, env []string) (inv SSHInvocation, ok bool) {
	if env == nil {
		env = os.Environ()
	}
	configCmd, configVariant, hasConfigVariant, ok := gitConfigSSH(ctx, dir, env)
	if v, found := envLookup(env, "GIT_SSH_VARIANT"); found {
		inv.Variant, inv.HasVariant = v, true
	} else if hasConfigVariant {
		inv.Variant, inv.HasVariant = configVariant, true
	}
	if v, found := envLookup(env, "GIT_SSH_COMMAND"); found {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			inv.Command = trimmed
			return inv, true
		}
	}
	if configCmd != "" {
		inv.Command = configCmd
		return inv, ok
	}
	if v, found := envLookup(env, "GIT_SSH"); found {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			inv.Command, inv.Program = shellQuoteProgram(trimmed), trimmed
			return inv, ok
		}
	}
	inv.Command = "ssh"
	return inv, ok
}

// EffectiveSSHCommand is ResolveSSH's command line. git runs GIT_SSH as a bare
// program path but GIT_SSH_COMMAND through sh, so a GIT_SSH path is
// shell-quoted when sh would otherwise mangle it, such as the backslashes in a
// Windows path. Config that cannot be read is treated as unset.
func EffectiveSSHCommand(ctx context.Context, dir string, env []string) string {
	inv, _ := ResolveSSH(ctx, dir, env)
	return inv.Command
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

// gitConfigSSH looks up core.sshCommand and ssh.variant in one `git config`
// call, run with env so the lookup honors any HOME/GIT_CONFIG_* overrides
// present in env (e.g. in tests). Unset values are "". ok is false when the
// lookup itself failed, as opposed to finding nothing (git's exit status 1).
func gitConfigSSH(ctx context.Context, dir string, env []string) (sshCommand, variant string, hasVariant, ok bool) {
	cmd := exec.CommandContext(ctx, "git", "config", "--get-regexp", `^(core\.sshcommand|ssh\.variant)$`)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		return "", "", false, errors.As(err, &exitErr) && exitErr.ExitCode() == 1
	}
	// Last value wins, as for git's own single-valued lookups.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "core.sshcommand":
			sshCommand = strings.TrimSpace(value)
		case "ssh.variant":
			variant, hasVariant = value, true
		}
	}
	return sshCommand, variant, hasVariant, true
}
