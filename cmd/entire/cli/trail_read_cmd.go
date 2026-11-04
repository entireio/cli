package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

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

// trailTimelineResponse is the text view's reading of a page: --json prints the
// server's response unchanged, and the text view reads each item on its own,
// so a new kind or field never breaks either.
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
			query := url.Values{"pageSize": {strconv.Itoa(pageSize)}}
			if pageToken != "" {
				query.Set("pageToken", pageToken)
			}
			var raw json.RawMessage
			if err := getTrailRepoIDResource(cmd.Context(), selected, "/timeline?"+query.Encode(), "timeline", &raw); err != nil {
				return err
			}
			if jsonOut {
				// The server's response unchanged, as sessions and stack print it.
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var out trailTimelineResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				return fmt.Errorf("decode trail timeline: %w", err)
			}
			return printTrailTimeline(cmd.OutOrStdout(), selected, out)
		})
	// The page size is checked before the trail is resolved, so a bad value
	// fails without authenticating or reading anything.
	cmd.PreRunE = func(*cobra.Command, []string) error {
		if pageSize < 1 || pageSize > trailListServerMaxLimit {
			return fmt.Errorf("--limit must be between 1 and %d", trailListServerMaxLimit)
		}
		return nil
	}
	cmd.Flags().IntVar(&pageSize, "limit", 50, fmt.Sprintf("Page size (1-%d)", trailListServerMaxLimit))
	cmd.Flags().StringVar(&pageToken, "page-token", "", "Continue from a previous page's token")
	return cmd
}

// looseJSONLabel prints a field the CLI could not read into its usual type: a
// string or number as itself, anything else blank.
func looseJSONLabel(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// trailUnreadableNote marks a row the CLI could not read; --json has it whole.
const trailUnreadableNote = "(unreadable item; see --json)"

func printTrailTimeline(w io.Writer, selected *trailWorkingContext, out trailTimelineResponse) error {
	// An empty page is only this page's: a page that could not be loaded in
	// full, or that has a next page, says so below.
	if len(out.Items) == 0 {
		fmt.Fprintf(w, "No activity on this page of %s\n", selected.description())
	}
	for _, raw := range out.Items {
		var item trailTimelineItem
		if err := json.Unmarshal(raw, &item); err != nil {
			// One item the CLI cannot read must not hide the rest: print what
			// can be read of it and point at --json for the whole item.
			var head struct {
				OccurredAt any `json:"occurredAt"`
				Kind       any `json:"kind"`
			}
			_ = json.Unmarshal(raw, &head) //nolint:errcheck // best effort; a non-object prints blanks
			line := fmt.Sprintf("%s  %-18s  %-16s  %s", tuiutil.SanitizeTerminalLabel(looseJSONLabel(head.OccurredAt)),
				tuiutil.SanitizeTerminalLabel(looseJSONLabel(head.Kind)), "", trailUnreadableNote)
			fmt.Fprintln(w, strings.TrimRight(line, " "))
			continue
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

// trailSessionsResponse is the text view's reading of the sessions; --json
// prints the server's response unchanged, as timeline and stack do.
type trailSessionsResponse struct {
	// Each session is read on its own, so one the CLI cannot read does not
	// hide the rest.
	Sessions []json.RawMessage `json:"sessions"`
}

// trailSessionItem keeps startedAt as the server's string, as the timeline
// keeps occurredAt, so both print times the same way.
type trailSessionItem struct {
	SessionID       string  `json:"sessionId"`
	DisplayName     *string `json:"displayName"`
	Agent           *string `json:"agent"`
	Model           *string `json:"model"`
	StartedAt       *string `json:"startedAt"`
	CheckpointCount int     `json:"checkpointCount"`
}

func newTrailSessionsCmd(mode *trailMode) *cobra.Command {
	return newTrailWorkingReadCmd(mode, "sessions", "List the agent sessions behind a trail's commits",
		"List the agent sessions whose checkpoints are on a trail's branch. 'entire trail resume' restores one.",
		func(cmd *cobra.Command, selected *trailWorkingContext, jsonOut bool) error {
			var raw json.RawMessage
			if err := getTrailRepoIDResource(cmd.Context(), selected, "/sessions", "sessions", &raw); err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var out trailSessionsResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				return fmt.Errorf("decode trail sessions: %w", err)
			}
			if len(out.Sessions) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No sessions on %s\n", selected.description())
				return nil
			}
			return printTable(cmd.OutOrStdout(), []string{"SESSION", "AGENT", colHeaderModel, "STARTED", "CHECKPOINTS", "NAME"}, out.Sessions, trailSessionRow)
		})
}

func trailSessionRow(raw json.RawMessage) []string {
	var s trailSessionItem
	if err := json.Unmarshal(raw, &s); err != nil {
		var head struct {
			SessionID any `json:"sessionId"`
		}
		_ = json.Unmarshal(raw, &head) //nolint:errcheck // best effort; a non-object prints blank
		return []string{tuiutil.SanitizeTerminalLabel(looseJSONLabel(head.SessionID)), "", "", "", "", trailUnreadableNote}
	}
	return []string{tuiutil.SanitizeTerminalLabel(s.SessionID), tuiutil.SanitizeTerminalLabel(stringPtrValue(s.Agent)),
		tuiutil.SanitizeTerminalLabel(stringPtrValue(s.Model)), tuiutil.SanitizeTerminalLabel(stringPtrValue(s.StartedAt)),
		strconv.Itoa(s.CheckpointCount), tuiutil.SanitizeTerminalLabel(stringPtrValue(s.DisplayName))}
}

// trailStackResponse is the text view's reading of the stack; --json prints
// the server's response unchanged, so fields the CLI does not model survive.
type trailStackResponse struct {
	Stack *trailStack `json:"stack"`
}

// trailStack reads position, size, and base loosely and each pull request on
// its own, so one field or entry the CLI cannot read does not hide the rest.
type trailStack struct {
	Position     any               `json:"position"`
	Size         any               `json:"size"`
	Base         any               `json:"base,omitempty"`
	PullRequests []json.RawMessage `json:"pull_requests"`
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
			var raw json.RawMessage
			if err := getTrailRepoIDResource(cmd.Context(), selected, "/stack", "stack", &raw); err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var out trailStackResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				return fmt.Errorf("decode trail stack: %w", err)
			}
			w := cmd.OutOrStdout()
			if out.Stack == nil {
				fmt.Fprintf(w, "%s is not in a pull request stack\n", selected.description())
				return nil
			}
			printTrailStack(w, selected, out.Stack)
			return nil
		})
}

func printTrailStack(w io.Writer, selected *trailWorkingContext, stack *trailStack) {
	base := ""
	if b := looseJSONLabel(stack.Base); b != "" {
		base = " onto " + tuiutil.SanitizeTerminalLabel(b)
	}
	position, size := looseJSONLabel(stack.Position), looseJSONLabel(stack.Size)
	if position == "" {
		position = "?"
	}
	if size == "" {
		size = "?"
	}
	fmt.Fprintf(w, "Position %s of %s%s:\n", tuiutil.SanitizeTerminalLabel(position), tuiutil.SanitizeTerminalLabel(size), base)
	for _, raw := range stack.PullRequests {
		var pr trailStackPullRequest
		if err := json.Unmarshal(raw, &pr); err != nil {
			fmt.Fprintf(w, "  %s\n", trailUnreadableNote)
			continue
		}
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
}
