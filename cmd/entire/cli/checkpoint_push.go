package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

type checkpointPushFlags struct {
	all  bool
	json bool
}

// checkpointPushJSON is `entire checkpoint push --json`. Results is never null.
type checkpointPushJSON struct {
	Remote  string                     `json:"checkpoint_sync_remote,omitempty"`
	Results []checkpointPushResultJSON `json:"results"`
	// Error is set when the command failed before it could report results.
	Error string `json:"error,omitempty"`
}

type checkpointPushResultJSON struct {
	CheckpointID    string `json:"checkpoint_id,omitempty"`
	Ref             string `json:"ref"`
	Commit          string `json:"commit,omitempty"`
	RequestedCommit string `json:"requested_commit,omitempty"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
}

func newCheckpointPushCmd() *cobra.Command {
	return newCheckpointPushCmdWithPrompt(interactive.CanPromptInteractively)
}

// newCheckpointPushCmdWithPrompt takes the "is a person at a terminal?" probe
// so tests can exercise both paths.
func newCheckpointPushCmdWithPrompt(canPrompt func() bool) *cobra.Command {
	var flags checkpointPushFlags
	cmd := &cobra.Command{
		Use:   "push [<checkpoint-id>[@<commit>]...]",
		Short: "Upload waiting checkpoints now",
		Long: `Upload the named checkpoints to the checkpoint sync remote now, the same way
git push would. Use it when the clone's checkpoint push policy is manual, so
git push leaves checkpoints on this machine, or to upload ahead of your next
push under automatic.

Each checkpoint is uploaded at its current version, with every earlier version
not yet uploaded. Run 'entire checkpoint list --outgoing' to see what is
waiting. <checkpoint-id>@<commit> uploads that checkpoint only while <commit>
is still its current version: if it changed since you looked, it is not sent.

--all and a bare <checkpoint-id> need a terminal. Without one, name each
checkpoint as <checkpoint-id>@<commit>, using the commit --outgoing --json
reports. With no arguments, it lists what is waiting and uploads nothing.

Examples:
  entire checkpoint push <id>
  entire checkpoint push --all
  entire checkpoint push <id>@<commit> --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if !flags.json {
				return runCheckpointPush(cmd, args, flags, canPrompt())
			}
			// --json callers read stdout as JSON, failures included.
			out := &countingWriter{w: cmd.OutOrStdout()}
			cmd.SetOut(out)
			err := runCheckpointPush(cmd, args, flags, canPrompt())
			if err != nil && out.n == 0 {
				if jsonErr := printJSON(out.w, checkpointPushJSON{Results: []checkpointPushResultJSON{}, Error: err.Error()}); jsonErr != nil {
					return jsonErr
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&flags.all, "all", false, "Upload every waiting checkpoint (needs a terminal)")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")
	return cmd
}

func runCheckpointPush(cmd *cobra.Command, args []string, flags checkpointPushFlags, canPrompt bool) error {
	ctx := cmd.Context()
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if flags.all && len(args) > 0 {
		return errors.New("--all cannot be combined with checkpoint IDs")
	}
	if checkDisabledGuard(ctx, errOut) {
		return NewSilentError(errors.New("entire is disabled in this repository"))
	}

	var requests []strategy.CheckpointPushRequest
	switch {
	case flags.all:
		if !canPrompt {
			return refuseCheckpointPushWithoutTerminal(errOut, "--all")
		}
		outgoing, err := strategy.ListOutgoingCheckpoints(ctx)
		if err != nil {
			return reportOutgoingErr(errOut, err)
		}
		for _, oc := range outgoing.Checkpoints {
			requests = append(requests, strategy.CheckpointPushRequest{Ref: oc.Ref})
		}
	case len(args) == 0 && flags.json:
		return errors.New("checkpoint push --json needs checkpoint IDs or --all; to see what is waiting, run entire checkpoint list --outgoing --json")
	case len(args) == 0:
		return printWaitingForCheckpointPush(cmd, errOut, canPrompt)
	default:
		parsed, err := parseCheckpointPushArgs(args, canPrompt, errOut)
		if err != nil {
			return err
		}
		requests = parsed
	}
	if len(requests) == 0 {
		fmt.Fprintln(errOut, "No checkpoints are waiting to upload.")
		if flags.json {
			return writeCheckpointPushJSON(out, strategy.CheckpointPushReport{})
		}
		return nil
	}

	report, err := strategy.PushCheckpoints(ctx, requests)
	if err != nil {
		if errors.Is(err, strategy.ErrCheckpointPushDisabled) {
			fmt.Fprintln(errOut, "Checkpoint pushing is disabled in settings (push_sessions=false), so nothing was uploaded.")
			return NewSilentError(err)
		}
		return reportOutgoingErr(errOut, err)
	}

	if flags.json {
		if err := writeCheckpointPushJSON(out, report); err != nil {
			return err
		}
	} else {
		writeCheckpointPushText(out, report)
	}
	for _, outcome := range report.Outcomes {
		// Already uploaded is done, not a failure; everything else is.
		if outcome.Status != strategy.CheckpointPushUploaded && outcome.Status != strategy.CheckpointPushNotWaiting {
			return NewSilentError(errors.New("some checkpoints were not uploaded"))
		}
	}
	return nil
}

// parseCheckpointPushArgs reads <id> and <id>@<commit>. Without a terminal
// every argument must pin a commit, so a caller sends only a version it saw.
func parseCheckpointPushArgs(args []string, canPrompt bool, errOut io.Writer) ([]strategy.CheckpointPushRequest, error) {
	requests := make([]strategy.CheckpointPushRequest, 0, len(args))
	for _, arg := range args {
		rawID, rawCommit, pinned := strings.Cut(arg, "@")
		if !pinned && !canPrompt {
			return nil, refuseCheckpointPushWithoutTerminal(errOut, arg)
		}
		cid, err := id.NewCheckpointID(rawID)
		if err != nil {
			return nil, fmt.Errorf("invalid checkpoint ID %q: pass the full ID (prefixes are not accepted)", rawID)
		}
		ref, err := checkpoint.RefName(cid)
		if err != nil {
			return nil, fmt.Errorf("checkpoint ref for %s: %w", cid, err)
		}
		request := strategy.CheckpointPushRequest{Ref: ref}
		if pinned {
			// The zero hash is how a request says "not pinned".
			if !plumbing.IsHash(rawCommit) || plumbing.NewHash(rawCommit).IsZero() {
				return nil, fmt.Errorf("invalid commit %q in %q: pass the full commit from 'entire checkpoint list --outgoing --json'", rawCommit, arg)
			}
			request.Commit = plumbing.NewHash(rawCommit)
		}
		requests = append(requests, request)
	}
	return requests, nil
}

func refuseCheckpointPushWithoutTerminal(errOut io.Writer, what string) error {
	fmt.Fprintf(errOut, "entire checkpoint push %s needs a terminal. Without one, name each checkpoint as <checkpoint-id>@<commit>, using the commit 'entire checkpoint list --outgoing --json' reports.\n", what)
	return NewSilentError(errors.New("checkpoint push needs exact versions without a terminal"))
}

// printWaitingForCheckpointPush answers a bare `entire checkpoint push`: it
// names what is waiting and how to send it. --all is offered only where it is
// accepted.
func printWaitingForCheckpointPush(cmd *cobra.Command, errOut io.Writer, canPrompt bool) error {
	outgoing, err := strategy.ListOutgoingCheckpoints(cmd.Context())
	if err != nil {
		return reportOutgoingErr(errOut, err)
	}
	if len(outgoing.Checkpoints) == 0 {
		fmt.Fprintln(errOut, "No checkpoints are waiting to upload.")
		return nil
	}
	fmt.Fprintf(errOut, "%d waiting to upload:\n", len(outgoing.Checkpoints))
	for _, oc := range outgoing.Checkpoints {
		fmt.Fprintf(errOut, "  %s\n", checkpointPushLabel(oc.CheckpointID, oc.Ref))
	}
	if canPrompt {
		fmt.Fprintln(errOut, "Upload them all with 'entire checkpoint push --all', or name each one as <checkpoint-id>@<commit>.")
	} else {
		fmt.Fprintln(errOut, "Upload with 'entire checkpoint push <checkpoint-id>@<commit>'.")
	}
	fmt.Fprintln(errOut, "Each checkpoint's commit is in 'entire checkpoint list --outgoing --json'.")
	return nil
}

func checkpointPushLabel(cid id.CheckpointID, ref plumbing.ReferenceName) string {
	if cid.IsEmpty() {
		return ref.String()
	}
	return cid.String()
}

func checkpointPushResults(report strategy.CheckpointPushReport) []checkpointPushResultJSON {
	results := make([]checkpointPushResultJSON, 0, len(report.Outcomes))
	for _, outcome := range report.Outcomes {
		result := checkpointPushResultJSON{
			Ref:    outcome.Target.Ref.String(),
			Status: string(outcome.Status),
		}
		if cid, ok := checkpoint.ParseRef(outcome.Target.Ref); ok {
			result.CheckpointID = cid.String()
		}
		if !outcome.Commit.IsZero() {
			result.Commit = outcome.Commit.String()
		}
		if !outcome.Target.Commit.IsZero() {
			result.RequestedCommit = outcome.Target.Commit.String()
		}
		if outcome.Err != nil {
			result.Error = outcome.Err.Error()
		}
		results = append(results, result)
	}
	return results
}

func writeCheckpointPushJSON(w io.Writer, report strategy.CheckpointPushReport) error {
	return printJSON(w, checkpointPushJSON{Remote: report.Remote, Results: checkpointPushResults(report)})
}

func writeCheckpointPushText(w io.Writer, report strategy.CheckpointPushReport) {
	for _, outcome := range report.Outcomes {
		cid, _ := checkpoint.ParseRef(outcome.Target.Ref)
		label := checkpointPushLabel(cid, outcome.Target.Ref)
		switch outcome.Status {
		case strategy.CheckpointPushUploaded:
			fmt.Fprintf(w, "%s  uploaded to %s\n", label, report.Remote)
		case strategy.CheckpointPushChanged:
			if outcome.Target.Commit.IsZero() {
				fmt.Fprintf(w, "%s  not sent: a newer version was written while it was uploading. Run the command again to upload it.\n", label)
				break
			}
			if outcome.Commit.IsZero() {
				fmt.Fprintf(w, "%s  not sent: it changed since you looked. Check it with 'entire checkpoint list --outgoing'.\n", label)
				break
			}
			fmt.Fprintf(w, "%s  not sent: it changed since you looked; it is now %s. Check it with 'entire checkpoint list --outgoing'.\n", label, outcome.Commit)
		case strategy.CheckpointPushNotWaiting:
			fmt.Fprintf(w, "%s  not sent: it is not waiting to upload\n", label)
		case strategy.CheckpointPushNotFound:
			fmt.Fprintf(w, "%s  not sent: there is no checkpoint with this ID here\n", label)
		case strategy.CheckpointPushFailed:
			fmt.Fprintf(w, "%s  failed: %v; it stays queued\n", label, outcome.Err)
		}
	}
}

// countingWriter records whether anything was written, so a --json run that
// failed before reporting can still put JSON on stdout.
type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err //nolint:wrapcheck // a pass-through writer
}
