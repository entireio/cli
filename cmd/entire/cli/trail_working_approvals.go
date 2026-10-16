package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func submitWorkingTrailApproval(cmd *cobra.Command, mode *trailMode, selector, branch, event, message, verb string) error {
	request, err := buildApprovalRequest(event, message)
	if err != nil {
		return err
	}
	selected, err := mode.workingContext(cmd, selector, branch, false)
	if err != nil {
		return err
	}
	resp, err := selected.Client.Post(cmd.Context(), trailApprovalsPath(selected.BasePath, selected.Work.Number), request)
	if err != nil {
		return fmt.Errorf("submit trail approval: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", verb, selected.description())
	return nil
}

func listWorkingTrailApprovals(cmd *cobra.Command, mode *trailMode, selector, branch string, jsonOut bool) error {
	selected, err := mode.workingContext(cmd, selector, branch, false)
	if err != nil {
		return err
	}
	resp, err := selected.Client.Get(cmd.Context(), trailApprovalsPath(selected.BasePath, selected.Work.Number))
	if err != nil {
		return fmt.Errorf("list trail approvals: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return err
	}
	var out api.TrailApprovalsResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return fmt.Errorf("decode trail approvals: %w", err)
	}
	if jsonOut {
		return printJSON(cmd.OutOrStdout(), toTrailApprovalsResponseJSON(out))
	}
	if selected.Target != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Approvals for %s\n", selected.description())
	}
	if len(out.Approvals) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No approvals on %s\n", selected.description())
	}
	renderTrailApprovals(cmd.OutOrStdout(), out.Approvals)
	return nil
}
