package providers_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/app/models"
	"github.com/entireio/cli/app/privacy"
	"github.com/entireio/cli/app/providers"
)

// mockRedactedCPProvider provides a test fixture with redacted context
type mockRedactedCPProvider struct {
	providers.DevCheckpointProvider
}

func (m *mockRedactedCPProvider) GetCheckpoints(ctx context.Context, repoID string) ([]models.Checkpoint, error) {
	return []models.Checkpoint{
		{
			CheckpointID:  "cp-redacted-001",
			CommitRef:     "78f4dc59700e",
			Timestamp:     time.Now(),
			IntentContext: "Implement OAuth flow with [REDACTED] secret token",
			FilesChanged:  []string{"app/auth.go"},
			VerificationInfo: "Partially redacted session context",
		},
	}, nil
}

func TestGenerateIntelligence_CompleteContext(t *testing.T) {
	cpProv := providers.NewDevCheckpointProvider()
	commitProv := providers.NewDevCommitProvider(cpProv)
	devAna := providers.NewDevAnalyzer()
	graphProv := providers.NewDevGraphProvider()
	san := privacy.NewPrivacySanitizer()

	engine := providers.NewLiveIntelligenceEngine(commitProv, cpProv, devAna, graphProv, san)

	intel, err := engine.GenerateIntelligence(context.Background(), "repo-cli-btw", "3dbdf8b83c39")
	if err != nil {
		t.Fatalf("GenerateIntelligence failed: %v", err)
	}

	if intel.ContextCompleteness != models.ContextComplete {
		t.Errorf("expected ContextComplete, got %s", intel.ContextCompleteness)
	}

	if intel.VerificationStatus != models.VerificationCompleted {
		t.Errorf("expected VerificationCompleted, got %s", intel.VerificationStatus)
	}

	if !intel.Evidence.Checkpoint.Available || !intel.Evidence.Commit.Available {
		t.Errorf("expected Checkpoint and Commit evidence to be available")
	}
}

func TestGenerateIntelligence_MissingContext(t *testing.T) {
	cpProv := providers.NewDevCheckpointProvider()
	commitProv := providers.NewDevCommitProvider(cpProv)
	devAna := providers.NewDevAnalyzer()
	graphProv := providers.NewDevGraphProvider()
	san := privacy.NewPrivacySanitizer()

	engine := providers.NewLiveIntelligenceEngine(commitProv, cpProv, devAna, graphProv, san)

	// Commit 'a1b2c3d4e5f6' has no associated Checkpoint
	intel, err := engine.GenerateIntelligence(context.Background(), "repo-cli-btw", "a1b2c3d4e5f6")
	if err != nil {
		t.Fatalf("GenerateIntelligence failed: %v", err)
	}

	if intel.ContextCompleteness != models.ContextUnavailable {
		t.Errorf("expected ContextUnavailable for Git-only commit, got %s", intel.ContextCompleteness)
	}

	if intel.VerificationStatus != models.VerificationNeedsVerification {
		t.Errorf("expected VerificationNeedsVerification, got %s", intel.VerificationStatus)
	}

	if intel.Intent != "CHECKPOINT CONTEXT UNAVAILABLE" {
		t.Errorf("expected explicit UNAVAILABLE intent message, got %s", intel.Intent)
	}

	if intel.Evidence.Checkpoint.Available {
		t.Errorf("expected Checkpoint evidence to be false for Git-only commit")
	}
}

func TestGenerateIntelligence_RedactedContext(t *testing.T) {
	redactedCPProv := &mockRedactedCPProvider{}
	commitProv := providers.NewDevCommitProvider(redactedCPProv)
	devAna := providers.NewDevAnalyzer()
	graphProv := providers.NewDevGraphProvider()
	san := privacy.NewPrivacySanitizer()

	engine := providers.NewLiveIntelligenceEngine(commitProv, redactedCPProv, devAna, graphProv, san)

	intel, err := engine.GenerateIntelligence(context.Background(), "repo-cli-btw", "78f4dc59700e")
	if err != nil {
		t.Fatalf("GenerateIntelligence failed: %v", err)
	}

	if intel.ContextCompleteness != models.ContextRedacted {
		t.Errorf("expected ContextRedacted, got %s", intel.ContextCompleteness)
	}

	if intel.VerificationStatus == models.VerificationCompleted {
		t.Errorf("redacted context MUST NOT be marked as COMPLETED authoritative")
	}

	if !strings.Contains(intel.Intent, "[REDACTED]") {
		t.Errorf("expected redacted token in intent text")
	}
}
