package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// Branch membership is expressed in repository/branch terms. The backing Change
// IDs stay inside the API adapter and are never accepted as user selectors.
func newTrailLinkCmd() *cobra.Command {
	var fields projectTrailFields
	var branch, base, action, key string
	cmd := &cobra.Command{
		Use: "link <trail>", Short: "Link a repository branch to a trail",
		Long: "Link an existing remote branch to a project trail. --branch defaults to the current branch. Use --branch-action create to create remote branch work from --base. Existing work and reviews are preserved; a branch belonging to another trail is rejected.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := fields.validate(cmd, false); err != nil {
				return err
			}
			if err := requireTrailWorkingTarget(trailRepoFlag(cmd), "", branch); err != nil {
				return err
			}
			var err error
			branch, err = resolveTrailBranch(cmd.Context(), branch)
			if err != nil {
				return err
			}
			if err := validateProjectTrailBranch(cmd, branch, action); err != nil {
				return err
			}
			target, err := resolveProjectTrailWithBranch(cmd, args[0], "")
			if err != nil {
				return err
			}
			parent, etag, err := target.read(cmd.Context())
			if err != nil {
				return err
			}
			if etag == "" {
				return errors.New("trail read returned no ETag; refusing an unprotected link")
			}
			if fields.Title == "" {
				fields.Title = branch // Stable across retries even if the parent's title changes.
			}
			request, err := projectTrailChangeRequest(cmd, fields, branch, base, action)
			if err != nil {
				return err
			}
			key = projectTrailIdempotencyKey(cmd.ErrOrStderr(), key)
			var out api.ChangeCreateResponse
			if _, err := target.Client.ProjectTrailRequest(cmd.Context(), http.MethodPost, target.path()+"/changes", request,
				http.Header{"If-Match": {etag}, "Idempotency-Key": {key}}, &out); err != nil {
				return fmt.Errorf("link branch (retry with --idempotency-key %s and the same fields): %w", key, err)
			}
			if !looksLikeULID(out.ID) || out.TrailID != target.TrailID || out.RepositoryID != request.RepositoryID {
				return errors.New("branch link response identity does not match the request")
			}
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), map[string]string{"trailId": out.TrailID, "repositoryId": out.RepositoryID, "branch": branch})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Linked branch %s to trail #%d\n", tuiutil.SanitizeTerminalLabel(branch), parent.Number)
			return nil
		},
	}
	fields.flags(cmd)
	addProjectTrailCreationFlags(cmd, &branch, &base, &action, &key)
	addJSONFlag(cmd)
	return cmd
}

func newTrailUnlinkCmd() *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use: "unlink [<trail>]", Short: "Unlink a repository branch without deleting its work",
		Long: "Remove the selected repository/branch from a trail. The branch, code, reviews, and sessions are preserved. Ambiguous branch selections require --branch.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selected, err := resolveTrailWorkingContext(cmd, projectTrailSelector(args), branch, false)
			if err != nil {
				return err
			}
			if selected.ETag == "" {
				return errors.New("trail read returned no ETag; refusing an unprotected unlink")
			}
			if _, err := selected.Target.Client.ProjectTrailRequest(cmd.Context(), http.MethodDelete,
				selected.Target.path()+"/changes/"+url.PathEscape(selected.Work.ID), nil,
				http.Header{"If-Match": {selected.ETag}}, nil); err != nil {
				return fmt.Errorf("unlink branch: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Unlinked %s; branch and work preserved\n", selected.description())
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Select the repository branch to unlink")
	return cmd
}
