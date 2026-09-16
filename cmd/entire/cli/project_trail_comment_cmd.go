package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

const (
	discussionShow      = "show"
	discussionAdd       = "add"
	discussionReply     = "reply"
	discussionEdit      = "edit"
	discussionDelete    = "delete"
	discussionResolve   = "resolve"
	discussionUnresolve = "unresolve"
)

type projectDiscussionResponse struct {
	Discussion  api.TrailThreadSummary   `json:"discussion"`
	Message     *api.TrailThreadMessage  `json:"message,omitempty"`
	Messages    []api.TrailThreadMessage `json:"messages,omitempty"`
	EventCursor string                   `json:"eventCursor,omitempty"`
}

func newProjectTrailCommentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "comment", Short: "Discuss the whole trail across repositories", Args: cobra.NoArgs}
	cmd.PersistentFlags().String("trail", "", "Project trail number or ID (defaults to the current branch's parent)")
	cmd.PersistentFlags().String("branch", "", "Discover the trail through this repository branch")
	for _, action := range []struct {
		name, description string
		count             int
	}{
		{cmdList, "List trail discussions", 0},
		{discussionShow, "Show a discussion and its messages", 1},
		{discussionAdd, "Start a discussion on the trail", 0},
		{discussionReply, "Reply to a trail discussion", 1},
		{discussionEdit, "Edit a discussion message", 2},
		{discussionDelete, "Delete a discussion message", 2},
		{discussionResolve, "Resolve a trail discussion", 1},
		{discussionUnresolve, "Reopen a trail discussion", 1},
	} {
		child := newProjectDiscussionAction(action.name)
		child.Use, child.Short, child.Args = action.name, action.description, cobra.ExactArgs(action.count)
		if action.count > 0 {
			child.Use += " <discussion-id>"
		}
		if action.count > 1 {
			child.Use += " <message-id>"
		}
		cmd.AddCommand(child)
	}
	return cmd
}

func discussionNeedsBody(action string) bool {
	return action == discussionAdd || action == discussionReply || action == discussionEdit
}

func newProjectDiscussionAction(action string) *cobra.Command {
	var body, title string
	var force bool
	cmd := &cobra.Command{RunE: func(cmd *cobra.Command, args []string) error {
		if discussionNeedsBody(action) && strings.TrimSpace(body) == "" {
			return errors.New("--body is required")
		}
		if action == discussionDelete && !force {
			return errors.New("deleting a message requires --force")
		}
		return runProjectDiscussion(cmd, action, args, body, title)
	}}
	if discussionNeedsBody(action) {
		cmd.Flags().StringVarP(&body, "body", "m", "", "Message body (required)")
	}
	if action == discussionAdd {
		cmd.Flags().StringVar(&title, "title", "", "Discussion title")
	}
	if action == discussionDelete {
		cmd.Flags().BoolVarP(&force, "force", "f", false, "Confirm message deletion")
	}
	addJSONFlag(cmd)
	return cmd
}

func runProjectDiscussion(cmd *cobra.Command, action string, args []string, body, title string) error {
	target, err := resolveProjectTrail(cmd, trailSubcommandSelector(cmd))
	if err != nil {
		return err
	}
	path := target.path() + "/discussions"
	if action == cmdList {
		items, err := fetchAllTrailThreads(cmd.Context(), target.Client, path)
		if err != nil {
			return err
		}
		if jsonRequested(cmd) {
			return printJSON(cmd.OutOrStdout(), items)
		}
		for _, item := range items {
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s (resolved: %t)\n", tuiutil.SanitizeTerminalLabel(item.ID), tuiutil.SanitizeTerminalLabel(item.Title), item.Resolved)
		}
		return nil
	}
	if len(args) > 0 {
		path += "/" + url.PathEscape(args[0])
	}
	var current projectDiscussionResponse
	var etag string
	if action != discussionAdd && action != discussionReply {
		etag, err = target.Client.ProjectTrailRequest(cmd.Context(), http.MethodGet, path, nil, nil, &current)
		if err != nil {
			return fmt.Errorf("read trail discussion: %w", err)
		}
		if current.Discussion.ID != args[0] {
			return errors.New("discussion response identity does not match the request")
		}
	}
	if action == discussionShow {
		if jsonRequested(cmd) {
			return printJSON(cmd.OutOrStdout(), current)
		}
		return printTrailThreadDetail(cmd.OutOrStdout(), api.TrailThreadDetailResponse{Thread: current.Discussion, Messages: current.Messages}, false)
	}
	method := http.MethodPost
	var request any
	headers := http.Header{}
	switch action {
	case discussionAdd:
		request = api.TrailThreadCreateRequest{Title: title, Body: body}
	case discussionReply:
		path += "/messages"
		request = api.TrailThreadMessageRequest{Body: body}
	case discussionEdit, discussionDelete:
		etag = projectDiscussionMessageETag(current.Messages, args[1])
		if etag == "" {
			return errors.New("message not found or missing its ETag; refusing an unprotected write")
		}
		headers.Set("If-Match", etag)
		path += "/messages/" + url.PathEscape(args[1])
		method = http.MethodDelete
		if action == discussionEdit {
			method, request = http.MethodPatch, api.TrailThreadMessageRequest{Body: body}
		}
	case discussionResolve, discussionUnresolve:
		if etag == "" {
			return errors.New("discussion read returned no ETag; refusing an unprotected write")
		}
		headers.Set("If-Match", etag)
		resolved := action == discussionResolve
		method, request = http.MethodPatch, api.TrailThreadUpdateRequest{Resolved: &resolved}
	default:
		return errors.New("unknown discussion operation")
	}
	var out projectDiscussionResponse
	var dest any = &out
	if action == discussionDelete {
		dest = nil
	}
	if _, err := target.Client.ProjectTrailRequest(cmd.Context(), method, path, request, headers, dest); err != nil {
		return fmt.Errorf("%s trail discussion: %w", action, err)
	}
	if action == discussionDelete {
		if jsonRequested(cmd) {
			return printJSON(cmd.OutOrStdout(), map[string]bool{"deleted": true})
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Message deleted")
		return nil
	}
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), out)
	}
	if out.Message != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Message %s saved\n", tuiutil.SanitizeTerminalLabel(out.Message.ID))
	}
	if out.Discussion.ID != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Discussion %s saved\n", tuiutil.SanitizeTerminalLabel(out.Discussion.ID))
	}
	return nil
}

func projectDiscussionMessageETag(messages []api.TrailThreadMessage, id string) string {
	for _, message := range messages {
		if message.ID == id {
			return message.ETag
		}
		for _, reply := range message.Replies {
			if reply.ID == id {
				return reply.ETag
			}
		}
	}
	return ""
}
