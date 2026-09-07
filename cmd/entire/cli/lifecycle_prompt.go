package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func appendPromptToFile(ctx context.Context, sessionID, prompt string) error {
	_, err := appendPrompt(ctx, sessionID, prompt, false)
	return err
}

func appendPromptToFileIfLastDiffers(ctx context.Context, sessionID, prompt string) (bool, error) {
	return appendPrompt(ctx, sessionID, prompt, true)
}

func appendPrompt(ctx context.Context, sessionID, prompt string, skipDuplicate bool) (bool, error) {
	if prompt == "" {
		return false, nil
	}

	sessionName := sessionMetadataName(sessionID)
	root, err := entiredir.Open(ctx)
	if err != nil {
		return false, fmt.Errorf("open .entire directory: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, sessionName, 0o750); err != nil {
		return false, fmt.Errorf("create session metadata directory: %w", err)
	}

	promptName := sessionName + "/" + paths.PromptFileName
	existing, err := entiredir.ReadFile(root, promptName)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read prompt.txt: %w", err)
	}
	prompts := checkpoint.SplitPromptContent(string(existing))
	if skipDuplicate && len(prompts) > 0 && prompts[len(prompts)-1] == prompt {
		return false, nil
	}

	content := prompt
	if len(existing) > 0 {
		content = string(existing) + checkpoint.PromptSeparator + prompt
	}
	if err := entiredir.WriteFile(root, promptName, []byte(content), 0o600); err != nil {
		return false, fmt.Errorf("write prompt.txt: %w", err)
	}
	return true, nil
}

func handleLifecyclePromptUpdate(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "prompt-update",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
	)

	if event.SessionID == "" {
		return fmt.Errorf("no session_id in %s event", event.Type)
	}
	if event.Prompt == "" {
		return nil
	}

	var appendedSkillEvents []agent.SkillEvent
	err := strategy.MutateSessionStateOnSaved(ctx, event.SessionID, func(state *strategy.SessionState) error {
		// Condensation clears prompt.txt while holding the same lock. Keep the
		// repair file and state writes ordered with that operation.
		promptAppended, err := appendPromptToFileIfLastDiffers(ctx, event.SessionID, event.Prompt)
		if err != nil {
			return err
		}
		lastPrompt := session.TruncatePromptForStorage(event.Prompt)
		lastPromptChanged := state.LastPrompt != lastPrompt
		state.LastPrompt = lastPrompt
		appendedSkillEvents = appendPromptSkillEventToState(ag, event, state)
		if !promptAppended && !lastPromptChanged && len(appendedSkillEvents) == 0 {
			return strategy.ErrMutationSkip
		}
		return nil
	}, func() {
		strategy.EmitSkillInvocationTelemetry(ctx, appendedSkillEvents)
	})
	if err != nil {
		return fmt.Errorf("update prompt storage: %w", err)
	}
	return nil
}
