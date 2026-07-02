package agent

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ExtractSkillEvents extracts normalized skill events from transcript data.
// Returns nil if the agent does not support skill-event extraction or extraction fails.
func ExtractSkillEvents(ctx context.Context, ag Agent, transcriptData []byte, fromOffset int) []SkillEvent {
	if ag == nil || len(transcriptData) == 0 {
		return nil
	}

	extractor, ok := AsSkillEventExtractor(ag)
	if !ok {
		return nil
	}

	events, err := extractor.ExtractSkillEvents(transcriptData, fromOffset)
	if err != nil {
		logging.Debug(ctx, "failed skill event extraction",
			slog.String("error", err.Error()))
		return nil
	}
	return events
}
