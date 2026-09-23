package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/spf13/cobra"
)

const (
	trailComparisonAvailable = "available"
	trailConflictConflicting = "conflicting"
	trailChecksNone          = "none"
	trailMergeUnknown        = "unknown"
)

type trailMergeOptions struct {
	Selector string
	DryRun   bool
	Force    bool
}

func newTrailMergeCmd() *cobra.Command {
	var opts trailMergeOptions

	cmd := &cobra.Command{
		Use:   "merge",
		Short: "Merge a trail's branch into its base",
		Long: `Merge a trail's branch into its base branch.

The trail's gates (approvals, CI checks, the base branch's CI, being up to date,
and any other configured gates) are checked first. When a blocking gate fails,
the failing gates are listed and nothing is merged.

Pass --force to merge anyway. This bypasses the failing gates and is recorded
on the trail; the repo's bypass policy decides who may do it.

With --trail, the trail may be given as a number, id, or branch. Without it, the
trail for the current branch is used. Pass --dry-run to only report whether the
trail can be merged; it exits non-zero when a gate blocks the merge and --force
is not given, so it can gate CI.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := ensureTrailRepoHasTarget(cmd, strings.TrimSpace(opts.Selector) != "", "pass --trail"); err != nil {
				return err
			}
			return runTrailMerge(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), trailRepoFlag(cmd), opts)
		},
	}

	cmd.Flags().StringVar(&opts.Selector, "trail", "", "Trail to merge (number, id, or branch; defaults to the current branch's trail)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "Only check whether the trail can be merged; do not merge")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "Merge even when blocking gates fail (requires bypass permission)")

	return cmd
}

func runTrailMerge(ctx context.Context, w, errW io.Writer, insecureHTTP bool, repoOverride string, opts trailMergeOptions) error {
	return runAuthenticatedTrailAPI(ctx, errW, insecureHTTP, repoOverride, func(ctx context.Context, client *api.Client, repoID string) error {
		forge, owner, repoName, err := resolveTrailRepoOrRemote(ctx, repoOverride)
		if err != nil {
			return err
		}
		basePath, err := trailRepoBasePath(forge, owner, repoName, repoID)
		if err != nil {
			return err
		}
		found, err := resolveNumberedTrailAtPath(ctx, client, basePath, forge, owner, repoName, opts.Selector, "")
		if err != nil {
			return err
		}
		// repo_id-addressed for every forge: the name-addressed routes were retired.
		mergePath, err := trailRepoIDNumberPath(repoID, found.Number)
		if err != nil {
			return err
		}

		m, err := fetchTrailMergeability(ctx, client, mergePath)
		if err != nil {
			return err
		}
		printTrailMergeability(w, found, m)

		var blockers []string
		bypassable := 0
		if !m.Mergeable {
			blockers, bypassable = describeTrailMergeBlockers(ctx, client, mergePath, m)
			fmt.Fprintln(w, "Blocked by:")
			for _, b := range blockers {
				fmt.Fprintf(w, "  - %s\n", b)
			}
			if !opts.Force {
				return trailMergeBlockedError(found.Number, m, bypassable)
			}
		}

		if opts.DryRun {
			if m.Mergeable {
				fmt.Fprintf(w, "Trail #%d is mergeable (dry run; no merge performed).\n", found.Number)
			} else {
				fmt.Fprintf(w, "Trail #%d is blocked; --force would bypass %d failing %s (dry run; no merge performed).\n",
					found.Number, bypassable, pluralize("gate", bypassable))
			}
			return nil
		}

		req := api.TrailMergeRequest{Bypass: opts.Force}
		if m.HeadSHA != nil {
			req.ExpectedHeadSha = strings.TrimSpace(*m.HeadSHA)
		}
		res, err := postTrailMerge(ctx, client, mergePath, found.Number, req, m.BypassPolicy)
		if err != nil {
			return err
		}

		commit := "fast-forward"
		if res.MergeCommitSha != "" {
			commit = res.MergeCommitSha
		}
		base := strings.TrimSpace(found.Base)
		if base == "" {
			base = "its base"
		}
		if m.Mergeable {
			fmt.Fprintf(w, "Merged trail #%d into %s (%s)\n", found.Number, base, commit)
		} else {
			fmt.Fprintf(w, "Merged trail #%d into %s (%s), bypassing %d failing %s\n",
				found.Number, base, commit, bypassable, pluralize("gate", bypassable))
		}
		return nil
	})
}

func trailRepoIDNumberPath(repoID string, number int) (string, error) {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return "", errors.New("cannot merge a trail without the repo's ID")
	}
	return trailNumberPathForBase("/api/v1/repos/"+url.PathEscape(repoID)+"/trails", number), nil
}

func fetchTrailMergeability(ctx context.Context, client *api.Client, trailPath string) (*api.TrailMergeabilityResponse, error) {
	resp, err := client.Get(ctx, trailPath+"/mergeability")
	if err != nil {
		return nil, fmt.Errorf("failed to check mergeability: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, err
	}
	var m api.TrailMergeabilityResponse
	if err := api.DecodeJSON(resp, &m); err != nil {
		return nil, fmt.Errorf("failed to decode mergeability response: %w", err)
	}
	return &m, nil
}

func fetchTrailGates(ctx context.Context, client *api.Client, trailPath string) (*api.TrailGatesResponse, error) {
	resp, err := client.Get(ctx, trailPath+"/gates")
	if err != nil {
		return nil, fmt.Errorf("failed to read gates: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, err
	}
	var g api.TrailGatesResponse
	if err := api.DecodeJSON(resp, &g); err != nil {
		return nil, fmt.Errorf("failed to decode gates response: %w", err)
	}
	return &g, nil
}

func postTrailMerge(ctx context.Context, client *api.Client, trailPath string, number int, req api.TrailMergeRequest, bypassPolicy string) (*api.TrailMergeResponse, error) {
	resp, err := client.Post(ctx, trailPath+"/merge", req)
	if err != nil {
		return nil, fmt.Errorf("failed to merge trail: %w", err)
	}
	defer resp.Body.Close()
	// Not checkTrailResponse: it would report a bypass denial (403) as an expired login.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		checkErr := api.CheckResponse(resp)
		var httpErr *api.HTTPError
		if errors.As(checkErr, &httpErr) {
			if mapped := trailMergeRefusal(httpErr, number, bypassPolicy); mapped != nil {
				return nil, mapped
			}
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return nil, fmt.Errorf("%w (run 'entire login' to re-authenticate)", checkErr)
			}
		}
		return nil, fmt.Errorf("trail API: %w", checkErr)
	}
	var res api.TrailMergeResponse
	if err := api.DecodeJSON(resp, &res); err != nil {
		return nil, fmt.Errorf("failed to decode merge response: %w", err)
	}
	if !res.OK {
		return nil, fmt.Errorf("trail API did not confirm merge of trail #%d", number)
	}
	return &res, nil
}

func trailMergeRefusal(e *api.HTTPError, number int, bypassPolicy string) error {
	code := strings.TrimSpace(e.Code)
	if code == "" {
		code = strings.TrimSpace(e.Message)
	}
	switch {
	case e.StatusCode == http.StatusForbidden && code == "merge_gates_bypass_forbidden":
		policy := strings.TrimSpace(bypassPolicy)
		if policy == "" {
			policy = trailMergeUnknown
		}
		return fmt.Errorf("cannot merge trail #%d with --force: this repo's bypass policy (%s) does not allow you to bypass failing gates", number, policy)
	case e.StatusCode == http.StatusUnprocessableEntity && code == "merge_gates_failed":
		return fmt.Errorf("trail #%d is not mergeable: a blocking gate failed when the merge was attempted\nhint: run 'entire trail merge --dry-run' to see why, or rerun with --force to bypass", number)
	case e.StatusCode == http.StatusUnprocessableEntity && code == "merge_gates_pending":
		return fmt.Errorf("trail #%d is not mergeable yet: a blocking gate is still pending\nhint: wait for it to finish, or rerun with --force to bypass", number)
	case e.StatusCode == http.StatusConflict && strings.HasPrefix(code, "merge_head_mismatch"):
		return fmt.Errorf("trail #%d changed while it was being merged (%s)\nhint: rerun 'entire trail merge' to merge the new head", number, code)
	case e.StatusCode == http.StatusConflict && code == "merge_in_progress":
		return fmt.Errorf("trail #%d is already being merged; try again shortly", number)
	}
	return nil
}

func trailMergeBlockedError(number int, m *api.TrailMergeabilityResponse, bypassable int) error {
	msg := fmt.Sprintf("trail #%d is not mergeable", number)
	switch {
	case bypassable == 0:
		// A conflict is not a gate; --force cannot get past it.
		return errors.New(msg)
	case m.BypassPolicy == "nobody":
		return fmt.Errorf("%s\nhint: this repo's bypass policy (nobody) does not allow bypassing gates, so --force will be refused", msg)
	default:
		return fmt.Errorf("%s\nhint: rerun with --force to merge anyway, bypassing the failing gates", msg)
	}
}

func printTrailMergeability(w io.Writer, t *api.TrailResource, m *api.TrailMergeabilityResponse) {
	fmt.Fprintf(w, "Trail #%d (%s → %s)\n", t.Number, t.Branch, t.Base)
	fmt.Fprintf(w, "  Approvals:  %s\n", checkmark(m.ApprovalGatePassed))
	fmt.Fprintf(w, "  Checks:     %s (%s)\n", checkmark(m.ChecksStatus == "success" || m.ChecksStatus == trailChecksNone), trailChecksStatusDisplay(m.ChecksStatus))
	fmt.Fprintf(w, "  Up to date: %s\n", trailUpToDateDisplay(m))
	fmt.Fprintf(w, "  Conflicts:  %s\n", trailConflictDisplay(m.ConflictStatus))
	fmt.Fprintf(w, "  Mergeable:  %s\n", checkmark(m.Mergeable))
}

func checkmark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func trailChecksStatusDisplay(status string) string {
	if strings.TrimSpace(status) == "" {
		return trailChecksNone
	}
	return status
}

func trailUpToDateDisplay(m *api.TrailMergeabilityResponse) string {
	if m.ComparisonStatus != trailComparisonAvailable {
		return trailMergeUnknown
	}
	if m.BehindBy == 0 {
		return checkmark(true)
	}
	return fmt.Sprintf("%s (%d %s behind)", checkmark(false), m.BehindBy, pluralize("commit", m.BehindBy))
}

func trailConflictDisplay(status string) string {
	switch status {
	case "clean":
		return "none"
	case trailConflictConflicting:
		return "conflicts with base"
	default:
		return trailMergeUnknown
	}
}

func describeTrailMergeBlockers(ctx context.Context, client *api.Client, trailPath string, m *api.TrailMergeabilityResponse) ([]string, int) {
	var blockers []string
	var bypassable int
	if gates, err := fetchTrailGates(ctx, client, trailPath); err == nil {
		for _, g := range gates.BlockingFailures {
			blockers = append(blockers, describeGateFailure(g))
		}
		bypassable = len(gates.BlockingFailures)
	} else {
		fallback := describeMergeabilityBlockers(m)
		blockers = append(blockers, fallback...)
		bypassable = len(fallback)
	}
	if m.ConflictStatus == trailConflictConflicting {
		blockers = append(blockers, "branch conflicts with its base; resolve the conflicts first (--force cannot bypass this)")
	}
	if len(blockers) == 0 {
		blockers = append(blockers, "the trail is not in a mergeable state")
	}
	return blockers, bypassable
}

func describeGateFailure(g api.TrailGateResult) string {
	name := strings.TrimSpace(g.GateKey)
	if name == "" {
		name = strings.TrimSpace(g.GateType)
	}
	reason := ""
	if g.Rationale != nil {
		reason = strings.TrimSpace(*g.Rationale)
	}
	if reason == "" {
		reason = strings.TrimSpace(g.Status)
	}
	line := name + ": " + reason
	var value struct {
		WebURL string `json:"web_url"`
	}
	if len(g.Value) > 0 && json.Unmarshal(g.Value, &value) == nil && strings.TrimSpace(value.WebURL) != "" {
		line += " (" + strings.TrimSpace(value.WebURL) + ")"
	}
	return tuiutil.SanitizeDisplayText(line)
}

func describeMergeabilityBlockers(m *api.TrailMergeabilityResponse) []string {
	var reasons []string
	if !m.ApprovalGatePassed {
		reasons = append(reasons, "required approvals are missing")
	}
	switch m.ChecksStatus {
	case "failure":
		reasons = append(reasons, "CI checks failed")
	case "pending":
		reasons = append(reasons, "CI checks are still running")
	}
	if m.ComparisonStatus == trailComparisonAvailable && m.BehindBy > 0 {
		reasons = append(reasons, fmt.Sprintf("branch is %d %s behind its base", m.BehindBy, pluralize("commit", m.BehindBy)))
	}
	if len(reasons) == 0 && m.ConflictStatus != trailConflictConflicting {
		reasons = append(reasons, "a blocking gate failed (run 'entire trail show' for details)")
	}
	return reasons
}
