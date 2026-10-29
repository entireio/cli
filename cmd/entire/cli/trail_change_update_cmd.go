package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// errTrailNotMarkedReady reports a ready request the server accepted but did
// not record: readiness is only tracked on repositories with an approval gate.
var errTrailNotMarkedReady = errors.New("the server accepted the request but did not mark the trail ready: readiness is only tracked on repositories with an approval gate; any linked pull request was still marked ready")

func newTrailReadyCmd(mode *trailMode) *cobra.Command {
	var branch string
	var undo bool
	cmd := &cobra.Command{
		Use:   "ready [<trail>]",
		Short: "Mark a trail ready for review",
		Long: "Mark a trail ready for review, or with --undo return it to draft. A linked pull request follows.\n\n" + mode.help(`If <trail> is omitted, marks the trail for the current branch (or --branch).
Only an open trail can be marked ready.`, `<trail> is a project trail number, ID, or <repo>/<number>. Without one, follow the current branch's
parent. --repo and --branch select a working context. Only that branch is marked,
not every repository on the trail. The branch work must be open.`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ready := !undo
			selected, updated, err := patchWorkingTrailChange(cmd, mode, selectorFromArgs(args), branch, api.TrailUpdateRequest{ReadyForReview: &ready})
			if err != nil {
				return err
			}
			switch {
			case updated.ReadyForReview == nil:
				// Nothing in the response confirms the state; do not claim it.
				requested := "draft"
				if ready {
					requested = "ready for review"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Requested %s for %s, but the server did not report its readiness; check it with 'entire trail show'\n",
					requested, selected.description())
				return nil
			case ready && !*updated.ReadyForReview:
				return errTrailNotMarkedReady
			case ready:
				fmt.Fprintf(cmd.OutOrStdout(), "Marked %s ready for review\n", selected.description())
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "Returned %s to draft\n", selected.description())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&undo, "undo", false, "Return the trail to draft instead")
	cmd.Flags().StringVar(&branch, "branch", "", mode.help("Branch of the trail (defaults to current); cannot be combined with a trail selector", "Select a repository branch within the trail"))
	return cmd
}

func newTrailRetargetCmd(mode *trailMode) *cobra.Command {
	var branch, base string
	cmd := &cobra.Command{
		Use:   "retarget [<trail>] --base <branch>",
		Short: "Change the base branch a trail merges into",
		Long: "Change the base branch a trail merges into. A linked pull request follows.\n\n" + mode.help(`If <trail> is omitted, retargets the trail for the current branch (or --branch).`, `<trail> is a project trail number, ID, or <repo>/<number>. Without one, follow the current branch's
parent. --repo and --branch select a working context. Only that branch is retargeted,
not every repository on the trail.`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base = strings.TrimSpace(base)
			if base == "" {
				return errors.New("--base is required and must not be empty")
			}
			selected, updated, err := patchWorkingTrailChange(cmd, mode, selectorFromArgs(args), branch, api.TrailUpdateRequest{Base: &base})
			if err != nil {
				return err
			}
			if updated.Base != "" {
				base = updated.Base
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Retargeted %s onto %s\n", selected.description(), tuiutil.SanitizeTerminalLabel(base))
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "New base branch")
	cmd.Flags().StringVar(&branch, "branch", "", mode.help("Branch of the trail (defaults to current); cannot be combined with a trail selector", "Select a repository branch within the trail"))
	return cmd
}

// patchWorkingTrailChange applies a metadata update to the one repository
// branch the selector resolves to, and returns the updated resource.
func patchWorkingTrailChange(cmd *cobra.Command, mode *trailMode, selector, branch string, request api.TrailUpdateRequest) (*trailWorkingContext, *api.TrailResource, error) {
	selected, err := mode.workingContext(cmd, selector, branch, false)
	if err != nil {
		return nil, nil, err
	}
	resp, err := selected.Client.Patch(cmd.Context(), trailNumberPathForBase(selected.BasePath, selected.Work.Number), request)
	if err != nil {
		return nil, nil, fmt.Errorf("update trail: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, nil, err
	}
	var out api.TrailUpdateResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return nil, nil, fmt.Errorf("decode trail update: %w", err)
	}
	return selected, &out.Trail, nil
}
