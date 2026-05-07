// Package review — see env.go for package-level rationale.
//
// synthesis_prompt.go builds the LLM prompt that asks a configured summary
// provider to synthesize a unified verdict across N per-agent review reports.
// It is a pure-function composer with no I/O; all logic is testable without
// network calls or TTY state.
package review

import (
	"fmt"
	"strings"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// composeSynthesisPrompt builds the LLM prompt asking the provider to
// synthesize a unified verdict across N agent reviews. Format:
//
//	You reviewed the same code change with N agents. Here are their reports:
//
//	─── claude-code ───
//	<narrative from agent's AssistantText events, joined>
//
//	─── codex ───
//	<narrative>
//
//	...
//
//	Synthesize a unified verdict with these sections:
//	  - Common findings (issues all agents flagged)
//	  - Unique findings (issues only one agent caught)
//	  - Disagreements (areas where agents reached different conclusions)
//	  - Priority order (top 5 issues to address first)
//
//	Be concise; aim for ~300 words.
//
//	<perRunPrompt, if any — appended as user's per-run instructions>
//
// Agents with no usable narrative (empty AssistantText) are filtered out
// upstream by usableAgentRuns, so the header count and the body are both
// scoped to agents that produced narrative output. SynthesisSink already
// guards on len(usable) >= 2 before calling, so the empty case won't reach
// the LLM in production.
func composeSynthesisPrompt(summary reviewtypes.RunSummary, perRunPrompt string) string {
	usable := usableAgentRuns(summary)
	if len(usable) == 0 {
		return ""
	}

	var b strings.Builder

	fmt.Fprintf(&b, "You reviewed the same code change with %d agents. Here are their reports:\n", len(usable))

	for _, run := range usable {
		narrative := joinAssistantText(run.Buffer)
		if narrative == "" {
			continue
		}
		fmt.Fprintf(&b, "\n─── %s ───\n", run.Name)
		b.WriteString(narrative)
		b.WriteString("\n")
	}

	b.WriteString(`
Synthesize a unified verdict with these sections:
  - Common findings (issues all agents flagged)
  - Unique findings (issues only one agent caught)
  - Disagreements (areas where agents reached different conclusions)
  - Priority order (top 5 issues to address first)

Be concise; aim for ~300 words.`)

	if perRunPrompt != "" {
		b.WriteString("\n\n")
		b.WriteString(perRunPrompt)
	}

	return b.String()
}

// usableAgentRuns returns agent runs that have non-empty AssistantText
// narrative in their event buffer, in the original order from the summary.
// The filter is on narrative content alone — Status is not checked. In
// practice this drops most cancelled and errored runs (they typically don't
// produce assistant output before exiting), but a cancelled agent that
// emitted text mid-stream is still considered usable. The synthesis prompt
// uses what the agent actually said, regardless of how the run terminated.
func usableAgentRuns(summary reviewtypes.RunSummary) []reviewtypes.AgentRun {
	var result []reviewtypes.AgentRun
	for _, run := range summary.AgentRuns {
		if joinAssistantText(run.Buffer) == "" {
			continue
		}
		result = append(result, run)
	}
	return result
}
