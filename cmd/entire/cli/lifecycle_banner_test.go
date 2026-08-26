package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
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
