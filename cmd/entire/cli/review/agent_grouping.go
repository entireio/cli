// Package review — see env.go for package-level rationale.
//
// agent_grouping.go reconciles the fan-out's per-worker execution with the
// dashboard's one-row-per-agent display. Skill fan-out runs N parallel
// workers per agent (claude-code:review, claude-code:pr-review), but that is
// behavior-only: the TUI shows a single row per agent. Three things diverge
// between the worker unit and the agent row — live event routing, live token
// totals, and the final summary — and this type owns all three so the
// collapse lives in one place instead of scattered across the sink.
package review

import (
	"strconv"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

type agentGrouping struct {
	rowOrder      []string            // per-agent row labels, in display order
	workerToAgent map[string]string   // worker label → agent row
	rowWorkers    map[string][]string // agent row → its worker labels
	// workerTokens holds each worker's latest cumulative Tokens so a row can
	// display the per-agent SUM live. Written/read only from the serial
	// dispatch goroutine (CU4 contract), so it needs no lock.
	workerTokens map[string]reviewtypes.Tokens
}

func newAgentGrouping(rowOrder []string, workerToAgent map[string]string) *agentGrouping {
	g := &agentGrouping{
		rowOrder:      rowOrder,
		workerToAgent: workerToAgent,
		rowWorkers:    make(map[string][]string, len(rowOrder)),
		workerTokens:  make(map[string]reviewtypes.Tokens, len(workerToAgent)),
	}
	for worker, row := range workerToAgent {
		g.rowWorkers[row] = append(g.rowWorkers[row], worker)
	}
	return g
}

// rowPlanEntry describes one worker for dashboard row planning.
type rowPlanEntry struct {
	workerKey string // profile key (exploded or pass-through)
	name      string // reviewer display name — the key events and summaries carry
	agentName string
	model     string
}

// planAgentRows assigns each worker its dashboard row. Only skill fan-out
// siblings (workers exploded from the same source worker, per skillOrigins)
// share a row; independently configured workers — including duplicate slots
// of the same agent+model — keep their own rows, since merging them would
// blend the live tokens, statuses, and summaries of unrelated reviewers.
// Labels prefer "agent (model)"; when two sources would collide, later ones
// fall back to the unique source worker key (numeric suffix as a last resort).
func planAgentRows(entries []rowPlanEntry, skillOrigins map[string]string) ([]string, map[string]string) {
	rowNames := make([]string, 0, len(entries))
	workerToRow := make(map[string]string, len(entries))
	rowBySource := make(map[string]string, len(entries))
	labelTaken := make(map[string]bool, len(entries))
	for _, e := range entries {
		source, exploded := skillOrigins[e.workerKey]
		if !exploded {
			source = e.workerKey
		}
		row, planned := rowBySource[source]
		if !planned {
			row = agentRowLabel(e.agentName, e.model)
			if labelTaken[row] {
				row = agentRowLabel(source, e.model)
			}
			for base, i := row, 2; labelTaken[row]; i++ {
				row = base + " #" + strconv.Itoa(i)
			}
			labelTaken[row] = true
			rowBySource[source] = row
			rowNames = append(rowNames, row)
		}
		workerToRow[e.name] = row
	}
	return rowNames, workerToRow
}

// rowFor resolves a worker label to its agent row, passing through any name
// absent from the map (judge/master labels, single-agent path).
func (g *agentGrouping) rowFor(name string) string {
	if row, ok := g.workerToAgent[name]; ok {
		return row
	}
	return name
}

// liveTokens records worker's latest cumulative Tokens and returns the summed
// total across every worker folded into its row. The row overwrites on each
// Tokens event, so forwarding one worker's cumulative value would bounce the
// live number between siblings; the sum shows the agent total.
func (g *agentGrouping) liveTokens(worker string, tk reviewtypes.Tokens) reviewtypes.Tokens {
	g.workerTokens[worker] = tk
	var sum reviewtypes.Tokens
	for _, w := range g.rowWorkers[g.rowFor(worker)] {
		wt := g.workerTokens[w]
		sum.In += wt.In
		sum.Out += wt.Out
	}
	return sum
}

// collapseSummary folds a per-worker summary into one AgentRun per agent row,
// in rowOrder, so the model's by-index row sync still aligns:
//   - status: worst wins (Failed > Cancelled > Succeeded)
//   - tokens: summed across the agent's workers
//   - duration: the span from the earliest worker start to the latest worker
//     end (not one worker's slice) — the row's live duration already uses
//     wall-clock, but the folded field must stay honest for any consumer
//   - error: the first non-nil
//
// The input summary is never mutated; other sinks (manifest, findings, trail)
// keep the full per-worker summary, so per-skill attribution is preserved.
func (g *agentGrouping) collapseSummary(summary reviewtypes.RunSummary) reviewtypes.RunSummary {
	type rowAcc struct {
		run      reviewtypes.AgentRun
		minStart time.Time
		maxEnd   time.Time
	}
	byRow := make(map[string]*rowAcc, len(g.rowOrder))
	for _, run := range summary.AgentRuns {
		row := g.rowFor(run.Name)
		end := run.StartedAt.Add(run.Duration)
		acc, ok := byRow[row]
		if !ok {
			cloned := run
			cloned.Name = row
			byRow[row] = &rowAcc{run: cloned, minStart: run.StartedAt, maxEnd: end}
			continue
		}
		acc.run.Tokens.In += run.Tokens.In
		acc.run.Tokens.Out += run.Tokens.Out
		if reviewStatusWorse(run.Status, acc.run.Status) {
			acc.run.Status = run.Status
		}
		if acc.run.Err == nil && run.Err != nil {
			acc.run.Err = run.Err
		}
		if !run.StartedAt.IsZero() && (acc.minStart.IsZero() || run.StartedAt.Before(acc.minStart)) {
			acc.minStart = run.StartedAt
		}
		if end.After(acc.maxEnd) {
			acc.maxEnd = end
		}
	}
	out := summary
	out.AgentRuns = make([]reviewtypes.AgentRun, 0, len(byRow))
	for _, row := range g.rowOrder {
		acc, ok := byRow[row]
		if !ok {
			continue
		}
		acc.run.StartedAt = acc.minStart
		if !acc.minStart.IsZero() && acc.maxEnd.After(acc.minStart) {
			acc.run.Duration = acc.maxEnd.Sub(acc.minStart)
		}
		out.AgentRuns = append(out.AgentRuns, acc.run)
	}
	return out
}

// reviewStatusWorse reports whether a is a worse terminal status than b, for
// worst-wins aggregation across an agent's workers.
func reviewStatusWorse(a, b reviewtypes.AgentStatus) bool {
	return reviewStatusRank(a) > reviewStatusRank(b)
}

func reviewStatusRank(s reviewtypes.AgentStatus) int {
	switch s {
	case reviewtypes.AgentStatusFailed:
		return 3
	case reviewtypes.AgentStatusCancelled:
		return 2
	case reviewtypes.AgentStatusSucceeded:
		return 1
	case reviewtypes.AgentStatusUnknown:
		return 0
	default:
		return 0
	}
}
