package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/spf13/cobra"
)

// trailApprovalsPath builds the approvals collection path for a trail number.
func trailApprovalsPath(basePath string, number int) string {
	return trailNumberPathForBase(basePath, number) + "/approvals"
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
func resolveNumberedTrailAtPath(ctx context.Context, client *api.Client, basePath, forge, owner, repoName, selector, branch string) (*api.TrailResource, error) {
	found, err := resolveTrailBySelectorAtPath(ctx, client, basePath, forge, owner, repoName, selector, branch)
	if err != nil {
		return nil, err
	}
	if found.Number <= 0 {
		return nil, errors.New("trail has no number yet")
	}
	return found, nil
}

// selectorFromArgs returns the first positional arg, mirroring `trail show`.
func selectorFromArgs(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}

func newTrailApproveCmd() *cobra.Command {
	var message, branch string
	cmd := &cobra.Command{
		Use:   "approve [<trail>]",
		Short: "Approve a trail",
		Long: `Approve a trail.

<trail> is a project trail number or ID. Without one, follow the current branch's
parent. --repo and --branch select a working context. Only that branch is approved,
not every repository on the trail. The branch work must be open.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureTrailRepoHasTarget(cmd, selectorFromArgs(args) != "" || strings.TrimSpace(branch) != "", "pass a trail selector or --branch"); err != nil {
				return err
			}
			return submitWorkingTrailApproval(cmd, selectorFromArgs(args), branch, "APPROVE", message, "Approved")
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Optional approval comment")
	cmd.Flags().StringVar(&branch, "branch", "", "Select a repository branch within the trail")
	return cmd
}

func newTrailRequestChangesCmd() *cobra.Command {
	var message, branch string
	cmd := &cobra.Command{
		Use:   "request-changes [<trail>]",
		Short: "Request changes on a trail",
		Long: `Request changes on a trail.

<trail> is a project trail number or ID. Without one, follow the current branch's
parent. --repo and --branch select a working context. A reason (--message) is
required. The decision applies only to the selected branch.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureTrailRepoHasTarget(cmd, selectorFromArgs(args) != "" || strings.TrimSpace(branch) != "", "pass a trail selector or --branch"); err != nil {
				return err
			}
			return submitWorkingTrailApproval(cmd, selectorFromArgs(args), branch, "REQUEST_CHANGES", message, "Requested changes on")
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Reason for requesting changes (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "Select a repository branch within the trail")
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
			return listWorkingTrailApprovals(cmd, selectorFromArgs(args), branch, jsonOut)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Select a repository branch within the trail")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
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
