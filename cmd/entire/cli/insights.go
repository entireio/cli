package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/spf13/cobra"
)

const (
	formatJSON     = "json"
	formatMarkdown = "markdown"
	formatHTML     = "html"
	periodWeek     = "week"
	periodMonth    = "month"
	periodYear     = "year"
)

func newInsightsCmd() *cobra.Command {
	var periodFlag string
	var repoFlag string
	var agentFlag string
	var formatJSONFlag bool
	var exportFlag bool
	var formatFlag string
	var outputFlag string
	var noCacheFlag bool

	cmd := &cobra.Command{
		Use:   "insights",
		Short: "Show session analytics and usage patterns",
		Long: `Insights provides analytics across your AI-assisted development sessions.

See metrics like session counts, token usage, estimated costs, tool usage,
and activity patterns to understand your AI development workflow.

Time periods:
  --period week   Last 7 days (default)
  --period month  Last 30 days
  --period year   Last 365 days

Filtering:
  --agent TYPE    Filter by agent type (e.g., claude-code, gemini-cli)
  --repo NAME     Filter by repository name (future)

Output formats:
  Default:         Human-readable terminal output
  --json           JSON to stdout
  --export         Structured export (requires --format)
  --format FORMAT  Export format: json, markdown, html
  --output FILE    Write to file instead of stdout

Performance:
  --no-cache      Force full re-analysis (ignore cache)

Examples:
  entire insights                                    # Week view
  entire insights --period month                     # Month view
  entire insights --agent claude-code                # Filter by agent
  entire insights --export --format json -o stats.json
  entire insights --export --format markdown -o INSIGHTS.md

Note: Uses incremental caching for fast analysis. First run analyzes all sessions,
subsequent runs only process new sessions since last run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if Entire is disabled
			if checkDisabledGuard(cmd.OutOrStdout()) {
				return nil
			}

			// Validate flag dependencies
			if formatJSONFlag && exportFlag {
				return errors.New("--formatJSON and --export are mutually exclusive")
			}
			if (formatFlag != "" || outputFlag != "") && !exportFlag {
				return errors.New("--format and --output require --export flag")
			}

			// Validate period
			if periodFlag != "" && periodFlag != periodWeek && periodFlag != periodMonth && periodFlag != periodYear {
				return fmt.Errorf("invalid period: %s (must be %s, %s, or %s)", periodFlag, periodWeek, periodMonth, periodYear)
			}

			// Validate format
			if exportFlag && formatFlag == "" {
				formatFlag = formatJSON // Default format
			}
			if formatFlag != "" && formatFlag != formatJSON && formatFlag != formatMarkdown && formatFlag != formatHTML {
				return fmt.Errorf("invalid format: %s (must be %s, %s, or %s)", formatFlag, formatJSON, formatMarkdown, formatHTML)
			}

			return runInsights(cmd.OutOrStdout(), cmd.ErrOrStderr(), periodFlag, repoFlag, agentFlag, formatJSONFlag, exportFlag, formatFlag, outputFlag, noCacheFlag)
		},
	}

	cmd.Flags().StringVar(&periodFlag, "period", periodWeek, "Time period: week, month, year")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "Filter by repository name")
	cmd.Flags().StringVar(&agentFlag, "agent", "", "Filter by agent type")
	cmd.Flags().BoolVar(&formatJSONFlag, "formatJSON", false, "Output as JSON to stdout")
	cmd.Flags().BoolVar(&exportFlag, "export", false, "Export in structured format")
	cmd.Flags().StringVar(&formatFlag, "format", "", "Export format: json, markdown, html (requires --export)")
	cmd.Flags().StringVarP(&outputFlag, "output", "o", "", "Write to file instead of stdout")
	cmd.Flags().BoolVar(&noCacheFlag, "no-cache", false, "Force full re-analysis (ignore cache)")

	cmd.MarkFlagsMutuallyExclusive("formatJSON", "export")

	return cmd
}

// runInsights executes the insights command.
func runInsights(w, _ io.Writer, period, repoFilter, agentFilter string, formatJSONOut, export bool, format, outputFile string, noCache bool) error {
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Get current repository name
	repoName, err := extractRepoName(repo)
	if err != nil {
		logging.Warn(context.Background(), "failed to extract repo name", "error", err)
		repoName = unknownStrategyName
	}

	// If repo filter is set and doesn't match current repo, return early
	if repoFilter != "" && repoFilter != repoName {
		fmt.Fprintf(w, "No data for repository: %s (current: %s)\n", repoFilter, repoName)
		return nil
	}

	// List all sessions
	sessions, err := strategy.ListSessions()
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	// Build query
	query := buildInsightsQuery(period, agentFilter)

	// Load cache if not disabled
	var cache *InsightsCache
	if !noCache {
		cache, err = loadCache(repoName)
		if err != nil {
			logging.Warn(context.Background(), "failed to load cache", "error", err)
			cache = nil
		}
	}

	// Compute insights
	report, cacheStats, err := computeInsights(repo, sessions, query, cache, noCache)
	if err != nil {
		return fmt.Errorf("failed to compute insights: %w", err)
	}

	// Save cache if enabled
	if !noCache && cache != nil {
		if err := saveCache(repoName, cache); err != nil {
			logging.Warn(context.Background(), "failed to save cache", "error", err)
		}
	}

	// Format output
	var output []byte
	switch {
	case formatJSONOut:
		output, err = formatInsightsJSON(report)
	case export:
		switch format {
		case formatJSON:
			output, err = formatInsightsJSON(report)
		case formatMarkdown:
			output, err = formatInsightsMarkdown(report)
		case formatHTML:
			output, err = formatInsightsHTML(report)
		default:
			return fmt.Errorf("unsupported format: %s", format)
		}
	default:
		output, err = formatInsightsTerminal(report, cacheStats)
	}

	if err != nil {
		return fmt.Errorf("failed to format output: %w", err)
	}

	// Write output
	if outputFile == "" {
		if _, err = w.Write(output); err != nil {
			return fmt.Errorf("failed to write output: %w", err)
		}
	} else {
		//nolint:gosec // G306: insights file is for user's own analysis, 0o644 is appropriate
		if err := os.WriteFile(outputFile, output, 0o644); err != nil {
			return fmt.Errorf("failed to write file: %w", err)
		}
		fmt.Fprintf(w, "Exported to %s\n", outputFile)
	}

	return nil
}

// extractRepoName extracts the repository name from git remote.
func extractRepoName(repo *git.Repository) (string, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return "", fmt.Errorf("failed to get repo root: %w", err)
	}

	remotes, err := repo.Remotes()
	//nolint:nilerr // Intentionally ignoring error - fallback to directory name
	if len(remotes) == 0 || err != nil {
		// Fallback to directory name (ignore error, use directory name as fallback)
		return filepath.Base(repoRoot), nil
	}

	// Parse first remote URL
	config := remotes[0].Config()
	if len(config.URLs) == 0 {
		return filepath.Base(repoRoot), nil
	}

	url := config.URLs[0]
	// Extract repo name from URL (handle both HTTPS and SSH)
	// https://github.com/entireio/cli.git -> cli
	// git@github.com:entireio/cli.git -> cli
	name := url
	if idx := lastIndexOf(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if idx := lastIndexOf(name, ":"); idx >= 0 {
		name = name[idx+1:]
	}
	if len(name) > 4 && name[len(name)-4:] == ".git" {
		name = name[:len(name)-4]
	}
	return name, nil
}

// lastIndexOf returns the last index of substr in s, or -1 if not found.
func lastIndexOf(s, substr string) int {
	for i := len(s) - len(substr); i >= 0; i-- {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// buildInsightsQuery builds an InsightsQuery from flags.
func buildInsightsQuery(period, agentFilter string) InsightsQuery {
	query := InsightsQuery{
		AgentFilter: agent.AgentType(agentFilter),
	}

	// Set time range based on period
	query.StartTime, query.EndTime = applyPeriodFilter(period)

	return query
}
