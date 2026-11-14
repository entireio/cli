package gitremote

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// ResolveInfo parses a configured remote and discovers the forge for unknown
// direct SSH hosts through plain OpenSSH's -G configuration evaluation. Only
// Forge is enriched: Host, Port and the original transport URL must still be
// used for transport and authentication. Forge identity does not authorize
// credential redirection or a new checkpoint destination.
//
// A non-nil Info with an error means discovery failed transiently, not that the
// forge is unsupported. A successful unknown hostname or a custom SSH command
// leaves Forge empty without an error. Persistent decisions must use this
// distinction; optional displays may use ResolveInfoBestEffort instead.
//
// dir/env follow GetRemoteURLInDirEnv: ambient callers use ""/nil; callers
// targeting another worktree pass its directory and EnvWithoutRepoOverrides.
// The five-second budget includes git config and honors shorter deadlines.
// ssh -G evaluates Host/Match, including local Match exec commands; it is not
// universally side-effect-free. Username, explicit port and directory are
// passed through, but arbitrary Git/SSH wrappers are deliberately unsupported.
func ResolveInfo(ctx context.Context, dir string, env []string, rawURL string) (*Info, error) {
	info, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return info, fmt.Errorf("resolve remote identity: %w", err)
	}
	if info.Protocol != ProtocolSSH || info.Forge != "" {
		return info, nil
	}
	// Git's helper::address syntax is not a direct SSH endpoint either.
	if !strings.Contains(rawURL, "://") && strings.Contains(rawURL, "::") {
		return info, nil
	}
	target, err := sshIdentityTarget(rawURL, info)
	if err != nil {
		return info, err
	}
	if env == nil {
		env = os.Environ()
	}
	host, err := cachedSSHHostname(ctx, dir, env, target)
	if ctx.Err() != nil {
		return info, fmt.Errorf("resolve remote identity: %w", ctx.Err())
	}
	if err != nil {
		return info, err
	}
	info.Forge = hostToForge[host]
	return info, nil
}

// ResolveInfoBestEffort retains the parsed identity when optional discovery
// fails, but never hides caller cancellation/deadline expiry or parse errors.
// Do not use it to persist unsupported/disabled decisions.
func ResolveInfoBestEffort(ctx context.Context, dir string, env []string, rawURL string) (*Info, error) {
	info, err := ResolveInfo(ctx, dir, env, rawURL)
	if ctx.Err() != nil {
		return info, fmt.Errorf("resolve remote identity: %w", ctx.Err())
	}
	if info != nil {
		return info, nil
	}
	return nil, err
}

// Keep userinfo out of Info, which is also used for diagnostics and transport
// URL construction. These values are only argv inputs to configuration lookup.
type sshTarget struct{ host, user, port string }

func sshIdentityTarget(rawURL string, info *Info) (sshTarget, error) {
	target := sshTarget{host: info.Host, port: info.Port}
	if strings.Contains(rawURL, "://") {
		u, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil {
			return target, errors.New("invalid SSH identity URL")
		}
		if u.User != nil {
			target.user = u.User.Username()
		}
	} else {
		authority, _, _ := strings.Cut(strings.TrimSpace(rawURL), ":")
		if user, _, ok := strings.Cut(authority, "@"); ok {
			target.user = user
		}
	}
	if !safeSSHValue(target.host) || strings.ContainsRune(target.host, '@') || (target.user != "" && !safeSSHValue(target.user)) {
		return target, errors.New("invalid SSH identity host or username")
	}
	if target.port != "" {
		port, err := strconv.ParseUint(target.port, 10, 16)
		if err != nil || port == 0 {
			return target, errors.New("invalid SSH identity port")
		}
	}
	return target, nil
}

func safeSSHValue(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-:@", c) {
			continue
		}
		return false
	}
	return true
}

func resolveSSHHostname(ctx context.Context, dir string, env []string, target sshTarget) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sshConfigTimeout)
	defer cancel()
	command, err := effectiveSSHCommand(ctx, dir, env)
	if err != nil {
		return "", err
	}
	if command != "ssh" {
		return "", nil
	}
	args := []string{"-G"}
	if target.user != "" {
		args = append(args, "-l", target.user)
	}
	if target.port != "" {
		args = append(args, "-p", target.port)
	}
	args = append(args, target.host)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := discoveryOutput(cmd)
	if ctx.Err() != nil {
		return "", fmt.Errorf("resolve SSH hostname: %w", ctx.Err())
	}
	if err != nil {
		return "", err
	}
	var host string
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "hostname" {
			continue
		}
		if host != "" || len(fields) != 2 || !safeSSHValue(fields[1]) || strings.ContainsRune(fields[1], '@') {
			return "", errors.New("invalid SSH hostname configuration output")
		}
		host = strings.ToLower(fields[1])
	}
	if host == "" {
		return "", errors.New("missing SSH hostname configuration output")
	}
	return host, nil
}

type resolutionCacheKey struct{}
type sshLookupKey struct {
	target   sshTarget
	dir, env string
}
type sshLookupResult struct {
	ready chan struct{}
	host  string
	err   error
}
type resolutionCache struct {
	mu      sync.Mutex
	results map[sshLookupKey]*sshLookupResult
}

// WithResolutionCache starts one operation's SSH discovery cache. It is never
// global or persisted. Start a fresh operation after changing CWD or Git/SSH
// config. Environment, explicit directory, user, port and host are in the key;
// paths on the same endpoint share discovery. Errors retain their distinction.
func WithResolutionCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, resolutionCacheKey{}, &resolutionCache{results: make(map[sshLookupKey]*sshLookupResult)})
}

// EnsureResolutionCache shares the surrounding operation's cache when present,
// or starts one for a standalone library operation. Unlike WithResolutionCache,
// it must not be used at the boundary of a new command invocation.
func EnsureResolutionCache(ctx context.Context) context.Context {
	if _, ok := ctx.Value(resolutionCacheKey{}).(*resolutionCache); ok {
		return ctx
	}
	return WithResolutionCache(ctx)
}

func cachedSSHHostname(ctx context.Context, dir string, env []string, target sshTarget) (string, error) {
	cache, ok := ctx.Value(resolutionCacheKey{}).(*resolutionCache)
	if !ok {
		return resolveSSHHostname(ctx, dir, env, target)
	}
	key := sshLookupKey{target: target, dir: dir, env: strings.Join(env, "\x00")}
	cache.mu.Lock()
	result, found := cache.results[key]
	if !found {
		result = &sshLookupResult{ready: make(chan struct{})}
		cache.results[key] = result
	}
	cache.mu.Unlock()
	if !found {
		result.host, result.err = resolveSSHHostname(ctx, dir, env, target)
		close(result.ready)
	}
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("resolve SSH hostname: %w", ctx.Err())
	case <-result.ready:
		return result.host, result.err
	}
}
