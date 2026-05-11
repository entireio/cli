package cli

import (
	"slices"
	"testing"
)

func TestPluginEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		parent   []string
		extra    []string
		wantHave []string // names that must be in the result
		wantMiss []string // names that must NOT be in the result
	}{
		{
			name:     "OS basics pass through",
			parent:   []string{"PATH=/bin", "HOME=/h", "TERM=xterm", "LANG=en_US.UTF-8"},
			wantHave: []string{"PATH", "HOME", "TERM", "LANG"},
		},
		{
			name:     "credentials are dropped",
			parent:   []string{"AWS_ACCESS_KEY_ID=x", "GITHUB_TOKEN=y", "DATABASE_URL=z", "OPENAI_API_KEY=k", "PATH=/bin"},
			wantHave: []string{"PATH"},
			wantMiss: []string{"AWS_ACCESS_KEY_ID", "GITHUB_TOKEN", "DATABASE_URL", "OPENAI_API_KEY"},
		},
		{
			name:     "tool-specific config dropped",
			parent:   []string{"EDITOR=vim", "VISUAL=vim", "PAGER=less", "GIT_ASKPASS=/x", "PATH=/bin"},
			wantHave: []string{"PATH"},
			wantMiss: []string{"EDITOR", "VISUAL", "PAGER", "GIT_ASKPASS"},
		},
		{
			name:     "ENTIRE namespace passes",
			parent:   []string{"ENTIRE_FOO=1", "ENTIRE_AUTH_TOKEN=secret", "PATH=/bin"},
			wantHave: []string{"ENTIRE_FOO", "ENTIRE_AUTH_TOKEN", "PATH"},
		},
		{
			name:     "LC_ prefix passes",
			parent:   []string{"LC_ALL=C", "LC_TIME=en_US", "LC_NUMERIC=en_US"},
			wantHave: []string{"LC_ALL", "LC_TIME", "LC_NUMERIC"},
		},
		{
			name:     "XDG_ prefix passes",
			parent:   []string{"XDG_CONFIG_HOME=/h/.cfg", "XDG_DATA_HOME=/h/.data"},
			wantHave: []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME"},
		},
		{
			name:     "CI detection passes",
			parent:   []string{"CI=true", "GITHUB_ACTIONS=true", "BUILDKITE=true"},
			wantHave: []string{"CI", "GITHUB_ACTIONS", "BUILDKITE"},
		},
		{
			name:     "proxies pass in both cases",
			parent:   []string{"HTTP_PROXY=p", "https_proxy=p", "NO_PROXY=localhost"},
			wantHave: []string{"HTTP_PROXY", "https_proxy", "NO_PROXY"},
		},
		{
			name:     "extras are always added",
			parent:   []string{"PATH=/bin"},
			extra:    []string{"ENTIRE_CLI_VERSION=1.0", "ENTIRE_REPO_ROOT=/r"},
			wantHave: []string{"ENTIRE_CLI_VERSION", "ENTIRE_REPO_ROOT", "PATH"},
		},
		{
			name:     "override admits an exact name",
			parent:   []string{"ENTIRE_PLUGIN_ENV=AWS_PROFILE", "AWS_PROFILE=dev", "AWS_REGION=us-east-1", "PATH=/bin"},
			wantHave: []string{"AWS_PROFILE", "PATH"},
			wantMiss: []string{"AWS_REGION"},
		},
		{
			name:     "override admits a wildcard prefix",
			parent:   []string{"ENTIRE_PLUGIN_ENV=AWS_*", "AWS_PROFILE=dev", "AWS_REGION=us-east-1", "GITHUB_TOKEN=x"},
			wantHave: []string{"AWS_PROFILE", "AWS_REGION"},
			wantMiss: []string{"GITHUB_TOKEN"},
		},
		{
			name:     "override accepts mixed list with whitespace",
			parent:   []string{"ENTIRE_PLUGIN_ENV= AWS_* , GH_TOKEN ", "AWS_PROFILE=dev", "GH_TOKEN=t", "GITHUB_TOKEN=x"},
			wantHave: []string{"AWS_PROFILE", "GH_TOKEN"},
			wantMiss: []string{"GITHUB_TOKEN"},
		},
		{
			name:     "malformed entries are skipped",
			parent:   []string{"PATH=/bin", "", "=novalue", "NOEQUAL"},
			wantHave: []string{"PATH"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pluginEnv(tc.parent, tc.extra...)
			gotNames := envNames(got)
			for _, want := range tc.wantHave {
				if !slices.Contains(gotNames, want) {
					t.Errorf("missing %q from result %v", want, gotNames)
				}
			}
			for _, missing := range tc.wantMiss {
				if slices.Contains(gotNames, missing) {
					t.Errorf("expected %q to be filtered out, got %v", missing, gotNames)
				}
			}
		})
	}
}

// TestPluginEnv_ExtrasOverrideParent documents the cmd/exec contract: when
// the env slice contains duplicate keys the last value wins. We rely on
// this so caller-injected ENTIRE_CLI_VERSION / ENTIRE_REPO_ROOT always
// reflect the parent CLI's state, not a stale shell value.
func TestPluginEnv_ExtrasOverrideParent(t *testing.T) {
	t.Parallel()
	got := pluginEnv(
		[]string{"ENTIRE_CLI_VERSION=stale", "PATH=/bin"},
		"ENTIRE_CLI_VERSION=fresh",
	)
	// Last occurrence in the slice should be the override.
	var last string
	for _, kv := range got {
		if k, v, ok := splitKV(kv); ok && k == "ENTIRE_CLI_VERSION" {
			last = v
		}
	}
	if last != "fresh" {
		t.Errorf("ENTIRE_CLI_VERSION (last) = %q, want %q (full env: %v)", last, "fresh", got)
	}
}

// TestPluginEnv_OverrideVarItselfPasses confirms the override declaration
// is forwarded to the child (matches the ENTIRE_ prefix). Useful so
// plugins can introspect what was opened up.
func TestPluginEnv_OverrideVarItselfPasses(t *testing.T) {
	t.Parallel()
	got := pluginEnv([]string{"ENTIRE_PLUGIN_ENV=AWS_*", "PATH=/bin"})
	if !slices.Contains(envNames(got), "ENTIRE_PLUGIN_ENV") {
		t.Errorf("ENTIRE_PLUGIN_ENV should pass through to plugins; got %v", envNames(got))
	}
}

func envNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := splitKV(kv); ok {
			out = append(out, k)
		}
	}
	return out
}

func splitKV(kv string) (key, value string, ok bool) {
	for i := range len(kv) {
		if kv[i] == '=' {
			if i == 0 {
				return "", "", false
			}
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}
