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

// trailApprovalsPath builds the approvals collection path for a trail number.
func trailApprovalsPath(forge, owner, repo string, number int) string {
	return trailNumberPath(forge, owner, repo, number) + "/approvals"
}

// buildApprovalRequest validates and constructs an approval request. A
// REQUEST_CHANGES decision requires a non-empty message; the server enforces
// this too, but a client-side check gives a clearer error before the round trip.
func buildApprovalRequest(event, message string) (api.TrailApprovalRequest, error) {
	msg := strings.TrimSpace(message)
	if event == "REQUEST_CHANGES" && msg == "" {
		return api.TrailApprovalRequest{}, errors.New("--message is required when requesting changes")
	}
	return api.TrailApprovalRequest{Event: event, Body: msg}, nil
}

// resolveNumberedTrail resolves a trail by optional selector, falling back to
// the current branch (or --branch), and requires it to have a number (the
// number-keyed subresource endpoints — approvals, threads — reject a trail
// without one).
func resolveNumberedTrail(ctx context.Context, client *api.Client, repoOverride, selector, branch string) (*api.TrailResource, string, string, string, error) {
	forge, owner, repoName, err := resolveTrailRepoOrRemote(ctx, repoOverride)
	if err != nil {
		return nil, "", "", "", err
	}
	found, err := resolveTrailBySelector(ctx, client, forge, owner, repoName, selector, branch)
	if err != nil {
		return nil, "", "", "", err
	}
	if found.Number <= 0 {
		return nil, "", "", "", errors.New("trail has no number yet")
	}
	return found, forge, owner, repoName, nil
}

// selectorFromArgs returns the first positional arg, mirroring `trail show`.
func selectorFromArgs(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}

func submitTrailApproval(ctx context.Context, w, errW io.Writer, insecureHTTP bool, repoOverride, selector, branch, event, message, successVerb string) error {
	if selector != "" && strings.TrimSpace(branch) != "" {
		return errors.New("pass a trail selector or --branch, not both")
	}
	req, err := buildApprovalRequest(event, message)
	if err != nil {
		return err
	}
	// Auth/not-logged-in messages go to stderr; w carries command output only.
	return runAuthenticatedTrailAPI(ctx, errW, insecureHTTP, repoOverride, func(ctx context.Context, client *api.Client) error {
		found, forge, owner, repoName, err := resolveNumberedTrail(ctx, client, repoOverride, selector, branch)
		if err != nil {
			return err
		}
		resp, err := client.Post(ctx, trailApprovalsPath(forge, owner, repoName, found.Number), req)
		if err != nil {
			return fmt.Errorf("failed to submit approval: %w", err)
		}
		defer resp.Body.Close()
		if err := checkTrailResponse(resp); err != nil {
			return err
		}
		var out api.TrailApprovalResponse
		if err := api.DecodeJSON(resp, &out); err != nil {
			return fmt.Errorf("failed to decode approval response: %w", err)
		}
		fmt.Fprintf(w, "%s trail #%d\n", successVerb, found.Number)
		return nil
	})
}

func newTrailApproveCmd() *cobra.Command {
	var message, branch string
	cmd := &cobra.Command{
		Use:   "approve [<trail>]",
		Short: "Approve a trail",
		Long: `Approve a trail.

If <trail> is omitted, approves the trail for the current branch (or --branch).
The trail must be open and have a linked branch.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureTrailRepoHasTarget(cmd, selectorFromArgs(args) != "" || strings.TrimSpace(branch) != "", "pass a trail selector or --branch"); err != nil {
				return err
			}
			return submitTrailApproval(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd),
				trailRepoFlag(cmd), selectorFromArgs(args), branch, "APPROVE", message, "Approved")
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Optional approval comment")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch of the trail (defaults to current); cannot be combined with a trail selector")
	return cmd
}

func newTrailRequestChangesCmd() *cobra.Command {
	var message, branch string
	cmd := &cobra.Command{
		Use:   "request-changes [<trail>]",
		Short: "Request changes on a trail",
		Long: `Request changes on a trail.

If <trail> is omitted, targets the trail for the current branch (or --branch).
A reason (--message) is required. The trail must be open and have a linked branch.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureTrailRepoHasTarget(cmd, selectorFromArgs(args) != "" || strings.TrimSpace(branch) != "", "pass a trail selector or --branch"); err != nil {
				return err
			}
			return submitTrailApproval(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd),
				trailRepoFlag(cmd), selectorFromArgs(args), branch, "REQUEST_CHANGES", message, "Requested changes on")
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Reason for requesting changes (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch of the trail (defaults to current); cannot be combined with a trail selector")
	return cmd
}

func newTrailApprovalsCmd() *cobra.Command {
	var branch string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "approvals [<trail>]",
		Short: "List approval decisions on a trail",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureTrailRepoHasTarget(cmd, selectorFromArgs(args) != "" || strings.TrimSpace(branch) != "", "pass a trail selector or --branch"); err != nil {
				return err
			}
			return runTrailApprovals(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd),
				trailRepoFlag(cmd), selectorFromArgs(args), branch, jsonOut)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch of the trail (defaults to current); cannot be combined with a trail selector")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runTrailApprovals(ctx context.Context, w, errW io.Writer, insecureHTTP bool, repoOverride, selector, branch string, jsonOut bool) error {
	if selector != "" && strings.TrimSpace(branch) != "" {
		return errors.New("pass a trail selector or --branch, not both")
	}
	// Auth/not-logged-in messages go to stderr; w carries command output only.
	return runAuthenticatedTrailAPI(ctx, errW, insecureHTTP, repoOverride, func(ctx context.Context, client *api.Client) error {
		found, forge, owner, repoName, err := resolveNumberedTrail(ctx, client, repoOverride, selector, branch)
		if err != nil {
			return err
		}
		resp, err := client.Get(ctx, trailApprovalsPath(forge, owner, repoName, found.Number))
		if err != nil {
			return fmt.Errorf("failed to list approvals: %w", err)
		}
		defer resp.Body.Close()
		if err := checkTrailResponse(resp); err != nil {
			return err
		}
		var out api.TrailApprovalsResponse
		if err := api.DecodeJSON(resp, &out); err != nil {
			return fmt.Errorf("failed to decode approvals response: %w", err)
		}
		if jsonOut {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
		if len(out.Approvals) == 0 {
			fmt.Fprintf(w, "No approvals on trail #%d\n", found.Number)
			return nil
		}
		renderTrailApprovals(w, out.Approvals)
		return nil
	})
}

// renderTrailApprovals prints one line per approval decision, plus an indented
// body when the reviewer left one. Split out so the render path is covered without
// a live API — the decode bug it exercises could only be seen against a trail that
// actually had approvals.
func renderTrailApprovals(w io.Writer, approvals []api.TrailApproval) {
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
