package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/spf13/cobra"
)

// changeApprovalsPath builds the approvals collection path for a change number.
func changeApprovalsPath(forge, owner, repo string, number int) string {
	return changeNumberPath(forge, owner, repo, number) + "/approvals"
}

// buildApprovalRequest validates and constructs an approval request. A
// REQUEST_CHANGES decision requires a non-empty message; the server enforces
// this too, but a client-side check gives a clearer error before the round trip.
func buildApprovalRequest(event, message string) (api.ChangeApprovalRequest, error) {
	msg := strings.TrimSpace(message)
	if event == "REQUEST_CHANGES" && msg == "" {
		return api.ChangeApprovalRequest{}, errors.New("--message is required when requesting changes")
	}
	return api.ChangeApprovalRequest{Event: event, Body: msg}, nil
}

// resolveNumberedChange resolves a change by optional selector, falling back to
// the current branch (or --branch), and requires it to have a number (the
// number-keyed subresource endpoints — approvals, threads — reject a change
// without one).
func resolveNumberedChange(ctx context.Context, client *api.Client, repoOverride, selector, branch string) (*api.ChangeResource, string, string, string, error) {
	forge, owner, repoName, err := resolveChangeRepoOrRemote(ctx, repoOverride)
	if err != nil {
		return nil, "", "", "", err
	}
	found, err := resolveChangeBySelector(ctx, client, forge, owner, repoName, selector, branch)
	if err != nil {
		return nil, "", "", "", err
	}
	if found.Number <= 0 {
		return nil, "", "", "", errors.New("change has no number yet")
	}
	return found, forge, owner, repoName, nil
}

// selectorFromArgs returns the first positional arg, mirroring `change show`.
func selectorFromArgs(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}

func submitChangeApproval(ctx context.Context, w, errW io.Writer, insecureHTTP bool, repoOverride, selector, branch, event, message, successVerb string) error {
	if selector != "" && strings.TrimSpace(branch) != "" {
		return errors.New("pass a change selector or --branch, not both")
	}
	req, err := buildApprovalRequest(event, message)
	if err != nil {
		return err
	}
	// Auth/not-logged-in messages go to stderr; w carries command output only.
	return runAuthenticatedChangeAPI(ctx, errW, insecureHTTP, repoOverride, func(ctx context.Context, client *api.Client) error {
		found, forge, owner, repoName, err := resolveNumberedChange(ctx, client, repoOverride, selector, branch)
		if err != nil {
			return err
		}
		resp, err := client.Post(ctx, changeApprovalsPath(forge, owner, repoName, found.Number), req)
		if err != nil {
			return fmt.Errorf("failed to submit approval: %w", err)
		}
		defer resp.Body.Close()
		if err := checkChangeResponse(resp); err != nil {
			return err
		}
		var out api.ChangeApprovalResponse
		if err := api.DecodeJSON(resp, &out); err != nil {
			return fmt.Errorf("failed to decode approval response: %w", err)
		}
		fmt.Fprintf(w, "%s change #%d\n", successVerb, found.Number)
		return nil
	})
}

func newChangeApproveCmd() *cobra.Command {
	var message, branch string
	cmd := &cobra.Command{
		Use:   "approve [<change>]",
		Short: "Approve a change",
		Long: `Approve a change.

If <change> is omitted, approves the change for the current branch (or --branch).
The change must be open and have a linked branch.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureChangeRepoHasTarget(cmd, selectorFromArgs(args) != "" || strings.TrimSpace(branch) != "", "pass a change selector or --branch"); err != nil {
				return err
			}
			return submitChangeApproval(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), changeInsecureHTTP(cmd),
				changeRepoFlag(cmd), selectorFromArgs(args), branch, "APPROVE", message, "Approved")
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Optional approval comment")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch of the change (defaults to current); cannot be combined with a change selector")
	return cmd
}

func newChangeRequestChangesCmd() *cobra.Command {
	var message, branch string
	cmd := &cobra.Command{
		Use:   "request-changes [<change>]",
		Short: "Request changes on a change",
		Long: `Request changes on a change.

If <change> is omitted, targets the change for the current branch (or --branch).
A reason (--message) is required. The change must be open and have a linked branch.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureChangeRepoHasTarget(cmd, selectorFromArgs(args) != "" || strings.TrimSpace(branch) != "", "pass a change selector or --branch"); err != nil {
				return err
			}
			return submitChangeApproval(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), changeInsecureHTTP(cmd),
				changeRepoFlag(cmd), selectorFromArgs(args), branch, "REQUEST_CHANGES", message, "Requested changes on")
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Reason for requesting changes (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch of the change (defaults to current); cannot be combined with a change selector")
	return cmd
}

func newChangeApprovalsCmd() *cobra.Command {
	var branch string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "approvals [<change>]",
		Short: "List approval decisions on a change",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureChangeRepoHasTarget(cmd, selectorFromArgs(args) != "" || strings.TrimSpace(branch) != "", "pass a change selector or --branch"); err != nil {
				return err
			}
			return runChangeApprovals(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), changeInsecureHTTP(cmd),
				changeRepoFlag(cmd), selectorFromArgs(args), branch, jsonOut)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch of the change (defaults to current); cannot be combined with a change selector")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runChangeApprovals(ctx context.Context, w, errW io.Writer, insecureHTTP bool, repoOverride, selector, branch string, jsonOut bool) error {
	if selector != "" && strings.TrimSpace(branch) != "" {
		return errors.New("pass a change selector or --branch, not both")
	}
	// Auth/not-logged-in messages go to stderr; w carries command output only.
	return runAuthenticatedChangeAPI(ctx, errW, insecureHTTP, repoOverride, func(ctx context.Context, client *api.Client) error {
		found, forge, owner, repoName, err := resolveNumberedChange(ctx, client, repoOverride, selector, branch)
		if err != nil {
			return err
		}
		resp, err := client.Get(ctx, changeApprovalsPath(forge, owner, repoName, found.Number))
		if err != nil {
			return fmt.Errorf("failed to list approvals: %w", err)
		}
		defer resp.Body.Close()
		if err := checkChangeResponse(resp); err != nil {
			return err
		}
		var out api.ChangeApprovalsResponse
		if err := api.DecodeJSON(resp, &out); err != nil {
			return fmt.Errorf("failed to decode approvals response: %w", err)
		}
		if jsonOut {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
		if len(out.Approvals) == 0 {
			fmt.Fprintf(w, "No approvals on change #%d\n", found.Number)
			return nil
		}
		renderChangeApprovals(w, out.Approvals)
		return nil
	})
}

// renderChangeApprovals prints one line per approval decision, plus an indented
// body when the reviewer left one. Split out so the render path is covered without
// a live API — the decode bug it exercises could only be seen against a change that
// actually had approvals.
func renderChangeApprovals(w io.Writer, approvals []api.ChangeApproval) {
	for _, a := range approvals {
		sha := a.CommitSHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		fmt.Fprintf(w, "%s  %s  %s  %s\n", a.Event, a.Author, sha, a.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
		if strings.TrimSpace(a.Body) != "" {
			fmt.Fprintf(w, "    %s\n", a.Body)
		}
	}
}
