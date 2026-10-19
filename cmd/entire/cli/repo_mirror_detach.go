package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// Detach statuses a real detach and GET /repos/{repoId}/detach answer with.
// The wire fields are open strings (see readModelEnumFields), so anything else
// renders verbatim rather than taking one of these branches.
const (
	detachStatusComplete   = "complete"
	detachStatusInProgress = "in_progress"
	detachStatusStalled    = "stalled"
)

type mirrorDetachOptions struct {
	into    string
	dryRun  bool
	noWait  bool
	timeout time.Duration
}

func newRepoMirrorDetachCmd() *cobra.Command {
	var opts mirrorDetachOptions
	cmd := &cobra.Command{
		Use:   "detach <repo> --into /et/<project>/<repo>",
		Short: "Convert a GitHub mirror into a native Entire repository",
		Long: "Converts a GitHub mirror into the Entire-native repository named by " +
			"--into. The repository keeps its history and its cluster; its " +
			"/gh/<owner>/<repo> address is released and answers \"moved\" from then on, " +
			"and the GitHub repository itself stays live and is no longer synced.\n\n" +
			"Every run first asks the server for a plan, which lists the " +
			"preconditions the detach needs. --dry-run prints the plan and changes nothing.\n\n" +
			"After the detach, only the --into project grants member access to the " +
			"repository: access through GitHub and grants on the mirror are removed. " +
			"Add everyone who still needs access to the project first. A public " +
			"repository stays public.\n\n" +
			"A real detach freezes writes, waits for the mirror to match GitHub, " +
			"and rewires the repository. It asks for confirmation first; pass " +
			"--yes to skip it, which a non-interactive run must. When the rewire " +
			"cannot finish in one call the repository stays frozen, and the command " +
			"waits for it to complete — through a stall the server resumes on its " +
			"own — up to --timeout. Pass --no-wait to return as soon as the " +
			"repository is native.\n\n" +
			"The mirror must have exactly one placement: remove the others with " +
			"`entire repo mirror remove` first.",
		Example: "  entire repo mirror detach /gh/octocat/hello-world --into /et/acme/hello-world --dry-run\n" +
			"  entire repo mirror detach /gh/octocat/hello-world --into /et/acme/hello-world\n" +
			"  entire repo mirror detach /gh/octocat/hello-world --into /et/acme/hello --yes",
		Args: cobra.ExactArgs(1),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			// Zero is an unbounded wait, matching `mirror add`.
			if opts.timeout < 0 {
				return errors.New("--timeout must be zero or positive")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMirrorDetach(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.into, "into", "", "Native repository to detach into, as /et/<project>/<repo> (required)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Print the preconditions and change nothing")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "Return once the repository is native, without waiting for the rewire to complete")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 30*time.Minute, "How long to wait for the rewire to complete (0 waits indefinitely)")
	markRequired(cmd, "into")
	// No --force: nothing overrides an ineligible plan, so the flag only
	// answers the prompt.
	addYesFlag(cmd)
	addJSONFlag(cmd)
	return cmd
}

// runMirrorDetach resolves the mirror and the target project, fetches the
// dry-run plan (which the confirmation shows, and which names the failed
// preconditions on a refusal), then confirms and runs the detach.
func runMirrorDetach(cmd *cobra.Command, repoRef string, opts mirrorDetachOptions) error {
	cmd.SilenceUsage = true
	ref, err := parseMirrorRepoRef(repoRef, mirrorCloneForge)
	if err != nil {
		return err
	}
	// owner is the project and repo the native name, as for any /et/ ref.
	into, err := parseMirrorRepoRef(opts.into, nativeCloneForge)
	if err != nil {
		return fmt.Errorf("invalid --into: %w", err)
	}
	yes := forceRequested(cmd)
	// An unanswerable prompt must not cost a request.
	if !opts.dryRun && !yes && !interactive.CanPromptInteractively() {
		return fmt.Errorf("refusing to detach %s without confirmation; pass --yes, or --dry-run to only see the plan", ref.qualified())
	}

	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		var (
			projectID, projectName, repoID string
			projectErr, repoErr            error
			g                              errgroup.Group
		)
		// Independent lookups. No shared context, so one failing does not
		// cancel the other into a misleading error; the project's error wins.
		g.Go(func() error {
			var project resolvedRef
			project, projectErr = resolveProjectByName(ctx, c, into.owner)
			projectID, projectName = project.ID, project.Name
			return nil
		})
		g.Go(func() error {
			repoID, repoErr = resolveDetachRepoID(ctx, c, ref)
			return nil
		})
		_ = g.Wait() //nolint:errcheck // both goroutines return nil; their errors are read below
		if projectErr != nil {
			return projectErr
		}
		if repoErr != nil {
			return repoErr
		}

		body := coreapi.DetachRepoBody{TargetProject: projectID, Name: coreapi.NewOptString(into.repo), DryRun: true}
		params := coreapi.DetachRepoParams{RepoId: repoID}
		plan, err := c.DetachRepo(ctx, &body, params)
		if err != nil {
			return fmt.Errorf("plan the detach of %s: %w", ref.qualified(), err)
		}
		target := nativeRepoPath(projectName + "/" + plan.Name)
		prompting := !opts.dryRun && plan.Eligible && !yes

		if opts.dryRun || !plan.Eligible {
			if jsonRequested(cmd) {
				err = printJSON(cmd.OutOrStdout(), plan)
			} else {
				err = writeDetachPlan(cmd.OutOrStdout(), ref, target, plan)
			}
			if err != nil || opts.dryRun {
				return err
			}
			return fmt.Errorf("%s cannot be detached; failed: %s", ref.qualified(), strings.Join(failedDetachPreconditions(plan), ", "))
		}

		if prompting {
			proceed, err := detachConfirmed(cmd, ref, target, plan)
			if err != nil || !proceed {
				return err
			}
		}

		body.DryRun = false
		errW := cmd.ErrOrStderr()
		fmt.Fprintf(errW, "Detaching %s into %s. This can take a few minutes; writes to it stay frozen until it finishes.\n", ref.qualified(), target)
		// The call itself catches the mirror up with GitHub before the rewire,
		// so the spinner starts here, not at the wait.
		update, stop := startUpdatableSpinner(errW, "Catching up with GitHub")
		res, err := c.DetachRepo(ctx, &body, params)
		if err != nil {
			stop(false)
			if !detachRefusedOutright(err) {
				// No answer, a 5xx, or an interruption: the repo may have been
				// frozen and rewired all the same.
				fmt.Fprintf(cmd.ErrOrStderr(), "The detach may have started. %s\n", detachFollowUpHint(repoID))
			}
			return fmt.Errorf("detach %s: %w", ref.qualified(), err)
		}
		return finishDetach(cmd, c, ref, repoID, projectID, res, opts, update, stop)
	})
}

// finishDetach waits on an unfinished detach, renders where it ended, and
// decides the exit. Every exit that leaves the repo frozen prints how to
// follow it up: the /gh/ ref answers "moved" now, so re-running this command
// cannot reach the detach.
//
// update and stop drive the spinner the real call started; it is stopped
// before anything is rendered.
func finishDetach(cmd *cobra.Command, c detachStateGetter, ref mirrorRepoRef, repoID, projectID string, res *coreapi.DetachRepoResult, opts mirrorDetachOptions, update func(string), stop func(bool)) error {
	errW := cmd.ErrOrStderr()
	var (
		state   *coreapi.RepoDetachState // nil without a state read
		waitErr error
	)
	if !opts.noWait && detachUnfinished(res.Status.Or("")) {
		update("Moving the repository")
		state, waitErr = awaitDetach(cmd.Context(), c, repoID, opts.timeout)
		if state != nil {
			// Report where the detach ended, not where it started.
			res.Status = coreapi.NewOptString(state.Status)
			if state.Status == detachStatusComplete {
				res.StatusUrl = coreapi.OptString{}
			}
		}
	}

	// Cleared rather than ticked: the result below says how it ended.
	stop(false)
	if jsonRequested(cmd) {
		if err := printJSON(cmd.OutOrStdout(), res); err != nil {
			return err
		}
	} else {
		renderDetachResult(cmd.OutOrStdout(), ref, res, state)
	}

	status := res.Status.Or("")
	if waitErr == nil && status == detachStatusComplete {
		return nil
	}
	if errors.Is(waitErr, errDetachNotRecorded) {
		// Nothing to follow up: the server contradicts the detach's own answer.
		return fmt.Errorf("the detach of %s answered %s, but %w", ref.qualified(), strconv.Quote(status), errDetachNotRecorded)
	}
	if state != nil && status == detachStatusStalled && !state.Resumable {
		fmt.Fprintln(errW, detachResumeHint(repoID, projectID))
		return fmt.Errorf("the detach of %s stalled and needs an admin of the target project to resume it", ref.qualified())
	}
	fmt.Fprintln(errW, detachFollowUpHint(repoID))
	switch {
	case waitErr != nil:
		var silent *SilentError
		if errors.As(waitErr, &silent) {
			return waitErr // an interruption: main re-raises the signal
		}
		// Rendered here: runCore's renderCoreError keeps only an API problem's
		// detail, which would drop that the detach ran.
		return fmt.Errorf("%s; the detach of %s carries on on the server", renderCoreError(waitErr).Error(), ref.qualified())
	case detachUnfinished(status):
		return nil // --no-wait
	default:
		return fmt.Errorf("the detach of %s answered an unexpected status %s", ref.qualified(), strconv.Quote(status))
	}
}

// detachRefusedOutright reports a 4xx answer to the real call: the server
// refused before changing anything. Anything else leaves it unknown.
func detachRefusedOutright(err error) bool {
	var se *coreapi.ErrorModelStatusCode
	return errors.As(err, &se) && se.StatusCode >= 400 && se.StatusCode < 500
}

func detachFollowUpHint(repoID string) string {
	return "Follow the detach with: entire api /api/v1/repos/" + repoID + "/detach"
}

func detachResumeHint(repoID, projectID string) string {
	return "An admin of the target project can resume it with: entire api -X POST /api/v1/repos/" + repoID +
		"/detach -f targetProject=" + projectID + " -F dryRun=false"
}

func detachUnfinished(status string) bool {
	return status == detachStatusInProgress || status == detachStatusStalled
}

// errDetachNotRecorded is a state read contradicting the detach's own answer.
var errDetachNotRecorded = errors.New("the server reports no detach recorded")

// detachStateGetter is the one call awaitDetach makes, so a test can script it.
type detachStateGetter interface {
	GetRepoDetach(ctx context.Context, params coreapi.GetRepoDetachParams) (*coreapi.RepoDetachState, error)
}

// awaitDetach polls until the rewire completes or stalls in a way the server
// will not resume on its own (a resumable stall is picked up by core's sweep).
// It returns the last state read, also on a timeout.
func awaitDetach(ctx context.Context, c detachStateGetter, repoID string, timeout time.Duration) (*coreapi.RepoDetachState, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(mirrorPollInterval)
	defer ticker.Stop()

	var last *coreapi.RepoDetachState
	var consecutiveErrs int
	for {
		state, err := c.GetRepoDetach(ctx, coreapi.GetRepoDetachParams{RepoId: repoID})
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return last, classifyWaitContextErr(ctx.Err(), "waiting for the detach")
			}
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutivePollErrors {
				return last, fmt.Errorf("poll detach status: %w", err)
			}
		case state.Status == "none":
			return last, errDetachNotRecorded
		default:
			consecutiveErrs = 0
			last = state
			switch state.Status {
			case detachStatusInProgress:
			case detachStatusStalled:
				if !state.Resumable {
					return state, nil
				}
			default:
				// complete, or a status this client does not know: the caller decides.
				return state, nil
			}
		}
		select {
		case <-ctx.Done():
			return last, classifyWaitContextErr(ctx.Err(), "waiting for the detach")
		case <-ticker.C:
		}
	}
}

// resolveDetachRepoID finds the mirror's placement ID, which the detach route
// is keyed by. With several placements the first is sent anyway, so the
// server's single-placement precondition explains the refusal.
func resolveDetachRepoID(ctx context.Context, c *coreapi.Client, ref mirrorRepoRef) (string, error) {
	placements, err := resolvePullablePlacements(ctx, c, ref.owner, ref.repo)
	if err != nil {
		return "", err
	}
	if len(placements) == 0 {
		return "", fmt.Errorf("%s is not mirrored on any cluster you can read; see `entire repo mirror list`", ref.qualified())
	}
	return placements[0].MirrorId, nil
}

// detachConfirmed is the seam the confirmation sits behind, as revokeConfirmed
// is for grants. The plan is written on the prompt's own writer, ahead of the
// form; the title repeats the consequences because huh's accessible mode
// renders nothing else.
var detachConfirmed = func(cmd *cobra.Command, ref mirrorRepoRef, target string, plan *coreapi.DetachRepoResult) (bool, error) {
	return confirmPrompt(cmd, "Detach", detachConfirmTitle(ref, target), "", func(w io.Writer) error {
		if err := writeDetachPlan(w, ref, target, plan); err != nil {
			return err
		}
		fmt.Fprintln(w)
		return nil
	})
}

func detachConfirmTitle(ref mirrorRepoRef, target string) string {
	return fmt.Sprintf("Detach %s into %s? Writes freeze until the rewire finishes, and afterwards only the target project grants member access.", ref.qualified(), target)
}

func failedDetachPreconditions(plan *coreapi.DetachRepoResult) []string {
	var failed []string
	for _, p := range plan.Preconditions {
		if !p.Passed {
			failed = append(failed, p.Precondition)
		}
	}
	return failed
}

var detachPreconditionColumns = []string{"PRECONDITION", "RESULT", "DETAIL"}

func detachPreconditionRow(p coreapi.DetachPrecondition) []string {
	result := "fail"
	if p.Passed {
		result = "pass"
	}
	return []string{p.Precondition, result, p.Detail.Or("")}
}

// writeDetachPlan prints the preconditions and whether the detach can run.
func writeDetachPlan(w io.Writer, ref mirrorRepoRef, target string, plan *coreapi.DetachRepoResult) error {
	fmt.Fprintf(w, "Detach plan: %s → %s\n\n", ref.qualified(), target)
	if err := printTable(w, detachPreconditionColumns, plan.Preconditions, detachPreconditionRow); err != nil {
		return err
	}
	fmt.Fprintln(w)
	if plan.Eligible {
		fmt.Fprintln(w, "Eligible. After the detach, only the target project grants member access.")
		return nil
	}
	failed := failedDetachPreconditions(plan)
	fmt.Fprintf(w, "Not eligible: %d %s failed.\n", len(failed), pluralize("precondition", len(failed)))
	return nil
}

// slashPath spells a wire address ("et/acme/web") the way refs are typed.
func slashPath(s string) string {
	return "/" + strings.TrimPrefix(s, "/")
}

// renderDetachResult prints a real detach's answer. Every status but complete
// leaves the repository frozen, so each says so. state is the last state read,
// nil when none was made: only it says whether a stall resumes on its own.
func renderDetachResult(w io.Writer, ref mirrorRepoRef, res *coreapi.DetachRepoResult, state *coreapi.RepoDetachState) {
	native := ref.qualified()
	if name := res.NativeName.Or(""); name != "" {
		native = slashPath(name)
	}
	switch status := res.Status.Or(""); status {
	case detachStatusComplete:
		fmt.Fprintf(w, "✓ Detached %s into %s\n", ref.qualified(), native)
	case detachStatusInProgress:
		fmt.Fprintf(w, "Detach of %s is in progress: it is now %s, and writes stay frozen until the rewire finishes.\n", ref.qualified(), native)
	case detachStatusStalled:
		resumer := "the rewire is resumed"
		if state != nil && state.Resumable {
			resumer = "the server's sweep resumes the rewire"
		} else if state != nil {
			resumer = "an admin of the target project resumes the rewire"
		}
		fmt.Fprintf(w, "Detach of %s stalled: it is now %s, and writes stay frozen until %s.\n", ref.qualified(), native, resumer)
	default:
		fmt.Fprintf(w, "Detach of %s answered status %s; %s is its native address.\n", ref.qualified(), strconv.Quote(status), native)
	}
	if len(res.ReleasedAddresses) > 0 {
		released := make([]string, len(res.ReleasedAddresses))
		for i, a := range res.ReleasedAddresses {
			released[i] = slashPath(a)
		}
		fmt.Fprintf(w, "Released: %s\n", strings.Join(released, ", "))
	}
	for _, n := range res.Notices {
		fmt.Fprintf(w, "Note: %s\n", n)
	}
}
