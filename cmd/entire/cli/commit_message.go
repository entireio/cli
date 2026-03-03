package cli

import (
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

// generateCommitMessage creates a commit message from the user's original prompt.
// If the prompt is empty or cleans to empty, falls back to "<agentType> session updates".
func generateCommitMessage(originalPrompt string, agentType types.AgentType) string {
	if originalPrompt != "" {
		cleaned := cleanPromptForCommit(originalPrompt)
		if cleaned != "" {
			return cleaned
		}
	}

	if agentType == "" {
		agentType = agent.AgentTypeUnknown
	}
	return fmt.Sprintf("%s session updates", agentType)
}

// cleanPromptForCommit cleans up a user prompt to make it suitable as a commit message
// Uses a loop to remove all matching prefixes until none remain
func cleanPromptForCommit(prompt string) string {
	cleaned := prompt

	prefixes := []string{
		"Can you ",
		"can you ",
		"Please ",
		"please ",
		"Let's ",
		"let's ",
		"Could you ",
		"could you ",
		"Would you ",
		"would you ",
		"I want you to ",
		"I'd like you to ",
		"I need you to ",
	}

	// Loop until no prefix is found
	for {
		found := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(cleaned, prefix) {
				cleaned = strings.TrimPrefix(cleaned, prefix)
				found = true
				break
			}
		}
		if !found {
			break
		}
	}

	cleaned = strings.TrimSuffix(cleaned, "?")
	cleaned = strings.TrimSpace(cleaned)

	// Truncate to 72 characters (rune-safe for multi-byte UTF-8)
	cleaned = stringutil.TruncateRunes(cleaned, 72, "")
	cleaned = strings.TrimSpace(cleaned)

	// Capitalize first letter (rune-safe for multi-byte UTF-8)
	cleaned = stringutil.CapitalizeFirst(cleaned)

	return cleaned
}
