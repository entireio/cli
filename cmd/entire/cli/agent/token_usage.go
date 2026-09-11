package agent

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ExtractWithSubagentInventory gives built-in agents an authoritative child
// ledger. It deliberately has no external-agent protocol equivalent: callers
// supply the inventory rather than asking an agent to infer children from text.
func ExtractWithSubagentInventory(ctx context.Context, ag Agent, transcriptData []byte, transcriptLinesAtStart int, refs []SubagentReference) (InventoryExtraction, bool) {
	extractor, ok := AsInventoryAwareExtractor(ag)
	if !ok {
		return InventoryExtraction{}, false
	}
	extraction, err := extractor.ExtractWithSubagentInventory(ctx, transcriptData, transcriptLinesAtStart, refs)
	if err != nil {
		logging.Debug(ctx, "failed inventory-aware token extraction", slog.String("error", err.Error()))
		return InventoryExtraction{}, false
	}
	return extraction, true
}

// CalculateTokenUsage calculates token usage from transcript data.
// Returns nil if the agent doesn't support token calculation or on error.
// Errors are debug-logged because callers treat nil token usage as "no data available".
func CalculateTokenUsage(ctx context.Context, ag Agent, transcriptData []byte, transcriptLinesAtStart int, subagentsDir string) *TokenUsage {
	if ag == nil {
		return nil
	}

	// Calculate token usage - prefer SubagentAwareExtractor to include subagent tokens
	if subagentExtractor, ok := AsSubagentAwareExtractor(ag); ok {
		usage, err := subagentExtractor.CalculateTotalTokenUsage(transcriptData, transcriptLinesAtStart, subagentsDir)
		if err != nil {
			logging.Debug(ctx, "failed subagent aware token extraction",
				slog.String("error", err.Error()))
			return nil
		}
		return usage
	}

	if calculator, ok := AsTokenCalculator(ag); ok {
		// Fall back to basic token calculation (main transcript only)
		usage, err := calculator.CalculateTokenUsage(transcriptData, transcriptLinesAtStart)
		if err != nil {
			logging.Debug(ctx, "failed token extraction",
				slog.String("error", err.Error()))
			return nil
		}
		return usage
	}

	return nil
}
