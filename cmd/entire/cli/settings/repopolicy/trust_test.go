package repopolicy

import (
	"path/filepath"
	"testing"
)

func TestEgressDecision_RequiresEveryFetchAndPushURL(t *testing.T) {
	root, _ := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	runPolicyGit(t, root, "remote", "set-url", "--add", "origin", "https://gitlab.com/acme/widgets.git")
	runPolicyGit(t, root, "remote", "set-url", "--add", "--push", "origin", "git@codeberg.org:acme/widgets.git")
	setPolicyGlobal(t, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets","gitlab.com/acme/widgets"]}}`)

	policy := policyAt(t, root)
	if policy.Trust.Allowed || policy.Trust.Reason != TrustReasonUntrusted {
		t.Fatalf("trust = %+v, want held until every fetch and push URL is trusted", policy.Trust)
	}
	if len(policy.Trust.Identity.OriginKeys) != 3 {
		t.Fatalf("origin keys = %q, want all three configured identities", policy.Trust.Identity.OriginKeys)
	}
}

func TestEgressDecision_UnparseableConfiguredOriginHolds(t *testing.T) {
	root, _ := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", filepath.Join(t.TempDir(), "origin.git"))
	setPolicyGlobal(t, `{"global":{"enabled":true,"trust_all":true}}`)

	policy := policyAt(t, root)
	if policy.Trust.Allowed || policy.Trust.Reason != TrustReasonInvalidOrigin {
		t.Fatalf("trust = %+v, want invalid configured origin to hold", policy.Trust)
	}
}

func TestEgressDecision_PathAppliesOnlyWhenOriginAbsent(t *testing.T) {
	root, _ := newPolicyRepo(t)
	setPolicyGlobal(t, `{"global":{"enabled":true,"trusted_paths":["`+filepath.ToSlash(root)+`"]}}`)

	withoutOrigin := policyAt(t, root)
	if !withoutOrigin.Trust.Allowed || withoutOrigin.Trust.Identity.Path == "" {
		t.Fatalf("trust without origin = %+v, want exact path consent", withoutOrigin.Trust)
	}

	runPolicyGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	withOrigin := policyAt(t, root)
	if withOrigin.Trust.Allowed || withOrigin.Trust.Identity.Path != "" {
		t.Fatalf("trust with origin = %+v, path consent must not apply", withOrigin.Trust)
	}
}

func TestEgressDecision_GlobalOffDeniesEvenWhenTrustAllIsTrue(t *testing.T) {
	root, _ := newPolicyRepo(t)
	setPolicyGlobal(t, `{"global":{"enabled":false,"trust_all":true}}`)

	policy := policyAt(t, root)
	if policy.Trust.Allowed || policy.Trust.Reason != TrustReasonInactive {
		t.Fatalf("trust = %+v, disabled global tier must deny trust_all", policy.Trust)
	}
}

// A repo-enabled repository is gated exactly like a globally tracked one
// while the global tier is on: once user-level hooks fire everywhere,
// consent has to live in the user settings file. With the tier off (or
// unconfigured) nothing changes from main.
func TestEgressDecision_LocalActivationIsGatedWhileGlobalIsOn(t *testing.T) {
	t.Parallel()
	_, repository := newPolicyRepo(t) // git-initialized, no origin: identity is the path
	policy := RepoPolicy{Active: true, ActivationSource: ActivationLocal, WorktreeRoot: repository.WorktreeRoot}

	held := DecideEgress(t.Context(), policy, &GlobalConfig{Enabled: true}, repository)
	if held.Allowed || held.Reason != TrustReasonUntrusted || held.Identity.Path != repository.WorktreeRoot {
		t.Fatalf("untrusted local repo with global on = %+v, want held by path identity", held)
	}

	trusted := DecideEgress(t.Context(), policy, &GlobalConfig{Enabled: true, TrustedPaths: []string{repository.WorktreeRoot}}, repository)
	if !trusted.Allowed || trusted.Source != TrustSourceRepo {
		t.Fatalf("trusted-path local repo with global on = %+v, want allowed by repo trust", trusted)
	}

	tierOff := DecideEgress(t.Context(), policy, nil, repository)
	if !tierOff.Allowed || tierOff.Source != TrustSourceLocal {
		t.Fatalf("local repo with global unconfigured = %+v, want main's behavior (allowed)", tierOff)
	}
}

func TestReadRepoActivation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		project string // "" means absent
		local   string // "" means absent
		want    RepoActivation
		wantErr bool
	}{
		{name: "no files"},
		{name: "project enabled", project: `{"enabled":true}`, want: RepoActivation{Configured: true, Enabled: true}},
		{name: "project without key defaults enabled", project: `{}`, want: RepoActivation{Configured: true, Enabled: true}},
		{name: "project disabled", project: `{"enabled":false}`, want: RepoActivation{Configured: true}},
		{name: "local without key is not activation", local: `{"log_level":"DEBUG"}`},
		{name: "local enabled", local: `{"enabled":true}`, want: RepoActivation{Configured: true, Enabled: true}},
		{name: "local veto wins over project", project: `{"enabled":true}`, local: `{"enabled":false}`, want: RepoActivation{Configured: true}},
		{name: "non-boolean enabled is an error", project: `{"enabled":"yes"}`, wantErr: true},
		{name: "malformed json is an error", project: `{"enabled":tru`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.project != "" {
				writePolicyFile(t, root, ".entire/settings.json", tc.project)
			}
			if tc.local != "" {
				writePolicyFile(t, root, ".entire/settings.local.json", tc.local)
			}
			got, err := ReadRepoActivation(root)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ReadRepoActivation = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadRepoActivation: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ReadRepoActivation = %+v, want %+v", got, tc.want)
			}
		})
	}
}
