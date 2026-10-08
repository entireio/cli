package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// repoCreateRequest is everything `repo create` sends, however it was
// gathered: from the command line in one shot, or from the wizard. An empty
// objectFormat or visibility leaves that setting to the server's default.
type repoCreateRequest struct {
	projectID string
	// projectName is the server's name for the project, used for display and
	// to spell the /et/<project>/<repo> ref in the next steps. Empty when the
	// project was given as a ULID and never looked up.
	projectName  string
	name         string
	objectFormat coreapi.CreateRepoInputBodyObjectFormat
	visibility   coreapi.SetRepoVisibilityInputBodyVisibility
}

// repoVisibilityGrace bounds the visibility write when --wait-timeout was
// spent on the readiness wait.
const repoVisibilityGrace = 30 * time.Second

// repoCreateOptions are the readiness settings shared by both entry points.
type repoCreateOptions struct {
	noWait      bool
	waitTimeout time.Duration
}

// refuseGitSuffixRepoName refuses a name that ends in `.git`, whatever its
// case. The suffix is never part of a repo name (see gitDirSuffix): every ref
// parser drops it, so the name would round-trip to a different string than
// the one typed. The server refuses it too; saying so here costs no round trip
// and names the spelling to use instead, when there is one the server would
// accept (suggestRepoName). An empty name passes: it is a missing one.
func refuseGitSuffixRepoName(name string) error {
	rest, had := gitremote.CutGitDirSuffix(name)
	if !had {
		return nil
	}
	err := fmt.Errorf("repo name %q must not end in %s, in any case: the suffix is never part of a repo name, so Entire could not address the repo by the name you typed", name, gitDirSuffix)
	if use, ok := suggestRepoName(rest); ok {
		err = fmt.Errorf("%w (use %q)", err, use)
	}
	return err
}

// errRepoCreateNeedsInput refuses a run that cannot be prompted and was not
// given everything a create needs, naming the flag form instead. It is the
// tail of the message repoCreateMissingInput builds.
var errRepoCreateNeedsInput = errors.New("required without an interactive terminal: " +
	"entire repo create <name> --project <project> ('entire project list' shows project names)")

// errRepoCreateFlagsNeedInput refuses a create flag given with an input
// missing. The wizard takes only the positional name, so a flag would be
// silently dropped; flags mean the flag form. Worded as `project create`
// words its own.
var errRepoCreateFlagsNeedInput = errors.New("required when create flags are given: " +
	"entire repo create <name> --project <project> (or, in a terminal and without flags, run 'entire repo create' or 'entire repo create <name>' to be asked)")

// repoCreateMissingInput names what is missing ahead of the refusal reason,
// so `--project acme` alone is not told that --project is required.
func repoCreateMissingInput(name, projectRef string, reason error) error {
	var missing string
	switch {
	case name == "" && projectRef == "":
		missing = "a repository name and --project are"
	case name == "":
		missing = "a repository name is"
	default:
		missing = "--project is"
	}
	return fmt.Errorf("%s %w", missing, reason)
}

// The flags that describe the repo itself. Readiness and output flags
// (--no-wait, --wait-timeout, --json) apply to the wizard too.
const (
	repoCreateFlagVisibility   = "visibility"
	repoCreateFlagObjectFormat = "object-format"
)

var repoCreateFlags = []string{projectFlagName, repoCreateFlagVisibility, repoCreateFlagObjectFormat}

// repoCreateFlagsGiven reports whether any create flag was passed, even with
// an empty value.
func repoCreateFlagsGiven(cmd *cobra.Command) bool {
	return slices.ContainsFunc(repoCreateFlags, cmd.Flags().Changed)
}

// createRepo sends the create request and returns the created repo in the
// shape the readiness wait and the report share.
func createRepo(ctx context.Context, c *coreapi.Client, req repoCreateRequest) (*coreapi.Repo, error) {
	body := &coreapi.CreateRepoInputBody{Name: req.name, ProjectId: req.projectID}
	if req.objectFormat != "" {
		body.ObjectFormat = coreapi.NewOptCreateRepoInputBodyObjectFormat(req.objectFormat)
	}
	response, err := c.CreateRepo(ctx, body)
	if err != nil {
		return nil, err
	}
	return createdRepoAsRepo(&response.Response)
}

// isRepoCreateConflict reports the server refusing a create with a 409. A
// name taken in the project is the expected cause, but the endpoint does not
// document it as the only one, so callers show the server's reason.
func isRepoCreateConflict(err error) bool {
	var se *coreapi.ErrorModelStatusCode
	return errors.As(err, &se) && se.StatusCode == http.StatusConflict
}

// isRepoCreateRefusal reports the server refusing a create over what was
// asked for — a conflict, or a name or setting it judged invalid — as opposed
// to an auth, routing or server failure. The wizard can send the user back to
// change the answer for these.
func isRepoCreateRefusal(err error) bool {
	var se *coreapi.ErrorModelStatusCode
	if !errors.As(err, &se) {
		return false
	}
	switch se.StatusCode {
	case http.StatusConflict, http.StatusBadRequest, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// finishRepoCreate runs everything after a successful create: the readiness
// wait, the visibility change, and the report. The repo exists by now, so
// every failure from here on is reported with the repository preserved rather
// than as a failed create — creating again is never the recovery.
//
// Visibility is set after the wait, or straight away with --no-wait: core
// accepts it on a provisioning repo (its records exist once the create
// returns, and the visibility write does not look at the repo's state).
func finishRepoCreate(ctx context.Context, cmd *cobra.Command, c *coreapi.Client, req repoCreateRequest, created *coreapi.Repo, opts repoCreateOptions) error {
	var waitErr error
	if !opts.noWait {
		var finish func(bool)
		waitErr = awaitRepoActive(ctx, c, created, func() {
			finish = startSpinner(cmd.ErrOrStderr(), "Waiting for repository "+created.Name+" to become active")
		})
		if finish != nil {
			finish(waitErr == nil)
		}
	}
	// A wait that ran out --wait-timeout leaves ctx expired, and what is
	// still owed on a repo that exists (its name for the output, the
	// visibility asked for) gets its own short budget. Only the deadline earns
	// that — an interrupted command (Ctrl+C) stays interrupted, since the
	// budget derives from the command's own context.
	visCtx := ctx
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		var cancel context.CancelFunc
		visCtx, cancel = context.WithTimeout(cmd.Context(), repoVisibilityGrace)
		defer cancel()
	}
	ref := repoCreateRef(created, req.projectName)
	// --json on a confirmed repo prints only the wire object, which names
	// nothing the lookup would add; every other outcome prints the repo's
	// name or a command that needs it.
	outputNamesRepo := !jsonRequested(cmd) || waitErr != nil || opts.noWait
	if ref == "" && req.projectName == "" && outputNamesRepo {
		// Nothing names the repo: the project was given as a ULID and the
		// server returned neither a full name nor an /et/ path. One lookup
		// names the project, only in this case, so the common path costs no
		// extra request.
		req.projectName = lookupRepoCreateProjectName(visCtx, c, req.projectID)
		ref = repoCreateRef(created, req.projectName)
	}
	// The visibility is applied before the report, so the report (and
	// --json) shows what now holds, but explained after it: the last thing on
	// screen must be the problem the nonzero exit is about, not a success
	// line.
	var visErr error
	visSkipped := false
	switch {
	case waitErr != nil && !errors.Is(waitErr, context.DeadlineExceeded) && req.visibility != "":
		// Provisioning failed, readiness could not be read, or the command
		// was interrupted: setting visibility now would pile a second error
		// onto a repo that may never become usable. Say how to finish once it
		// is active instead.
		visSkipped = true
	default:
		visErr = applyRepoVisibility(visCtx, c, created, req.visibility)
	}
	reportErr := reportRepoCreation(cmd, created, ref, opts.noWait, waitErr)
	finish := shellArg(cmp.Or(ref, created.ID))
	if visSkipped {
		fmt.Fprintf(cmd.ErrOrStderr(), "Visibility was not set. Once the repository is active, set it with: entire repo edit %s --visibility %s\n",
			finish, req.visibility)
	}
	if visErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "The repository was created, but setting its visibility to %s failed: %v\nSet it with: entire repo edit %s --visibility %s\n",
			req.visibility, renderCoreError(visErr), finish, req.visibility)
		return NewSilentError(errors.Join(visErr, reportErr))
	}
	if reportErr == nil && !jsonRequested(cmd) {
		printRepoCreateNextSteps(cmd.OutOrStdout(), ref)
	}
	return reportErr
}

// lookupRepoCreateProjectName is the project's name for the output, or ""
// when it cannot be had: the output then falls back to the repo ID rather than
// failing a create that succeeded.
func lookupRepoCreateProjectName(ctx context.Context, c *coreapi.Client, projectID string) string {
	if projectID == "" || ctx.Err() != nil {
		return ""
	}
	p, err := c.GetProject(ctx, coreapi.GetProjectParams{ProjectId: projectID})
	if err != nil {
		logging.Debug(ctx, "repo create: could not name the project for the output", "error", err.Error())
		return ""
	}
	return p.Name
}

// applyRepoVisibility sets the requested visibility on a freshly created repo.
// The create endpoint takes no visibility, so this is a second call. It is
// skipped when nothing was asked for, or when the create response already
// reports the requested value — the common case of asking for private on a
// server that defaults to it. On success the repo's visibility is updated so
// the report (and --json) shows what now holds.
func applyRepoVisibility(ctx context.Context, c *coreapi.Client, created *coreapi.Repo, vis coreapi.SetRepoVisibilityInputBodyVisibility) error {
	if vis == "" {
		return nil
	}
	if current, ok := created.Visibility.Get(); ok && current == string(vis) {
		return nil
	}
	out, err := c.SetRepoVisibility(ctx, &coreapi.SetRepoVisibilityInputBody{Visibility: vis}, coreapi.SetRepoVisibilityParams{RepoId: created.ID})
	if err != nil {
		return fmt.Errorf("set repository visibility: %w", err)
	}
	created.Visibility = coreapi.NewOptString(string(out.Visibility))
	return nil
}

// repoCreateRef is the /et/<project>/<repo> ref the output names the new repo
// by. The server's own spellings win: its full name, else its /et/ path (see
// repoViewRef) — which is how a project given as a ULID still gets named,
// with no extra lookup. Otherwise it is composed from the project name the
// command resolved. Empty when none is known: no next steps are printed then,
// rather than ones that cannot work.
func repoCreateRef(r *coreapi.Repo, projectName string) string {
	if ref := nativeRepoPath(r.FullName.Or("")); ref != "" {
		return ref
	}
	if path := repoViewRef(r); strings.HasPrefix(path, "/"+nativeCloneForge+"/") {
		return path
	}
	if projectName == "" || r.Name == "" {
		return ""
	}
	return "/" + nativeCloneForge + "/" + projectName + "/" + r.Name
}

// printRepoCreateNextSteps prints the commands that put a new repo to use:
// clone it or — the alternative, for an existing checkout — point that
// checkout's origin at it; then, either way, mirror it onto more clusters.
func printRepoCreateNextSteps(w io.Writer, ref string) {
	if ref == "" {
		return
	}
	// Quoted: the ref is server-derived and these lines are pasted into a
	// shell.
	ref = shellArg(ref)
	fmt.Fprintf(w, "\nNext steps\n"+
		"  Clone it:                entire repo clone %[1]s\n"+
		"  Or point origin at it:   entire repo remote add origin %[1]s --override\n"+
		"  Mirror it:               entire repo mirror add %[1]s\n", ref)
}
