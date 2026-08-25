package repopolicy

import (
	"errors"
	"os"
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

func TestEgressDecision_CommittedProjectSettingsNeverGrantConsent(t *testing.T) {
	root, _ := newPolicyRepo(t)
	setPolicyGlobal(t, `{"global":{"enabled":true}}`)
	writePolicyFile(t, root, ".entire/settings.json", `{"enabled":true}`)
	runPolicyGit(t, root, "add", ".entire/settings.json")
	runPolicyGit(t, root, "commit", "--no-gpg-sign", "-m", "tracked settings")

	policy := policyAt(t, root)
	if !policy.Active || policy.ActivationSource != ActivationGlobal || policy.Trust.Allowed {
		t.Fatalf("policy = %+v, committed project settings must not grant egress", policy)
	}
}

func TestTrustLedger_RejectsCopiedRepositoryIdentity(t *testing.T) {
	_, first := newPolicyRepo(t)
	_, second := newPolicyRepo(t)
	if err := WriteTrustGrantLedger(first, TrustGrantLedger{OriginKeys: []string{"github.com/acme/widgets"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(trustLedgerPath(first))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRegistryDir(second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trustLedgerPath(second), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadTrustGrantLedger(second); err == nil {
		t.Fatal("copied trust ledger unexpectedly passed repository identity validation")
	}
}

func TestModifyTrustGrantLedger_CallbackFailureDropsOwnership(t *testing.T) {
	t.Parallel()
	_, repository := newPolicyRepo(t)
	if err := WriteTrustGrantLedger(repository, TrustGrantLedger{
		OriginKeys: []string{"github.com/acme/widgets"},
	}); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("settings write failed")
	err := ModifyTrustGrantLedger(t.Context(), repository, func(TrustGrantLedger) (TrustGrantLedger, error) {
		return TrustGrantLedger{}, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ModifyTrustGrantLedger error = %v, want %v", err, wantErr)
	}
	ledger, found, err := ReadTrustGrantLedger(repository)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("trust ledger missing after failed mutation")
	}
	if len(ledger.OriginKeys) != 0 || len(ledger.Paths) != 0 {
		t.Fatalf("trust ledger retained stale ownership after failed mutation: %+v", ledger)
	}
}
