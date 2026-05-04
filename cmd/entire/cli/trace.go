package cli

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// traceStep represents a single timed step within a trace span.
// Nested spans are represented as SubSteps. Loop iterations keep their numeric
// suffixes in the step name, e.g. "process_sessions.0".
type traceStep struct {
	Name       string
	DurationMs int64
	Error      bool
	SubSteps   []traceStep
}

// traceEntry represents a parsed performance trace log entry.
type traceEntry struct {
	Op         string
	DurationMs int64
	Error      bool
	Time       time.Time
	Steps      []traceStep
}

// parseTraceEntry parses a JSON log line into a traceEntry.
// Returns nil if the line is not valid JSON or is not a trace entry (msg != "perf").
func parseTraceEntry(line string) *traceEntry {
	// Cheap pre-filter: skip full JSON parse for lines that can't be perf entries.
	// Most lines in the shared log file are non-perf, so this avoids the
	// marshalling cost for the common reject path.
	if !strings.Contains(line, `"msg":"perf"`) {
		return nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil
	}

	// Verify msg == "perf" after full parse (the pre-filter could match substrings)
	var msg string
	if msgRaw, ok := raw["msg"]; !ok {
		return nil
	} else if err := json.Unmarshal(msgRaw, &msg); err != nil || msg != "perf" {
		return nil
	}

	entry := &traceEntry{}

	// Best-effort field extraction: missing or mistyped fields keep their
	// zero values rather than discarding the entire entry.
	if opRaw, ok := raw["op"]; ok {
		_ = json.Unmarshal(opRaw, &entry.Op) //nolint:errcheck // best-effort
	}
	if dRaw, ok := raw["duration_ms"]; ok {
		_ = json.Unmarshal(dRaw, &entry.DurationMs) //nolint:errcheck // best-effort
	}
	if errRaw, ok := raw["error"]; ok {
		_ = json.Unmarshal(errRaw, &entry.Error) //nolint:errcheck // best-effort
	}

	// Extract time
	if tRaw, ok := raw["time"]; ok {
		var ts string
		if err := json.Unmarshal(tRaw, &ts); err == nil {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				entry.Time = parsed
			}
		}
	}

	// Extract steps by finding keys matching "steps.*_ms"
	stepDurations := make(map[string]int64)
	stepErrors := make(map[string]bool)

	for key, val := range raw {
		if strings.HasPrefix(key, "steps.") && strings.HasSuffix(key, "_ms") {
			name := strings.TrimPrefix(key, "steps.")
			name = strings.TrimSuffix(name, "_ms")

			var ms int64
			if err := json.Unmarshal(val, &ms); err == nil {
				stepDurations[name] = ms
			}
		} else if strings.HasPrefix(key, "steps.") && strings.HasSuffix(key, "_err") {
			name := strings.TrimPrefix(key, "steps.")
			name = strings.TrimSuffix(name, "_err")

			var errFlag bool
			if err := json.Unmarshal(val, &errFlag); err == nil {
				stepErrors[name] = errFlag
			}
		}
	}

	entry.Steps = buildTraceSteps(stepDurations, stepErrors)

	return entry
}

type traceStepNode struct {
	step     traceStep
	children []*traceStepNode
}

func buildTraceSteps(stepDurations map[string]int64, stepErrors map[string]bool) []traceStep {
	nodes := make(map[string]*traceStepNode, len(stepDurations))
	for name, ms := range stepDurations {
		nodes[name] = &traceStepNode{
			step: traceStep{
				Name:       name,
				DurationMs: ms,
				Error:      stepErrors[name],
			},
		}
	}

	roots := make([]*traceStepNode, 0, len(nodes))
	for name, node := range nodes {
		parentName, ok := traceStepParent(name, stepDurations)
		if !ok {
			roots = append(roots, node)
			continue
		}
		nodes[parentName].children = append(nodes[parentName].children, node)
	}

	return traceStepNodesToSteps(roots, "")
}

func traceStepParent(name string, allSteps map[string]int64) (string, bool) {
	candidate := name
	for {
		idx := strings.LastIndex(candidate, ".")
		if idx < 0 {
			return "", false
		}
		candidate = candidate[:idx]
		if _, ok := allSteps[candidate]; ok {
			return candidate, true
		}
	}
}

func traceStepNodesToSteps(nodes []*traceStepNode, parentName string) []traceStep {
	sortTraceStepNodes(nodes, parentName)

	steps := make([]traceStep, 0, len(nodes))
	for _, node := range nodes {
		step := node.step
		step.SubSteps = traceStepNodesToSteps(node.children, step.Name)
		steps = append(steps, step)
	}
	return steps
}

func sortTraceStepNodes(nodes []*traceStepNode, parentName string) {
	slices.SortFunc(nodes, func(a, b *traceStepNode) int {
		if parentName == "" {
			return cmp.Compare(a.step.Name, b.step.Name)
		}

		aIdx, aNumeric := traceStepChildIndex(parentName, a.step.Name)
		bIdx, bNumeric := traceStepChildIndex(parentName, b.step.Name)
		if aNumeric && bNumeric {
			return cmp.Compare(aIdx, bIdx)
		}
		if aNumeric {
			return -1
		}
		if bNumeric {
			return 1
		}
		return cmp.Compare(a.step.Name, b.step.Name)
	})
}

func traceStepChildIndex(parentName, childName string) (int, bool) {
	prefix := parentName + "."
	if !strings.HasPrefix(childName, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(childName, prefix)
	idx, err := strconv.Atoi(suffix)
	if err != nil || idx < 0 {
		return 0, false
	}
	return idx, true
}

// collectTraceEntries reads a JSONL log file and returns the last N trace entries,
// ordered newest first. If hookFilter is non-empty, only entries with a matching
// Op field are included.
func collectTraceEntries(logFile string, last int, hookFilter string) ([]traceEntry, error) {
	f, err := os.Open(logFile) //nolint:gosec // logFile is a CLI-resolved path, not user-supplied input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening log file: %w", err)
	}
	defer f.Close()

	var entries []traceEntry

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), 1024*1024) // allow up to 1MB lines in shared log file
	for scanner.Scan() {
		entry := parseTraceEntry(scanner.Text())
		if entry == nil {
			continue
		}
		if hookFilter != "" && entry.Op != hookFilter {
			continue
		}
		entries = append(entries, *entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading log file: %w", err)
	}

	// Take the last N entries
	if len(entries) > last {
		entries = entries[len(entries)-last:]
	}

	// Reverse so newest entries are first
	slices.Reverse(entries)

	return entries, nil
}

// renderTraceEntries writes a formatted table of trace entries to w.
// If entries is empty, it prints a help message about enabling traces.
func renderTraceEntries(w io.Writer, entries []traceEntry) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "No trace entries found.")
		fmt.Fprintln(w, `Traces are logged at DEBUG level. Make sure ENTIRE_LOG_LEVEL=DEBUG is set`)
		fmt.Fprintln(w, `in your shell profile, or set log_level to "DEBUG" in .entire/settings.json.`)
		return
	}

	for i, entry := range entries {
		if i > 0 {
			fmt.Fprintln(w)
		}

		header := fmt.Sprintf("%s  %dms", entry.Op, entry.DurationMs)
		if !entry.Time.IsZero() {
			header += "  " + entry.Time.Format(time.RFC3339)
		}
		fmt.Fprintln(w, header)
		fmt.Fprintln(w)

		if len(entry.Steps) == 0 {
			continue
		}

		rows := flattenTraceSteps(entry.Steps)
		nameWidth := lipgloss.Width("STEP")
		for _, r := range rows {
			nameWidth = max(nameWidth, lipgloss.Width(r.label))
		}

		renderTraceTableRow(w, nameWidth, "STEP", "DURATION", false)
		for _, r := range rows {
			renderTraceTableRow(w, nameWidth, r.label, fmt.Sprintf("%dms", r.durationMs), r.err)
		}
	}
}

type traceRenderRow struct {
	label      string
	durationMs int64
	err        bool
}

func flattenTraceSteps(steps []traceStep) []traceRenderRow {
	var rows []traceRenderRow
	for _, s := range steps {
		rows = append(rows, traceRenderRow{label: s.Name, durationMs: s.DurationMs, err: s.Error})
		appendChildRows(&rows, s.SubSteps, "  ")
	}
	return rows
}

func appendChildRows(rows *[]traceRenderRow, steps []traceStep, prefix string) {
	for i, step := range steps {
		connector, childPrefix := "├─", prefix+"│  "
		if i == len(steps)-1 {
			connector, childPrefix = "└─", prefix+"   "
		}
		*rows = append(*rows, traceRenderRow{
			label:      prefix + connector + " " + step.Name,
			durationMs: step.DurationMs,
			err:        step.Error,
		})
		appendChildRows(rows, step.SubSteps, childPrefix)
	}
}

func renderTraceTableRow(w io.Writer, nameWidth int, label, duration string, hasError bool) {
	pad := nameWidth - lipgloss.Width(label)
	if pad < 0 {
		pad = 0
	}
	line := fmt.Sprintf("  %s%s  %8s", label, strings.Repeat(" ", pad), duration)
	if hasError {
		line += "  x"
	}
	fmt.Fprintln(w, line)
}
