package repopolicy

import (
	"context"
	"path/filepath"
	"testing"
)

func stubLocalSettingsVerdict(t *testing.T, verdict LocalSettingsVerdict) {
	t.Helper()
	previous := ClassifyLocalSettings
	ClassifyLocalSettings = func(context.Context, string) LocalSettingsVerdict { return verdict }
	t.Cleanup(func() { ClassifyLocalSettings = previous })
}

// The local file's "enabled" is honored whenever it is not proven repository
// content, but only a VERIFIED own file may lift the user's exclusions: an
// unverifiable one (repository unreadable) is not evidence of the developer's
// action, and treating it as such would let a committed "local" file bypass
// the exclude lists exactly when the check that catches it fails.
// Not parallel: swaps the package-level probe seam.
func TestReadRepoActivation_LocalVerdictGatesOverride(t *testing.T) {
	tests := []struct {
		name         string
		verdict      LocalSettingsVerdict
		wantEnabled  bool
		wantOverride bool
	}{
		{name: "own file overrides", verdict: LocalSettingsOwn, wantEnabled: true, wantOverride: true},
		{name: "unverifiable file enables but does not override", verdict: LocalSettingsUnverifiable, wantEnabled: true, wantOverride: false},
		{name: "tracked file is ignored", verdict: LocalSettingsTracked, wantEnabled: false, wantOverride: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writePolicyFile(t, root, ".entire/settings.local.json", `{"enabled": true}`)
			stubLocalSettingsVerdict(t, tc.verdict)
			activation, err := ReadRepoActivation(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			if activation.Enabled != tc.wantEnabled || activation.LocalOverride != tc.wantOverride {
				t.Fatalf("activation = %+v, want enabled=%v override=%v", activation, tc.wantEnabled, tc.wantOverride)
			}
		})
	}
}

// End to end through the classifier: an excluded repo whose local enable
// cannot be verified stays excluded.
// Not parallel: t.Chdir and the probe seam.
func TestClassify_UnverifiableLocalEnableDoesNotBypassExclusion(t *testing.T) {
	root, _ := newPolicyRepo(t)
	writePolicyFile(t, root, ".entire/settings.local.json", `{"enabled": true}`)
	setPolicyGlobal(t, `{"global":{"enabled":true,"exclude_paths":["`+slashedResolved(t, root)+`"]}}`)

	stubLocalSettingsVerdict(t, LocalSettingsOwn)
	if policy := policyAt(t, root); !policy.Active || policy.ActivationSource != ActivationLocal {
		t.Fatalf("verified own local file must activate an excluded repo: %+v", policy)
	}
	stubLocalSettingsVerdict(t, LocalSettingsUnverifiable)
	if policy := policyAt(t, root); policy.Active || policy.InactiveReason != InactiveReasonGlobalExcluded {
		t.Fatalf("unverifiable local file must not lift the exclusion: %+v", policy)
	}
}

func slashedResolved(t *testing.T, root string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(resolved)
}
