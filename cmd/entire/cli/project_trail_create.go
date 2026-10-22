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
	"github.com/entireio/cli/internal/coreapi"
)

func createRepositoryScopedProjectTrail(cmd *cobra.Command, request api.ProjectTrailCreateRequest, change api.ChangeCreateRequest, key string) error {
	target, repoID, err := resolveRepositoryProjectTrailTarget(cmd)
	if err != nil {
		return err
	}
	request.RepositoryIDs = []string{repoID}
	change.RepositoryID = repoID
	if change.BranchName != "" && change.BranchAction == trailBranchActionLink && trailRepoFlag(cmd) == "" {
		if err := publishProjectTrailBranch(cmd, change.BranchName); err != nil {
			return err
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
			return fmt.Errorf("create repository-scoped trail (retry with --idempotency-key %s and the same fields): %w", key, err)
		}
		return fmt.Errorf("create repository-scoped trail (inspect trails before retrying; creation may have succeeded): %w", err)
	}
	if err := target.validateResponse(out); err != nil {
		return err
	}
	if change.BranchName != "" {
		target.TrailID, target.Number = out.ID, out.Number
		fmt.Fprintf(cmd.ErrOrStderr(), "Created trail #%d (%s); linking branch\n", out.Number, out.ID)
		key = projectTrailIdempotencyKey(cmd.ErrOrStderr(), key)
		if _, err := linkProjectTrailChange(cmd.Context(), target, change, etag, key); err != nil {
			return fmt.Errorf("trail %s was created but branch linking did not complete: %w; do not rerun trail create\n%s", out.ID, err, projectTrailLinkRetry(cmd, target, change, key))
		}
		out, _, err = target.read(cmd.Context())
		if err != nil {
			return fmt.Errorf("trail %s was created and branch linked, but reading the result failed: %w; do not rerun trail create", target.TrailID, err)
		}
	}
	return printProjectTrail(cmd, out)
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
