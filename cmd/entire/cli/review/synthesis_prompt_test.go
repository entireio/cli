package review_test

import (
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

func makeSummaryWithNarratives(agents []struct {
	name      string
	narrative string
	status    reviewtypes.AgentStatus
}) reviewtypes.RunSummary {
	runs := make([]reviewtypes.AgentRun, len(agents))
	for i, a := range agents {
		var buf []reviewtypes.Event
		if a.narrative != "" {
			buf = append(buf, reviewtypes.AssistantText{Text: a.narrative})
		}
		runs[i] = reviewtypes.AgentRun{
			Name:   a.name,
			Status: a.status,
			Buffer: buf,
		}
	}
	return reviewtypes.RunSummary{
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		AgentRuns:  runs,
	}
}

// TestComposeSynthesisPrompt_IncludesAgentNarratives verifies each agent's
// narrative appears under its name header.
func TestComposeSynthesisPrompt_IncludesAgentNarratives(t *testing.T) {
	t.Parallel()
	summary := makeSummaryWithNarratives([]struct {
		name      string
		narrative string
		status    reviewtypes.AgentStatus
	}{
		{"claude-code", "Missing input validation.", reviewtypes.AgentStatusSucceeded},
		{"codex", "SQL injection risk in query builder.", reviewtypes.AgentStatusSucceeded},
	})

	prompt := review.ExposedComposeSynthesisPrompt(summary, "")

	for _, want := range []string{
		"claude-code",
		"Missing input validation.",
		"codex",
		"SQL injection risk in query builder.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\nfull prompt:\n%s", want, prompt)
		}
	}
}

// TestComposeSynthesisPrompt_ExcludesEmptyNarrativeAgents verifies agents with
// no AssistantText in their buffer are excluded from the prompt body.
func TestComposeSynthesisPrompt_ExcludesEmptyNarrativeAgents(t *testing.T) {
	t.Parallel()
	summary := makeSummaryWithNarratives([]struct {
		name      string
		narrative string
		status    reviewtypes.AgentStatus
	}{
		{"claude-code", "Good code.", reviewtypes.AgentStatusSucceeded},
		{"codex", "", reviewtypes.AgentStatusFailed}, // no narrative
	})

	prompt := review.ExposedComposeSynthesisPrompt(summary, "")

	if strings.Contains(prompt, "codex") {
		t.Errorf("prompt should not include agent with no narrative, but found 'codex'\nfull prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "claude-code") {
		t.Errorf("prompt should include claude-code\nfull prompt:\n%s", prompt)
	}
}

// TestComposeSynthesisPrompt_PerRunPromptAppended verifies the per-run prompt
// is appended at the end when non-empty.
func TestComposeSynthesisPrompt_PerRunPromptAppended(t *testing.T) {
	t.Parallel()
	summary := makeSummaryWithNarratives([]struct {
		name      string
		narrative string
		status    reviewtypes.AgentStatus
	}{
		{"claude-code", "Some findings.", reviewtypes.AgentStatusSucceeded},
		{"codex", "Other findings.", reviewtypes.AgentStatusSucceeded},
	})

	perRun := "Focus on security issues only."
	prompt := review.ExposedComposeSynthesisPrompt(summary, perRun)

	if !strings.Contains(prompt, perRun) {
		t.Errorf("prompt missing per-run instructions %q\nfull prompt:\n%s", perRun, prompt)
	}
	// Per-run prompt should appear after the verdict template.
	verdictIdx := strings.Index(prompt, "Priority order")
	perRunIdx := strings.Index(prompt, perRun)
	if verdictIdx < 0 || perRunIdx < 0 || perRunIdx < verdictIdx {
		t.Errorf("per-run prompt should appear after verdict template\nfull prompt:\n%s", prompt)
	}
}

// TestComposeSynthesisPrompt_NoPerRunPrompt verifies no extra newline when
// perRunPrompt is empty.
func TestComposeSynthesisPrompt_NoPerRunPrompt(t *testing.T) {
	t.Parallel()
	summary := makeSummaryWithNarratives([]struct {
		name      string
		narrative string
		status    reviewtypes.AgentStatus
	}{
		{"claude-code", "Findings.", reviewtypes.AgentStatusSucceeded},
		{"codex", "More findings.", reviewtypes.AgentStatusSucceeded},
	})

	prompt := review.ExposedComposeSynthesisPrompt(summary, "")
	// Should not end with more than one blank line after template.
	if strings.HasSuffix(prompt, "\n\n\n") {
		t.Errorf("prompt has trailing extra newlines\nfull prompt:\n%s", prompt)
	}
}

// TestComposeSynthesisPrompt_Deterministic verifies same input produces same output.
func TestComposeSynthesisPrompt_Deterministic(t *testing.T) {
	t.Parallel()
	summary := makeSummaryWithNarratives([]struct {
		name      string
		narrative string
		status    reviewtypes.AgentStatus
	}{
		{"agent-a", "Finding A.", reviewtypes.AgentStatusSucceeded},
		{"agent-b", "Finding B.", reviewtypes.AgentStatusSucceeded},
	})

	p1 := review.ExposedComposeSynthesisPrompt(summary, "extra context")
	p2 := review.ExposedComposeSynthesisPrompt(summary, "extra context")

	if p1 != p2 {
		t.Errorf("composeSynthesisPrompt is not deterministic:\np1=%s\np2=%s", p1, p2)
	}
}

// TestComposeSynthesisPrompt_SectionsPresent verifies all four required
// verdict sections appear in the prompt template.
func TestComposeSynthesisPrompt_SectionsPresent(t *testing.T) {
	t.Parallel()
	summary := makeSummaryWithNarratives([]struct {
		name      string
		narrative string
		status    reviewtypes.AgentStatus
	}{
		{"claude-code", "Analysis.", reviewtypes.AgentStatusSucceeded},
		{"codex", "More analysis.", reviewtypes.AgentStatusSucceeded},
	})

	prompt := review.ExposedComposeSynthesisPrompt(summary, "")

	for _, section := range []string{
		"Common findings",
		"Unique findings",
		"Disagreements",
		"Priority order",
	} {
		if !strings.Contains(prompt, section) {
			t.Errorf("prompt missing required section %q\nfull prompt:\n%s", section, prompt)
		}
	}
}

// TestComposeSynthesisPrompt_AgentCountInHeader verifies the agent count
// in the header reflects only agents with usable narratives.
func TestComposeSynthesisPrompt_AgentCountInHeader(t *testing.T) {
	t.Parallel()
	summary := makeSummaryWithNarratives([]struct {
		name      string
		narrative string
		status    reviewtypes.AgentStatus
	}{
		{"agent-a", "Finding A.", reviewtypes.AgentStatusSucceeded},
		{"agent-b", "Finding B.", reviewtypes.AgentStatusSucceeded},
		{"agent-c", "", reviewtypes.AgentStatusFailed}, // no narrative, excluded
	})

	prompt := review.ExposedComposeSynthesisPrompt(summary, "")

	if !strings.Contains(prompt, "2 agents") {
		t.Errorf("header should say '2 agents' (agent-c excluded), got:\n%s", prompt)
	}
}
