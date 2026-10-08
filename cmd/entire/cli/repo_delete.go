package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// repoDeleteOptions are the cascade flags of `repo delete`.
type repoDeleteOptions struct {
	cascade     bool
	noWait      bool
	waitTimeout time.Duration
}

func newRepoDeleteCmd() *cobra.Command {
	var (
		project string
		opts    repoDeleteOptions
	)
	cmd := &cobra.Command{
		Use:   "delete <repo>",
		Short: "Delete a repository by /et/<project>/<repo> path or name",
		Long: "Delete a repository.\n\n" +
			"A repo with mirrors on other clusters (see `entire repo mirror add`) " +
			"is refused until its mirrors are removed. Pass --cascade to delete the repo " +
			"and every mirror in one command: the server removes the mirrors first, and " +
			"the command waits until the repo is gone. --no-wait returns as soon as the " +
			"server accepts the request.",
		Example: "  entire repo delete /et/acme/web\n" +
			"  entire repo delete /et/acme/web --cascade\n" +
			"  entire repo delete /et/acme/web --cascade --no-wait",
		Args: cobra.ExactArgs(1),
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if opts.noWait && !opts.cascade {
				return errors.New("--no-wait requires --cascade")
			}
			if opts.waitTimeout <= 0 {
				return errors.New("--wait-timeout must be positive")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoDelete(cmd, args[0], project, opts)
		},
	}
	bindRepoProjectFlag(cmd, &project)
	addForceFlag(cmd)
	cmd.Flags().BoolVar(&opts.cascade, "cascade", false, "Also delete the repo's mirrors on other clusters")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "Return once the server accepts a cascade delete")
	cmd.Flags().DurationVar(&opts.waitTimeout, "wait-timeout", 10*time.Minute, "Time limit for a cascade delete to finish")
	return cmd
}

// runRepoDelete is `repo delete`. It is not runControlPlaneDelete because a
// cascade can answer 202: the server then owns completion, and the command
// polls until the repo is gone instead of reporting a finished delete.
func runRepoDelete(cmd *cobra.Command, ref, project string, opts repoDeleteOptions) error {
	force := forceRequested(cmd)
	out := cmd.OutOrStdout()
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		resolved, err := resolveRepoRefResolved(ctx, c, ref, project)
		if err != nil {
			return err
		}
		name := repoDeleteName(ref, resolved)
		label := "repo " + name
		mirrors := ""
		if opts.cascade {
			mirrors = mirrorsSuffix(ctx, c, resolved.ID)
		}
		proceed, err := confirmControlPlaneDeletion(ctx, out, label+mirrors, force, interactive.CanPromptInteractively())
		if err != nil || !proceed {
			return err
		}
		params := coreapi.DeleteRepoParams{RepoId: resolved.ID}
		if opts.cascade {
			params.Cascade = coreapi.NewOptBool(true)
		}
		res, err := c.DeleteRepo(ctx, params)
		switch {
		case isCoreNotFound(err):
			fmt.Fprintf(out, "%s not found; nothing to delete\n", label)
			return nil
		case err != nil:
			return explainMirrorConflict(err, label, opts.cascade)
		}
		if _, accepted := res.(*coreapi.DeleteRepoAccepted); !accepted {
			fmt.Fprintf(out, "✓ Deleted %s\n", label)
			return nil
		}
		if opts.noWait {
			fmt.Fprintf(out, "Deleting %s%s in the background.\n", label, mirrors)
			return nil
		}
		fmt.Fprintf(out, "Deleting %s%s…\n", label, mirrors)
		waitCtx, cancel := context.WithTimeout(ctx, opts.waitTimeout)
		defer cancel()
		if err := awaitRepoDeleted(waitCtx, c, resolved.ID); err != nil {
			return reportUnfinishedDelete(cmd.ErrOrStderr(), label+mirrors, repoCheckCommand(name, project), opts.waitTimeout, err)
		}
		fmt.Fprintf(out, "✓ Deleted %s\n", label)
		return nil
	})
}

// repoDeleteName is how `repo delete` names the repo: the path the server
// resolved, else the ref as typed. Unlike resolvedRefLabel it never appends
// the ULID; a repo is addressed publicly by its path.
func repoDeleteName(ref string, r resolvedRef) string {
	if r.Name != "" {
		return r.Name
	}
	return strings.TrimSpace(ref)
}

// reportUnfinishedDelete explains a wait that ended early. check is empty
// when there is no command to suggest.
func reportUnfinishedDelete(w io.Writer, what, check string, timeout time.Duration, err error) error {
	fmt.Fprintf(w, "The server is still deleting %s.\n", what)
	if check != "" {
		fmt.Fprintf(w, "Check with: %s\n", check)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("stopped waiting after %s (--wait-timeout)", timeout)
	}
	return err
}

// repoCheckCommand suggests how to follow a delete the wait stopped watching.
// `repo view` takes only /et/<project>/<repo> paths. name is usually the
// server-resolved path, but a repo found by bare name can lack one (a project
// ULID routes through the project listing, whose answer may carry no path);
// the --project listing then shows it instead. A ULID ref with neither gets no
// suggestion.
func repoCheckCommand(name, project string) string {
	if strings.HasPrefix(name, "/"+nativeCloneForge+"/") {
		return "entire repo view " + name
	}
	if project = strings.TrimSpace(project); project != "" {
		return "entire repo list --project " + project
	}
	return ""
}

// mirrorsSuffix names the mirrors a cascade removes, for the prompt and the
// progress line. Best-effort: the server decides what the cascade removes,
// so a failed count degrades the wording rather than the command.
// Mirrors already being removed are not counted.
func mirrorsSuffix(ctx context.Context, c *coreapi.Client, repoID string) string {
	mirrors, err := listNativeMirrors(ctx, c, repoID)
	if err != nil {
		return " and any mirrors"
	}
	n := 0
	for _, m := range mirrors {
		if m.DesiredState != coreapi.NativeMirrorPlacementDesiredStateDeleted {
			n++
		}
	}
	switch n {
	case 0:
		return ""
	case 1:
		return " and its mirror"
	default:
		return fmt.Sprintf(" and its %d mirrors", n)
	}
}

// explainMirrorConflict rewords the one refusal whose remedy is a flag of this
// command: the server declines to delete a repo while its mirrors exist. The
// server's detail speaks of native mirrors and primaries, so the CLI says it
// in its own words. Under --cascade the same refusal means the server did not
// take the cascade (one deployed before it existed answers this way).
// The message rides on a plain error: renderCoreError replaces a wrapped
// *ErrorModelStatusCode with the server's detail alone, which would drop it
// (see renderNativeMirrorCreateError).
func explainMirrorConflict(err error, label string, cascade bool) error {
	var problem *coreapi.ErrorModelStatusCode
	if !errors.As(err, &problem) || problem.StatusCode != http.StatusConflict {
		return err
	}
	if !strings.Contains(strings.ToLower(coreapi.APIError(err)), "native mirror") {
		return err
	}
	if cascade {
		return fmt.Errorf("%s has mirrors on other clusters and the server did not delete them with --cascade; remove them with `entire repo mirror remove`, then delete the repo", label)
	}
	return fmt.Errorf("%s has mirrors on other clusters; add --cascade to delete them too", label)
}

// awaitRepoDeleted polls until the repo read answers 404. A 202 hands
// completion to the server, so every 200 keeps the wait going whatever
// state it reports: the server removes the row once the last mirror is gone.
// A stale read costs one more poll.
func awaitRepoDeleted(ctx context.Context, c repoLifecycleGetter, repoID string) error {
	ticker := time.NewTicker(mirrorPollInterval)
	defer ticker.Stop()

	var consecutiveErrs int
	for {
		_, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
		switch {
		case isCoreNotFound(err):
			return nil
		case err != nil:
			if ctx.Err() != nil {
				return classifyWaitContextErr(ctx.Err(), "waiting for the repository to be deleted")
			}
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutivePollErrors {
				return fmt.Errorf("poll repository: %w", err)
			}
		default:
			consecutiveErrs = 0
		}
		select {
		case <-ctx.Done():
			return classifyWaitContextErr(ctx.Err(), "waiting for the repository to be deleted")
		case <-ticker.C:
		}
	}
}
