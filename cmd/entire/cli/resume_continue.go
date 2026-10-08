package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"charm.land/huh/v2"
)

type restoredSessionStartPrompt func(context.Context, []strategy.RestoredSession) (bool, error)
type restoredSessionPicker func(context.Context, io.Writer, []strategy.RestoredSession) (strategy.RestoredSession, bool, error)
type restoredSessionLauncher func(context.Context, io.Writer, strategy.RestoredSession) error
type restoredSessionDisplayer func(io.Writer, []strategy.RestoredSession) error
type restoredSessionSummaryPrinter func(io.Writer, []strategy.RestoredSession)

type restoredSessionContinueOptions struct {
	CanPrompt          bool
	PreferredSessionID string
	PromptStartAgent   restoredSessionStartPrompt
	PromptSession      restoredSessionPicker
	Launch             restoredSessionLauncher
	Display            restoredSessionDisplayer
	PrintSummary       restoredSessionSummaryPrinter
}

func continueRestoredSessions(ctx context.Context, w io.Writer, sessions []strategy.RestoredSession, opts restoredSessionContinueOptions) error {
	if len(sessions) == 0 {
		return nil
	}

	display := opts.Display
	if display == nil {
		display = displayRestoredSessions
	}
	launch := opts.Launch
	if launch == nil {
		launch = launchTrailRestoredSession
	}
	promptStart := opts.PromptStartAgent
	if promptStart == nil {
		promptStart = promptStartRestoredAgent
	}
	promptSession := opts.PromptSession
	if promptSession == nil {
		promptSession = promptTrailRestoredSession
	}

	if opts.PreferredSessionID != "" {
		session, ok := findTrailRestoredSession(sessions, opts.PreferredSessionID)
		if !ok {
			return fmt.Errorf("session %q was not found in the restored checkpoint", opts.PreferredSessionID)
		}
		return continueSelectedRestoredSession(ctx, w, session, opts.CanPrompt, promptStart, launch, display, opts.PrintSummary)
	}

	if !opts.CanPrompt {
		return display(w, sessions)
	}

	startAgent, err := promptStart(ctx, sessions)
	if err != nil {
		return err
	}
	if !startAgent {
		return display(w, sessions)
	}

	if opts.PrintSummary != nil {
		opts.PrintSummary(w, sessions)
	}
	if len(sessions) == 1 {
		return launch(ctx, w, sessions[0])
	}

	selected, ok, err := promptSession(ctx, w, sessions)
	if err != nil || !ok {
		return err
	}
	return launch(ctx, w, selected)
}

func continueSelectedRestoredSession(
	ctx context.Context,
	w io.Writer,
	session strategy.RestoredSession,
	canPrompt bool,
	promptStart restoredSessionStartPrompt,
	launch restoredSessionLauncher,
	display restoredSessionDisplayer,
	printSummary restoredSessionSummaryPrinter,
) error {
	sessions := []strategy.RestoredSession{session}
	if !canPrompt {
		return display(w, sessions)
	}

	startAgent, err := promptStart(ctx, sessions)
	if err != nil {
		return err
	}
	if !startAgent {
		return display(w, sessions)
	}

	if printSummary != nil {
		printSummary(w, sessions)
	}
	return launch(ctx, w, session)
}

func promptStartRestoredAgent(ctx context.Context, sessions []strategy.RestoredSession) (bool, error) {
	startAgent := true
	form := NewAccessibleForm(
		huh.NewGroup(
			newStartRestoredAgentConfirm(&startAgent, restoredSessionsCheckpointID(sessions)),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return false, nil
		}
		return false, fmt.Errorf("failed to choose resume action: %w", err)
	}
	return startAgent, nil
}

func newStartRestoredAgentConfirm(startAgent *bool, checkpointID string) *huh.Confirm {
	description := "Entire restored the checkpoint session log. Choose No to print the resume command instead."
	if checkpointID != "" {
		description = fmt.Sprintf("Entire restored checkpoint %s. Choose No to print the resume command instead.", checkpointID)
	}
	return huh.NewConfirm().
		Title("Start the agent now?").
		Description(description).
		Affirmative("Yes").
		Negative("No").
		Value(startAgent)
}

func restoredSessionsCheckpointID(sessions []strategy.RestoredSession) string {
	var checkpointID string
	for _, session := range sessions {
		current := strings.TrimSpace(session.CheckpointID)
		if current == "" {
			return ""
		}
		if checkpointID == "" {
			checkpointID = current
			continue
		}
		if checkpointID != current {
			return ""
		}
	}
	return checkpointID
}
