package cli

import (
	"runtime"
	"strings"
)

// Plugin process environment is filtered, not inherited wholesale, so that
// credentials and other tool-specific config don't leak into third-party
// binaries on PATH. The threat model is "defense in depth" — a malicious
// plugin can still read files under $HOME — but a small allowlist makes
// accidental exposure (AWS_*, GITHUB_TOKEN, OPENAI_API_KEY) require an
// explicit user opt-in.

// pluginEnvAllowed is the exact-match allowlist of OS-plumbing and
// TUI-detection variables that virtually every program needs. Add entries
// here only when the whole namespace is unambiguously non-credential.
var pluginEnvAllowed = map[string]struct{}{
	// POSIX basics
	"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {},
	"SHELL": {}, "PWD": {}, "TMPDIR": {}, "TZ": {},

	// Locale (LC_* covered by prefix below)
	"LANG": {}, "LANGUAGE": {},

	// Terminal / color rendering
	"TERM": {}, "TERM_PROGRAM": {}, "TERM_PROGRAM_VERSION": {},
	"COLORTERM": {}, "NO_COLOR": {}, "FORCE_COLOR": {},
	"CLICOLOR": {}, "CLICOLOR_FORCE": {},

	// CI detection — broad set because most tools can't detect CI any
	// other way and many writeups assume these are visible.
	"CI": {}, "GITHUB_ACTIONS": {}, "GITLAB_CI": {},
	"BUILDKITE": {}, "CIRCLECI": {}, "JENKINS_URL": {},
	"TEAMCITY_VERSION": {}, "TRAVIS": {},

	// Proxies. curl and git read both upper- and lowercase forms; we keep
	// both because the user's shell may set either.
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {}, "ALL_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "no_proxy": {}, "all_proxy": {},

	// SSH agent — plugins that fetch via git/ssh keep working.
	"SSH_AUTH_SOCK": {}, "SSH_CONNECTION": {},

	// Windows essentials. Names are upper-case here; lookup is
	// case-insensitive on Windows.
	"SYSTEMROOT": {}, "WINDIR": {}, "COMSPEC": {}, "PATHEXT": {},
	"APPDATA": {}, "LOCALAPPDATA": {}, "PROGRAMDATA": {},
	"PROGRAMFILES": {}, "PROGRAMFILES(X86)": {},
	"USERPROFILE": {}, "USERNAME": {}, "HOMEDRIVE": {}, "HOMEPATH": {},

	// Documented in CLAUDE.md as the toggle for accessibility mode.
	"ACCESSIBLE": {},
}

// pluginEnvPrefixes are namespaces we either own (ENTIRE_*) or that are
// long-standing passthrough conventions (LC_*, XDG_*).
var pluginEnvPrefixes = []string{
	"ENTIRE_",
	"LC_",
	"XDG_",
}

// pluginEnvOverrideVar lets a user opt names back into the plugin env
// without a CLI release. Comma-separated list of exact names or
// `PREFIX_*` wildcards. Example:
//
//	ENTIRE_PLUGIN_ENV="AWS_*,GH_TOKEN,EDITOR"
const pluginEnvOverrideVar = "ENTIRE_PLUGIN_ENV"

// pluginEnv builds the child environment from the parent. Only allowlisted
// names plus user-declared overrides are forwarded. Caller-provided extras
// are appended verbatim; per cmd/exec docs, when Env contains duplicate
// keys the last one wins, so extras override any matching parent value.
func pluginEnv(parent []string, extra ...string) []string {
	exact, prefixes := parsePluginEnvOverride(lookupEnv(parent, pluginEnvOverrideVar))
	out := make([]string, 0, len(parent)+len(extra))
	for _, kv := range parent {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if isPluginEnvAllowed(kv[:eq], exact, prefixes) {
			out = append(out, kv)
		}
	}
	out = append(out, extra...)
	return out
}

func isPluginEnvAllowed(name string, userExact map[string]struct{}, userPrefixes []string) bool {
	if _, ok := pluginEnvAllowed[name]; ok {
		return true
	}
	if runtime.GOOS == windowsGOOS {
		// Env var names are case-insensitive on Windows. The allowlist
		// stores the canonical upper-case form, so normalize here.
		if _, ok := pluginEnvAllowed[strings.ToUpper(name)]; ok {
			return true
		}
	}
	for _, p := range pluginEnvPrefixes {
		if hasPrefixOSAware(name, p) {
			return true
		}
	}
	if _, ok := userExact[name]; ok {
		return true
	}
	for _, p := range userPrefixes {
		if hasPrefixOSAware(name, p) {
			return true
		}
	}
	return false
}

// parsePluginEnvOverride splits the comma-separated override into exact
// names and prefix patterns (`FOO_*`). Whitespace around items is trimmed.
func parsePluginEnvOverride(s string) (exact map[string]struct{}, prefixes []string) {
	if s == "" {
		return nil, nil
	}
	exact = map[string]struct{}{}
	for _, raw := range strings.Split(s, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if strings.HasSuffix(item, "*") {
			prefixes = append(prefixes, strings.TrimSuffix(item, "*"))
			continue
		}
		exact[item] = struct{}{}
	}
	return exact, prefixes
}

// lookupEnv returns the value of name from a KEY=VALUE slice, or empty if
// absent. Used to read ENTIRE_PLUGIN_ENV out of the parent slice without
// touching process state, so tests stay parallel-safe.
func lookupEnv(env []string, name string) string {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

func hasPrefixOSAware(name, prefix string) bool {
	if runtime.GOOS == windowsGOOS {
		return strings.HasPrefix(strings.ToUpper(name), strings.ToUpper(prefix))
	}
	return strings.HasPrefix(name, prefix)
}
