package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
)

// Thread subresource path builders (keyed by change number).
func changeThreadsPath(forge, owner, repo string, number int) string {
	return changeNumberPath(forge, owner, repo, number) + "/threads"
}

func changeThreadPath(forge, owner, repo string, number int, threadID string) string {
	return changeThreadsPath(forge, owner, repo, number) + "/" + threadID
}

func changeThreadMessagesPath(forge, owner, repo string, number int, threadID string) string {
	return changeThreadPath(forge, owner, repo, number, threadID) + "/messages"
}

func changeThreadMessagePath(forge, owner, repo string, number int, threadID, messageID string) string {
	return changeThreadMessagesPath(forge, owner, repo, number, threadID) + "/" + messageID
}

// changeSubcommandSelector reads the subtree's persistent --change flag.
func changeSubcommandSelector(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("change") //nolint:errcheck // flag registered on the subtree parent
	return v
}

// withNumberedChange resolves a numbered change (by --change selector or current
// branch / --branch) and invokes fn inside an authenticated API context. It
// centralizes the resolution boilerplate for the comment subtree.
func withNumberedChange(cmd *cobra.Command, fn func(ctx context.Context, client *api.Client, found *api.ChangeResource, forge, owner, repo string) error) error {
	repoOverride := changeRepoFlag(cmd)
	selector := changeSubcommandSelector(cmd)
	branch := changeBranchFlag(cmd)
	if selector != "" && strings.TrimSpace(branch) != "" {
		return errors.New("pass --change or --branch, not both")
	}
	if err := ensureChangeRepoHasTarget(cmd, selector != "" || strings.TrimSpace(branch) != "", "pass --change or --branch"); err != nil {
		return err
	}
	// Auth/not-logged-in messages go to stderr; w carries command output only.
	return runAuthenticatedChangeAPI(cmd.Context(), cmd.ErrOrStderr(), changeInsecureHTTP(cmd), repoOverride, func(ctx context.Context, client *api.Client) error {
		found, forge, owner, repo, err := resolveNumberedChange(ctx, client, repoOverride, selector, branch)
		if err != nil {
			return err
		}
		return fn(ctx, client, found, forge, owner, repo)
	})
}

func newChangeCommentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "comment",
		Short: "Manage discussion threads on a change",
		Long: `Manage discussion threads (comments) on a change.

A thread is a titled conversation with one or more messages; messages can have
replies. Code-review comments are managed separately under 'entire change finding'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.PersistentFlags().String("change", "", "Change number, id, or branch (defaults to the current branch)")
	cmd.PersistentFlags().String("branch", "", "Branch of the change (defaults to current); cannot be combined with --change")

	cmd.AddCommand(newChangeCommentListCmd())
	cmd.AddCommand(newChangeCommentShowCmd())
	cmd.AddCommand(newChangeCommentAddCmd())
	cmd.AddCommand(newChangeCommentReplyCmd())
	cmd.AddCommand(newChangeCommentEditCmd())
	cmd.AddCommand(newChangeCommentDeleteCmd())
	cmd.AddCommand(newChangeCommentResolveCmd("resolve", true, "Resolve", "Resolved"))
	cmd.AddCommand(newChangeCommentResolveCmd("unresolve", false, "Reopen", "Reopened"))
	return cmd
}

func newChangeCommentListCmd() *cobra.Command {
	var jsonOut, all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List discussion threads on a change",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withNumberedChange(cmd, func(ctx context.Context, client *api.Client, found *api.ChangeResource, forge, owner, repo string) error {
				items, err := fetchAllChangeThreads(ctx, client, changeThreadsPath(forge, owner, repo, found.Number))
				if err != nil {
					return err
				}
				return printChangeThreads(cmd.OutOrStdout(), items, found.Number, jsonOut, all)
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&all, "all", false, "Include code-review threads (managed via 'change finding')")
	return cmd
}

func fetchAllChangeThreads(ctx context.Context, client *api.Client, path string) ([]api.ChangeThreadSummary, error) {
	const pageSize = 100
	var items []api.ChangeThreadSummary
	pageToken := ""
	seen := map[string]bool{}
	for {
		q := url.Values{"pageSize": {strconv.Itoa(pageSize)}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		resp, err := client.Get(ctx, path+"?"+q.Encode())
		if err != nil {
			return nil, fmt.Errorf("failed to list threads: %w", err)
		}
		var page api.ChangeThreadsResponse
		decodeErr := func() error {
			defer resp.Body.Close()
			if err := checkChangeResponse(resp); err != nil {
				return err
			}
			if err := api.DecodeJSON(resp, &page); err != nil {
				return fmt.Errorf("failed to decode threads response: %w", err)
			}
			return nil
		}()
		if decodeErr != nil {
			return nil, decodeErr
		}
		items = append(items, page.Items...)
		if page.NextPageToken == nil || strings.TrimSpace(*page.NextPageToken) == "" {
			break
		}
		pageToken = strings.TrimSpace(*page.NextPageToken)
		if seen[pageToken] {
			return nil, fmt.Errorf("thread list pagination repeated page token %q", pageToken)
		}
		seen[pageToken] = true
	}
	return items, nil
}

func printChangeThreads(w io.Writer, items []api.ChangeThreadSummary, number int, jsonOut, all bool) error {
	filtered := items
	if !all {
		filtered = make([]api.ChangeThreadSummary, 0, len(items))
		for _, it := range items {
			if it.Kind == "discussion" {
				filtered = append(filtered, it)
			}
		}
	}
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(api.ChangeThreadsResponse{Items: filtered}); err != nil {
			return fmt.Errorf("encode threads JSON: %w", err)
		}
		return nil
	}
	if len(filtered) == 0 {
		fmt.Fprintf(w, "No discussion threads on change #%d\n", number)
		return nil
	}
	for _, it := range filtered {
		marker := "unresolved"
		if it.Resolved {
			marker = "resolved"
		}
		fmt.Fprintf(w, "%s  [%s]  %s  (%d message(s))\n", it.ID, marker, it.Title, it.MessageCount)
		if it.LastMessageAuthor != nil && it.LastMessageAt != nil {
			fmt.Fprintf(w, "    last: %s at %s\n", *it.LastMessageAuthor, it.LastMessageAt.Format(time.RFC3339))
		}
	}
	return nil
}

func newChangeCommentShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show <thread-id>",
		Short: "Show a discussion thread and its messages",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			threadID := args[0]
			return withNumberedChange(cmd, func(ctx context.Context, client *api.Client, found *api.ChangeResource, forge, owner, repo string) error {
				resp, err := client.Get(ctx, changeThreadPath(forge, owner, repo, found.Number, threadID))
				if err != nil {
					return fmt.Errorf("failed to fetch thread: %w", err)
				}
				defer resp.Body.Close()
				if err := checkChangeResponse(resp); err != nil {
					return err
				}
				var out api.ChangeThreadDetailResponse
				if err := api.DecodeJSON(resp, &out); err != nil {
					return fmt.Errorf("failed to decode thread response: %w", err)
				}
				return printChangeThreadDetail(cmd.OutOrStdout(), out, jsonOut)
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func printChangeThreadDetail(w io.Writer, out api.ChangeThreadDetailResponse, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fmt.Errorf("encode thread JSON: %w", err)
		}
		return nil
	}
	t := out.Thread
	marker := "unresolved"
	if t.Resolved {
		marker = "resolved"
	}
	fmt.Fprintf(w, "Thread %s [%s]: %s\n\n", t.ID, marker, t.Title)
	for _, m := range out.Messages {
		// The message ID is the argument `comment edit`/`delete` take, so it
		// must be visible here (the only plain-text read that shows messages).
		fmt.Fprintf(w, "%s  %s  %s\n%s\n", m.ID, m.Author, m.CreatedAt.Format(time.RFC3339), m.Body)
		for _, r := range m.Replies {
			fmt.Fprintf(w, "  ↳ %s  %s  %s\n  %s\n", r.ID, r.Author, r.CreatedAt.Format(time.RFC3339), r.Body)
		}
		fmt.Fprintln(w)
	}
	return nil
}

func newChangeCommentAddCmd() *cobra.Command {
	var body, title string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Start a discussion thread on a change",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(body) == "" {
				return errors.New("--body is required")
			}
			return withNumberedChange(cmd, func(ctx context.Context, client *api.Client, found *api.ChangeResource, forge, owner, repo string) error {
				req := api.ChangeThreadCreateRequest{Title: strings.TrimSpace(title), Body: body}
				resp, err := client.Post(ctx, changeThreadsPath(forge, owner, repo, found.Number), req)
				if err != nil {
					return fmt.Errorf("failed to create thread: %w", err)
				}
				defer resp.Body.Close()
				if err := checkChangeResponse(resp); err != nil {
					return err
				}
				var out api.ChangeThreadCreateResponse
				if err := api.DecodeJSON(resp, &out); err != nil {
					return fmt.Errorf("failed to decode thread response: %w", err)
				}
				if jsonOut {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(out)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Created thread %s on change #%d\n", out.Thread.ID, found.Number)
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&body, "body", "m", "", "Message body (required)")
	cmd.Flags().StringVar(&title, "title", "", "Thread title (optional)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func newChangeCommentReplyCmd() *cobra.Command {
	var body string
	cmd := &cobra.Command{
		Use:   "reply <thread-id>",
		Short: "Reply to a discussion thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			threadID := args[0]
			return withNumberedChange(cmd, func(ctx context.Context, client *api.Client, found *api.ChangeResource, forge, owner, repo string) error {
				if strings.TrimSpace(body) == "" {
					return errors.New("--body is required")
				}
				req := api.ChangeThreadMessageRequest{Body: body}
				resp, err := client.Post(ctx, changeThreadMessagesPath(forge, owner, repo, found.Number, threadID), req)
				if err != nil {
					return fmt.Errorf("failed to reply: %w", err)
				}
				defer resp.Body.Close()
				if err := checkChangeResponse(resp); err != nil {
					return err
				}
				var out api.ChangeThreadMessageResponse
				if err := api.DecodeJSON(resp, &out); err != nil {
					return fmt.Errorf("failed to decode message response: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Added message %s to thread %s\n", out.Message.ID, threadID)
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&body, "body", "m", "", "Message body (required)")
	return cmd
}

func newChangeCommentEditCmd() *cobra.Command {
	var body string
	cmd := &cobra.Command{
		Use:   "edit <thread-id> <message-id>",
		Short: "Edit a message in a discussion thread",
		Long:  "Edit a message in a discussion thread.\n\nFind <thread-id> with 'entire change comment list' and <message-id> with 'entire change comment show <thread-id>'.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			threadID, messageID := args[0], args[1]
			return withNumberedChange(cmd, func(ctx context.Context, client *api.Client, found *api.ChangeResource, forge, owner, repo string) error {
				if strings.TrimSpace(body) == "" {
					return errors.New("--body is required")
				}
				req := api.ChangeThreadMessageRequest{Body: body}
				resp, err := client.Patch(ctx, changeThreadMessagePath(forge, owner, repo, found.Number, threadID, messageID), req)
				if err != nil {
					return fmt.Errorf("failed to edit message: %w", err)
				}
				defer resp.Body.Close()
				if err := checkChangeResponse(resp); err != nil {
					return err
				}
				var out api.ChangeThreadMessageResponse
				if err := api.DecodeJSON(resp, &out); err != nil {
					return fmt.Errorf("failed to decode message response: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Edited message %s\n", out.Message.ID)
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&body, "body", "m", "", "New message body (required)")
	return cmd
}

func newChangeCommentDeleteCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "delete <thread-id> <message-id>",
		Short: "Delete a message from a discussion thread",
		Long:  "Delete a message from a discussion thread.\n\nFind <thread-id> with 'entire change comment list' and <message-id> with 'entire change comment show <thread-id>'.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			threadID, messageID := args[0], args[1]
			if !force && !interactive.CanPromptInteractively() {
				return fmt.Errorf("refusing to delete message %s without confirmation; pass --force", messageID)
			}
			if !force {
				confirmed := false
				form := NewAccessibleForm(
					huh.NewGroup(huh.NewConfirm().Title(fmt.Sprintf("Delete message %s?", messageID)).Value(&confirmed)),
				)
				if err := form.RunWithContext(cmd.Context()); err != nil {
					if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
						return nil
					}
					return fmt.Errorf("message deletion prompt: %w", err)
				}
				if !confirmed {
					fmt.Fprintln(cmd.OutOrStdout(), "Deletion cancelled.")
					return nil
				}
			}
			return withNumberedChange(cmd, func(ctx context.Context, client *api.Client, found *api.ChangeResource, forge, owner, repo string) error {
				resp, err := client.Delete(ctx, changeThreadMessagePath(forge, owner, repo, found.Number, threadID, messageID))
				if err != nil {
					return fmt.Errorf("failed to delete message: %w", err)
				}
				defer resp.Body.Close()
				if err := checkChangeResponse(resp); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted message %s\n", messageID)
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip the confirmation prompt")
	return cmd
}

func newChangeCommentResolveCmd(use string, resolved bool, shortVerb, successVerb string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <thread-id>",
		Short: shortVerb + " a discussion thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			threadID := args[0]
			return withNumberedChange(cmd, func(ctx context.Context, client *api.Client, found *api.ChangeResource, forge, owner, repo string) error {
				req := api.ChangeThreadUpdateRequest{Resolved: &resolved}
				resp, err := client.Patch(ctx, changeThreadPath(forge, owner, repo, found.Number, threadID), req)
				if err != nil {
					return fmt.Errorf("failed to update thread: %w", err)
				}
				defer resp.Body.Close()
				if err := checkChangeResponse(resp); err != nil {
					return err
				}
				var out api.ChangeThreadUpdateResponse
				if err := api.DecodeJSON(resp, &out); err != nil {
					return fmt.Errorf("failed to decode thread response: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s thread %s\n", successVerb, out.Thread.ID)
				return nil
			})
		},
	}
}
