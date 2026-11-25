package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// defaultTrailFindingWaitTimeout bounds `finding list --wait`. A caller with a
// shorter command limit (an agent's shell tool) passes --timeout and runs the
// command again while the review is still going.
const defaultTrailFindingWaitTimeout = 10 * time.Minute

const (
	trailFindingWaitPollInterval     = 15 * time.Second
	trailFindingWaitProgressInterval = 60 * time.Second
	// trailFindingWaitListReserve is kept back from the timeout for listing
	// the findings once the wait ends.
	trailFindingWaitListReserve = 5 * time.Second
	// trailFindingWaitStaleGrace is how long after the first run on the head
	// is seen that agents whose latest run is still on an older head are taken
	// as not run by this push (a manual or ready-only agent, say). Runs one
	// push starts are queued together, well inside this.
	trailFindingWaitStaleGrace = 60 * time.Second
)

// trailAgentRunTerminal lists the agent-run statuses after which a run will
// not change (the server's enum is queued, running, succeeded, failed,
// timed_out, canceled). Any other status, including one this CLI does not know
// yet, counts as still running, so an unknown status costs a timeout rather
// than an early, incomplete answer.
var trailAgentRunTerminal = map[string]bool{
	"succeeded": true,
	"failed":    true,
	"timed_out": true,
	"canceled":  true,
}

type trailFindingWaitOptions struct {
	Enabled bool
	Timeout time.Duration
	Head    string
}

// trailReviewProgress is how far the change's agents are on one head.
type trailReviewProgress struct {
	Head       string // the head being waited on; empty until the server reports one
	ServerHead string
	Done       int
	Total      int
	// HeadPending is set while the server's head is not yet the requested one.
	HeadPending bool
	// OnHead is set once any agent has a run on the head.
	OnHead bool
	// NoAgents is set when the server's read carried no agents at all, so
	// there is nothing to wait for.
	NoAgents bool
	// NoHead is set when neither --head nor the server names the head, so
	// there is no commit to wait on.
	NoHead bool
	// Read is set once the server has answered; before that nothing is known.
	Read bool
}

func (p trailReviewProgress) settled() bool {
	if !p.Read {
		return false
	}
	return p.NoAgents || p.NoHead || (!p.HeadPending && p.Done == p.Total)
}

// trailReviewProgressFor reports how many of the change's agents have
// finished on the head: wantHead, or the server's head when it is empty.
//
// An agent with no run on the change that waits for the change to become (or
// stop being) ready for review will not run on this head, so it is left out.
// An agent whose latest run is on an older head counts as pending, unless
// ignoreStale says enough time has passed since runs on this head appeared to
// take it as one this push does not run. The same goes for an agent with no
// run at all once ignoreStale is set (a manual agent, say).
func trailReviewProgressFor(agents []api.TrailAgent, serverHead, wantHead string, ignoreStale bool) trailReviewProgress {
	p := trailReviewProgress{Head: serverHead, ServerHead: serverHead, Read: true}
	if wantHead != "" {
		p.Head = wantHead
		p.HeadPending = !sameCommit(serverHead, wantHead)
	}
	if p.Head == "" {
		p.NoHead = true
		return p
	}
	for _, a := range agents {
		if a.LatestRun == nil {
			waitsForReadiness := a.WaitingFor != nil && (*a.WaitingFor == "ready" || *a.WaitingFor == "not_ready")
			if waitsForReadiness || ignoreStale {
				continue
			}
			p.Total++
			continue
		}
		onHead := sameCommit(stringPtrValue(a.LatestRun.HeadSHA), p.Head)
		terminal := trailAgentRunTerminal[a.LatestRun.Status]
		if !onHead && terminal && ignoreStale {
			continue
		}
		p.Total++
		if onHead {
			p.OnHead = true
			if terminal {
				p.Done++
			}
		}
	}
	return p
}

// sameCommit compares full or abbreviated SHAs.
func sameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && strings.HasPrefix(b, a)
}

// fetchTrailReviewProgress reads the change's detail for its head and agents.
func fetchTrailReviewProgress(ctx context.Context, client *api.Client, trailID, wantHead string, ignoreStale bool) (trailReviewProgress, error) {
	detail, err := fetchTrailDetail(ctx, client, trailID)
	if err != nil {
		return trailReviewProgress{}, err
	}
	serverHead := ""
	if mg, mErr := detail.DecodeMergeability(); mErr == nil && mg != nil && isCommitSHA(stringPtrValue(mg.HeadSHA)) {
		serverHead = stringPtrValue(mg.HeadSHA)
	}
	if api.RawJSONAbsent(detail.Agents) {
		return trailReviewProgress{Head: firstNonEmpty(wantHead, serverHead), ServerHead: serverHead, NoAgents: true, Read: true}, nil
	}
	agents, err := detail.DecodeAgents()
	if err != nil {
		return trailReviewProgress{}, fmt.Errorf("trail %s: %w", trailID, err)
	}
	return trailReviewProgressFor(agents, serverHead, wantHead, ignoreStale), nil
}

// isCommitSHA reports whether s looks like a full or abbreviated commit SHA.
// A server value that does not is not used, nor printed.
func isCommitSHA(s string) bool {
	return len(s) >= 7 && len(s) <= 64 && strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// trailReviewPoll reads the review's progress once; ignoreStale is passed
// through to trailReviewProgressFor.
type trailReviewPoll func(ctx context.Context, ignoreStale bool) (trailReviewProgress, error)

// waitForTrailReview polls the change until its review on the head settles
// or the deadline passes, writing a progress line to errW now and then. A
// failed read is retried until the deadline. It returns the last progress
// seen (its Read is false when no read succeeded); only cancellation of ctx
// is an error.
func waitForTrailReview(ctx context.Context, errW io.Writer, poll trailReviewPoll, deadline time.Time, now func() time.Time, sleep func(context.Context, time.Duration) error) (trailReviewProgress, error) {
	var (
		last         trailReviewProgress
		lastErr      error
		lastProgress time.Time
		onHeadSince  time.Time
	)
	for {
		ignoreStale := !onHeadSince.IsZero() && now().Sub(onHeadSince) >= trailFindingWaitStaleGrace
		pollCtx, cancel := context.WithTimeout(ctx, deadline.Sub(now()))
		p, err := poll(pollCtx, ignoreStale)
		hitDeadline := pollCtx.Err() != nil
		cancel()
		if ctx.Err() != nil {
			return last, fmt.Errorf("wait for the review: %w", ctx.Err())
		}
		if err != nil && hitDeadline {
			// A read cut off by the deadline is the timeout, not a failure.
			warnTrailReviewUnread(errW, lastErr)
			return last, nil //nolint:nilerr // reaching the deadline ends the wait; the findings so far are listed
		}
		if err == nil {
			last, lastErr = p, nil
			if p.settled() {
				return p, nil
			}
			if p.OnHead && onHeadSince.IsZero() {
				onHeadSince = now()
			}
		} else {
			lastErr = err
		}
		if lastProgress.IsZero() || now().Sub(lastProgress) >= trailFindingWaitProgressInterval {
			fmt.Fprintln(errW, trailReviewProgressLine(last, lastErr))
			lastProgress = now()
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			warnTrailReviewUnread(errW, lastErr)
			return last, nil
		}
		if err := sleep(ctx, min(trailFindingWaitPollInterval, remaining)); err != nil {
			return last, err
		}
	}
}

// warnTrailReviewUnread reports, when the wait ends, a read failure that was
// still being retried.
func warnTrailReviewUnread(errW io.Writer, lastErr error) {
	if lastErr != nil {
		fmt.Fprintf(errW, "Warning: could not read the review's progress: %s\n", tuiutil.SanitizeTerminalText(lastErr.Error()))
	}
}

func trailReviewProgressLine(p trailReviewProgress, err error) string {
	switch {
	case err != nil:
		return "Waiting for the review (could not read its progress: " + tuiutil.SanitizeTerminalText(err.Error()) + "; retrying)…"
	case !p.Read || p.Head == "":
		return "Waiting for the review…"
	case p.HeadPending:
		return fmt.Sprintf("Waiting for the trail to reach %s (it is at %s)…", shortSHA(p.Head), shortSHA(p.ServerHead))
	}
	return fmt.Sprintf("Waiting for the review of %s: %d of %d agents done…", shortSHA(p.Head), p.Done, p.Total)
}

// trailReviewWaitResultLine is the line printed once the wait ends.
func trailReviewWaitResultLine(p trailReviewProgress) string {
	switch {
	case !p.Read:
		return "Could not read the review's progress; showing findings so far."
	case p.NoAgents:
		return "The trail does not report its review agents; showing its findings now."
	case p.NoHead:
		return "The trail does not report its head commit; showing its findings now. Pass --head to wait for a commit's review."
	case p.settled() && p.Total == 0:
		return "No review agent runs on this commit; showing the trail's findings."
	case p.settled():
		return fmt.Sprintf("Review of %s finished (%d agents).", shortSHA(p.Head), p.Total)
	case p.HeadPending && p.ServerHead == "":
		return fmt.Sprintf("The trail has not reached %s yet; showing findings so far. Run the command again to keep waiting.", shortSHA(p.Head))
	case p.HeadPending:
		return fmt.Sprintf("The trail is at %s, not %s; showing its findings so far. Run the command again to keep waiting.", shortSHA(p.ServerHead), shortSHA(p.Head))
	}
	return fmt.Sprintf("Review of %s still running (%d of %d agents done); showing findings so far. Run the command again to keep waiting.", shortSHA(p.Head), p.Done, p.Total)
}

// trailReviewWaitJSON reports the wait under --json, so a script can tell a
// finished review from findings so far.
type trailReviewWaitJSON struct {
	Head     string `json:"head,omitempty"`
	Finished bool   `json:"finished"`
	Done     int    `json:"agents_done"`
	Total    int    `json:"agents_total"`
}

func toTrailReviewWaitJSON(p trailReviewProgress) *trailReviewWaitJSON {
	return &trailReviewWaitJSON{Head: p.Head, Finished: p.settled(), Done: p.Done, Total: p.Total}
}

// trailFindingWaitSleep is replaced in tests so a wait does not sleep.
var trailFindingWaitSleep = sleepContext

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for the review: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// trailFindingWaitDeadline is when polling stops: the timeout from the
// command's start, less a little time to list the findings.
func trailFindingWaitDeadline(start time.Time, timeout time.Duration) time.Time {
	reserve := min(trailFindingWaitListReserve, timeout/4)
	return start.Add(timeout - reserve)
}

func validateTrailFindingWaitOptions(opts trailFindingWaitOptions, timeoutChanged, headChanged bool) error {
	if !opts.Enabled {
		if timeoutChanged || headChanged {
			return errors.New("--timeout and --head need --wait")
		}
		return nil
	}
	if opts.Timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if h := strings.TrimSpace(opts.Head); h != "" && !isCommitSHA(h) {
		return fmt.Errorf("--head %q is not a commit SHA (7 or more hex characters)", tuiutil.SanitizeTerminalLabel(h))
	}
	return nil
}
