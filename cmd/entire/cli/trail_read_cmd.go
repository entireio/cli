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

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// getTrailRepoIDResource reads a subresource of the selected change and
// decodes it into out.
func getTrailRepoIDResource(ctx context.Context, selected *trailWorkingContext, suffix, what string, out any) error {
	path, err := trailRepoIDPath(selected, suffix)
	if err != nil {
		return err
	}
	resp, err := selected.Client.Get(ctx, path)
	if err != nil {
		return fmt.Errorf("read trail %s: %w", what, err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return err
	}
	if err := api.DecodeJSON(resp, out); err != nil {
		return fmt.Errorf("decode trail %s: %w", what, err)
	}
	return nil
}

func trailReadSelectorHelp(mode *trailMode) string {
	return mode.help("If <trail> is omitted, reads the trail for the current branch (or --branch).",
		`<trail> is a project trail number, ID, or <repo>/<number>. Without one, follow the current branch's
parent. --repo and --branch select a working context; only that branch is read.`)
}

// newTrailWorkingReadCmd builds a read-only command on the selected change.
func newTrailWorkingReadCmd(mode *trailMode, use, short, long string, run func(cmd *cobra.Command, selected *trailWorkingContext, jsonOut bool) error) *cobra.Command {
	var branch string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   use + " [<trail>]",
		Short: short,
		Long:  long + "\n\n" + trailReadSelectorHelp(mode),
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selected, err := mode.workingContext(cmd, selectorFromArgs(args), branch, false)
			if err != nil {
				return err
			}
			return run(cmd, selected, jsonOut)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", mode.help("Branch of the trail (defaults to current); cannot be combined with a trail selector", "Select a repository branch within the trail"))
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

// trailTimelineResponse keeps items raw: --json passes them through, and the
// text view reads only the fields it shows, so a new kind never breaks either.
type trailTimelineResponse struct {
	Items         []json.RawMessage `json:"items"`
	CommitCount   int               `json:"commitCount"`
	Incomplete    bool              `json:"incomplete"`
	NextPageToken *string           `json:"nextPageToken,omitempty"`
}

type trailTimelineItem struct {
	Kind  string `json:"kind"`
	Actor *struct {
		Login   *string `json:"login"`
		ActorID *string `json:"actorId"`
	} `json:"actor"`
	OccurredAt     string   `json:"occurredAt"`
	Title          string   `json:"title"`
	CommitSHA      string   `json:"commitSha"`
	Additions      *int     `json:"additions"`
	Deletions      *int     `json:"deletions"`
	FindingCount   *int     `json:"findingCount"`
	CommentID      string   `json:"commentId"`
	ApprovalStatus string   `json:"approvalStatus"`
	Agents         []string `json:"agents"`
}

func newTrailTimelineCmd(mode *trailMode) *cobra.Command {
	var pageSize int
	var pageToken string
	cmd := newTrailWorkingReadCmd(mode, "timeline", "Show a trail's activity: commits, reviews, findings, and approvals",
		"Show the activity on a trail's branch, newest first: commits, reviews, finding resolutions, approvals, and lifecycle events. "+
			"Use --page-token with the printed token to continue.",
		func(cmd *cobra.Command, selected *trailWorkingContext, jsonOut bool) error {
			if pageSize < 1 || pageSize > trailListServerMaxLimit {
				return errors.New("--limit must be between 1 and 100")
			}
			query := url.Values{"pageSize": {strconv.Itoa(pageSize)}}
			if pageToken != "" {
				query.Set("pageToken", pageToken)
			}
			var out trailTimelineResponse
			if err := getTrailRepoIDResource(cmd.Context(), selected, "/timeline?"+query.Encode(), "timeline", &out); err != nil {
				return err
			}
			if out.Items == nil {
				out.Items = []json.RawMessage{}
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), out)
			}
			return printTrailTimeline(cmd.OutOrStdout(), selected, out)
		})
	cmd.Flags().IntVar(&pageSize, "limit", 50, "Page size (1-100)")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "Continue from a previous page's token")
	return cmd
}

func printTrailTimeline(w io.Writer, selected *trailWorkingContext, out trailTimelineResponse) error {
	if len(out.Items) == 0 {
		fmt.Fprintf(w, "No activity on %s\n", selected.description())
		return nil
	}
	for _, raw := range out.Items {
		var item trailTimelineItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("decode timeline item: %w", err)
		}
		line := fmt.Sprintf("%s  %-18s  %-16s  %s", tuiutil.SanitizeTerminalLabel(item.OccurredAt), tuiutil.SanitizeTerminalLabel(item.Kind),
			tuiutil.SanitizeTerminalLabel(trailTimelineActor(item)), trailTimelineSummary(item))
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	if out.Incomplete {
		fmt.Fprintln(w, "(incomplete: some activity could not be loaded)")
	}
	if out.NextPageToken != nil && *out.NextPageToken != "" {
		fmt.Fprintf(w, "Next page: --page-token %s\n", tuiutil.SanitizeTerminalText(*out.NextPageToken))
	}
	return nil
}

func trailTimelineActor(item trailTimelineItem) string {
	if item.Actor == nil {
		return "-"
	}
	if login := stringPtrValue(item.Actor.Login); login != "" {
		return login
	}
	if id := stringPtrValue(item.Actor.ActorID); id != "" {
		return id
	}
	return "-"
}

func trailTimelineSummary(item trailTimelineItem) string {
	var parts []string
	if item.CommitSHA != "" {
		parts = append(parts, shortSHA(tuiutil.SanitizeTerminalLabel(item.CommitSHA)))
	}
	if item.Title != "" {
		parts = append(parts, tuiutil.SanitizeTerminalLabel(item.Title))
	}
	if item.Additions != nil && item.Deletions != nil {
		parts = append(parts, fmt.Sprintf("+%d/-%d", *item.Additions, *item.Deletions))
	}
	if len(item.Agents) > 0 {
		parts = append(parts, "("+tuiutil.SanitizeTerminalLabel(strings.Join(item.Agents, ", "))+")")
	}
	if item.FindingCount != nil {
		parts = append(parts, strconv.Itoa(*item.FindingCount)+" finding(s)")
	}
	if item.ApprovalStatus != "" {
		parts = append(parts, tuiutil.SanitizeTerminalLabel(item.ApprovalStatus))
	}
	if item.CommentID != "" && item.FindingCount == nil {
		parts = append(parts, "finding "+tuiutil.SanitizeTerminalLabel(item.CommentID))
	}
	return strings.Join(parts, " ")
}

// trailSessionsResponse keeps sessions raw for --json; the text view decodes
// only what it lists.
type trailSessionsResponse struct {
	Sessions []json.RawMessage `json:"sessions"`
}

type trailSessionItem struct {
	SessionID       string     `json:"sessionId"`
	DisplayName     *string    `json:"displayName"`
	Agent           *string    `json:"agent"`
	Model           *string    `json:"model"`
	StartedAt       *time.Time `json:"startedAt"`
	CheckpointCount int        `json:"checkpointCount"`
}

func newTrailSessionsCmd(mode *trailMode) *cobra.Command {
	return newTrailWorkingReadCmd(mode, "sessions", "List the agent sessions behind a trail's commits",
		"List the agent sessions whose checkpoints are on a trail's branch. 'entire trail resume' restores one.",
		func(cmd *cobra.Command, selected *trailWorkingContext, jsonOut bool) error {
			var out trailSessionsResponse
			if err := getTrailRepoIDResource(cmd.Context(), selected, "/sessions", "sessions", &out); err != nil {
				return err
			}
			if out.Sessions == nil {
				out.Sessions = []json.RawMessage{}
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), out.Sessions)
			}
			if len(out.Sessions) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No sessions on %s\n", selected.description())
				return nil
			}
			items := make([]trailSessionItem, len(out.Sessions))
			for i, raw := range out.Sessions {
				if err := json.Unmarshal(raw, &items[i]); err != nil {
					return fmt.Errorf("decode trail session: %w", err)
				}
			}
			return printTable(cmd.OutOrStdout(), []string{"SESSION", "AGENT", colHeaderModel, "STARTED", "CHECKPOINTS", "NAME"}, items, func(s trailSessionItem) []string {
				started := ""
				if s.StartedAt != nil {
					started = s.StartedAt.Format(time.RFC3339)
				}
				return []string{tuiutil.SanitizeTerminalLabel(s.SessionID), tuiutil.SanitizeTerminalLabel(stringPtrValue(s.Agent)),
					tuiutil.SanitizeTerminalLabel(stringPtrValue(s.Model)), started, strconv.Itoa(s.CheckpointCount),
					tuiutil.SanitizeTerminalLabel(stringPtrValue(s.DisplayName))}
			})
		})
}

type trailStackResponse struct {
	Stack *trailStack `json:"stack"`
}

type trailStack struct {
	Position     int                     `json:"position"`
	Size         int                     `json:"size"`
	Base         string                  `json:"base,omitempty"`
	PullRequests []trailStackPullRequest `json:"pull_requests"`
}

type trailStackPullRequest struct {
	PRNumber    int     `json:"pr_number"`
	TrailNumber *int    `json:"trail_number"`
	Title       *string `json:"title"`
	Status      *string `json:"status"`
	Current     bool    `json:"current"`
}

func newTrailStackCmd(mode *trailMode) *cobra.Command {
	return newTrailWorkingReadCmd(mode, "stack", "Show the pull request stack a trail belongs to",
		"Show the stack of pull requests a trail's pull request belongs to, bottom (closest to the base) first. "+
			"Only GitHub-backed trails in a stack have one.",
		func(cmd *cobra.Command, selected *trailWorkingContext, jsonOut bool) error {
			var out trailStackResponse
			if err := getTrailRepoIDResource(cmd.Context(), selected, "/stack", "stack", &out); err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), out)
			}
			w := cmd.OutOrStdout()
			if out.Stack == nil {
				fmt.Fprintf(w, "%s is not in a pull request stack\n", selected.description())
				return nil
			}
			base := ""
			if out.Stack.Base != "" {
				base = " onto " + tuiutil.SanitizeTerminalLabel(out.Stack.Base)
			}
			fmt.Fprintf(w, "Position %d of %d%s:\n", out.Stack.Position, out.Stack.Size, base)
			for _, pr := range out.Stack.PullRequests {
				marker := "  "
				if pr.Current {
					marker = "> "
				}
				trailRef := "-"
				if pr.TrailNumber != nil {
					trailRef = selected.Repo + "/" + strconv.Itoa(*pr.TrailNumber)
				}
				fmt.Fprintf(w, "%s#%-6d %-10s %-20s %s\n", marker, pr.PRNumber, tuiutil.SanitizeTerminalLabel(stringPtrValue(pr.Status)),
					tuiutil.SanitizeTerminalLabel(trailRef), tuiutil.SanitizeTerminalLabel(stringPtrValue(pr.Title)))
			}
			return nil
		})
}
