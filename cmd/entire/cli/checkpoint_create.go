package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/spf13/cobra"
)

// checkpointCreateJSON is the --json envelope for `checkpoint create`.
// Resolution is omitted for an explicit session ID, matching session tokens.
type checkpointCreateJSON struct {
	CheckpointID string `json:"checkpoint_id"`
	SessionID    string `json:"session_id"`
	Resolution   string `json:"resolution,omitempty"`
}

func newCheckpointCreateCmd() *cobra.Command {
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:    "create [session-id]",
		Short:  "Create a checkpoint from a session's transcript",
		Hidden: true,
		Long: `Create a checkpoint from a session's current transcript and print its ID.

For sessions that change no files, such as research, planning or review: they
are otherwise never checkpointed, because checkpoints are made when changed
files are committed. A session with uncommitted file changes is refused; its
next commit checkpoints that work.

With no argument, checkpoints the agent session running this command. That
requires the session to be identified from the agent's environment or process
ancestry; a most-recent-session guess is refused, so pass the session's ID
explicitly when running this outside the agent.

The checkpoint is written, redacted, and queued for push exactly as a commit's
checkpoint is, but it is not linked to any commit. It is a snapshot: the
session's own state is left alone, so repeated snapshots each cover the session
from the same point.

Examples:
  entire checkpoint create
  entire checkpoint create <session-id> --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cmd.SilenceUsage = true
			// Stdout carries only the checkpoint ID (or JSON), which callers
			// capture, so the disabled notice goes to stderr with a failing exit
			// instead of being read back as an ID.
			if checkDisabledGuard(ctx, cmd.ErrOrStderr()) {
				return NewSilentError(errors.New("entire is disabled in this repository"))
			}

			sessionID, resolution, err := resolveCheckpointCreateSession(cmd, args)
			if err != nil {
				return err
			}

			// Checkpoint writes must never fall back to the default scanner
			// set on a broken redaction config.
			if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
				return fmt.Errorf("configure redaction: %w", err)
			}
			// The session may belong to an external agent, which condensation
			// can only read once registered.
			external.DiscoverAndRegister(ctx)

			checkpointID, err := GetStrategy(ctx).CreateSnapshotCheckpoint(ctx, sessionID)
			switch {
			case errors.Is(err, strategy.ErrNothingToCheckpoint):
				return fmt.Errorf("session %s has no transcript or files to checkpoint yet", sessionID)
			case errors.Is(err, strategy.ErrPendingFileChanges):
				return fmt.Errorf("session %s has uncommitted file changes; its work is checkpointed when you commit", sessionID)
			case err != nil:
				return err //nolint:wrapcheck // strategy errors already say which step failed
			}

			out := cmd.OutOrStdout()
			if !jsonFlag {
				fmt.Fprintln(out, checkpointID.String())
				return nil
			}
			payload := checkpointCreateJSON{CheckpointID: checkpointID.String(), SessionID: sessionID}
			if resolution != strategy.ResolutionNone {
				payload.Resolution = string(resolution)
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(payload); err != nil {
				return fmt.Errorf("encode JSON: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	return cmd
}

// resolveCheckpointCreateSession returns the session to checkpoint: the
// explicit argument, or else the calling agent's session. Creating a
// checkpoint acts on a session, so only a resolution that identified the
// caller (IsCaller) is accepted — the worktree and other-worktree tiers are
// "most recent session" guesses that can name someone else's work.
func resolveCheckpointCreateSession(cmd *cobra.Command, args []string) (string, strategy.SessionResolution, error) {
	if len(args) == 1 {
		if err := validation.ValidateSessionID(args[0]); err != nil {
			return "", strategy.ResolutionNone, fmt.Errorf("invalid session ID: %w", err)
		}
		return args[0], strategy.ResolutionNone, nil
	}

	resolved := strategy.ResolveCallerSession(cmd.Context())
	switch {
	case !resolved.Found():
		return "", resolved.Resolution, errors.New("no agent session is running this command; pass a session ID")
	case !resolved.Resolution.IsCaller():
		// Deliberately does not print resolved.SessionID: it is a most-recent
		// guess, and naming it invites passing it straight back as an explicit
		// ID, which would act on a session that may not be the caller's.
		return "", resolved.Resolution, fmt.Errorf(
			"could not identify the agent session running this command (resolution %q); pass your session's ID explicitly",
			resolved.Resolution)
	case !resolved.Tracked:
		return "", resolved.Resolution, fmt.Errorf(
			"this command is running inside %s session %s, but Entire has no session state for it yet",
			resolved.AgentType, resolved.SessionID)
	}
	return resolved.SessionID, resolved.Resolution, nil
}
