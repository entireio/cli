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
// the published branch with --checkout, or names the switch command otherwise.
// previous is the branch checked out before publishing ("" when detached).
// Git itself refuses a switch that would overwrite local changes; the trail
// already exists then, so the error says so instead of suggesting a rerun.
func finishLocalTrailBranch(cmd *cobra.Command, created api.ProjectTrail, branch, previous string, checkout bool) error {
	if branch == "" || branch == previous {
		return nil
	}
	display := tuiutil.SanitizeTerminalText(branch)
	if !checkout {
		fmt.Fprintf(cmd.ErrOrStderr(), "Next: git switch %s\n", display)
		return nil
	}
	if err := CheckoutBranch(cmd.Context(), branch); err != nil {
		return fmt.Errorf("trail #%d was created; %w; run `git switch %s`", created.Number, err, display)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Switched to branch %s\n", display)
	return nil
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
