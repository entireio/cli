package compact

import "testing"

func TestCompact_CopilotFixture(t *testing.T) {
	t.Parallel()

	assertFixtureTransform(t, agentOpts("copilot-cli"), "testdata/copilot_full.jsonl", "testdata/copilot_expected.jsonl")
}
