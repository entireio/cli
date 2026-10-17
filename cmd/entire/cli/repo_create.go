package cli

import (
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
	"github.com/entireio/cli/internal/coreapi"
)

// repoCreateRequest is everything `repo create` sends, however it was
// gathered: from the command line in one shot, or from the wizard. An empty
// objectFormat or visibility leaves that setting to the server's default.
type repoCreateRequest struct {
	projectID string
	// projectName is the server's name for the project, used for display and
	// to spell the /et/<project>/<repo> ref in the next steps.
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
//
// prefix is what the suggestion is spelled under: "" for a bare name, or the
// /et/<project>/ the user typed it in, so the advice is a ref they can paste.
func refuseGitSuffixRepoName(prefix, name string) error {
	rest, had := gitremote.CutGitDirSuffix(name)
	if !had {
		return nil
	}
	err := fmt.Errorf("repo name %q must not end in %s, in any case: the suffix is never part of a repo name, so Entire could not address the repo by the name you typed", name, gitDirSuffix)
	if use, ok := suggestRepoName(rest); ok {
		err = fmt.Errorf("%w (use %q)", err, prefix+use)
	}
	return err
}

// errRepoCreateNeedsInput refuses a run that cannot be prompted and was not
// given the path a create needs. It is the tail of the message
// repoCreateMissingInput builds.
var errRepoCreateNeedsInput = errors.New("required without an interactive terminal")

// errRepoCreateFlagsNeedInput refuses a create flag given without a path. The
// wizard takes only a positional name to start from, so a flag would be
// silently dropped; flags mean the path form. Worded as `project create` words
// its own.
var errRepoCreateFlagsNeedInput = errors.New("required when create flags are given")

// repoCreatePathShape is the one spelling that creates without the wizard.
var repoCreatePathShape = nativeRepoRef("<project>", "<repo>")

// repoCreateMissingInput refuses a create given no path, naming the path form
// with the typed name already in it, so `entire repo create web` is shown the
// command it was one project away from.
func repoCreateMissingInput(name string, reason error) error {
	want := repoCreatePathShape
	if name != "" {
		// Quoted as the wizard's summary quotes it: the line is pasted into
		// a shell.
		want = nativeRepoRef("<project>", shellArg(name))
	}
	hint := "'entire project list' shows project names"
	if errors.Is(reason, errRepoCreateFlagsNeedInput) {
		hint = "or, in a terminal and without flags, run 'entire repo create' or 'entire repo create <name>' to be asked"
	}
	return fmt.Errorf("a %s path is %w: entire repo create %s (%s)", repoCreatePathShape, reason, want, hint)
}

// parseRepoCreateRef reads the positional of `repo create`. A
// /et/<project>/<repo> path names everything a create needs. A bare name only
// starts the wizard, which asks for the project; it never creates on its own,
// since a repo name is unique only within a project. Anything else with a
// slash, or another forge, is no spelling this verb takes.
//
// A `.git` suffix is refused rather than dropped, as the bare name refuses it:
// the clone parser treats the suffix as an alias of the suffix-free repo, but a
// create would make the repo under the name the user did NOT type. The suffix
// is judged only once the path has its shape — two names after et/, the first
// a project name — so the suggestion it makes is a path that parses;
// `/et/web.git` is told about its missing project instead. The repo name's own
// rules come after it, so a name refused for the suffix hears about the
// suffix first.
func parseRepoCreateRef(ref string) (project, name string, err error) {
	ref = strings.TrimSpace(ref)
	if !declaresForge(ref, nativeCloneForge) {
		if strings.Contains(ref, "/") {
			return "", "", fmt.Errorf("repo %q must be a %s path, or a bare name to start the wizard with", ref, repoCreatePathShape)
		}
		return "", ref, refuseGitSuffixRepoName("", ref)
	}
	if segs := strings.Split(trimRefPrefix(ref), "/"); len(segs) == 3 && nativeProjectRe.MatchString(segs[1]) {
		cut := strings.LastIndex(ref, "/") + 1
		if err := refuseGitSuffixRepoName(ref[:cut], ref[cut:]); err != nil {
			return "", "", err
		}
	}
	project, name, err = parseNativeCloneRef(ref)
	if err != nil {
		return "", "", fmt.Errorf("invalid repo ref %q: %w", ref, err)
	}
	return project, name, nil
}

// The flags that describe the repo itself. Readiness and output flags
// (--no-wait, --wait-timeout, --json) apply to the wizard too.
const (
	repoCreateFlagVisibility   = "visibility"
	repoCreateFlagObjectFormat = "object-format"
)

var repoCreateFlags = []string{repoCreateFlagVisibility, repoCreateFlagObjectFormat}

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
	finish := repoEditVisibilityHint(ref, created.ID, req.visibility)
	if visSkipped {
		fmt.Fprintf(cmd.ErrOrStderr(), "Visibility was not set. Once the repository is active, set it with%s\n", finish)
	}
	if visErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "The repository was created, but setting its visibility to %s failed: %v\nSet it with%s\n",
			req.visibility, renderCoreError(visErr), finish)
		return NewSilentError(errors.Join(visErr, reportErr))
	}
	if reportErr == nil && !jsonRequested(cmd) {
		printRepoCreateNextSteps(cmd.OutOrStdout(), ref)
	}
	return reportErr
}

// repoEditVisibilityHint completes "set it with" with the command that
// finishes a visibility the create could not set. `repo edit` takes only the
// repo's path, so without one there is no command to offer — a ULID would be
// refused — and the ID is named for support instead, as reportRepoCreation
// does.
func repoEditVisibilityHint(ref, id string, vis coreapi.SetRepoVisibilityInputBodyVisibility) string {
	if ref == "" {
		return fmt.Sprintf(" `entire repo edit %s --visibility %s` when the server reports the repository's path (repository ID: %s)", repoCreatePathShape, vis, id)
	}
	return fmt.Sprintf(": entire repo edit %s --visibility %s", shellArg(ref), vis)
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
	return nativeRepoRef(projectName, r.Name)
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
