package main

import "testing"

// TestNextSubagentIDsAreUniquePerTask guards the per-task uniqueness the framework
// depends on. A vogon process handles every prompt of a session, so two delegated
// prompts must not share IDs: pre-task state is keyed on the tool-use ID
// (pre-task-<id>.json) and the subagent transcript on the agent ID, so a collision
// silently overwrites the first task's state and transcript.
//
// Deriving them from prompt length or action count did collide — the common case is
// one file per prompt, so every delegation produced the same agent ID.
func TestNextSubagentIDsAreUniquePerTask(t *testing.T) {
	subagentSeq = 0
	t.Cleanup(func() { subagentSeq = 0 })

	seenTool := map[string]bool{}
	seenAgent := map[string]bool{}
	for i := range 5 {
		toolUseID, agentID := nextSubagentIDs()
		if toolUseID == "" || agentID == "" {
			t.Fatalf("call %d returned an empty ID (%q, %q)", i, toolUseID, agentID)
		}
		if seenTool[toolUseID] {
			t.Errorf("duplicate tool-use ID %q on call %d", toolUseID, i)
		}
		if seenAgent[agentID] {
			t.Errorf("duplicate agent ID %q on call %d", agentID, i)
		}
		seenTool[toolUseID] = true
		seenAgent[agentID] = true
	}
}

// TestDelegatesToSubagent covers the phrasings the e2e prompts use, and that an
// ordinary prompt is not mistaken for a delegation.
func TestDelegatesToSubagent(t *testing.T) {
	t.Parallel()

	delegating := []string{
		"use a subagent: create docs/red.md",
		"Using a sub-agent, create a file",
		"delegate to a subagent and commit",
		"do it with a subagent",
	}
	for _, p := range delegating {
		if !delegatesToSubagent(p) {
			t.Errorf("delegatesToSubagent(%q) = false, want true", p)
		}
	}

	direct := []string{
		"create a markdown file at docs/red.md",
		"commit the changes",
		"modify two existing files",
	}
	for _, p := range direct {
		if delegatesToSubagent(p) {
			t.Errorf("delegatesToSubagent(%q) = true, want false", p)
		}
	}
}
