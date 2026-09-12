package repopolicy

import (
	"path/filepath"
	"testing"
)

// The exclude lists are the user's veto over a machine-wide tier, so their
// matching semantics are security-relevant: a miss captures a repo the user
// carved out. This pins each list's rule against ExcludedByGlobalConfig, the
// single entry point the classifier calls.
func TestExcludedByGlobalConfig_Matching(t *testing.T) {
	t.Parallel()
	root, repository := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	slashed := filepath.ToSlash(resolved)
	parent := filepath.ToSlash(filepath.Dir(resolved))

	tests := []struct {
		name     string
		config   GlobalConfig
		excluded bool
		wantErr  bool
	}{
		{name: "nothing configured", config: GlobalConfig{}},
		{name: "exact path", config: GlobalConfig{ExcludePaths: []string{slashed}}, excluded: true},
		{name: "parent directory covers children", config: GlobalConfig{ExcludePaths: []string{parent}}, excluded: true},
		{name: "parent glob covers children", config: GlobalConfig{ExcludePaths: []string{parent + "/*"}}, excluded: true},
		{name: "sibling with a shared prefix does not match", config: GlobalConfig{ExcludePaths: []string{slashed + "-other"}}},
		{name: "unrelated path does not match", config: GlobalConfig{ExcludePaths: []string{"/definitely/not/here"}}},
		{name: "exact list: identical path", config: GlobalConfig{ExcludePathsExact: []string{slashed}}, excluded: true},
		{name: "exact list: parent is not enough", config: GlobalConfig{ExcludePathsExact: []string{parent}}},
		{name: "exact list: glob characters are literal", config: GlobalConfig{ExcludePathsExact: []string{parent + "/*"}}},
		{name: "origin exact", config: GlobalConfig{ExcludeOrigins: []string{"github.com/acme/widgets"}}, excluded: true},
		{name: "origin owner glob", config: GlobalConfig{ExcludeOrigins: []string{"github.com/acme/*"}}, excluded: true},
		{name: "origin with .git suffix and scheme normalizes", config: GlobalConfig{ExcludeOrigins: []string{"https://github.com/acme/widgets.git"}}, excluded: true},
		{name: "origin different owner", config: GlobalConfig{ExcludeOrigins: []string{"github.com/other/*"}}},
		{name: "invalid path glob fails closed", config: GlobalConfig{ExcludePaths: []string{"/repo/[unclosed"}}, excluded: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.config
			excluded, err := ExcludedByGlobalConfig(t.Context(), &cfg, repository)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if excluded != tc.excluded {
				t.Fatalf("excluded = %v, want %v", excluded, tc.excluded)
			}
		})
	}
}

// A bare-path origin cannot be normalized to a host/owner/repo key, so an
// exclude_origins list cannot be evaluated against it: fail closed (excluded,
// with an error the caller surfaces) rather than capture a repo the user may
// have meant to exclude.
func TestExcludedByGlobalConfig_UnnormalizableOriginFailsClosed(t *testing.T) {
	t.Parallel()
	root, repository := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", t.TempDir())
	excluded, err := ExcludedByGlobalConfig(t.Context(), &GlobalConfig{ExcludeOrigins: []string{"github.com/acme/*"}}, repository)
	if err == nil || !excluded {
		t.Fatalf("excluded = %v, err = %v; want fail-closed exclusion with an error", excluded, err)
	}
	// With no origin rules the origin is never consulted.
	excluded, err = ExcludedByGlobalConfig(t.Context(), &GlobalConfig{ExcludePaths: []string{"/elsewhere"}}, repository)
	if err != nil || excluded {
		t.Fatalf("excluded = %v, err = %v; want not excluded when no origin rule exists", excluded, err)
	}
}

// A pushurl-only exclusion still applies: exclusions consult every configured
// URL of origin (fetch and push) — the conservative direction for a veto.
func TestExcludedByGlobalConfig_PushURLIsConsulted(t *testing.T) {
	t.Parallel()
	root, repository := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	runPolicyGit(t, root, "remote", "set-url", "--push", "origin", "git@codeberg.org:acme/widgets.git")
	excluded, err := ExcludedByGlobalConfig(t.Context(), &GlobalConfig{ExcludeOrigins: []string{"codeberg.org/acme/*"}}, repository)
	if err != nil || !excluded {
		t.Fatalf("excluded = %v, err = %v; want the pushurl to match", excluded, err)
	}
}
