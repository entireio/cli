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
	id := strings.TrimSpace(stringPtrValue(comment.DiscussionID))
	if id == "" {
		return "", fmt.Errorf("finding %s has no reply thread", tuiutil.SanitizeTerminalLabel(comment.ID))
	}
	if err := checkTrailPathSegment("reply thread ID", id); err != nil {
		return "", err
	}
	return id, nil
}

// checkTrailPathSegment refuses an ID that cannot stand as one URL path
// segment. Escaping leaves "." and ".." alone and resolving the URL drops
// them, so an empty or dot ID would address the thread or the message list
// instead of one message.
func checkTrailPathSegment(what, id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("invalid %s %q", what, tuiutil.SanitizeTerminalLabel(id))
	}
	return nil
}

// trailFindingReplyArgs splits [<trail>] <finding-id> <message-id> and checks
// the message ID before any request.
func trailFindingReplyArgs(args []string) ([]string, string, error) {
	messageID := strings.TrimSpace(args[len(args)-1])
	if err := checkTrailPathSegment("message ID", messageID); err != nil {
		return nil, "", err
	}
	return args[:len(args)-1], messageID, nil
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
			fmt.Fprintf(cmd.OutOrStdout(), "Added reply %s to finding %s\n", tuiutil.SanitizeTerminalLabel(message.ID), tuiutil.SanitizeTerminalLabel(comment.ID))
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
		Long:  "Edit a reply you wrote on a finding. 'entire trail finding show <finding-id>' prints the replies and their message IDs.",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(body) == "" {
				return errors.New("--body is required")
			}
			findingArgs, messageID, err := trailFindingReplyArgs(args)
			if err != nil {
				return err
			}
			client, trailID, _, discussionID, err := resolveTrailFindingThread(cmd, targetOpts, findingArgs)
			if err != nil {
				return err
			}
			message, err := sendTrailFindingMessage(cmd.Context(), client.Patch, trailFindingMessagePath(trailID, discussionID, messageID), body)
			if err != nil {
				return fmt.Errorf("edit finding reply: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Edited reply %s\n", tuiutil.SanitizeTerminalLabel(message.ID))
			return nil
		},
	}
	cmd.Flags().StringVarP(&body, "body", "m", "", "New reply body (required)")
	return cmd
}

func newTrailFindingDeleteReplyCmd(targetOpts *trailReviewTargetOptions) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "delete-reply [<trail>] <finding-id> <message-id>",
		Short: "Delete your reply to a finding",
		Long: "Delete a reply you wrote on a finding. 'entire trail finding show <finding-id>' prints the replies and their message IDs.\n\n" +
			"A terminal asks for confirmation; without one, pass --force.",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			findingArgs, messageID, err := trailFindingReplyArgs(args)
			if err != nil {
				return err
			}
			if proceed, err := confirmTrailMessageDelete(cmd, messageID, force); !proceed {
				return err
			}
			client, trailID, _, discussionID, err := resolveTrailFindingThread(cmd, targetOpts, findingArgs)
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
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted reply %s\n", tuiutil.SanitizeTerminalLabel(messageID))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Delete without asking for confirmation")
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
	if strings.TrimSpace(stringPtrValue(comment.DiscussionID)) == "" {
		return nil, nil
	}
	discussionID, err := trailFindingDiscussionID(comment)
	if err != nil {
		return nil, err
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
		fmt.Fprintf(w, "  %s  %s  %s\n", tuiutil.SanitizeTerminalLabel(m.ID), tuiutil.SanitizeTerminalLabel(m.Author), m.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(w, "  %s\n", indentTrailFindingReply(m.Body, "  "))
		for _, r := range m.Replies {
			fmt.Fprintf(w, "    ↳ %s  %s  %s\n", tuiutil.SanitizeTerminalLabel(r.ID), tuiutil.SanitizeTerminalLabel(r.Author), r.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(w, "    %s\n", indentTrailFindingReply(r.Body, "    "))
		}
	}
}

func indentTrailFindingReply(body, indent string) string {
	return strings.ReplaceAll(tuiutil.SanitizeTerminalText(strings.TrimSpace(body)), "\n", "\n"+indent)
}
