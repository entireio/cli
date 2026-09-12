package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestGlobalTrustBannerSuffix: the session-start banner names `entire trust`
// exactly when this repo's checkpoint sync is held — a repo-enabled repo
// included once the tier is on — and stays empty otherwise.
func TestGlobalTrustBannerSuffix(t *testing.T) {
	repoEnabledWithOrigin(t)
	ctx := context.Background()

	writeUserSettings(t, `{"global":{"enabled":true}}`)
	claude := globalTrustBannerSuffix(ctx, agent.AgentNameClaudeCode)
	if !strings.HasPrefix(claude, "\n  ") || !strings.Contains(claude, "run `entire trust`") {
		t.Fatalf("claude suffix = %q", claude)
	}
	codex := globalTrustBannerSuffix(ctx, agent.AgentNameCodex)
	if !strings.HasPrefix(codex, " Entire") || strings.Contains(codex, "\n") {
		t.Fatalf("codex suffix must be single-line with a space prefix, got %q", codex)
	}

	writeUserSettings(t, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
	if got := globalTrustBannerSuffix(ctx, agent.AgentNameClaudeCode); got != "" {
		t.Fatalf("trusted repo must not get the banner, got %q", got)
	}

	writeUserSettings(t, `{"global":{"enabled":false}}`)
	if got := globalTrustBannerSuffix(ctx, agent.AgentNameClaudeCode); got != "" {
		t.Fatalf("tier off must not get the banner, got %q", got)
	}
}

// TestGlobalTrustBannerSuffix_TrustAll: a globally tracked repo that syncs
// only because trust_all is on is announced — captured AND synced in silence
// is the one state the user never chose for this repo — while per-repo
// consent and repo-enabled repos stay quiet.
func TestGlobalTrustBannerSuffix_TrustAll(t *testing.T) {
	isolatedUserHome(t)
	pretendAgentBinaries(t)
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/widgets.git")
	ctx := context.Background()

	writeUserSettings(t, `{"global":{"enabled":true,"trust_all":true}}`)
	got := globalTrustBannerSuffix(ctx, agent.AgentNameClaudeCode)
	if !strings.Contains(got, "trust_all") || !strings.Contains(got, "exclude_paths") {
		t.Fatalf("globally tracked repo under trust_all must be announced, got %q", got)
	}
	if codex := globalTrustBannerSuffix(ctx, agent.AgentNameCodex); !strings.HasPrefix(codex, " Entire") || strings.Contains(codex, "\n") {
		t.Fatalf("codex suffix must be single-line, got %q", codex)
	}

	writeUserSettings(t, `{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`)
	if got := globalTrustBannerSuffix(ctx, agent.AgentNameClaudeCode); got != "" {
		t.Fatalf("per-repo consent is the user's own choice and stays silent, got %q", got)
	}

	// A repo the user enabled themselves is not a surprise, trust_all or not.
	writeSettings(t, `{"enabled": true}`)
	writeUserSettings(t, `{"global":{"enabled":true,"trust_all":true}}`)
	if got := globalTrustBannerSuffix(ctx, agent.AgentNameClaudeCode); got != "" {
		t.Fatalf("repo-enabled repo must not get the trust_all banner, got %q", got)
	}
}
