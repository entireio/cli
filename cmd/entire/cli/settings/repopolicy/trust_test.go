package repopolicy

import (
	"context"
	"errors"
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

// A bare local path is a legitimate origin (a mirror, a bare repo on a share)
// that does not reduce to host/owner/repo. It must neither defeat trust_all
// nor leave the repo with no way to be trusted: the identity flips to the
// path key, exactly as it does for a repo with no origin at all.
func TestEgressDecision_UnparseableOriginFallsBackToPathKey(t *testing.T) {
	root, _ := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", filepath.Join(t.TempDir(), "origin.git"))

	setPolicyGlobal(t, `{"global":{"enabled":true}}`)
	held := policyAt(t, root)
	if held.Trust.Allowed || held.Trust.Reason != TrustReasonUntrusted {
		t.Fatalf("trust = %+v, want held as untrusted (not an identity error)", held.Trust)
	}
	if held.Trust.Identity.OriginKeyed() || held.Trust.Identity.Path == "" {
		t.Fatalf("identity = %+v, want the path key", held.Trust.Identity)
	}

	setPolicyGlobal(t, `{"global":{"enabled":true,"trusted_paths":["`+filepath.ToSlash(root)+`"]}}`)
	if byPath := policyAt(t, root); !byPath.Trust.Allowed || byPath.Trust.Source != TrustSourceRepo {
		t.Fatalf("trust = %+v, want trusted by path", byPath.Trust)
	}

	setPolicyGlobal(t, `{"global":{"enabled":true,"trust_all":true}}`)
	if all := policyAt(t, root); !all.Trust.Allowed || all.Trust.Source != TrustSourceAll {
		t.Fatalf("trust = %+v, want trust_all to cover a repo whose origin cannot be normalized", all.Trust)
	}
}

// Consent for every repo cannot depend on being able to name this one: a
// mixed origin (one keyable URL, one bare path) and a repo with no origin
// are both covered by trust_all.
func TestEgressDecision_TrustAllNeedsNoIdentity(t *testing.T) {
	root, _ := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	runPolicyGit(t, root, "remote", "set-url", "--add", "--push", "origin", filepath.Join(t.TempDir(), "mirror.git"))
	setPolicyGlobal(t, `{"global":{"enabled":true,"trust_all":true}}`)

	policy := policyAt(t, root)
	if !policy.Trust.Allowed || policy.Trust.Source != TrustSourceAll {
		t.Fatalf("trust = %+v, want allowed by trust_all", policy.Trust)
	}
	if policy.Trust.Identity.OriginKeyed() {
		t.Fatalf("identity = %+v, want the path key (one push URL is unkeyable, so the key set must not be partial)", policy.Trust.Identity)
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
			got, err := ReadRepoActivation(t.Context(), root)
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

// swapSyncRemoteResolver installs a fake election for the test and restores
// the default afterwards. Not for parallel tests (package-level seam).
func swapSyncRemoteResolver(t *testing.T, fn SyncRemoteResolver) {
	t.Helper()
	previous := ResolveSyncRemote
	ResolveSyncRemote = fn
	t.Cleanup(func() { ResolveSyncRemote = previous })
}

// Consent is keyed on the remote checkpoints actually go to. With the sync
// remote elected away from origin (a fork, checkpoint_push_remote, a captured
// push), trusting origin's key must NOT open egress, and trusting the elected
// remote's key must.
func TestEgressDecision_KeysOnElectedSyncRemote(t *testing.T) {
	root, _ := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	runPolicyGit(t, root, "remote", "add", "fork", "https://github.com/me/widgets.git")
	swapSyncRemoteResolver(t, func(ctx context.Context, repository Repository) (SyncRemote, error) {
		return RemoteURLsInDir(ctx, repository.WorktreeRoot, "fork")
	})

	setPolicyGlobal(t, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
	byOrigin := policyAt(t, root)
	if byOrigin.Trust.Allowed || byOrigin.Trust.Reason != TrustReasonUntrusted {
		t.Fatalf("trust = %+v, want held: origin's key does not cover the fork checkpoints go to", byOrigin.Trust)
	}
	if got := byOrigin.Trust.Identity; got.RemoteName != "fork" || len(got.OriginKeys) != 1 || got.OriginKeys[0] != "github.com/me/widgets" {
		t.Fatalf("identity = %+v, want keyed on fork's URL", got)
	}

	setPolicyGlobal(t, `{"global":{"enabled":true,"trusted_origins":["github.com/me/widgets"]}}`)
	if byFork := policyAt(t, root); !byFork.Trust.Allowed || byFork.Trust.Source != TrustSourceRepo {
		t.Fatalf("trust = %+v, want trusted via the elected remote's key", byFork.Trust)
	}
}

// A re-election changes the key, so consent recorded for the old destination
// stops covering the new one and the next push re-asks.
func TestEgressDecision_ReelectionReasksConsent(t *testing.T) {
	root, _ := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	runPolicyGit(t, root, "remote", "add", "fork", "https://github.com/me/widgets.git")
	elected := "origin"
	swapSyncRemoteResolver(t, func(ctx context.Context, repository Repository) (SyncRemote, error) {
		return RemoteURLsInDir(ctx, repository.WorktreeRoot, elected)
	})
	setPolicyGlobal(t, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
	if before := policyAt(t, root); !before.Trust.Allowed {
		t.Fatalf("trust = %+v, want trusted while origin is elected", before.Trust)
	}
	elected = "fork"
	if after := policyAt(t, root); after.Trust.Allowed || after.Trust.Reason != TrustReasonUntrusted {
		t.Fatalf("trust = %+v, want held after the election moved to fork", after.Trust)
	}
}

// An election failure (unreadable settings, checkpoint_push_remote naming a
// missing remote) disables sync; the gate fails closed with it — except under
// trust_all, which never depended on an identity.
func TestEgressDecision_ElectionErrorHoldsUnlessTrustAll(t *testing.T) {
	root, _ := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	swapSyncRemoteResolver(t, func(context.Context, Repository) (SyncRemote, error) {
		return SyncRemote{}, errors.New("checkpoint_push_remote \"gone\" is not a configured git remote")
	})

	setPolicyGlobal(t, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
	if held := policyAt(t, root); held.Trust.Allowed || held.Trust.Reason != TrustReasonInvalidOrigin {
		t.Fatalf("trust = %+v, want invalid_origin hold on an election error", held.Trust)
	}
	setPolicyGlobal(t, `{"global":{"enabled":true,"trust_all":true}}`)
	if all := policyAt(t, root); !all.Trust.Allowed || all.Trust.Source != TrustSourceAll {
		t.Fatalf("trust = %+v, want trust_all to allow regardless of the election", all.Trust)
	}
}

// No remote at all: path identity, and the dedicated-store flag is carried
// for display when the resolver reports one.
func TestResolveTrustIdentity_NoRemoteIsPathKeyed(t *testing.T) {
	_, repository := newPolicyRepo(t)
	swapSyncRemoteResolver(t, func(context.Context, Repository) (SyncRemote, error) { return SyncRemote{}, nil })
	id, err := ResolveTrustIdentity(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if id.OriginKeyed() || id.Path != repository.WorktreeRoot || id.RemoteName != "" {
		t.Fatalf("identity = %+v, want path-keyed with no remote name", id)
	}
	swapSyncRemoteResolver(t, func(context.Context, Repository) (SyncRemote, error) {
		return SyncRemote{Name: "origin", URLs: []string{"https://github.com/acme/checkpoints.git"}, Dedicated: true}, nil
	})
	id, err = ResolveTrustIdentity(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Dedicated || !id.OriginKeyed() || id.OriginKeys[0] != "github.com/acme/checkpoints" {
		t.Fatalf("identity = %+v, want the dedicated store's key", id)
	}
}
