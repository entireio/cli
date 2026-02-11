//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInsights_NoSessions(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Without any sessions, insights should show zero stats
		output, err := env.RunCLIWithError("insights")

		if err != nil {
			t.Errorf("expected success for empty insights, got error: %v, output: %s", err, output)
			return
		}

		// Should show header
		if !strings.Contains(output, "Entire Insights") {
			t.Errorf("expected 'Entire Insights' header in output, got: %s", output)
		}

		// Should show zero sessions
		if !strings.Contains(output, "Sessions:") {
			t.Errorf("expected 'Sessions:' in output, got: %s", output)
		}
	})
}

func TestInsights_JSONOutput(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Test JSON output format
		output, err := env.RunCLIWithError("insights", "--json")

		if err != nil {
			t.Errorf("expected success for JSON output, got error: %v, output: %s", err, output)
			return
		}

		// Parse as JSON to validate structure
		var result map[string]interface{}
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Errorf("failed to parse JSON output: %v, output: %s", err, output)
			return
		}

		// Check for expected fields
		expectedFields := []string{
			"TotalSessions",
			"TotalCheckpoints",
			"TotalTime",
			"TotalTokens",
			"EstimatedCost",
		}

		for _, field := range expectedFields {
			if _, ok := result[field]; !ok {
				t.Errorf("expected field %s in JSON output, got: %v", field, result)
			}
		}
	})
}

func TestInsights_PeriodFilters(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		tests := []string{"week", "month", "year"}

		for _, period := range tests {
			output, err := env.RunCLIWithError("insights", "--period", period)

			if err != nil {
				t.Errorf("period %s: expected success, got error: %v, output: %s", period, err, output)
				continue
			}

			// Should show period in output
			if !strings.Contains(output, "Period:") {
				t.Errorf("period %s: expected 'Period:' in output, got: %s", period, output)
			}
		}
	})
}

func TestInsights_InvalidPeriod(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		output, err := env.RunCLIWithError("insights", "--period", "invalid")

		if err == nil {
			t.Errorf("expected error for invalid period, got output: %s", output)
			return
		}

		if !strings.Contains(output, "invalid period") {
			t.Errorf("expected 'invalid period' error, got: %s", output)
		}
	})
}

func TestInsights_ExportJSON(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		output, err := env.RunCLIWithError("insights", "--export", "--format", "json")

		if err != nil {
			t.Errorf("expected success for export, got error: %v, output: %s", err, output)
			return
		}

		// Parse as JSON
		var result map[string]interface{}
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Errorf("failed to parse exported JSON: %v, output: %s", err, output)
		}
	})
}

func TestInsights_ExportMarkdown(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		output, err := env.RunCLIWithError("insights", "--export", "--format", "markdown")

		if err != nil {
			t.Errorf("expected success for markdown export, got error: %v, output: %s", err, output)
			return
		}

		// Should contain markdown headers
		if !strings.Contains(output, "# Entire Insights") {
			t.Errorf("expected markdown header in output, got: %s", output)
		}

		if !strings.Contains(output, "## Summary") {
			t.Errorf("expected '## Summary' section in output, got: %s", output)
		}
	})
}

func TestInsights_ExportHTML(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		output, err := env.RunCLIWithError("insights", "--export", "--format", "html")

		if err != nil {
			t.Errorf("expected success for HTML export, got error: %v, output: %s", err, output)
			return
		}

		// Should contain HTML structure
		if !strings.Contains(output, "<!DOCTYPE html>") {
			t.Errorf("expected HTML doctype in output, got: %s", output)
		}

		// Check for design elements
		expectedElements := []string{
			"sidebar",       // Left sidebar
			"nav-item",      // Navigation items
			"greeting",      // Greeting header
			"stat-card",     // Stat cards
			"chart-scatter", // Scatter chart
			"badge-claude",  // Claude Code badge
			"diff-stat",     // Diff stats
		}

		for _, elem := range expectedElements {
			if !strings.Contains(output, elem) {
				t.Errorf("expected '%s' element in HTML output", elem)
			}
		}

		// Check for greeting variants
		hasGreeting := strings.Contains(output, "Morning, developer") ||
			strings.Contains(output, "Afternoon, developer") ||
			strings.Contains(output, "Evening, developer")

		if !hasGreeting {
			t.Errorf("expected time-based greeting in HTML output")
		}
	})
}

func TestInsights_MutualExclusivity(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// --json and --export are mutually exclusive
		output, err := env.RunCLIWithError("insights", "--json", "--export", "--format", "json")

		if err == nil {
			t.Errorf("expected error for --json and --export together, got output: %s", output)
			return
		}

		if !strings.Contains(output, "mutually exclusive") {
			t.Errorf("expected 'mutually exclusive' error, got: %s", output)
		}
	})
}

func TestInsights_FormatWithoutExport(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// --format requires --export
		output, err := env.RunCLIWithError("insights", "--format", "json")

		if err == nil {
			t.Errorf("expected error for --format without --export, got output: %s", output)
			return
		}

		if !strings.Contains(output, "require --export") {
			t.Errorf("expected 'require --export' error, got: %s", output)
		}
	})
}

func TestInsights_InvalidFormat(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		output, err := env.RunCLIWithError("insights", "--export", "--format", "invalid")

		if err == nil {
			t.Errorf("expected error for invalid format, got output: %s", output)
			return
		}

		if !strings.Contains(output, "invalid format") {
			t.Errorf("expected 'invalid format' error, got: %s", output)
		}
	})
}
