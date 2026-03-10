//go:build e2e

package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
)

// TestLineAttributionReasonable: agent creates a file, attribution metadata
// should reflect that agent wrote most/all of the content.
// GH #344: attribution metadata is significantly off for simple agent-created content.
func TestLineAttributionReasonable(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		_, err := s.RunPrompt(t, ctx,
			"create a single markdown file at docs/example.md with a few paragraphs about software testing. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "docs/example.md")

		s.Git(t, "add", "docs/")
		s.Git(t, "commit", "-m", "Add example.md")

		testutil.WaitForCheckpoint(t, s, 15*time.Second)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		sm := testutil.ReadSessionMetadata(t, s.Dir, cpID, 0)

		assert.Greater(t, sm.InitialAttribution.AgentLines, 0,
			"agent lines should be > 0")
		assert.Greater(t, sm.InitialAttribution.TotalCommitted, 0,
			"total committed should be > 0")
		assert.Greater(t, sm.InitialAttribution.AgentPercentage, 50.0,
			"agent created 100%% of content, percentage should be > 50%%")
		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}

// TestInteractiveAttributionOnAgentCommit: agent creates a file and commits it in the
// same prompt (interactive session). The first commit's checkpoint should have
// initial_attribution populated.
func TestInteractiveAttributionOnAgentCommit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		prompt := s.Agent.PromptPattern()

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skipf("agent %s does not support interactive mode", s.Agent.Name())
		}

		s.WaitFor(t, session, prompt, 30*time.Second)

		s.Send(t, session, "create a file called hello.txt containing 'hello world', then commit it. Do not ask for confirmation.")
		s.WaitFor(t, session, prompt, 90*time.Second)
		testutil.AssertNewCommits(t, s, 1)

		testutil.WaitForCheckpoint(t, s, 15*time.Second)
		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		sm := testutil.ReadSessionMetadata(t, s.Dir, cpID, 0)

		assert.Greater(t, sm.InitialAttribution.AgentLines, 0,
			"agent lines should be > 0 on first agent commit")
		assert.Greater(t, sm.InitialAttribution.TotalCommitted, 0,
			"total committed should be > 0 on first agent commit")
		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}

// TestInteractiveAttributionMultiCommitSameSession: two prompts in the same interactive
// session, agent modifies the same file and commits both times. The second
// checkpoint's initial_attribution should have non-zero values.
func TestInteractiveAttributionMultiCommitSameSession(t *testing.T) {
	testutil.ForEachAgent(t, 4*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		prompt := s.Agent.PromptPattern()

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skipf("agent %s does not support interactive mode", s.Agent.Name())
		}

		s.WaitFor(t, session, prompt, 30*time.Second)

		// First prompt: create file and commit.
		s.Send(t, session, "create a file called poem.txt with a short poem about coding, then commit it. Do not ask for confirmation.")
		s.WaitFor(t, session, prompt, 60*time.Second)
		testutil.AssertNewCommits(t, s, 1)

		testutil.WaitForCheckpoint(t, s, 15*time.Second)
		cpBranch1 := testutil.GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")

		// Second prompt: modify same file and commit again.
		s.Send(t, session, "add another stanza to poem.txt about debugging, then create a NEW commit (do not amend). Do not ask for confirmation.")
		s.WaitFor(t, session, prompt, 90*time.Second)
		testutil.AssertNewCommits(t, s, 2)

		testutil.WaitForCheckpointAdvanceFrom(t, s.Dir, cpBranch1, 15*time.Second)
		cpID2 := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		sm := testutil.WaitForSessionMetadata(t, s.Dir, cpID2, 0, 10*time.Second)

		assert.Greater(t, sm.InitialAttribution.AgentLines, 0,
			"agent lines should be > 0 on second commit")
		assert.Greater(t, sm.InitialAttribution.TotalCommitted, 0,
			"total committed should be > 0 on second commit")
		assert.Greater(t, sm.InitialAttribution.AgentPercentage, 50.0,
			"agent wrote all content, percentage should be > 50%%")
		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}

// TestInteractiveShadowBranchCleanedAfterAgentCommit: after an agent creates a file
// and commits it, there should be no lingering shadow branches. Shadow
// branches match entire/* but are not entire/checkpoints/*.
func TestInteractiveShadowBranchCleanedAfterAgentCommit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		prompt := s.Agent.PromptPattern()

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skipf("agent %s does not support interactive mode", s.Agent.Name())
		}

		s.WaitFor(t, session, prompt, 30*time.Second)

		s.Send(t, session, "create a file called hello.txt containing 'hello world', then commit it. Do not ask for confirmation.")
		s.WaitFor(t, session, prompt, 90*time.Second)
		testutil.AssertNewCommits(t, s, 1)

		testutil.WaitForCheckpoint(t, s, 15*time.Second)

		testutil.AssertNoShadowBranches(t, s.Dir)
	})
}

// TestAttributionMixedHumanAndAgent: agent creates one file, human creates
// another with known content, both are committed together. Attribution should
// correctly separate agent lines from human lines.
func TestAttributionMixedHumanAndAgent(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		// Agent creates a file.
		_, err := s.RunPrompt(t, ctx,
			"create a file called agent.txt with a few lines about software testing. Do not create any other files. Do not ask for confirmation, just make the change.")
		if err != nil {
			t.Fatalf("agent failed: %v", err)
		}
		testutil.AssertFileExists(t, s.Dir, "agent.txt")

		// Count agent lines before human changes.
		agentContent, err := os.ReadFile(filepath.Join(s.Dir, "agent.txt"))
		if err != nil {
			t.Fatalf("read agent.txt: %v", err)
		}
		agentLines := len(strings.Split(strings.TrimRight(string(agentContent), "\n"), "\n"))

		// Human creates a known file.
		humanContent := "line one\nline two\nline three\n"
		humanLines := 3
		if err := os.WriteFile(filepath.Join(s.Dir, "human.txt"), []byte(humanContent), 0o644); err != nil {
			t.Fatalf("write human.txt: %v", err)
		}

		// Commit both (stage only intended files — attribution checks exact line counts).
		s.Git(t, "add", "agent.txt", "human.txt")
		s.Git(t, "commit", "-m", "Add agent and human files")

		testutil.WaitForCheckpoint(t, s, 15*time.Second)

		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		sm := testutil.ReadSessionMetadata(t, s.Dir, cpID, 0)

		assert.Equal(t, agentLines, sm.InitialAttribution.AgentLines,
			"agent_lines should match actual lines in agent.txt")
		assert.Equal(t, humanLines, sm.InitialAttribution.HumanAdded,
			"human_added should match lines in human.txt")
		assert.Equal(t, agentLines+humanLines, sm.InitialAttribution.TotalCommitted,
			"total_committed should be sum of agent and human lines")
		assert.Greater(t, sm.InitialAttribution.AgentPercentage, 0.0,
			"agent_percentage should be > 0 when agent wrote content")
		assert.Less(t, sm.InitialAttribution.AgentPercentage, 100.0,
			"agent_percentage should be < 100 when human also wrote content")
		// Shadow branch may persist if agent created extra files beyond
		// agent.txt — we only stage the two known files for precise
		// attribution counting.
	})
}
