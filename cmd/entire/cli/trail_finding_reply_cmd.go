package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// A finding's replies live in its own discussion, which the repository's
// discussions route serves like any other; the finding names it through
// discussion_id. The project-trail discussions route does not know it.
func trailFindingDiscussionPath(trailID, discussionID string) string {
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/discussions/" + url.PathEscape(discussionID)
}

func trailFindingMessagesPath(trailID, discussionID string) string {
	return trailFindingDiscussionPath(trailID, discussionID) + "/messages"
}

func trailFindingMessagePath(trailID, discussionID, messageID string) string {
	return trailFindingMessagesPath(trailID, discussionID) + "/" + url.PathEscape(messageID)
}

func trailFindingDiscussionID(comment api.TrailReviewComment) (string, error) {
	if id := strings.TrimSpace(stringPtrValue(comment.DiscussionID)); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("finding %s has no reply thread", comment.ID)
}

// resolveTrailFindingThread resolves the finding and its discussion for the
// reply subcommands.
func resolveTrailFindingThread(cmd *cobra.Command, targetOpts *trailReviewTargetOptions, args []string) (*api.Client, string, api.TrailReviewComment, string, error) {
	selector, commentID, err := parseTrailSelectorAndCommentID(args, targetOpts.Selector)
	if err != nil {
		return nil, "", api.TrailReviewComment{}, "", err
	}
	client, target, err := targetOpts.Mode.reviewTarget(cmd, selector, false)
	if err != nil {
		return nil, "", api.TrailReviewComment{}, "", err
	}
	comment, err := resolveTrailReviewComment(cmd.Context(), client, target.Trail.ID, commentID)
	if err != nil {
		return nil, "", api.TrailReviewComment{}, "", err
	}
	discussionID, err := trailFindingDiscussionID(comment)
	if err != nil {
		return nil, "", api.TrailReviewComment{}, "", err
	}
	return client, target.Trail.ID, comment, discussionID, nil
}

func newTrailFindingReplyCmd(targetOpts *trailReviewTargetOptions) *cobra.Command {
	var body string
	cmd := &cobra.Command{
		Use:   "reply [<trail>] <finding-id> -m <body>",
		Short: "Reply to a finding",
		Long:  "Reply to a finding. 'entire trail finding show <finding-id>' prints the replies and their message IDs.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(body) == "" {
				return errors.New("--body is required")
			}
			client, trailID, comment, discussionID, err := resolveTrailFindingThread(cmd, targetOpts, args)
			if err != nil {
				return err
			}
			message, err := sendTrailFindingMessage(cmd.Context(), client.Post, trailFindingMessagesPath(trailID, discussionID), body)
			if err != nil {
				return fmt.Errorf("reply to finding: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added reply %s to finding %s\n", message.ID, comment.ID)
			return nil
		},
	}
	cmd.Flags().StringVarP(&body, "body", "m", "", "Reply body (required)")
	return cmd
}

func newTrailFindingEditReplyCmd(targetOpts *trailReviewTargetOptions) *cobra.Command {
	var body string
	cmd := &cobra.Command{
		Use:   "edit-reply [<trail>] <finding-id> <message-id> -m <body>",
		Short: "Edit your reply to a finding",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(body) == "" {
				return errors.New("--body is required")
			}
			messageID := args[len(args)-1]
			client, trailID, _, discussionID, err := resolveTrailFindingThread(cmd, targetOpts, args[:len(args)-1])
			if err != nil {
				return err
			}
			message, err := sendTrailFindingMessage(cmd.Context(), client.Patch, trailFindingMessagePath(trailID, discussionID, messageID), body)
			if err != nil {
				return fmt.Errorf("edit finding reply: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Edited reply %s\n", message.ID)
			return nil
		},
	}
	cmd.Flags().StringVarP(&body, "body", "m", "", "New reply body (required)")
	return cmd
}

func newTrailFindingDeleteReplyCmd(targetOpts *trailReviewTargetOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete-reply [<trail>] <finding-id> <message-id>",
		Short: "Delete your reply to a finding",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			messageID := args[len(args)-1]
			client, trailID, _, discussionID, err := resolveTrailFindingThread(cmd, targetOpts, args[:len(args)-1])
			if err != nil {
				return err
			}
			resp, err := client.Delete(cmd.Context(), trailFindingMessagePath(trailID, discussionID, messageID))
			if err != nil {
				return fmt.Errorf("delete finding reply: %w", err)
			}
			defer resp.Body.Close()
			if err := checkTrailResponse(resp); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted reply %s\n", messageID)
			return nil
		},
	}
	return cmd
}

// sendTrailFindingMessage writes one message with send (the client's Post to
// add a reply, Patch to edit one) and returns the stored message.
func sendTrailFindingMessage(ctx context.Context, send func(context.Context, string, any) (*http.Response, error), path, body string) (api.TrailDiscussionMessage, error) {
	resp, err := send(ctx, path, api.TrailDiscussionMessageRequest{Body: body})
	if err != nil {
		return api.TrailDiscussionMessage{}, err
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return api.TrailDiscussionMessage{}, err
	}
	var out api.TrailDiscussionMessageResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return api.TrailDiscussionMessage{}, fmt.Errorf("decode message response: %w", err)
	}
	return out.Message, nil
}

// fetchTrailFindingReplies reads a finding's reply thread; nil without one.
func fetchTrailFindingReplies(ctx context.Context, client *api.Client, trailID string, comment api.TrailReviewComment) ([]api.TrailDiscussionMessage, error) {
	discussionID := strings.TrimSpace(stringPtrValue(comment.DiscussionID))
	if discussionID == "" {
		return nil, nil
	}
	resp, err := client.Get(ctx, trailFindingDiscussionPath(trailID, discussionID))
	if err != nil {
		return nil, fmt.Errorf("fetch finding replies: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, err
	}
	var out api.TrailDiscussionDetailResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return nil, fmt.Errorf("decode finding replies: %w", err)
	}
	return out.Messages, nil
}

// printTrailFindingReplies renders the thread under a finding's detail. The
// message IDs are what edit-reply and delete-reply take.
func printTrailFindingReplies(w io.Writer, messages []api.TrailDiscussionMessage) {
	if len(messages) == 0 {
		return
	}
	fmt.Fprintf(w, "\nReplies:\n")
	for _, m := range messages {
		fmt.Fprintf(w, "  %s  %s  %s\n", m.ID, tuiutil.SanitizeTerminalLabel(m.Author), m.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(w, "  %s\n", indentTrailFindingReply(m.Body, "  "))
		for _, r := range m.Replies {
			fmt.Fprintf(w, "    ↳ %s  %s  %s\n", r.ID, tuiutil.SanitizeTerminalLabel(r.Author), r.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(w, "    %s\n", indentTrailFindingReply(r.Body, "    "))
		}
	}
}

func indentTrailFindingReply(body, indent string) string {
	return strings.ReplaceAll(tuiutil.SanitizeTerminalText(strings.TrimSpace(body)), "\n", "\n"+indent)
}
