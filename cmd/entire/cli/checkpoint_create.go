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

With no argument, checkpoints the agent session running this command. That
requires the session to be identified from the agent's environment or process
ancestry; a most-recent-session guess is refused, so pass a session ID
explicitly when running this outside an agent.

The checkpoint is written, redacted, and queued for push exactly as a commit's
checkpoint is, but it is not linked to any commit. It is a snapshot: the
session's own checkpoint window is left alone, so the next commit's checkpoint
covers the same work again.

Examples:
  entire checkpoint create
  entire checkpoint create <session-id> --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if checkDisabledGuard(ctx, cmd.OutOrStdout()) {
				return nil
			}
			cmd.SilenceUsage = true

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
			if errors.Is(err, strategy.ErrNothingToCheckpoint) {
				return fmt.Errorf("session %s has no transcript or files to checkpoint yet", sessionID)
			}
			if err != nil {
				return err //nolint:wrapcheck // strategy errors already name the session and step
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
		return "", resolved.Resolution, fmt.Errorf(
			"could not identify the agent session running this command (resolution %q, candidate %s); pass a session ID",
			resolved.Resolution, resolved.SessionID)
	case !resolved.Tracked:
		return "", resolved.Resolution, fmt.Errorf(
			"this command is running inside %s session %s, but Entire has no session state for it yet",
			resolved.AgentType, resolved.SessionID)
	}
	return resolved.SessionID, resolved.Resolution, nil
}
