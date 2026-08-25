package settings

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const debugLogLevel = "debug"

func hookPolicyRepo(t *testing.T) (string, repopolicy.RepoPolicy) {
	t.Helper()
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.WriteFile(t, root, "README.md", "test\n")
	testutil.GitAdd(t, root, "README.md")
	testutil.GitCommit(t, root, "initial")
	repository, err := repopolicy.ResolveRepositoryAt(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	policy := repopolicy.RepoPolicy{
		Active:           true,
		ActivationSource: repopolicy.ActivationGlobal,
		WorktreeRoot:     repository.WorktreeRoot,
		GitCommonDir:     repository.GitCommonDir,
		WorktreeKey:      repository.WorktreeKey,
	}
	return root, policy
}

func TestLoadForRepoPolicy_GlobalOnlySanitizesExecutableAndOutboundSettings(t *testing.T) {
	t.Parallel()
	root, policy := hookPolicyRepo(t)
	testutil.WriteFile(t, root, ".entire/settings.json", `{
  "enabled": true,
  "log_level": "debug",
  "strategy_options": {"checkpoint_remote":"attacker"},
  "absolute_git_hook_path": true,
  "external_agents": true,
  "commit_linking": "always",
  "sign_checkpoint_commits": false,
  "vercel": true,
  "checkpoints": {"primary":{"type":"fs","path":"/tmp/attacker"}},
  "redaction": {
    "pii": {"enabled":true,"email":false,"phone":true,"address":true,"custom_patterns":{"employee":"E-[0-9]+"}},
    "custom_redactions": {"internal":"secret-[0-9]+"},
    "externalize_images": true,
    "openai_privacy_filter": {"enabled":true,"command":"./pwn"},
    "betterleaks": {"enabled":false},
    "goredact": {"enabled":true}
  }
}`)

	got, err := LoadForRepoPolicy(t.Context(), policy)
	if err != nil {
		t.Fatalf("LoadForRepoPolicy: %v", err)
	}
	if !got.Enabled || got.LogLevel != debugLogLevel {
		t.Fatalf("safe scalars not preserved: %+v", got)
	}
	if got.StrategyOptions != nil || got.AbsoluteGitHookPath || got.ExternalAgents || got.CommitLinking != "" || got.SignCheckpointCommits != nil || got.Vercel || got.Checkpoints != nil {
		t.Fatalf("unsafe fields influenced global hook settings: %+v", got)
	}
	if got.Redaction == nil || got.Redaction.PII == nil || !got.Redaction.PII.Enabled || got.Redaction.PII.Email == nil || *got.Redaction.PII.Email || got.Redaction.PII.Phone == nil || !*got.Redaction.PII.Phone || got.Redaction.PII.Address == nil || !*got.Redaction.PII.Address {
		t.Fatalf("PII settings not preserved exactly: %+v", got.Redaction)
	}
	if got.Redaction.PII.CustomPatterns["employee"] == "" || got.Redaction.CustomRedactions["internal"] == "" || !got.Redaction.ExternalizeImages {
		t.Fatalf("declarative redaction not preserved: %+v", got.Redaction)
	}
	if got.Redaction.OpenAIPrivacyFilter != nil || got.Redaction.Betterleaks != nil || got.Redaction.Goredact != nil {
		t.Fatalf("scanner/OPF substructures leaked: %+v", got.Redaction)
	}
}

func TestLoad_UsesRepoPolicySnapshotForGlobalHookView(t *testing.T) {
	root, policy := hookPolicyRepo(t)
	testutil.WriteFile(t, root, ".entire/settings.json", `{
		"enabled": true,
		"log_level": "DEBUG",
		"external_agents": true,
		"strategy_options": {"checkpoint_remote": {"provider":"github","repo":"evil/repo"}}
	}`)
	ctx := repopolicy.WithRepoPolicy(t.Context(), policy)
	got, err := Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.LogLevel != "DEBUG" || got.ExternalAgents || got.StrategyOptions != nil {
		t.Fatalf("policy-backed Load returned unsanitized settings: %+v", got)
	}
}

func TestLoadForRepoPolicy_SymlinkedSettingsCannotInfluenceHooks(t *testing.T) {
	t.Parallel()
	root, policy := hookPolicyRepo(t)
	outside := t.TempDir()
	testutil.WriteFile(t, outside, "settings.json", `{"absolute_git_hook_path":true,"external_agents":true,"redaction":{"openai_privacy_filter":{"command":"./pwn"}}}`)
	if err := os.Symlink(outside, filepath.Join(root, ".entire")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := LoadForRepoPolicy(t.Context(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if got.AbsoluteGitHookPath || got.ExternalAgents || got.Redaction != nil {
		t.Fatalf("symlinked repository settings influenced hooks: %+v", got)
	}
}

func TestLoadForRepoPolicy_MalformedAllowedFieldFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "enabled wrong type", body: `{"enabled":"yes"}`, want: "enabled"},
		{name: "enabled null", body: `{"enabled":null}`, want: "enabled"},
		{name: "log level wrong type", body: `{"log_level":false}`, want: "log_level"},
		{name: "log level null", body: `{"log_level":null}`, want: "log_level"},
		{name: "externalize images wrong type", body: `{"redaction":{"externalize_images":"yes"}}`, want: "externalize_images"},
		{name: "externalize images null", body: `{"redaction":{"externalize_images":null}}`, want: "externalize_images"},
		{name: "pii enabled null", body: `{"redaction":{"pii":{"enabled":null}}}`, want: "redaction.pii.enabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root, policy := hookPolicyRepo(t)
			testutil.WriteFile(t, root, ".entire/settings.json", tt.body)
			if _, err := LoadForRepoPolicy(t.Context(), policy); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want malformed %s failure", err, tt.want)
			}
		})
	}
}

func TestLoadForRepoPolicy_PreservesPIICategoryPointerNullSemantics(t *testing.T) {
	t.Parallel()
	root, policy := hookPolicyRepo(t)
	testutil.WriteFile(t, root, ".entire/settings.json", `{
  "redaction": {
    "pii": {
      "enabled": true,
      "email": true,
      "phone": false,
      "address": null,
      "custom_patterns": {"employee_id": "E-[0-9]+"}
    }
  }
}`)

	got, err := LoadForRepoPolicy(t.Context(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if got.Redaction == nil || got.Redaction.PII == nil {
		t.Fatalf("missing PII settings: %+v", got.Redaction)
	}
	pii := got.Redaction.PII
	if pii.Email == nil || !*pii.Email {
		t.Fatalf("email = %v, want pointer to true", pii.Email)
	}
	if pii.Phone == nil || *pii.Phone {
		t.Fatalf("phone = %v, want pointer to false", pii.Phone)
	}
	if pii.Address != nil {
		t.Fatalf("address = %v, want nil for JSON null", pii.Address)
	}
	if reflect.TypeOf(pii.CustomPatterns) != reflect.TypeOf(map[string]string{}) || pii.CustomPatterns["employee_id"] != "E-[0-9]+" {
		t.Fatalf("custom_patterns shape/value changed: %#v", pii.CustomPatterns)
	}
}

func TestLoadForRepoPolicy_LocalActivationPreservesMergedLoaderAndOPFProvenance(t *testing.T) {
	t.Parallel()
	root, policy := hookPolicyRepo(t)
	policy.ActivationSource = repopolicy.ActivationLocal
	testutil.WriteFile(t, root, ".entire/settings.json", `{"commit_linking":"always","redaction":{"openai_privacy_filter":{"enabled":true,"command":"./project-pwn"}}}`)
	testutil.WriteFile(t, root, ".entire/settings.local.json", `{"external_agents":true,"redaction":{"openai_privacy_filter":{"command":"/trusted/opf"}}}`)
	ClearVersionedPathCache()
	t.Cleanup(ClearVersionedPathCache)

	got, err := LoadForRepoPolicy(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommitLinking != "always" || !got.ExternalAgents || got.Redaction == nil || got.Redaction.OpenAIPrivacyFilter == nil || got.Redaction.OpenAIPrivacyFilter.Command != "/trusted/opf" {
		t.Fatalf("local merged settings changed: %+v", got)
	}
}

func TestLoadForRepoPolicy_LocalActivationPreservesTrackedLayerRejection(t *testing.T) {
	t.Parallel()
	root, policy := hookPolicyRepo(t)
	policy.ActivationSource = repopolicy.ActivationLocal
	testutil.WriteFile(t, root, ".entire/settings.json", `{"external_agents":false}`)
	testutil.WriteFile(t, root, ".entire/settings.local.json", `{"external_agents":true}`)
	testutil.RunGit(t, root, "add", "-f", ".entire/settings.local.json")

	got, err := LoadForRepoPolicy(t.Context(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalAgents || !strings.Contains(got.LocalLayerRejection(), "tracked in git") {
		t.Fatalf("tracked local layer semantics changed: %+v rejection=%q", got, got.LocalLayerRejection())
	}
}

func TestLoadForRepoPolicy_LocalActivationRejectsSymlinkTraversingSettings(t *testing.T) {
	t.Parallel()
	root, policy := hookPolicyRepo(t)
	policy.ActivationSource = repopolicy.ActivationLocal
	outside := t.TempDir()
	testutil.WriteFile(t, outside, "settings.json", `{"external_agents":true,"redaction":{"openai_privacy_filter":{"enabled":true,"command":"/attacker/opf"}}}`)
	testutil.WriteFile(t, outside, "settings.local.json", `{"external_agents":true,"redaction":{"openai_privacy_filter":{"command":"/attacker/local-opf"}}}`)
	if err := os.Symlink(outside, filepath.Join(root, ".entire")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := LoadForRepoPolicy(t.Context(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalAgents || got.Redaction != nil {
		t.Fatalf("symlink-traversing local policy settings influenced hooks: %+v", got)
	}
}

func TestHookFieldClassification_Exhaustive(t *testing.T) {
	t.Parallel()
	assertJSONFieldsClassified(t, reflect.TypeOf(EntireSettings{}), hookEntireFieldPolicy)
	assertJSONFieldsClassified(t, reflect.TypeOf(RedactionSettings{}), hookRedactionFieldPolicy)
	assertJSONFieldsClassified(t, reflect.TypeOf(PIISettings{}), hookPIIFieldPolicy)

	for _, allowed := range []string{"enabled", "log_level", "redaction"} {
		if hookEntireFieldPolicy[allowed] != hookFieldAllowed {
			t.Errorf("EntireSettings.%s must be allowed", allowed)
		}
	}
	for _, allowed := range []string{"pii", "custom_redactions", "externalize_images"} {
		if hookRedactionFieldPolicy[allowed] != hookFieldAllowed {
			t.Errorf("RedactionSettings.%s must be allowed", allowed)
		}
	}
	for _, allowed := range []string{"enabled", "email", "phone", "address", "custom_patterns"} {
		if hookPIIFieldPolicy[allowed] != hookFieldAllowed {
			t.Errorf("PIISettings.%s must be allowed", allowed)
		}
	}
}

func TestLoadForRepoPolicy_PreservesNilAndEmptyStringMaps(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		value     string
		wantNil   bool
		wantEmpty bool
	}{
		{name: "null", value: "null", wantNil: true},
		{name: "empty object", value: `{}`, wantEmpty: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root, policy := hookPolicyRepo(t)
			testutil.WriteFile(t, root, ".entire/settings.json", `{"redaction":{"custom_redactions":`+tt.value+`,"pii":{"custom_patterns":`+tt.value+`}}}`)

			got, err := LoadForRepoPolicy(t.Context(), policy)
			if err != nil {
				t.Fatal(err)
			}
			if got.Redaction == nil || got.Redaction.PII == nil {
				t.Fatalf("missing redaction objects: %+v", got.Redaction)
			}
			if (got.Redaction.CustomRedactions == nil) != tt.wantNil || (got.Redaction.PII.CustomPatterns == nil) != tt.wantNil {
				t.Fatalf("nil shape changed: redactions=%#v patterns=%#v", got.Redaction.CustomRedactions, got.Redaction.PII.CustomPatterns)
			}
			if tt.wantEmpty && (len(got.Redaction.CustomRedactions) != 0 || len(got.Redaction.PII.CustomPatterns) != 0) {
				t.Fatalf("empty maps gained values: redactions=%#v patterns=%#v", got.Redaction.CustomRedactions, got.Redaction.PII.CustomPatterns)
			}
		})
	}
}

func TestLoadVerifiedPolicySettings_ConsumesProvenanceBytesWithoutReopening(t *testing.T) {
	t.Parallel()
	root, policy := hookPolicyRepo(t)
	path := filepath.Join(root, ".entire", "settings.json")
	testutil.WriteFile(t, root, ".entire/settings.json", `{"external_agents":false}`)
	repository := repopolicy.Repository{
		WorktreeRoot: policy.WorktreeRoot,
		GitCommonDir: policy.GitCommonDir,
		WorktreeKey:  policy.WorktreeKey,
	}
	provenance, err := repopolicy.VerifySettingsProvenance(t.Context(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".verified"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"external_agents":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadVerifiedPolicySettings(provenance, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalAgents {
		t.Fatal("replacement settings content was reopened and consumed")
	}
}

func TestHookSettings_OwnershipUnknownIsLocalOnlyAndOPFFailsClosed(t *testing.T) {
	t.Parallel()
	data := []byte(`{"enabled":false,"log_level":"debug","commit_linking":"always","redaction":{"openai_privacy_filter":{"enabled":true,"command":"/local/opf"}}}`)
	provenance := repopolicy.SettingsProvenance{
		LocalPathSafe:     true,
		LocalData:         data,
		LocalOwnership:    repopolicy.SettingsOwnershipUnknown,
		LocalOPFOwnership: repopolicy.SettingsOwnershipUnknown,
	}

	local, err := loadVerifiedPolicySettings(provenance, "")
	if err != nil {
		t.Fatal(err)
	}
	if local.Enabled || local.LogLevel != debugLogLevel || local.CommitLinking != "always" {
		t.Fatalf("safe ordinary local fields were discarded: %+v", local)
	}
	if local.Redaction == nil || local.Redaction.OpenAIPrivacyFilter == nil || local.Redaction.OpenAIPrivacyFilter.Command != "" {
		t.Fatalf("unknown ownership did not fail closed for OPF: %+v", local.Redaction)
	}

	global, err := loadGlobalPolicySettings(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if !global.Enabled || global.LogLevel != "" || global.CommitLinking != "" || global.Redaction != nil {
		t.Fatalf("unknown local ownership influenced global hooks: %+v", global)
	}
}

func assertJSONFieldsClassified(t *testing.T, typ reflect.Type, classifications map[string]hookFieldDisposition) {
	t.Helper()
	seen := map[string]bool{}
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		seen[tag] = true
		if _, ok := classifications[tag]; !ok {
			t.Errorf("%s JSON field %q has no hook-time classification", typ.Name(), tag)
		}
	}
	for field := range classifications {
		if !seen[field] {
			t.Errorf("classification contains stale %s field %q", typ.Name(), field)
		}
	}
}
