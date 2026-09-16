package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// These values mirror the provisioning enum in entiredb api/corev1/repos.go.
// Mirror clone readiness has its own enum: an active repo need not be cloned.
const (
	repoStateProvisioning = "provisioning"
	repoStateActive       = "active"
	repoStateFailed       = "failed"
)

// repoPollInterval is the first delay after the immediate probe. Reusing mirror's
// fixed 2s cadence mostly polls between scans: entiredb core/regional/reconciler.go
// scans every 30s by default and fails after 10 provisioning attempts. Double
// 2s through 4/8/16s to a 30s cap, with ±20% jitter to spread concurrent creates.
// This preserves quick activation feedback while allowing roughly 23 reads in
// the 10m command budget, rather than 300. Tests may shorten the initial delay.
var repoPollInterval = 2 * time.Second

// repoPollRandom supplies independent jitter samples. Tests replace it to pin
// actual timer schedules; a shared global random seed would affect other users.
// This randomness spreads requests and carries no security-sensitive value.
var repoPollRandom = rand.Float64

const repoPollMaxInterval = 30 * time.Second

// repoLifecycleGetter is the slice of *coreapi.Client needed by awaitRepoActive,
// declared as an interface so the poll is unit-testable with a fake. It uses the
// federation-validated transport: a cluster-specific client is no shortcut,
// because ENTIRE_TOKEN still chooses its audience core and a foreign registry
// snapshot cannot confirm lifecycle state.
type repoLifecycleGetter interface {
	GetRepo(ctx context.Context, params coreapi.GetRepoParams) (*coreapi.Repo, error)
}

// awaitRepoActive waits for an authoritative repository snapshot to become active.
// ctx must carry the caller's deadline; unlike awaitMirrorReady, this function
// has no overall timeout of its own. result is overwritten in place with the
// last observed snapshot, retaining creation coordinates omitted by enrichment.
// Only provisioning warrants polling; foreign, missing or unknown state cannot
// confirm readiness. onPoll, when non-nil, runs once just before polling starts.
func awaitRepoActive(ctx context.Context, c repoLifecycleGetter, result *coreapi.Repo, onPoll func()) error {
	timer := time.NewTimer(repoPollInterval)
	defer timer.Stop()

	interval := repoPollInterval
	var failures repoPollFailures
	started := false
	authoritative := false
	for {
		if result.Foreign.Or(false) {
			return errors.New("repository readiness unconfirmed: server returned a foreign registry snapshot")
		}
		switch result.State.Or("") {
		case repoStateActive:
			// POST may report registry state. Only a successful authoritative
			// read can confirm readiness, including after transient read errors.
			if authoritative {
				return nil
			}
		case repoStateFailed:
			return fmt.Errorf("repository provisioning failed: %s", result.ProvisionReason.Or("no reason supplied"))
		case repoStateProvisioning:
		case "":
			return errors.New("the server did not return repository readiness information")
		default:
			return fmt.Errorf("repository readiness unconfirmed: unsupported lifecycle state %q", result.State.Or(""))
		}
		if err := ctx.Err(); err != nil {
			return classifyWaitContextErr(err, "waiting for repository provisioning")
		}
		if failures.expired() {
			return fmt.Errorf("poll repository lifecycle: %w", failures.last)
		}
		if !started {
			started = true
			if onPoll != nil {
				onPoll()
			}
		}

		// Once reads fail, bound in-flight retries too: clipping only the sleep
		// would let a stuck request spend the entire creation deadline.
		pollCtx := ctx
		cancel := func() {}
		if failures.last != nil {
			pollCtx, cancel = context.WithDeadline(ctx, failures.deadline)
		}
		snapshot, err := c.GetRepo(pollCtx, coreapi.GetRepoParams{RepoId: result.ID, Authoritative: coreapi.NewOptBool(true)})
		cancel()
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return classifyWaitContextErr(ctx.Err(), "waiting for repository provisioning")
			}
			if failures.expired() {
				return fmt.Errorf("poll repository lifecycle: %w", failures.last)
			}
			if failures.record(err) {
				// Preserve the raw chain, as awaitMirrorReady does. Rendering is
				// reportRepoCreation's job; callers still need API/signal identity.
				return fmt.Errorf("poll repository lifecycle: %w", err)
			}
		default:
			if snapshot.ID != result.ID {
				return errors.New("repository readiness unconfirmed: inspection returned a different repository ID")
			}
			failures = repoPollFailures{}
			retainRepoCreation(result, snapshot)
			authoritative = true
			// Observe terminal or incompatible state before sleeping.
			if result.State.Or("") != repoStateProvisioning || result.Foreign.Or(false) {
				continue
			}
		}
		delay := repoPollDelay(interval, repoPollRandom())
		interval = min(interval*2, repoPollMaxInterval)
		if failures.last != nil {
			delay = min(delay, time.Until(failures.deadline))
		}
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			return classifyWaitContextErr(ctx.Err(), "waiting for repository provisioning")
		case <-timer.C:
		}
	}
}

// repoPollDelay adds bounded jitter to an interval; sample is in [0, 1].
// The pure calculation lets boundary tests exercise endpoints exactly, instead
// of relying on the random source to eventually generate them.
func repoPollDelay(interval time.Duration, sample float64) time.Duration {
	return time.Duration(float64(interval) * (0.8 + 0.4*sample))
}

// repoPollFailures bounds an uninterrupted run of read errors, independently of
// the provisioning timeout. Mirror's 15-error/~30s visibility allowance does not
// transfer to a repo we just created: give ordinary 4xx only two attempts/10s for
// replication lag, and 408/429/5xx/transport errors
// six attempts/60s for a transient outage. The wall-clock window starts at the
// first failed response and bounds subsequent requests and sleeps; successful
// reads reset it. Counts alone would make backoff stretch failure unpredictably.
//
// ErrorModelStatusCode exposes status and body, not Retry-After headers. The
// server's per-user limiter uses http.Error (text/plain), which ogen surfaces as
// a decode error, likewise without Retry-After. Both shapes use the bounded
// transient policy until the transport exposes headers; do not pretend that a
// problem body's optional fields are an HTTP retry directive.
type repoPollFailures struct {
	last     error
	first    time.Time
	deadline time.Time
	count    int
}

func (f *repoPollFailures) expired() bool {
	return f.last != nil && !time.Now().Before(f.deadline)
}

func (f *repoPollFailures) record(err error) bool {
	if f.last == nil {
		f.first = time.Now()
	}
	f.deadline = f.first.Add(time.Minute)
	f.last = err
	f.count++
	limit := 6
	var problem *coreapi.ErrorModelStatusCode
	if errors.As(err, &problem) && problem.StatusCode >= 400 && problem.StatusCode < 500 &&
		problem.StatusCode != http.StatusRequestTimeout && problem.StatusCode != http.StatusTooManyRequests {
		// The window is recomputed from f.first for the CURRENT error's class,
		// so a 4xx after a transient run moves the deadline into the past. That
		// is unobservable only because this limit is 2: such a 4xx is always
		// failure #2-or-later, so count ends the run on this same call, while a
		// 4xx that is the FIRST failure has f.first == now and nothing to
		// backdate. Raise the limit and the retroactive expiry becomes
		// reachable — runs would then end through expired() rather than the
		// count, so make that a deliberate choice rather than a side effect.
		limit = 2
		f.deadline = f.first.Add(10 * time.Second)
	}
	return f.count >= limit || f.expired()
}

// retainRepoCreation fills omitted creation coordinates into snapshot, then
// overwrites result with that snapshot. Both arguments are mutated. Name and
// owning project identify the successful POST; cluster host, path and the remote
// additional property let the user recover the clone URL if later enrichment
// fails (including after cleanup; remote coordinates follow COR-699).
// Preserve omitted additional properties too, including create-only fields
// such as commitToken. Fresh snapshot properties win collisions. Typed lifecycle,
// reason, permissions and foreign fields are replaced to avoid stale readiness.
func retainRepoCreation(result, snapshot *coreapi.Repo) {
	if snapshot.Name == "" {
		snapshot.Name = result.Name
	}
	if snapshot.OwningProjectId == "" {
		snapshot.OwningProjectId = result.OwningProjectId
	}
	if snapshot.ClusterHost.Or("") == "" {
		snapshot.ClusterHost = result.ClusterHost
	}
	if snapshot.Path.Or("") == "" {
		snapshot.Path = result.Path
	}
	for key, value := range result.AdditionalProps {
		if snapshot.AdditionalProps == nil {
			snapshot.AdditionalProps = make(coreapi.RepoAdditional)
		}
		if _, set := snapshot.AdditionalProps[key]; !set {
			snapshot.AdditionalProps[key] = value
		}
	}
	*result = *snapshot
}

// reportRepoCreation reports the successful POST even when waiting failed,
// unlike runCoreMutation. A nonzero exit does not mean another POST is safe.
func reportRepoCreation(cmd *cobra.Command, result *coreapi.Repo, noWait bool, waitErr error) error {
	// repoRemoteURL answers "" for both an invalid host and a repo still
	// provisioning; warn so the missing remote does not suggest waiting.
	if host := strings.TrimSpace(result.ClusterHost.Or("")); host != "" {
		if err := validateClusterHost(host); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: the server returned an invalid cluster host for this repository, so no remote URL was derived: %v\n", err)
		}
	}
	var outputErr error
	if jsonRequested(cmd) {
		wire, err := repoCreateOutput(result)
		if err != nil {
			outputErr = err
		} else {
			outputErr = printJSON(cmd.OutOrStdout(), wire)
		}
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "✓ Created repository %s (%s)\n  Last observed state: %s\n", result.Name, result.ID, result.State.Or("unavailable"))
		if reason := result.ProvisionReason.Or(""); reason != "" {
			fmt.Fprintln(cmd.OutOrStdout(), "  Provision reason: "+reason)
		}
		if remote := repoRemoteURL(*result); remote != "" {
			fmt.Fprintln(cmd.OutOrStdout(), "  Remote: "+remote)
		}
	}
	if waitErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Repository creation succeeded: %s (%s). Readiness was not confirmed: %v\n", result.Name, result.ID, renderRepoReadError(waitErr))
		fmt.Fprintf(cmd.ErrOrStderr(), "Inspect repository details with: entire repo get %s\nCheck readiness with: entire repo get %s --authoritative\nWhen that command reports active, retry the intended push or mirror creation. If readiness remains unavailable, contact support with this repository ID. Do not create the repository again. For future creates, --no-wait skips readiness checks.\n", result.ID, result.ID)
		return NewSilentError(errors.Join(waitErr, outputErr))
	}
	if noWait && (result.State.Or("") != repoStateActive || result.Foreign.Or(false)) {
		fmt.Fprintf(cmd.ErrOrStderr(), "Repository readiness is unconfirmed (--no-wait). Check readiness with: entire repo get %s --authoritative\n", result.ID)
	}
	return outputErr
}

// renderRepoReadError keeps compatibility diagnostics local to readiness reads.
// Match the structured validation location and message, not a generic 422:
// unrelated validation failures must not be described as an older core.
func renderRepoReadError(err error) error {
	if readinessParameterUnsupported(err) {
		return fmt.Errorf("%w: query.authoritative: unknown query parameter; the server does not support repository readiness checks", renderCoreError(err))
	}
	return renderCoreError(err)
}

// readinessParameterUnsupported reports whether the server rejected the
// authoritative query parameter itself. See renderRepoReadError on why the
// structured location and message are matched rather than a bare 422.
func readinessParameterUnsupported(err error) bool {
	var problem *coreapi.ErrorModelStatusCode
	if !errors.As(err, &problem) || problem.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	for _, detail := range problem.Response.Errors {
		if detail.Location.Or("") == "query.authoritative" && detail.Message.Or("") == "unknown query parameter" {
			return true
		}
	}
	return false
}

// readinessCheckUnavailable reports whether dropping the readiness check would
// plausibly let the read succeed: the core cannot serve or route the lifecycle
// read (503), or it does not know the parameter at all (the compatibility 422
// above). Every other failure — the repository is missing, the caller cannot
// see it, the ID is malformed — is about the repository rather than about
// readiness, so a retry without the check answers nothing and the hint is
// withheld.
func readinessCheckUnavailable(err error) bool {
	var problem *coreapi.ErrorModelStatusCode
	if errors.As(err, &problem) && problem.StatusCode == http.StatusServiceUnavailable {
		return true
	}
	return readinessParameterUnsupported(err)
}
