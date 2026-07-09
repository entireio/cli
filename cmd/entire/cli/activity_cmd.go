package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

const (
	agentUnknown      = "unknown"
	dateUnknown       = "unknown"
	activityTimeframe = "last-month"
	activityLimit     = 1000
	// sessionsOverviewLimit mirrors entire.io's USER_OVERVIEW_RECENT_SESSIONS_LIMIT
	// so the CLI's recent-session list matches the web Overview page's window.
	sessionsOverviewLimit = 50
)

// knownAgents maps normalized agent strings from the API to display IDs.
// Used for the commit list, where per-checkpoint agent strings are free-form.
// The /me/activity endpoint returns already-normalized canonical IDs.
var knownAgents = map[string]string{
	"claude":     "claude",
	"claudecode": "claude",
	"gemini":     "gemini",
	"geminicli":  "gemini",
	"amp":        "amp",
	"codex":      "codex",
	"opencode":   "opencode",
	"copilot":    "copilot",
	"copilotcli": "copilot",
	"pi":         "pi",
	"cursor":     "cursor",
	"droid":      "droid",
	"kiro":       "kiro",
}

func newActivityCmd() *cobra.Command {
	var showCommits bool
	cmd := &cobra.Command{
		Use:   "activity",
		Short: "Show your activity overview",
		Long: "Display your activity overview, repository breakdown, and recent sessions from entire.io.\n\n" +
			"The recent list shows your sessions by default, matching the entire.io Overview page. " +
			"Pass --commits to show recent commits instead.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runActivity(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), showCommits)
		},
	}
	cmd.Flags().BoolVar(&showCommits, "commits", false, "Show recent commits instead of recent sessions")
	return cmd
}

func runActivity(ctx context.Context, w, errW io.Writer, showCommits bool) error {
	return runAuthenticatedActivityAPI(ctx, errW, false, func(ctx context.Context, client *api.Client) error {
		// Non-interactive fallback: piped output or accessibility mode
		if !interactive.IsTerminalWriter(w) || IsAccessibleMode() {
			return runActivityStatic(ctx, w, client, showCommits)
		}

		return runActivityTUI(ctx, client, showCommits)
	})
}

func runActivityStatic(ctx context.Context, w io.Writer, client *api.Client, showCommits bool) error {
	sty := newActivityStyles(w)

	if showCommits {
		activity, commits, err := fetchActivityWith(ctx, client, fetchCommits)
		if err != nil {
			return err
		}
		renderActivityHeader(w, sty, statsFromActivity(activity), activity.Repos, activity.HourlyContributions)
		renderCommitList(w, sty, groupCommitsByDay(commits))
		return nil
	}

	activity, sessions, err := fetchActivityWith(ctx, client, fetchSessions)
	if err != nil {
		return err
	}
	renderActivityHeader(w, sty, statsFromActivity(activity), activity.Repos, activity.HourlyContributions)
	renderSessionList(w, sty, groupSessionsByDay(sessions))
	return nil
}

// statsFromActivity projects the aggregated /me/activity response onto the
// stat-card view model. Shared by the static and TUI render paths.
func statsFromActivity(activity *userActivityResponse) contributionStats {
	return contributionStats{
		Tasks:         activity.Stats.Tasks,
		Throughput:    activity.Stats.Throughput,
		Iteration:     activity.Stats.Iteration,
		ContinuityH:   activity.Stats.ContinuityHours,
		Streak:        activity.Stats.LifetimeStreak,
		CurrentStreak: activity.Stats.LifetimeCurrentStreak,
	}
}

// fetchActivityWith fetches the always-needed /me/activity aggregate
// concurrently with the caller's chosen recent-list fetch (sessions or
// commits). Either fetch failing fails the whole call.
func fetchActivityWith[T any](ctx context.Context, client *api.Client, fetchList func(context.Context, *api.Client) (T, error)) (*userActivityResponse, T, error) {
	var activity *userActivityResponse
	var list T

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		activity, err = fetchActivity(gCtx, client)
		return err
	})
	g.Go(func() error {
		var err error
		list, err = fetchList(gCtx, client)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, list, fmt.Errorf("fetch activity: %w", err)
	}
	return activity, list, nil
}

func fetchActivity(ctx context.Context, client *api.Client) (*userActivityResponse, error) {
	q := url.Values{}
	q.Set("timezone", detectTimezone())
	q.Set("timeframe", activityTimeframe)
	q.Set("limit", strconv.Itoa(activityLimit))
	path := "/api/v1/me/activity?" + q.Encode()

	resp, err := client.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("GET activity: %w", err)
	}
	defer resp.Body.Close()

	if err := api.CheckResponse(resp); err != nil {
		return nil, fmt.Errorf("activity response: %w", err)
	}

	var result userActivityResponse
	if err := api.DecodeJSON(resp, &result); err != nil {
		return nil, fmt.Errorf("decode activity: %w", err)
	}
	return &result, nil
}

func fetchCommits(ctx context.Context, client *api.Client) ([]userCommit, error) {
	path := fmt.Sprintf("/api/v1/me/commits?timeframe=%s&limit=%d", activityTimeframe, activityLimit)
	resp, err := client.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("GET commits: %w", err)
	}
	defer resp.Body.Close()

	if err := api.CheckResponse(resp); err != nil {
		return nil, fmt.Errorf("commits response: %w", err)
	}

	var result userCommitsResponse
	if err := api.DecodeJSON(resp, &result); err != nil {
		return nil, fmt.Errorf("decode commits: %w", err)
	}
	return result.Commits, nil
}

func fetchSessions(ctx context.Context, client *api.Client) ([]userSession, error) {
	q := url.Values{}
	q.Set("timeframe", activityTimeframe)
	q.Set("limit", strconv.Itoa(sessionsOverviewLimit))
	path := "/api/v1/me/sessions?" + q.Encode()

	resp, err := client.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("GET sessions: %w", err)
	}
	defer resp.Body.Close()

	if err := api.CheckResponse(resp); err != nil {
		return nil, fmt.Errorf("sessions response: %w", err)
	}

	var result userSessionsResponse
	if err := api.DecodeJSON(resp, &result); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}
	return result.Sessions, nil
}

// detectTimezone returns a best-effort timezone name for the current host.
// Order: $TZ → /etc/localtime symlink → time.Local → "UTC" as last resort.
// A candidate that fails normalization is skipped (not forwarded, not coerced
// to UTC), so a bogus $TZ on a correctly-configured box still yields the
// system timezone from /etc/localtime. The server is the canonical authority
// for what counts as a valid zone and falls back to UTC for anything it
// doesn't recognize, so we only do enough validation to avoid sending
// obvious garbage (paths, POSIX forms Go can't load, the "Local" sentinel).
func detectTimezone() string {
	if tz := normalizeTimezone(os.Getenv("TZ")); tz != "" {
		return tz
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if tz := normalizeTimezone(link); tz != "" {
			return tz
		}
	}
	if tz := normalizeTimezone(time.Local.String()); tz != "" {
		return tz
	}
	return "UTC"
}

// normalizeTimezone returns a name Go can load as a time zone, or "" if the
// input can't be resolved. It strips the POSIX ":" prefix and zoneinfo path
// prefix, then requires time.LoadLocation to succeed.
//
// This is not strict IANA-only validation: Go's LoadLocation accepts legacy
// aliases like EST5EDT, GMT0, and PST8PDT in addition to Area/Location
// names. Those may or may not be canonically understood by the server — if
// the server doesn't recognize one, it falls back to UTC on its end. We
// accept that mild mis-bucketing risk as the price of a simple check that
// catches the common failure modes (paths, unknown POSIX forms like UTC0,
// typos, the "Local" sentinel).
func normalizeTimezone(raw string) string {
	name := strings.TrimPrefix(raw, ":")
	const marker = "/zoneinfo/"
	if idx := strings.LastIndex(name, marker); idx >= 0 {
		name = name[idx+len(marker):]
	}
	if name == "" || name == "Local" {
		return ""
	}
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
}

// localDayOf returns the local "2006-01-02" day of an RFC3339 timestamp, or
// dateUnknown when the pointer is nil/empty or the value can't be parsed.
func localDayOf(ts *string) string {
	if ts == nil || *ts == "" {
		return dateUnknown
	}
	t, err := parseFlexibleTime(*ts)
	if err != nil {
		return dateUnknown
	}
	return t.Local().Format("2006-01-02")
}

// groupItemsByDay buckets items by a local-day key, returning the distinct keys
// ordered newest-first (with dateUnknown pushed to the end) plus the by-day map.
// Shared by the commit and session day-grouped lists.
func groupItemsByDay[T any](items []T, dayOf func(T) string) (order []string, byDate map[string][]T) {
	byDate = make(map[string][]T)
	for _, it := range items {
		date := dayOf(it)
		if _, exists := byDate[date]; !exists {
			order = append(order, date)
		}
		byDate[date] = append(byDate[date], it)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i] == dateUnknown {
			return false
		}
		if order[j] == dateUnknown {
			return true
		}
		return order[i] > order[j]
	})
	return order, byDate
}

func groupCommitsByDay(commits []userCommit) []commitDay {
	order, byDate := groupItemsByDay(commits, func(c userCommit) string {
		return localDayOf(c.CommitDate)
	})
	result := make([]commitDay, 0, len(order))
	for _, d := range order {
		result = append(result, commitDay{Date: d, Commits: byDate[d]})
	}
	return result
}

func groupSessionsByDay(sessions []userSession) []sessionDay {
	order, byDate := groupItemsByDay(sessions, func(s userSession) string {
		return localDayOf(&s.LastActivityAt)
	})
	result := make([]sessionDay, 0, len(order))
	for _, d := range order {
		result = append(result, sessionDay{Date: d, Sessions: byDate[d]})
	}
	return result
}

func normalizeAgentString(s string) string {
	if s == "" {
		return agentUnknown
	}

	var sb strings.Builder
	for _, r := range s {
		if r == ' ' || r == '-' || r == '_' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			sb.WriteByte(byte(r + 32))
		} else {
			sb.WriteRune(r)
		}
	}
	lower := sb.String()

	if id, ok := knownAgents[lower]; ok {
		return id
	}

	for _, suffix := range []string{"code", "cli"} {
		if len(lower) > len(suffix) && lower[len(lower)-len(suffix):] == suffix {
			if id, ok := knownAgents[lower[:len(lower)-len(suffix)]]; ok {
				return id
			}
		}
	}

	if strings.HasPrefix(lower, "factoryaidroid") {
		return "droid"
	}

	return agentUnknown
}

// parseFlexibleTime tries RFC3339, then RFC3339Nano.
func parseFlexibleTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
		}
	}
	return t, nil
}
