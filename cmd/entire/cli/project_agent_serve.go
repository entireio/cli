package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/spf13/cobra"
)

const agentJobsPath = "/api/v1/self-hosted-runner/jobs"
const agentSubmitFail = "fail"

const agentReportTimeout = 15 * time.Second

// Leave time to kill the harness and report before the server deadline.
const agentDeadlineReserve = execx.KillWaitDelay + agentReportTimeout + 5*time.Second

var (
	errAgentExecution  = errors.New("agent execution failed")
	errAgentClaimRetry = errors.New("claim temporarily unavailable")
)

type projectAgentJob struct {
	RunID     string `json:"run_id"`
	ProjectID string `json:"project_id"`
	Harness   string `json:"harness"`
	State     string `json:"state"`
}

type projectAgentClaim struct {
	projectAgentJob

	Model        string          `json:"model"`
	GitRemoteURL string          `json:"git_remote_url"`
	BaseSHA      string          `json:"base_sha"`
	HeadSHA      string          `json:"head_sha"`
	Prompt       string          `json:"prompt"`
	OutputSchema json.RawMessage `json:"output_schema"`
	DeadlineAt   time.Time       `json:"deadline_at"`
}

type projectAgentSubmission struct {
	RunID  string          `json:"run_id"`
	Action string          `json:"action"`
	Body   json.RawMessage `json:"body"`
}

type projectAgentWorker struct {
	pendingPath string
	target      *projectAgentTarget
	out         io.Writer
	execute     func(context.Context, projectAgentClaim) (json.RawMessage, error)
	save        func(*projectAgentSubmission) error
	wait        func(context.Context, time.Duration) error
}

func newProjectAgentServeCmd() *cobra.Command {
	var once bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use: "serve", Short: "Execute self-hosted project Agent jobs in the foreground",
		Long: `Poll and claim Claude jobs for this project using the service account in
ENTIRE_TOKEN. Requires git, git-remote-entire and Claude Code with --restricted.
Unsupported harnesses are left unclaimed. Each job uses a fresh pinned checkout;
Claude can only Read/Grep/Glob, with no repository settings, hooks or MCP servers.

Use one service account per machine. Processes on this machine serialize for the
same identity and project. Claimed runs are recovered on restart; pending results
are saved before submission and retried without rerunning the harness. Ctrl-C
stops execution and attempts to report failure. No daemon is installed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if interval < time.Second {
				return errors.New("--poll-interval must be at least 1s")
			}
			if os.Getenv("ENTIRE_TLS_SKIP_VERIFY") != "" {
				// git-remote-entire's ENTIRE_TOKEN cluster check relies on TLS verification.
				return errors.New("unset ENTIRE_TLS_SKIP_VERIFY: serve holds a service-account secret and requires TLS verification")
			}
			if os.Getenv(auth.EnvTokenVar) == "" {
				return errors.New("set ENTIRE_TOKEN to a service-account secret or JWT; setup uses your admin login, serve uses the service account")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// A second Ctrl-C force-quits.
			context.AfterFunc(ctx, stop)
			cmd.SetContext(ctx)
			t, err := resolveProjectAgentServeTarget(cmd)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Acquiring local worker lock...")
			journal, err := openProjectAgentJournal(ctx, t)
			if err != nil {
				return err
			}
			defer journal.unlock()
			w := projectAgentWorker{target: t, out: cmd.ErrOrStderr(), save: journal.save, pendingPath: journal.path(), wait: waitProjectAgent}
			pending, err := journal.load()
			if err != nil {
				return err
			}
			if pending != nil {
				if err := w.submit(ctx, *pending); err != nil {
					return err
				}
				if once {
					return nil
				}
			}
			if err := journal.sweepWorkspaces(); err != nil {
				logging.Warn(ctx, "sweep project agent workspaces", "error", err)
			}
			executor, err := newProjectAgentExecutor(ctx)
			if err != nil {
				return err
			}
			executor.workspace = journal.workspace
			w.execute = executor.execute
			return projectAgentServeExit(ctx, cmd.ErrOrStderr(), w.serve(ctx, once, interval))
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "Process at most one supported job; exit if none are available")
	cmd.Flags().DurationVar(&interval, "poll-interval", 5*time.Second, "Delay between polls")
	return cmd
}

// projectAgentServeExit treats an operator stop between jobs as success. An
// interrupted run or any other failure keeps its error.
func projectAgentServeExit(ctx context.Context, out io.Writer, err error) error {
	if ctx.Err() != nil && errors.Is(err, context.Canceled) && !errors.Is(err, errAgentExecution) {
		fmt.Fprintln(out, "Worker stopped.")
		return nil
	}
	return err
}

func waitProjectAgent(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("worker stopped: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (w *projectAgentWorker) request(ctx context.Context, method, path string, body, out any) error {
	var err error
	for attempt := range 3 {
		err = w.target.client.ProjectConfigRequest(ctx, method, path, nil, body, out)
		if err == nil {
			return nil
		}
		if !retryProjectAgent(err) {
			return fmt.Errorf("worker request: %w", err)
		}
		if attempt < 2 {
			if waitErr := w.wait(ctx, time.Duration(attempt+1)*time.Second); waitErr != nil {
				return waitErr
			}
		}
	}
	return fmt.Errorf("worker request retries exhausted: %w", err)
}

func retryProjectAgent(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var h *api.HTTPError
	if errors.As(err, &h) {
		return h.StatusCode == http.StatusTooManyRequests || h.StatusCode >= 500
	}
	// *url.Error is itself a net.Error but also wraps non-network failures, such
	// as a refused ENTIRE_TOKEN secret; judge its cause instead.
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	var n net.Error
	return errors.As(err, &n) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED)
}

// projectAgentJobScan walks the job queue once, fetching pages lazily.
type projectAgentJobScan struct {
	w         *projectAgentWorker
	page      []projectAgentJob
	cursor    string
	exhausted bool
	seen      map[string]bool
}

func (w *projectAgentWorker) scanJobs() *projectAgentJobScan {
	return &projectAgentJobScan{w: w, seen: map[string]bool{}}
}

func (s *projectAgentJobScan) next(ctx context.Context) (*projectAgentJob, error) {
	for {
		for len(s.page) > 0 {
			job := s.page[0]
			s.page = s.page[1:]
			if job.ProjectID == s.w.target.projectID && job.Harness == activityAgentClaude {
				return &job, nil
			}
		}
		if s.exhausted {
			return nil, nil //nolint:nilnil // An empty queue is a successful poll, not an error.
		}
		path := agentJobsPath + "?per_page=100"
		if s.cursor != "" {
			path += "&cursor=" + url.QueryEscape(s.cursor)
		}
		var page api.ProjectConfigPage[projectAgentJob]
		if err := s.w.request(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		s.page = page.Items
		if page.NextCursor == nil || *page.NextCursor == "" {
			s.exhausted = true
			continue
		}
		if s.seen[*page.NextCursor] {
			return nil, errors.New("jobs API repeated its cursor")
		}
		s.seen[*page.NextCursor] = true
		s.cursor = *page.NextCursor
	}
}

func (w *projectAgentWorker) serve(ctx context.Context, once bool, interval time.Duration) error {
	for {
		fmt.Fprintln(w.out, "Waiting for Claude jobs...")
		stop, err := w.serveScan(ctx, once)
		if err != nil || stop {
			return err
		}
		if err := w.wait(ctx, interval); err != nil {
			return err
		}
	}
}

// serveScan makes one pass over the queue; refused claims are retried next pass.
func (w *projectAgentWorker) serveScan(ctx context.Context, once bool) (bool, error) {
	jobs := w.scanJobs()
	for {
		job, err := jobs.next(ctx)
		if err != nil {
			if !retryProjectAgent(err) || once {
				return true, err
			}
			fmt.Fprintf(w.out, "Polling temporarily unavailable; retrying: %v\n", err)
			return false, nil
		}
		if job == nil {
			return once, nil
		}
		processed, err := w.run(ctx, *job)
		if !once && errors.Is(err, errAgentClaimRetry) {
			fmt.Fprintf(w.out, "Claim temporarily unavailable; retrying: %v\n", err)
			return false, nil // Wait before polling again; do not skip an uncertain claim.
		}
		if err != nil && (once || !errors.Is(err, errAgentExecution)) {
			return true, err
		}
		if processed {
			return once, nil
		}
		fmt.Fprintf(w.out, "Skipping refused claim %s for this scan.\n", job.RunID)
	}
}

func (w *projectAgentWorker) run(ctx context.Context, job projectAgentJob) (bool, error) {
	var claim projectAgentClaim
	err := w.request(ctx, http.MethodPost, agentJobsPath+"/"+url.PathEscape(job.RunID)+"/claim", nil, &claim)
	if err != nil {
		if api.IsHTTPErrorStatus(err, http.StatusNotFound) || api.IsHTTPErrorStatus(err, http.StatusConflict) {
			return false, nil
		}
		if retryProjectAgent(err) {
			return false, fmt.Errorf("%w: %w", errAgentClaimRetry, err)
		}
		return false, err
	}
	if claim.RunID != job.RunID || claim.ProjectID != w.target.projectID {
		// Never publish a result against an identity we cannot verify.
		return false, errors.New("claim does not match the requested project or run")
	}
	var result json.RawMessage
	var runErr error
	if claim.Harness != activityAgentClaude || claim.DeadlineAt.IsZero() {
		// We own this run, but cannot execute its contract. Persist and report
		// failure just like an execution error rather than recovering it forever.
		runErr = errors.New("claim has an unsupported harness or missing deadline")
	} else {
		fmt.Fprintf(w.out, "Claimed %s; reviewing.\n", claim.RunID)
		// Leave time for result publication before the server's deadline.
		runCtx, cancel := context.WithDeadline(ctx, claim.DeadlineAt.Add(-agentDeadlineReserve))
		result, runErr = w.execute(runCtx, claim)
		cancel()
	}
	action := "complete"
	body := any(map[string]any{"result": result, "harness": activityAgentClaude, "model": claim.Model})
	if runErr != nil {
		action = agentSubmitFail
		// Do not upload subprocess output: it may contain source or credentials.
		body = map[string]string{"error": "CLI could not execute the review; inspect the worker's local error"}
		fmt.Fprintf(w.out, "Run %s failed: %v\n", claim.RunID, runErr)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return true, fmt.Errorf("encode submission: %w", err)
	}
	pending := projectAgentSubmission{RunID: claim.RunID, Action: action, Body: encoded}
	if err := w.save(&pending); err != nil {
		return true, fmt.Errorf("save submission before sending: %w", err)
	}
	// Ctrl-C must not prevent reporting that execution stopped.
	reportCtx, reportCancel := context.WithTimeout(context.WithoutCancel(ctx), agentReportTimeout)
	defer reportCancel()
	if err := w.submit(reportCtx, pending); err != nil {
		return true, err
	}
	if runErr != nil {
		return true, fmt.Errorf("run %s: %w: %w", claim.RunID, errAgentExecution, runErr)
	}
	return true, nil
}

func (w *projectAgentWorker) submit(ctx context.Context, s projectAgentSubmission) (retErr error) {
	defer func() {
		if retErr != nil && w.pendingPath != "" {
			retErr = fmt.Errorf("%w\nPending submission file: %q", retErr, w.pendingPath)
		}
	}()
	if !looksLikeULID(s.RunID) || (s.Action != "complete" && s.Action != agentSubmitFail) || !json.Valid(s.Body) {
		return errors.New("invalid pending submission")
	}
	path := "/api/v1/self-hosted-runner/runs/" + s.RunID + "/" + s.Action
	if err := w.request(ctx, http.MethodPost, path, s.Body, nil); err != nil {
		// Terminal refusals must not block this machine forever. Validation errors
		// and uncertain outcomes retain the exact payload for operator recovery.
		if api.IsHTTPErrorStatus(err, http.StatusNotFound) || api.IsHTTPErrorStatus(err, http.StatusConflict) {
			if clearErr := w.save(nil); clearErr != nil {
				return clearErr
			}
			fmt.Fprintf(w.out, "Run %s is no longer submit-able.\n", s.RunID)
			return nil
		}
		if api.IsHTTPErrorStatus(err, http.StatusUnprocessableEntity) {
			return fmt.Errorf("submit %s: payload retained; correct the saved body before restarting serve (keep run_id and action unchanged): %w", s.RunID, err)
		}
		return fmt.Errorf("submit %s: payload retained; restart serve to retry: %w", s.RunID, err)
	}
	if err := w.save(nil); err != nil {
		return err
	}
	fmt.Fprintf(w.out, "Submitted %s (%s).\n", s.RunID, s.Action)
	return nil
}
