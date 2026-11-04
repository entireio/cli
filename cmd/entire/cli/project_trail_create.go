package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/internal/coreapi"
)

// publishesLocalBranch reports whether create pushes a branch from this clone,
// the only case where there is a local branch to check out afterwards.
func publishesLocalBranch(cmd *cobra.Command, change api.ChangeCreateRequest) bool {
	return change.BranchName != "" && change.BranchAction == trailBranchActionLink && trailRepoFlag(cmd) == ""
}

func createRepositoryScopedProjectTrail(cmd *cobra.Command, request api.ProjectTrailCreateRequest, change api.ChangeCreateRequest, key string) (api.ProjectTrail, error) {
	target, repoID, err := resolveRepositoryProjectTrailTarget(cmd)
	if err != nil {
		return api.ProjectTrail{}, err
	}
	request.RepositoryIDs = []string{repoID}
	change.RepositoryID = repoID
	if publishesLocalBranch(cmd, change) {
		if err := publishProjectTrailBranch(cmd, change.BranchName); err != nil {
			return api.ProjectTrail{}, err
		}
	}
	path := target.BasePath + "?" + url.Values{"project_id": {target.ProjectID}}.Encode()
	var headers http.Header
	if change.BranchName == "" && key != "" {
		headers = http.Header{"Idempotency-Key": {projectTrailIdempotencyKey(cmd.ErrOrStderr(), key)}}
	}
	var out api.ProjectTrail
	etag, err := target.Client.ProjectTrailRequest(cmd.Context(), http.MethodPost, path, request, headers, &out)
	if err != nil {
		if headers != nil {
			return api.ProjectTrail{}, fmt.Errorf("create repository-scoped trail (retry with --idempotency-key %s and the same fields): %w", key, err)
		}
		return api.ProjectTrail{}, fmt.Errorf("create repository-scoped trail (inspect trails before retrying; creation may have succeeded): %w", err)
	}
	if err := target.validateResponse(out); err != nil {
		return api.ProjectTrail{}, err
	}
	if change.BranchName != "" {
		target.TrailID, target.Number = out.ID, out.Number
		fmt.Fprintf(cmd.ErrOrStderr(), "Created trail #%d (%s); linking branch\n", out.Number, out.ID)
		key = projectTrailIdempotencyKey(cmd.ErrOrStderr(), key)
		if _, err := linkProjectTrailChange(cmd.Context(), target, change, etag, key); err != nil {
			return api.ProjectTrail{}, fmt.Errorf("trail %s was created but branch linking did not complete: %w; do not rerun trail create\n%s", out.ID, err, projectTrailLinkRetry(cmd, target, change, key))
		}
		out, _, err = target.read(cmd.Context())
		if err != nil {
			return api.ProjectTrail{}, fmt.Errorf("trail %s was created and branch linked, but reading the result failed: %w; do not rerun trail create", target.TrailID, err)
		}
	}
	return out, nil
}

// finishLocalTrailBranch runs after the created trail is printed. It switches to
// the published branch with --checkout, or names how to reach it otherwise.
// previous is the branch checked out before publishing ("" when detached).
// Git itself refuses a switch that would overwrite local changes; the trail
// already exists then, so the error says so instead of suggesting a rerun.
func finishLocalTrailBranch(cmd *cobra.Command, created api.ProjectTrail, branch, previous string, checkout bool) error {
	if branch == "" || branch == previous {
		return nil
	}
	ctx := cmd.Context()
	if !checkout {
		fmt.Fprintf(cmd.ErrOrStderr(), "Next: %s\n", trailBranchNextStep(ctx, branch))
		return nil
	}
	shown := tuiutil.SanitizeTerminalText(branch)
	if err := CheckoutBranch(ctx, branch); err != nil {
		// A failing post-checkout hook makes git exit non-zero after HEAD moved;
		// telling the user to switch again would be wrong.
		if now, headErr := GetCurrentBranch(ctx); headErr == nil && now == branch {
			return fmt.Errorf("trail #%d was created and switched to %s, but git reported an error: %w; do not rerun trail create", created.Number, shown, err)
		}
		return fmt.Errorf("trail #%d was created; %w; %s; do not rerun trail create", created.Number, err, gitSwitchInstruction(branch))
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Switched to branch %s\n", shown)
	return nil
}

// refuseCheckoutHeldElsewhere stops --checkout before anything is created:
// git refuses to check out a branch another worktree holds.
func refuseCheckoutHeldElsewhere(ctx context.Context, branch string) error {
	path, ok := branchCheckedOutElsewhere(ctx, branch)
	if !ok {
		return nil
	}
	return fmt.Errorf("cannot --checkout %s: it is checked out in worktree %s; nothing was created. Rerun without --checkout and continue in that worktree",
		tuiutil.SanitizeTerminalText(branch), tuiutil.SanitizeTerminalText(path))
}

// trailBranchNextStep names how to reach branch: the worktree that already has
// it checked out (git switch would refuse), or the switch command.
func trailBranchNextStep(ctx context.Context, branch string) string {
	if path, ok := branchCheckedOutElsewhere(ctx, branch); ok {
		return fmt.Sprintf("continue in worktree %s, where %s is checked out", tuiutil.SanitizeTerminalText(path), tuiutil.SanitizeTerminalText(branch))
	}
	if command, ok := gitSwitchCommand(branch); ok {
		return command
	}
	return hiddenBranchSwitchHint
}

// hiddenBranchSwitchHint replaces a switch command whose branch name would not
// survive display sanitizing: a command built from the sanitized name would
// target a different branch, or one that does not exist.
const hiddenBranchSwitchHint = "switch to the new branch with git switch; its name contains characters that are not shown"

// gitSwitchInstruction is the sentence an error ends with to send the user to branch.
func gitSwitchInstruction(branch string) string {
	if command, ok := gitSwitchCommand(branch); ok {
		return "run `" + command + "`"
	}
	return hiddenBranchSwitchHint
}

// gitSwitchCommand is the copy-paste command that switches to branch. Git
// allows shell metacharacters in branch names, so anything beyond a plain name
// is quoted for the user's shell, as the link retry hint does. It reports false
// when display sanitizing would change the name, so no command is runnable.
func gitSwitchCommand(branch string) (string, bool) {
	if tuiutil.SanitizeTerminalText(branch) != branch {
		return "", false
	}
	// A plain name stays unquoted so the common hint reads naturally.
	if strings.Trim(branch, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._/-") != "" {
		branch = quoteForUserShell(branch)
	}
	return "git switch " + branch, true
}

// Repository access supplies placement and ownership without project inspection.
func resolveRepositoryProjectTrailTarget(cmd *cobra.Command) (*projectTrailTarget, string, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), requiredCellResolveTimeout)
	defer cancel()
	forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, trailRepoFlag(cmd))
	if err != nil {
		return nil, "", err
	}
	host, project, err := trailProjectReferenceOr(cmd, forge, owner)
	if err != nil {
		return nil, "", err
	}
	if host != forge || !strings.EqualFold(project, owner) {
		return nil, "", errors.New("--project must match the scoped repository's namespace")
	}
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), forge, owner, repo)
	if err != nil {
		return nil, "", renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), owner+"/"+repo, err)
	}
	core, err := newCellCoreClient()
	if err != nil {
		return nil, "", renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), owner+"/"+repo, err)
	}
	repository, err := core.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
	if err != nil {
		return nil, "", renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), owner+"/"+repo, fmt.Errorf("resolve repository ownership: %w", err))
	}
	if repository == nil || repository.Response.ID != repoID || !looksLikeULID(repoID) {
		return nil, "", errors.New("repository response identity does not match the request")
	}
	target, err := projectTrailTargetForReference(api.TrailParentReference{
		ProjectID: repository.Response.OwningProjectId, Host: host, Project: project,
	})
	if err != nil {
		return nil, "", err
	}
	target.Client = client
	target.RepoRef = forge + "/" + owner + "/" + repo
	return target, repoID, nil
}
