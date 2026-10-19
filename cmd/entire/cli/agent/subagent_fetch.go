package agent

import (
	"context"
	"errors"
	"time"
)

// SubagentFetchBudget is how long one condensation or SessionEnd sweep may
// spend re-exporting subagent transcripts in all, however many tasks it holds.
const SubagentFetchBudget = 30 * time.Second

// ErrSubagentFetchBudgetSpent is returned for a fetch made after its budget
// ran out.
var ErrSubagentFetchBudgetSpent = errors.New("subagent transcript fetch budget spent")

type subagentFetchDeadlineKey struct{}

// WithSubagentFetchBudget bounds every FetchSubagentTranscript made under the
// returned context to one deadline, d from now. It bounds only the fetches,
// never the state writes around them. An outer budget already on ctx is kept.
func WithSubagentFetchBudget(ctx context.Context, d time.Duration) context.Context {
	if _, ok := ctx.Value(subagentFetchDeadlineKey{}).(time.Time); ok {
		return ctx
	}
	return context.WithValue(ctx, subagentFetchDeadlineKey{}, time.Now().Add(d))
}

// FetchSubagentTranscript runs fetcher's FetchSubagentTranscript within ctx's
// fetch budget, if any: refused once the budget is spent, otherwise cut off at
// its deadline. Each export is a separate agent process, so without a budget a
// commit holding several unreadable tasks waits for each in turn.
func FetchSubagentTranscript(ctx context.Context, fetcher SubagentTranscriptFetcher, agentID, toolUseID string, startedAt, completedAt time.Time) (string, error) {
	deadline, ok := ctx.Value(subagentFetchDeadlineKey{}).(time.Time)
	if !ok {
		return fetcher.FetchSubagentTranscript(ctx, agentID, toolUseID, startedAt, completedAt) //nolint:wrapcheck // passthrough
	}
	if !time.Now().Before(deadline) {
		return "", ErrSubagentFetchBudgetSpent
	}
	fetchCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return fetcher.FetchSubagentTranscript(fetchCtx, agentID, toolUseID, startedAt, completedAt) //nolint:wrapcheck // passthrough
}
