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

// WithSubagentFetchDeadline caps ctx's fetch budget at deadline, keeping an
// earlier one. It gives one kind of fetch a smaller share of a budget, so it
// cannot spend what the rest of the pass needs.
func WithSubagentFetchDeadline(ctx context.Context, deadline time.Time) context.Context {
	if outer, ok := ctx.Value(subagentFetchDeadlineKey{}).(time.Time); ok && outer.Before(deadline) {
		return ctx
	}
	return context.WithValue(ctx, subagentFetchDeadlineKey{}, deadline)
}

// subagentFetchContext returns the context a fetch under ctx's budget runs
// with, or ErrSubagentFetchBudgetSpent once the budget is spent.
func subagentFetchContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	deadline, ok := ctx.Value(subagentFetchDeadlineKey{}).(time.Time)
	if !ok {
		return ctx, func() {}, nil
	}
	if !time.Now().Before(deadline) {
		return nil, nil, ErrSubagentFetchBudgetSpent
	}
	fetchCtx, cancel := context.WithDeadline(ctx, deadline)
	return fetchCtx, cancel, nil
}

// FetchSubagentTranscript runs fetcher's FetchSubagentTranscript within ctx's
// fetch budget, if any: refused once the budget is spent, otherwise cut off at
// its deadline. Each export is a separate agent process, so without a budget a
// commit holding several unreadable tasks waits for each in turn.
func FetchSubagentTranscript(ctx context.Context, fetcher SubagentTranscriptFetcher, agentID, toolUseID string, startedAt, completedAt time.Time) (string, error) {
	fetchCtx, cancel, err := subagentFetchContext(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()
	return fetcher.FetchSubagentTranscript(fetchCtx, agentID, toolUseID, startedAt, completedAt) //nolint:wrapcheck // passthrough
}

// LiveSubagentModifiedFiles runs reporter's LiveSubagentModifiedFiles within
// ctx's fetch budget, like FetchSubagentTranscript.
func LiveSubagentModifiedFiles(ctx context.Context, reporter LiveSubagentFileReporter, agentID, toolUseID string, startedAt time.Time) ([]string, error) {
	fetchCtx, cancel, err := subagentFetchContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return reporter.LiveSubagentModifiedFiles(fetchCtx, agentID, toolUseID, startedAt) //nolint:wrapcheck // passthrough
}
