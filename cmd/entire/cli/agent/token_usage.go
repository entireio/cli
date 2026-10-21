package agent

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ExtractWithSubagentInventory gives built-in agents an authoritative child
// ledger. It deliberately has no external-agent protocol equivalent: callers
// supply the inventory rather than asking an agent to infer children from text.
//
// home is the session's recorded agent home, or "". When it is trusted (see
// ResolveTrustedHome) and the agent is a HomeScopedInventoryExtractor, child
// transcripts are looked up beneath it; otherwise beneath the active home.
func ExtractWithSubagentInventory(ctx context.Context, ag Agent, transcriptData []byte, transcriptLinesAtStart int, refs []SubagentReference, home string) (InventoryExtraction, bool) {
	extractor, ok := AsInventoryAwareExtractor(ag)
	if !ok {
		return InventoryExtraction{}, false
	}
	var extraction InventoryExtraction
	var err error
	if scoped, trusted, ok := homeScopedInventoryExtractor(ctx, ag, home); ok {
		extraction, err = scoped.ExtractWithSubagentInventoryUnderHome(ctx, transcriptData, transcriptLinesAtStart, refs, trusted)
	} else {
		extraction, err = extractor.ExtractWithSubagentInventory(ctx, transcriptData, transcriptLinesAtStart, refs)
	}
	if err != nil {
		logging.Debug(ctx, "failed inventory-aware token extraction", slog.String("error", err.Error()))
		return InventoryExtraction{}, false
	}
	return extraction, true
}

// homeScopedInventoryExtractor returns ag as a HomeScopedInventoryExtractor
// and the home to scope it to, when home is set and trusted, is not spelled as
// the active home, and ag implements the interface. The returned home is the
// spelling ResolveTrustedHome checked, which is also the one the session's
// transcript paths use. An untrusted home is logged and ignored.
func homeScopedInventoryExtractor(ctx context.Context, ag Agent, home string) (HomeScopedInventoryExtractor, string, bool) {
	if home == "" {
		return nil, "", false
	}
	scoped, ok := AsHomeScopedInventoryExtractor(ag)
	if !ok {
		return nil, "", false
	}
	provider, ok := AsHomeLayoutProvider(ag)
	if !ok {
		return nil, "", false
	}
	home, err := ResolveTrustedHome(provider, home)
	if err != nil {
		logging.Debug(ctx, "ignoring recorded agent home for child transcripts", slog.String("error", err.Error()))
		return nil, "", false
	}
	// The active home, as the agent spells it, needs no scoping, and scoping it
	// would lose the agent's own resolution of its session directory. Another
	// spelling of the same directory is scoped, so its paths stay comparable.
	if active, err := provider.SessionHome(); err == nil && sameDir(filepath.Clean(active), home) {
		return nil, "", false
	}
	return scoped, home, true
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
