package cli

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/perf"

	"charm.land/lipgloss/v2"
)

// traceStep represents a single timed step within a trace span.
// Nested spans are represented as SubSteps. Loop iterations keep their numeric
// suffixes in the step name, e.g. "process_sessions.0".
type traceStep struct {
	Name       string      `json:"name"`
	DurationMs int64       `json:"duration_ms"`
	Error      bool        `json:"error,omitempty"`
	SubSteps   []traceStep `json:"sub_steps,omitempty"`
}

// traceEntry represents a parsed performance trace log entry.
type traceEntry struct {
	Op         string `json:"op"`
	DurationMs int64  `json:"duration_ms"`
	Error      bool   `json:"error,omitempty"`
	// Slow marks a root span that exceeded the slow threshold and was therefore
	// logged at WARN rather than DEBUG (see perf.DefaultSlowSpanThreshold), which
	// is what makes these visible in a default-level session at all.
	Slow bool `json:"slow,omitempty"`
	// Time is omitted when zero rather than serialized as year 1: the parser
	// tolerates a missing or malformed time key, and a JSON consumer should see
	// the absence rather than a timestamp that was never recorded.
	Time  time.Time   `json:"time,omitzero"`
	Steps []traceStep `json:"steps,omitempty"`
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
	if slowRaw, ok := raw["slow"]; ok {
		_ = json.Unmarshal(slowRaw, &entry.Slow) //nolint:errcheck // best-effort
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
// Op field are included; if slowOnly is set, only entries flagged slow are.
//
// Both filters are applied before the last-N truncation, so "the last N" always
// means the last N entries the caller asked to see. Filtering after truncation
// instead would make --slow return the slow entries among the last N traces
// rather than the last N slow traces — which reads as "no traces" whenever
// DEBUG logging fills the window with fast ones.
func collectTraceEntries(root *os.Root, name string, last int, hookFilter string, slowOnly bool) ([]traceEntry, error) {
	f, err := osroot.OpenNoFollow(root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
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
		if slowOnly && !entry.Slow {
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
// renderNoTraces explains how traces get emitted. Shared by the per-entry and
// summary renderers so there is one description of the rules, not two that drift.
func renderNoTraces(w io.Writer) {
	fmt.Fprintln(w, "No trace entries found.")
	fmt.Fprintf(w, "By default, hooks taking %s or longer are traced at WARN; set %s=0\n",
		perf.DefaultSlowSpanThreshold, perf.SlowSpanEnvVar)
	fmt.Fprintln(w, `to turn that off, and note that a log_level above WARN hides them too.`)
	fmt.Fprintln(w, `To trace every hook, set ENTIRE_LOG_LEVEL=DEBUG in your shell profile,`)
	fmt.Fprintln(w, `or log_level to "DEBUG" in .entire/settings.json.`)
}

func renderTraceEntries(w io.Writer, entries []traceEntry) {
	if len(entries) == 0 {
		renderNoTraces(w)
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

// dominantStep returns the name of the longest step anywhere in the entry's tree,
// nested steps included. That is the question a slow hook actually raises — which
// piece of work owned the time — and nesting means the top-level step is often a
// wrapper rather than the culprit.
func dominantStepMs(entry traceEntry) (string, int64, bool) {
	var best string
	var bestMs int64 = -1
	var walk func(steps []traceStep)
	walk = func(steps []traceStep) {
		for _, s := range steps {
			if s.DurationMs > bestMs {
				best, bestMs = s.Name, s.DurationMs
			}
			walk(s.SubSteps)
		}
	}
	walk(entry.Steps)
	if bestMs < 0 {
		return "", 0, false
	}
	return best, bestMs, true
}

// renderTraceJSON writes entries as a JSON array, newest first.
func renderTraceJSON(w io.Writer, entries []traceEntry) error {
	if entries == nil {
		entries = []traceEntry{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entries); err != nil {
		return fmt.Errorf("encoding trace entries: %w", err)
	}
	return nil
}

// traceOpSummary aggregates the traces recorded for one hook.
type traceOpSummary struct {
	Op       string `json:"op"`
	Count    int    `json:"count"`
	Slow     int    `json:"slow"`
	P50Ms    int64  `json:"p50_ms"`
	P90Ms    int64  `json:"p90_ms"`
	MaxMs    int64  `json:"max_ms"`
	Dominant string `json:"dominant_step,omitempty"`
}

// traceSummary is the aggregate view across many traces.
type traceSummary struct {
	Total      int              `json:"total"`
	Slow       int              `json:"slow"`
	Ops        []traceOpSummary `json:"ops"`
	StepCounts []traceStepCount `json:"dominant_steps"`
}

// traceStepCount is how often a step was the dominant one, and how much time it
// accounted for while dominant. Total time is the stronger signal — one 4s
// offender matters more than three 200ms ones — so output sorts on it.
type traceStepCount struct {
	Step    string `json:"step"`
	Count   int    `json:"count"`
	TotalMs int64  `json:"total_ms"`
}

// summarizeTraces aggregates entries per hook and counts which step dominated.
// Individual traces answer "why was this invocation slow"; this answers "what is
// slow in general", which is the question worth asking once slow traces are
// plentiful.
func summarizeTraces(entries []traceEntry) traceSummary {
	byOp := map[string][]traceEntry{}
	stepCounts := map[string]int{}
	stepTotals := map[string]int64{}
	summary := traceSummary{Total: len(entries)}

	for _, e := range entries {
		byOp[e.Op] = append(byOp[e.Op], e)
		if e.Slow {
			summary.Slow++
		}
		if step, ms, ok := dominantStepMs(e); ok {
			stepCounts[step]++
			stepTotals[step] += ms
		}
	}

	for op, group := range byOp {
		durations := make([]int64, 0, len(group))
		slow := 0
		perOpSteps := map[string]int64{}
		for _, e := range group {
			durations = append(durations, e.DurationMs)
			if e.Slow {
				slow++
			}
			if step, ms, ok := dominantStepMs(e); ok {
				perOpSteps[step] += ms
			}
		}
		slices.Sort(durations)
		summary.Ops = append(summary.Ops, traceOpSummary{
			Op:       op,
			Count:    len(group),
			Slow:     slow,
			P50Ms:    percentileMs(durations, 50),
			P90Ms:    percentileMs(durations, 90),
			MaxMs:    durations[len(durations)-1],
			Dominant: heaviestStep(perOpSteps),
		})
	}
	// Busiest hook first; name breaks ties so output is stable.
	slices.SortFunc(summary.Ops, func(a, b traceOpSummary) int {
		if a.Count != b.Count {
			return cmp.Compare(b.Count, a.Count)
		}
		return cmp.Compare(a.Op, b.Op)
	})

	for step, n := range stepCounts {
		summary.StepCounts = append(summary.StepCounts, traceStepCount{
			Step: step, Count: n, TotalMs: stepTotals[step],
		})
	}
	slices.SortFunc(summary.StepCounts, func(a, b traceStepCount) int {
		if a.TotalMs != b.TotalMs {
			return cmp.Compare(b.TotalMs, a.TotalMs)
		}
		return cmp.Compare(a.Step, b.Step)
	})

	return summary
}

// percentileMs returns the p-th percentile of a sorted slice using nearest-rank,
// so p50 of one sample is that sample rather than an interpolation.
//
// The rank is ceil(p/100 * n), converted to a 0-based index. Truncating instead
// of rounding up overshoots by one whole sample whenever p*n divides evenly,
// which is exactly the round-numbered case: p90 of 10 samples would report the
// maximum, making the P90 and MAX columns duplicates of each other.
func percentileMs(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	n := len(sorted)
	rank := (p*n + 99) / 100 // ceil(p*n/100)
	idx := max(rank-1, 0)
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// heaviestStep returns the step accounting for the most time, breaking ties by
// name so the result does not depend on map iteration order.
func heaviestStep(totals map[string]int64) string {
	best, bestMs := "", int64(-1)
	for k, ms := range totals {
		if ms > bestMs || (ms == bestMs && k < best) {
			best, bestMs = k, ms
		}
	}
	return best
}

// renderTraceSummary writes the aggregate view as a table.
func renderTraceSummary(w io.Writer, summary traceSummary) {
	if summary.Total == 0 {
		renderNoTraces(w)
		return
	}

	fmt.Fprintf(w, "%d trace(s), %d slow\n\n", summary.Total, summary.Slow)

	// Durations are rendered into strings and padded as strings so the header and
	// the rows share one width. Padding the number and appending "ms" instead
	// makes each column two characters wider than its header, which stays
	// invisible until a five-digit duration shows up.
	ms := func(v int64) string { return strconv.FormatInt(v, 10) + "ms" }

	fmt.Fprintf(w, "  %-22s %6s %10s %10s %10s %10s  %s\n", "HOOK", "N", "SLOW", "P50", "P90", "MAX", "DOMINANT STEP")
	for _, op := range summary.Ops {
		fmt.Fprintf(w, "  %-22s %6d %10d %10s %10s %10s  %s\n",
			op.Op, op.Count, op.Slow, ms(op.P50Ms), ms(op.P90Ms), ms(op.MaxMs), op.Dominant)
	}

	if len(summary.StepCounts) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %-42s %6s %11s\n", "DOMINANT STEP", "N", "TOTAL")
		for _, sc := range summary.StepCounts {
			fmt.Fprintf(w, "  %-42s %6d %11s\n", sc.Step, sc.Count, ms(sc.TotalMs))
		}
	}
}
