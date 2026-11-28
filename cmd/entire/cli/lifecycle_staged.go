package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func appendStoredPrompt(ctx context.Context, sessionID, prompt string) error {
	err := strategy.WithSessionMetadata(ctx, sessionID, func(root *os.Root) error {
		name := sessionMetadataName(sessionID) + "/" + paths.PromptFileName
		existing, err := entiredir.ReadFile(root, name)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read stored prompt: %w", err)
		}
		content := prompt
		if len(existing) > 0 {
			content = string(existing) + "\n\n---\n\n" + prompt
		}
		if err := entiredir.WriteFile(root, name, []byte(content), 0o600); err != nil {
			return fmt.Errorf("write stored prompt: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("append session prompt: %w", err)
	}
	return nil
}

// Extract outside the gate; only the conditional publication needs to serialize
// with condensation. Recheck the prompt and checkpoint state under the gate so
// another hook's prompt is never overwritten by a delayed backfill.
func backfillStoredPrompt(ctx context.Context, ag agent.Agent, sessionID, transcriptRef string, offset int) string {
	extractor, ok := agent.AsPromptExtractor(ag)
	if !ok {
		return ""
	}
	logCtx := logging.WithComponent(ctx, "lifecycle")
	// Avoid reading/parsing the transcript when no backfill is needed.
	root, err := strategy.OpenSessionEntireDir(ctx, sessionID)
	if err != nil {
		logging.Warn(logCtx, "failed to open prompt directory", slog.String("error", err.Error()))
		return ""
	}
	existing, err := entiredir.ReadFile(root, sessionMetadataName(sessionID)+"/"+paths.PromptFileName)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		logging.Warn(logCtx, "failed to read prompt.txt, skipping backfill", slog.String("error", err.Error()))
		return ""
	}
	if len(existing) > 0 || turnHasMidTurnCheckpoints(ctx, sessionID) {
		return ""
	}
	prompts, err := extractor.ExtractPrompts(transcriptRef, offset)
	if err != nil {
		logging.Warn(logCtx, "failed to extract prompts from transcript", slog.String("error", err.Error()))
		return ""
	}
	if len(prompts) == 0 {
		return ""
	}
	var backfilled string
	err = strategy.WithSessionMetadata(ctx, sessionID, func(root *os.Root) error {
		name := sessionMetadataName(sessionID) + "/" + paths.PromptFileName
		existing, err := entiredir.ReadFile(root, name)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read stored prompt before backfill: %w", err)
		}
		if len(existing) > 0 || turnHasMidTurnCheckpoints(ctx, sessionID) {
			return nil
		}
		if err := entiredir.WriteFile(root, name, []byte(strings.Join(prompts, "\n\n---\n\n")), 0o600); err != nil {
			return fmt.Errorf("write backfilled prompt: %w", err)
		}
		backfilled = prompts[len(prompts)-1]
		return nil
	})
	if err != nil {
		logging.Warn(logCtx, "failed to backfill prompt.txt from transcript", slog.String("error", err.Error()))
	}
	return backfilled
}
