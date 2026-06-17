package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

type TestEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

type subtest struct {
	name    string
	action  string // pass, fail, skip
	elapsed float64
	output  []string
}

type parentTest struct {
	name     string
	action   string
	elapsed  float64
	children []*subtest
}

func main() {
	colorFlag := flag.Bool("color", false, "Force color output (default: auto-detect TTY)")
	outputFile := flag.String("o", "", "Write output to file (ANSI + .nocolor.txt)")
	flag.Parse()

	useColor := *colorFlag || interactive.IsTerminalWriter(os.Stdout)

	r, err := openInput(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	parents := parseEvents(r)
	report := renderReport(parents, useColor)

	fmt.Print(report)

	if *outputFile != "" {
		colorReport := renderReport(parents, true)
		if err := os.WriteFile(*outputFile, []byte(colorReport), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", *outputFile, err)
			os.Exit(1)
		}
		noColorFile := strings.TrimSuffix(*outputFile, ".txt") + ".nocolor.txt"
		plainReport := stripANSI(colorReport)
		if err := os.WriteFile(noColorFile, []byte(plainReport), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", noColorFile, err)
			os.Exit(1)
		}
	}
}

func openInput(path string) (io.Reader, error) {
	if path == "" {
		return os.Stdin, nil
	}
	return os.Open(path)
}

func parseEvents(r io.Reader) []*parentTest {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	type testInfo struct {
		action  string
		elapsed float64
		output  []string
	}

	tests := make(map[string]*testInfo)

	for scanner.Scan() {
		var ev TestEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Test == "" {
			continue
		}

		info, ok := tests[ev.Test]
		if !ok {
			info = &testInfo{}
			tests[ev.Test] = info
		}

		switch ev.Action {
		case "pass", "fail", "skip":
			info.action = ev.Action
			info.elapsed = ev.Elapsed
		case "output":
			info.output = append(info.output, ev.Output)
		}
	}

	// Group by parent
	parentMap := make(map[string]*parentTest)
	for name, info := range tests {
		parentName, childName := splitTestName(name)
		if childName == "" {
			// This is a parent test (or standalone)
			p, ok := parentMap[parentName]
			if !ok {
				p = &parentTest{name: parentName}
				parentMap[parentName] = p
			}
			p.action = info.action
			p.elapsed = info.elapsed
			continue
		}

		p, ok := parentMap[parentName]
		if !ok {
			p = &parentTest{name: parentName}
			parentMap[parentName] = p
		}
		p.children = append(p.children, &subtest{
			name:    childName,
			action:  info.action,
			elapsed: info.elapsed,
			output:  info.output,
		})
	}

	// Sort parents alphabetically, children alphabetically
	var result []*parentTest
	for _, p := range parentMap {
		sort.Slice(p.children, func(i, j int) bool {
			return p.children[i].name < p.children[j].name
		})
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].name < result[j].name
	})

	return result
}

func splitTestName(name string) (parent, child string) {
	parent, child, ok := strings.Cut(name, "/")
	if !ok {
		return name, ""
	}
	return parent, child
}

func renderReport(parents []*parentTest, color bool) string {
	var b strings.Builder

	// Count totals
	var total, passed, failed, skipped int
	for _, p := range parents {
		total++
		switch p.action {
		case "pass":
			passed++
		case "fail":
			failed++
		case "skip":
			skipped++
		}
	}

	// Header
	b.WriteString("E2E Test Report\n")
	b.WriteString("═══════════════\n\n")
	fmt.Fprintf(&b, "Total: %d  Passed: %d  Failed: %d  Skipped: %d\n\n", total, passed, failed, skipped)

	for _, p := range parents {
		icon := statusIcon(p.action, color)
		if p.elapsed > 0 {
			fmt.Fprintf(&b, "%s %s (%.1fs)\n", icon, p.name, p.elapsed)
		} else {
			fmt.Fprintf(&b, "%s %s\n", icon, p.name)
		}

		for _, c := range p.children {
			cIcon := statusIcon(c.action, color)
			switch c.action {
			case "skip":
				reason := extractSkipReason(c.output)
				fmt.Fprintf(&b, "  %s %-20s SKIP", cIcon, c.name)
				if reason != "" {
					fmt.Fprintf(&b, ": %s", reason)
				}
				b.WriteString("\n")
			case "fail":
				fmt.Fprintf(&b, "  %s %-20s %.1fs\n", cIcon, c.name, c.elapsed)
				for _, line := range filterFailureOutput(c.output) {
					fmt.Fprintf(&b, "      %s\n", line)
				}
			default:
				fmt.Fprintf(&b, "  %s %-20s %.1fs\n", cIcon, c.name, c.elapsed)
			}
		}

		b.WriteString("\n")
	}

	// Rerun commands for failed tests
	if failed > 0 {
		b.WriteString("Rerun failed tests:\n")
		for _, p := range parents {
			if p.action != "fail" {
				continue
			}
			var failedAgents []string
			for _, c := range p.children {
				if c.action == "fail" {
					failedAgents = append(failedAgents, c.name)
				}
			}
			if len(failedAgents) > 0 {
				for _, agent := range failedAgents {
					fmt.Fprintf(&b, "  mise run test:e2e --agent %s %s\n", agent, p.name)
				}
			} else {
				// Standalone test with no subtests
				fmt.Fprintf(&b, "  mise run test:e2e %s\n", p.name)
			}
		}
		b.WriteString("\n")
	}

	// Footer banner
	if failed > 0 {
		banner := fmt.Sprintf("💥 FAILED (%d/%d passed) 💥", passed, total)
		if color {
			fmt.Fprintf(&b, "%s%s%s\n", colorRed, banner, colorReset)
		} else {
			b.WriteString(banner + "\n")
		}
	} else {
		banner := fmt.Sprintf("🎉 ALL %d TESTS PASSED 🎉", total)
		if color {
			fmt.Fprintf(&b, "%s%s%s\n", colorGreen, banner, colorReset)
		} else {
			b.WriteString(banner + "\n")
		}
	}
	b.WriteString("\n")

	return b.String()
}

const (
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorReset  = "\033[0m"
)

func statusIcon(action string, color bool) string {
	switch action {
	case "pass":
		if color {
			return colorGreen + "✓" + colorReset
		}
		return "✓"
	case "fail":
		if color {
			return colorRed + "✗" + colorReset
		}
		return "✗"
	case "skip":
		if color {
			return colorYellow + "−" + colorReset
		}
		return "−"
	default:
		return "?"
	}
}

func extractSkipReason(output []string) string {
	for _, line := range output {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--- SKIP") || strings.HasPrefix(trimmed, "=== RUN") {
			continue
		}
		if trimmed == "" {
			continue
		}
		return trimmed
	}
	return ""
}

func filterFailureOutput(output []string) []string {
	var lines []string
	for _, line := range output {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "=== RUN") ||
			strings.HasPrefix(trimmed, "=== PAUSE") ||
			strings.HasPrefix(trimmed, "=== CONT") ||
			strings.HasPrefix(trimmed, "--- FAIL") ||
			strings.HasPrefix(trimmed, "--- PASS") {
			continue
		}
		lines = append(lines, trimmed)
	}
	return lines
}

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}
