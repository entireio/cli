package cli

import (
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newSessionCurrentCmd() *cobra.Command {
	var jsonFlag bool
	var transcriptFlag bool

	cmd := &cobra.Command{
		Use:   "current",
		Short: "Show the session running this command, or the most recent one here",
		Long: `Show the session running this command, or the most recent one in this worktree.

When an agent runs this, it reports that agent's own session: agents publish
their session ID into the environment of the commands they run, and Entire
reads it back. Failing that, it falls back to the most recently active session
recorded in this worktree, and then to the most recent one anywhere in the
repository's shared session store.

Those are different answers to different questions, so the output always says
which one you got: the "Resolved" line in text mode, or the "resolution" field
under --json. Only "caller-env" and "ancestry" identify the session running this
command; "other-worktree" can be any unrelated session in the store, because
every worktree of a repository shares one. Check it before acting on the
session rather than merely displaying it.

Output modes:
  Default       Human-readable summary.
  --json        Metadata-only JSON envelope (no transcript bytes).
  --transcript  Stream the live raw agent transcript bytes to stdout.

Examples:
  entire session current
  entire session current --json
  entire session current --transcript > session.jsonl`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if _, err := paths.WorktreeRoot(ctx); err != nil {
				cmd.SilenceUsage = true
				return errors.New("not a git repository")
			}

			machineReadable := jsonFlag || transcriptFlag
			resolved := strategy.ResolveCallerSession(ctx)
			if !resolved.Found() {
				return reportNoSessionEnvelope(cmd, machineReadable,
					errors.New("no active session found in this worktree"),
					"No active session found in this worktree.")
			}
			if !resolved.Tracked {
				return reportUntrackedCallerSession(cmd, resolved, machineReadable)
			}
			// Loud on stderr as well as labelled in the output, for every
			// resolution that did not identify the caller: these are the
			// answers that used to be indistinguishable from a real one, and
			// the ones an agent must not act on.
			switch resolved.Resolution {
			case strategy.ResolutionOtherWorktree:
				fmt.Fprintln(cmd.ErrOrStderr(),
					"[entire] No session is recorded in this worktree; showing the most recent one from elsewhere in this repository. It is not this command's caller.")
			case strategy.ResolutionCallerAmbiguous:
				fmt.Fprintln(cmd.ErrOrStderr(),
					"[entire] Several agent sessions claim this command and none could be ordered; showing the most plausible. Do not act on it without confirming which session is yours.")
			case strategy.ResolutionCallerEnv, strategy.ResolutionAncestry:
				// Identified the caller; the answer needs no caveat.
			case strategy.ResolutionWorktree:
				// "The session that has been working here" is what someone
				// typing this in a terminal already assumes, so saying it adds
				// noise. It is still labelled in the output and not IsCaller.
			case strategy.ResolutionNone:
				// Unreachable: handled by the !Found() branch above. Named so
				// the exhaustive check forces a decision on a future tier.
			}

			return runSessionInfo(ctx, cmd, resolved.SessionID, sessionOutputModeFromFlags(jsonFlag, transcriptFlag), resolved.Resolution)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&transcriptFlag, "transcript", false, "Stream raw agent transcript bytes to stdout")
	cmd.MarkFlagsMutuallyExclusive("json", "transcript")
	return cmd
}

// reportUntrackedCallerSession handles the case where the agent running us
// named its session but Entire holds no state for it.
//
// This is a diagnosis, not a lookup failure, and it is the reason the resolver
// reports the ID rather than discarding it: we know which session the caller
// is in — or, when several agents claim it, that it is one of a named few —
// and we know Entire is not recording it — hooks are not
// installed, they failed, or the session's first turn has not landed yet
// (state is created at turn start). Answering "session not found" would hide
// the useful half, and falling through to the most-recent tiers would answer a
// question nobody asked with a session that isn't the caller's.
//
// Machine-readable modes exit non-zero for the same reason as the no-session
// branch above: there is no session envelope to emit, so stdout must stay
// empty rather than carry prose a parser would choke on.
func reportUntrackedCallerSession(cmd *cobra.Command, resolved strategy.ResolvedSession, machineReadable bool) error {
	agentLabel := string(resolved.AgentType)
	if agentLabel == "" {
		agentLabel = unknownPlaceholder
	}
	// An ambiguous resolution must not be narrated as a fact. With several
	// claims and no state behind any of them there is nothing to order them
	// by — no owner to compare, no interaction time — so the reported session
	// is the first of several, and "This command is running inside X" would
	// state that arbitrary pick as the answer. The diagnosis (you are inside
	// an untracked session) survives; the identification does not.
	if resolved.Resolution == strategy.ResolutionCallerAmbiguous {
		return reportNoSessionEnvelope(cmd, machineReadable,
			fmt.Errorf("several untracked agent sessions claim this command, including %s", resolved.SessionID),
			"Several agent sessions claim this command and Entire has state for none of them, so which one is running it could not be determined.",
			fmt.Sprintf("One of them is %s session %s. Pass a session ID explicitly rather than relying on this.",
				agentLabel, resolved.SessionID),
			"Entire may not be enabled here, or its hooks may not have run yet. Run 'entire status' to check.")
	}
	return reportNoSessionEnvelope(cmd, machineReadable,
		fmt.Errorf("no session state for caller session %s", resolved.SessionID),
		fmt.Sprintf("This command is running inside %s session %s, but Entire has no session state for it.",
			agentLabel, resolved.SessionID),
		"Entire may not be enabled here, its hooks may not have run yet, or this session's first turn has not completed. Run 'entire status' to check.")
}

// reportNoSessionEnvelope reports that there is no session envelope to emit.
//
// Machine-readable modes must not put prose on stdout with a zero exit —
// downstream parsers treat stdout as JSON (or raw transcript bytes) and would
// choke on it — so the lines go to stderr and the command exits non-zero,
// letting a caller detect the case. The human default keeps the lines on
// stdout and succeeds. Shared because both no-session cases need exactly this
// shape, and a third would otherwise copy it again.
func reportNoSessionEnvelope(cmd *cobra.Command, machineReadable bool, sentinel error, lines ...string) error {
	out := cmd.OutOrStdout()
	if machineReadable {
		cmd.SilenceUsage = true
		out = cmd.ErrOrStderr()
	}
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
	if machineReadable {
		return NewSilentError(sentinel)
	}
	return nil
}
