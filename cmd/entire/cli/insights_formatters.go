package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// formatInsightsTerminal formats insights for terminal output.
func formatInsightsTerminal(report *InsightsReport, cacheStats CacheStats) ([]byte, error) {
	var sb strings.Builder

	// Header
	sb.WriteString("═══════════════════════════════════════════════════════\n")
	sb.WriteString("  Entire Insights\n")
	sb.WriteString("═══════════════════════════════════════════════════════\n\n")

	// Time range
	if !report.StartTime.IsZero() && !report.EndTime.IsZero() {
		sb.WriteString(fmt.Sprintf("Period: %s to %s\n",
			report.StartTime.Format("2006-01-02"),
			report.EndTime.Format("2006-01-02")))
	}

	// Cache stats
	if cacheStats.TotalSessions > 0 {
		sb.WriteString(fmt.Sprintf("Analysis: %d new sessions, %d cached\n",
			cacheStats.NewSessions, cacheStats.CachedSessions))
	}
	sb.WriteString("\n")

	// Summary stats
	sb.WriteString("SUMMARY\n")
	sb.WriteString("───────────────────────────────────────────────────────\n")
	sb.WriteString(fmt.Sprintf("  Sessions:        %s\n", formatNumber(report.TotalSessions)))
	sb.WriteString(fmt.Sprintf("  Total time:      %s\n", formatDuration(report.TotalTime)))
	sb.WriteString(fmt.Sprintf("  Tokens:          %s\n", formatNumber(report.TotalTokens)))
	sb.WriteString(fmt.Sprintf("  Estimated cost:  $%.2f\n", report.EstimatedCost))
	sb.WriteString(fmt.Sprintf("  Files modified:  %s\n", formatNumber(report.FilesModified)))
	sb.WriteString("\n")

	// Agent breakdown
	if len(report.AgentStats) > 0 {
		sb.WriteString("AGENTS\n")
		sb.WriteString("───────────────────────────────────────────────────────\n")
		for _, stat := range report.AgentStats {
			agentName := string(stat.Agent)
			if agentName == "" {
				agentName = "unknown"
			}
			sb.WriteString(fmt.Sprintf("  %-20s %3d sessions  %s tokens  %.1f hours\n",
				agentName,
				stat.Sessions,
				formatNumber(stat.Tokens),
				stat.Hours))
		}
		sb.WriteString("\n")
	}

	// Top tools
	if len(report.TopTools) > 0 {
		sb.WriteString("TOP TOOLS\n")
		sb.WriteString("───────────────────────────────────────────────────────\n")
		for _, tool := range report.TopTools {
			sb.WriteString(fmt.Sprintf("  %-30s %s uses\n",
				tool.Name,
				formatNumber(tool.Count)))
		}
		sb.WriteString("\n")
	}

	// Peak hours
	sb.WriteString("PEAK HOURS\n")
	sb.WriteString("───────────────────────────────────────────────────────\n")
	sb.WriteString(renderHeatmap(report.PeakHours))
	sb.WriteString("\n")

	// Recent sessions
	if len(report.RecentSessions) > 0 {
		sb.WriteString("RECENT SESSIONS\n")
		sb.WriteString("───────────────────────────────────────────────────────\n")
		for _, sess := range report.RecentSessions {
			desc := sess.Description
			if desc == "" {
				desc = sess.ID
			}
			if len(desc) > 50 {
				desc = desc[:47] + "..."
			}
			sb.WriteString(fmt.Sprintf("  %s  %s  %s  %s tokens\n",
				sess.StartTime.Format("01-02 15:04"),
				formatDuration(sess.Duration),
				desc,
				formatNumber(sess.Tokens)))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("───────────────────────────────────────────────────────\n")

	return []byte(sb.String()), nil
}

// formatInsightsJSON formats insights as JSON.
func formatInsightsJSON(report *InsightsReport) ([]byte, error) {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON: %w", err)
	}
	return data, nil
}

// formatInsightsMarkdown formats insights as Markdown.
func formatInsightsMarkdown(report *InsightsReport) ([]byte, error) {
	var sb strings.Builder

	sb.WriteString("# Entire Insights\n\n")

	// Time range
	if !report.StartTime.IsZero() && !report.EndTime.IsZero() {
		sb.WriteString(fmt.Sprintf("**Period:** %s to %s\n\n",
			report.StartTime.Format("2006-01-02"),
			report.EndTime.Format("2006-01-02")))
	}

	// Summary stats
	sb.WriteString("## Summary\n\n")
	sb.WriteString(fmt.Sprintf("- **Sessions:** %s\n", formatNumber(report.TotalSessions)))
	sb.WriteString(fmt.Sprintf("- **Total time:** %s\n", formatDuration(report.TotalTime)))
	sb.WriteString(fmt.Sprintf("- **Tokens:** %s\n", formatNumber(report.TotalTokens)))
	sb.WriteString(fmt.Sprintf("- **Estimated cost:** $%.2f\n", report.EstimatedCost))
	sb.WriteString(fmt.Sprintf("- **Files modified:** %s\n\n", formatNumber(report.FilesModified)))

	// Agent breakdown
	if len(report.AgentStats) > 0 {
		sb.WriteString("## Agents\n\n")
		sb.WriteString("| Agent | Sessions | Tokens | Hours |\n")
		sb.WriteString("|-------|----------|--------|-------|\n")
		for _, stat := range report.AgentStats {
			agentName := string(stat.Agent)
			if agentName == "" {
				agentName = "unknown"
			}
			sb.WriteString(fmt.Sprintf("| %s | %d | %s | %.1f |\n",
				agentName,
				stat.Sessions,
				formatNumber(stat.Tokens),
				stat.Hours))
		}
		sb.WriteString("\n")
	}

	// Top tools
	if len(report.TopTools) > 0 {
		sb.WriteString("## Top Tools\n\n")
		sb.WriteString("| Tool | Uses |\n")
		sb.WriteString("|------|------|\n")
		for _, tool := range report.TopTools {
			sb.WriteString(fmt.Sprintf("| %s | %s |\n",
				tool.Name,
				formatNumber(tool.Count)))
		}
		sb.WriteString("\n")
	}

	// Peak hours
	sb.WriteString("## Peak Hours\n\n")
	sb.WriteString("```\n")
	sb.WriteString(renderHeatmap(report.PeakHours))
	sb.WriteString("```\n\n")

	// Recent sessions
	if len(report.RecentSessions) > 0 {
		sb.WriteString("## Recent Sessions\n\n")
		sb.WriteString("| Date | Duration | Description | Tokens |\n")
		sb.WriteString("|------|----------|-------------|--------|\n")
		for _, sess := range report.RecentSessions {
			desc := sess.Description
			if desc == "" {
				desc = sess.ID
			}
			if len(desc) > 50 {
				desc = desc[:47] + "..."
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				sess.StartTime.Format("01-02 15:04"),
				formatDuration(sess.Duration),
				desc,
				formatNumber(sess.Tokens)))
		}
		sb.WriteString("\n")
	}

	return []byte(sb.String()), nil
}

// formatInsightsHTML formats insights as HTML with a light, minimal design.
func formatInsightsHTML(report *InsightsReport) ([]byte, error) {
	var sb strings.Builder

	// Get time-based greeting
	now := time.Now()
	hour := now.Hour()
	greeting := "Evening"
	if hour < 12 {
		greeting = "Morning"
	} else if hour < 18 {
		greeting = "Afternoon"
	}

	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Entire Insights</title>
    <style>
        * {
            margin: 0;
            padding: 0;
            box-sizing: border-box;
        }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, sans-serif;
            background: #ffffff;
            color: #1f2328;
            display: flex;
            min-height: 100vh;
        }
        /* Left Sidebar */
        .sidebar {
            width: 240px;
            padding: 32px 16px;
            background: #ffffff;
            border-right: 1px solid #d0d7de;
        }
        .sidebar nav {
            margin-top: 24px;
        }
        .nav-item {
            display: block;
            padding: 8px 12px;
            color: #1f2328;
            text-decoration: none;
            border-radius: 6px;
            margin-bottom: 4px;
            font-size: 14px;
        }
        .nav-item.active {
            background: #f6f8fa;
            font-weight: 600;
        }
        .nav-item:hover {
            background: #f6f8fa;
        }
        /* Main Content */
        .main {
            flex: 1;
            padding: 32px 48px;
            max-width: 1200px;
        }
        /* Greeting Header */
        .greeting {
            font-size: 24px;
            font-weight: 300;
            color: #656d76;
            margin-bottom: 32px;
        }
        /* Stats Grid */
        .stats-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
            gap: 24px;
            margin-bottom: 48px;
        }
        .stat-card {
            background: #ffffff;
            padding: 24px;
        }
        .stat-label {
            font-size: 11px;
            color: #656d76;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            margin-bottom: 8px;
        }
        .stat-value {
            font-family: 'SF Mono', Monaco, 'Cascadia Code', 'Roboto Mono', Consolas, monospace;
            font-size: 32px;
            font-weight: 500;
            color: #1f2328;
            line-height: 1.2;
        }
        /* Section Headers */
        .section {
            margin-bottom: 48px;
        }
        .section-title {
            font-size: 16px;
            font-weight: 600;
            color: #1f2328;
            margin-bottom: 16px;
        }
        /* Activity Chart (Scatter/Bubble) */
        .chart-container {
            background: #ffffff;
            padding: 24px;
            position: relative;
            height: 400px;
        }
        .chart-scatter {
            position: relative;
            width: 100%;
            height: 100%;
            display: grid;
            grid-template-columns: 40px 1fr;
            gap: 8px;
        }
        .chart-y-axis {
            display: flex;
            flex-direction: column-reverse;
            justify-content: space-between;
            padding: 20px 0;
        }
        .chart-y-label {
            font-size: 11px;
            color: #656d76;
            text-align: right;
            font-family: monospace;
        }
        .chart-plot {
            position: relative;
            border-left: 1px solid #d0d7de;
            border-bottom: 1px solid #d0d7de;
        }
        .chart-dot {
            position: absolute;
            width: 8px;
            height: 8px;
            background: #fb923c;
            border-radius: 50%;
            opacity: 0.7;
            transition: opacity 0.2s;
        }
        .chart-dot:hover {
            opacity: 1;
            transform: scale(1.5);
        }
        /* GitHub-style Table */
        .table-container {
            background: #ffffff;
        }
        table {
            width: 100%;
            border-collapse: collapse;
            font-size: 14px;
        }
        th {
            text-align: left;
            padding: 8px 16px;
            font-weight: 600;
            color: #656d76;
            font-size: 12px;
            border-bottom: 1px solid #d0d7de;
        }
        td {
            padding: 8px 16px;
            border-bottom: 1px solid #d0d7de;
        }
        tr:last-child td {
            border-bottom: none;
        }
        /* Badges */
        .badge {
            display: inline-block;
            padding: 2px 8px;
            border-radius: 12px;
            font-size: 12px;
            font-weight: 500;
        }
        .badge-claude {
            background: #fb923c;
            color: #ffffff;
        }
        .badge-gemini {
            background: #4285f4;
            color: #ffffff;
        }
        /* Diff Stats */
        .diff-stat {
            display: inline-flex;
            align-items: center;
            gap: 8px;
            font-family: monospace;
            font-size: 12px;
        }
        .diff-add {
            color: #1a7f37;
        }
        .diff-del {
            color: #cf222e;
        }
        /* Minimal borders, lots of whitespace */
        .spacer {
            height: 24px;
        }
    </style>
</head>
<body>
    <!-- Left Sidebar -->
    <div class="sidebar">
        <div style="font-weight: 600; font-size: 14px; padding: 0 12px;">Entire</div>
        <nav>
            <a href="#" class="nav-item active">Overview</a>
            <a href="#" class="nav-item">Repositories</a>
        </nav>
    </div>

    <!-- Main Content -->
    <div class="main">
        <!-- Greeting Header -->
        <div class="greeting">`)
	sb.WriteString(greeting)
	sb.WriteString(`, developer</div>

        <!-- Stats Grid -->
        <div class="stats-grid">
            <div class="stat-card">
                <div class="stat-label">Sessions</div>
                <div class="stat-value">`)
	sb.WriteString(formatNumber(report.TotalSessions))
	sb.WriteString(`</div>
            </div>
            <div class="stat-card">
                <div class="stat-label">Total Time</div>
                <div class="stat-value">`)
	sb.WriteString(formatDuration(report.TotalTime))
	sb.WriteString(`</div>
            </div>
            <div class="stat-card">
                <div class="stat-label">Tokens</div>
                <div class="stat-value">`)
	sb.WriteString(formatNumber(report.TotalTokens))
	sb.WriteString(`</div>
            </div>
            <div class="stat-card">
                <div class="stat-label">Cost</div>
                <div class="stat-value">$`)
	sb.WriteString(fmt.Sprintf("%.2f", report.EstimatedCost))
	sb.WriteString(`</div>
            </div>
        </div>

        <!-- Activity Chart (Scatter/Bubble) -->
        <div class="section">
            <div class="section-title">Activity Pattern</div>
            <div class="chart-container">
                <div class="chart-scatter">
                    <div class="chart-y-axis">`)

	// Y-axis labels (hours 0-23)
	for hour := 23; hour >= 0; hour-- {
		if hour%4 == 0 {
			sb.WriteString(fmt.Sprintf(`
                        <div class="chart-y-label">%02d:00</div>`, hour))
		} else {
			sb.WriteString(`
                        <div class="chart-y-label"></div>`)
		}
	}

	sb.WriteString(`
                    </div>
                    <div class="chart-plot">`)

	// Generate scatter dots based on daily activity and peak hours
	// Map dates to X position, hours to Y position
	if len(report.DailyActivity) > 0 {
		// For each day with activity, place dots for each hour with activity
		for dayIdx := range report.DailyActivity {
			xPercent := float64(dayIdx) / float64(len(report.DailyActivity)) * 100

			// For this day, find which hours had activity
			// (We'll distribute the day's activity across the peak hours)
			for hour := range 24 {
				if report.PeakHours[hour] > 0 {
					// Y position (inverted - hour 0 at bottom, hour 23 at top)
					yPercent := (1.0 - float64(hour)/24.0) * 100

					// Bubble size based on activity count (not implemented in this simple version)
					sb.WriteString(fmt.Sprintf(`
                        <div class="chart-dot" style="left: %.1f%%; bottom: %.1f%%;"></div>`,
						xPercent, yPercent))
				}
			}
		}
	}

	sb.WriteString(`
                    </div>
                </div>
            </div>
        </div>

        <div class="spacer"></div>

        <!-- Agents Table -->`)

	if len(report.AgentStats) > 0 {
		sb.WriteString(`
        <div class="section">
            <div class="section-title">Agents</div>
            <div class="table-container">
                <table>
                    <thead>
                        <tr>
                            <th>Agent</th>
                            <th>Sessions</th>
                            <th>Tokens</th>
                            <th>Hours</th>
                        </tr>
                    </thead>
                    <tbody>`)

		for _, stat := range report.AgentStats {
			agentName := string(stat.Agent)
			badgeClass := "badge"
			if strings.Contains(strings.ToLower(agentName), "claude") {
				badgeClass = "badge badge-claude"
				agentName = "Claude Code"
			} else if strings.Contains(strings.ToLower(agentName), "gemini") {
				badgeClass = "badge badge-gemini"
				agentName = "Gemini CLI"
			}

			sb.WriteString(fmt.Sprintf(`
                        <tr>
                            <td><span class="%s">%s</span></td>
                            <td>%d</td>
                            <td>%s</td>
                            <td>%.1fh</td>
                        </tr>`,
				badgeClass,
				agentName,
				stat.Sessions,
				formatNumber(stat.Tokens),
				stat.Hours))
		}

		sb.WriteString(`
                    </tbody>
                </table>
            </div>
        </div>

        <div class="spacer"></div>`)
	}

	// Top Tools Table
	if len(report.TopTools) > 0 {
		sb.WriteString(`
        <div class="section">
            <div class="section-title">Top Tools</div>
            <div class="table-container">
                <table>
                    <thead>
                        <tr>
                            <th>Tool</th>
                            <th>Uses</th>
                        </tr>
                    </thead>
                    <tbody>`)

		for _, tool := range report.TopTools {
			sb.WriteString(fmt.Sprintf(`
                        <tr>
                            <td><code>%s</code></td>
                            <td>%s</td>
                        </tr>`,
				tool.Name,
				formatNumber(tool.Count)))
		}

		sb.WriteString(`
                    </tbody>
                </table>
            </div>
        </div>

        <div class="spacer"></div>`)
	}

	// Recent Sessions (GitHub-style with diff stats)
	if len(report.RecentSessions) > 0 {
		sb.WriteString(`
        <div class="section">
            <div class="section-title">Recent Sessions</div>
            <div class="table-container">
                <table>
                    <thead>
                        <tr>
                            <th>Date</th>
                            <th>Description</th>
                            <th>Duration</th>
                            <th>Tokens</th>
                        </tr>
                    </thead>
                    <tbody>`)

		for _, sess := range report.RecentSessions {
			desc := sess.Description
			if desc == "" {
				desc = sess.ID
			}
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}

			// Estimate diff stats (mock data for visualization)
			// In a real implementation, these would come from the session data
			addedLines := sess.Tokens / 100   // rough estimate
			deletedLines := sess.Tokens / 200 // rough estimate

			sb.WriteString(fmt.Sprintf(`
                        <tr>
                            <td>%s</td>
                            <td>%s</td>
                            <td>%s</td>
                            <td>
                                <div class="diff-stat">
                                    <span class="diff-add">+%s</span>
                                    <span class="diff-del">-%s</span>
                                </div>
                            </td>
                        </tr>`,
				sess.StartTime.Format("Jan 2, 15:04"),
				desc,
				formatDuration(sess.Duration),
				formatNumber(addedLines),
				formatNumber(deletedLines)))
		}

		sb.WriteString(`
                    </tbody>
                </table>
            </div>
        </div>`)
	}

	sb.WriteString(`
    </div>
</body>
</html>`)

	return []byte(sb.String()), nil
}

// formatDuration formats a duration for display.
func formatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// formatNumber formats a number with thousands separators.
func formatNumber(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}

	s := strconv.Itoa(n)
	var result strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result.WriteRune(',')
		}
		result.WriteRune(c)
	}
	return result.String()
}

// renderHeatmap renders a 24-hour heatmap.
func renderHeatmap(hours [24]int) string {
	if len(hours) == 0 {
		return "(no data)\n"
	}

	// Find max for scaling
	max := 0
	for _, count := range hours {
		if count > max {
			max = count
		}
	}

	if max == 0 {
		return "  No activity\n"
	}

	var sb strings.Builder

	// Two rows: 00-11 and 12-23
	sb.WriteString("  ")
	for hour := range 12 {
		sb.WriteString(fmt.Sprintf("%2d ", hour))
	}
	sb.WriteString("\n  ")
	for hour := range 12 {
		intensity := float64(hours[hour]) / float64(max)
		sb.WriteString(barChar(intensity))
		sb.WriteString("  ")
	}
	sb.WriteString("\n\n  ")
	for hour := 12; hour < 24; hour++ {
		sb.WriteString(fmt.Sprintf("%2d ", hour))
	}
	sb.WriteString("\n  ")
	for hour := 12; hour < 24; hour++ {
		intensity := float64(hours[hour]) / float64(max)
		sb.WriteString(barChar(intensity))
		sb.WriteString("  ")
	}
	sb.WriteString("\n")

	return sb.String()
}

// barChar returns a bar character based on intensity (0.0 to 1.0).
func barChar(intensity float64) string {
	if intensity < 0.1 {
		return "░"
	} else if intensity < 0.3 {
		return "▒"
	} else if intensity < 0.6 {
		return "▓"
	}
	return "█"
}
