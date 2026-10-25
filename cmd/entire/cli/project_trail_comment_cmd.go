package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const discussionShow = "show"

type projectDiscussionResponse struct {
	Discussion  api.TrailDiscussionSummary   `json:"discussion"`
	Messages    []api.TrailDiscussionMessage `json:"messages,omitempty"`
	EventCursor string                       `json:"event_cursor,omitempty"`
}

// Project trail discussions are human conversation: the API accepts writes
// only from the Entire web app, so the CLI offers reads alone. Repo-trail
// comments (trail_comment_cmd.go) and findings are unaffected.
func newProjectTrailCommentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "comment",
		Short: "Read discussions on the whole trail",
		Long: `Read discussions on the whole trail across repositories.

Trail discussions are for people: start, reply to, edit, and resolve them in the
Entire web app. Use 'entire trail finding' for agent review findings.`,
		Args: cobra.NoArgs,
	}
	cmd.PersistentFlags().String("trail", "", "Project trail number, ID, or <repo>/<number> (defaults to the current branch's parent)")
	cmd.PersistentFlags().String("branch", "", "Discover the trail through this repository branch")
	list := newProjectDiscussionAction(cmdList)
	list.Use, list.Short, list.Args = cmdList, "List trail discussions", cobra.NoArgs
	show := newProjectDiscussionAction(discussionShow)
	show.Use, show.Short, show.Args = discussionShow+" <discussion-id>", "Show a discussion and its messages", cobra.ExactArgs(1)
	cmd.AddCommand(list, show)
	return cmd
}

func newProjectDiscussionAction(action string) *cobra.Command {
	cmd := &cobra.Command{RunE: func(cmd *cobra.Command, args []string) error {
		return runProjectDiscussion(cmd, action, args)
	}}
	addJSONFlag(cmd)
	return cmd
}

func runProjectDiscussion(cmd *cobra.Command, action string, args []string) error {
	target, err := resolveProjectTrail(cmd, trailSubcommandSelector(cmd))
	if err != nil {
		return err
	}
	path := target.path() + "/discussions"
	if action == cmdList {
		items, err := fetchAllTrailDiscussions(cmd.Context(), target.Client, path)
		if err != nil {
			return err
		}
		return printTrailDiscussions(cmd.OutOrStdout(), items, target.label(), jsonRequested(cmd), true)
	}
	var current projectDiscussionResponse
	if _, err := target.Client.ProjectTrailRequest(cmd.Context(), http.MethodGet, path+"/"+url.PathEscape(args[0]), nil, nil, &current); err != nil {
		return fmt.Errorf("read trail discussion: %w", err)
	}
	if current.Discussion.ID != args[0] {
		return errors.New("discussion response identity does not match the request")
	}
	// Render through the shared view types: raw API structs carry each
	// resource's ETag, which the JSON output deliberately omits.
	return printTrailDiscussionDetail(cmd.OutOrStdout(), api.TrailDiscussionDetailResponse{Discussion: current.Discussion, Messages: current.Messages}, jsonRequested(cmd))
}
