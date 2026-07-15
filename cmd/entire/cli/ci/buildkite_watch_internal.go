//go:build internal

package ci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

const (
	// defaultWatchInterval is the poll cadence when --interval is unset.
	defaultWatchInterval = 2 * time.Second
	// ciBuildsLimit is how many recent builds we request per poll. The server
	// clamps this to 1..200 (default 50); we ask for enough headroom to find a
	// named build among recent ones and to spot in-flight builds without paging.
	ciBuildsLimit = 100
)

// activeBuildStates are the Buildkite build states treated as "in flight" when
// picking a default build to watch.
var activeBuildStates = []string{"scheduled", "running", "failing", "canceling"}

// newBuildkiteWatchCmd is the `entire ci buildkite watch` verb: a `gh run
// watch`-style live view of a repo's Buildkite build, driven by the neutral
// renderer in buildkite_watch.go over the core-mediated builds endpoint.
func newBuildkiteWatchCmd() *cobra.Command {
	var (
		pipeline string
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "watch <repo> [build]",
		Short: "Watch a repo's Buildkite build status live (like `gh run watch`)",
		Long: "Watch a repo's Buildkite build progress from the terminal, like `gh run watch`.\n\n" +
			"<repo> is a native <project>/<repo> path or a repo ULID. With no [build]\n" +
			"number the most recent in-progress build is watched; pass a number to watch a\n" +
			"specific one. Use --pipeline to narrow to one pipeline when a repo feeds\n" +
			"several.\n\n" +
			"Build status comes from entire-core's mediated builds endpoint (projected from\n" +
			"ingested Buildkite webhook events) using your login — no Buildkite API token is\n" +
			"needed. On a TTY the step tree redraws in place; piped output logs each state\n" +
			"transition. The watcher exits when the build reaches a terminal state.",
		Example: "  entire ci buildkite watch acme/web\n" +
			"  entire ci buildkite watch acme/web 42\n" +
			"  entire ci buildkite watch acme/web --pipeline web --interval 5s",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			number := 0
			if len(args) == 2 {
				n, err := strconv.Atoi(strings.TrimSpace(args[1]))
				if err != nil || n <= 0 {
					cmd.SilenceUsage = true
					return fmt.Errorf("invalid build number %q: want a positive integer", args[1])
				}
				number = n
			}
			if interval <= 0 {
				interval = defaultWatchInterval
			}
			pipeline = strings.TrimSpace(pipeline)
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				repoID, err := ResolveNativeRepo(ctx, c, args[0])
				if err != nil {
					return err
				}
				src := &coreBuildSource{client: c, repoID: repoID, pipeline: pipeline}
				out := cmd.OutOrStdout()
				err = runWatch(ctx, out, src, number, interval, time.Now, isTerminalWriter(out))
				if errors.Is(err, errNoActiveBuilds) {
					fmt.Fprintf(out, "no in-progress builds for %s%s; pass a build number to watch a specific build\n", args[0], pipelineSuffix(pipeline))
					return nil
				}
				return err
			})
		},
	}
	cmd.Flags().StringVar(&pipeline, "pipeline", "", "Only watch builds for this Buildkite pipeline slug (default: all pipelines)")
	cmd.Flags().DurationVar(&interval, "interval", defaultWatchInterval, "Poll interval")
	return cmd
}

// coreBuildSource is the production buildSource. It reads build status from the
// core-mediated builds endpoint (GET /api/v1/repos/{repoId}/ci-builds), which
// projects the Buildkite webhook events ingested by ci-webhooks. It reuses the
// caller's coreapi client — authenticating as the logged-in user via the same
// bearer plumbing as the other `ci` verbs — so it needs no per-user Buildkite
// token and no leaf ops token.
type coreBuildSource struct {
	client   *coreapi.Client
	repoID   string
	pipeline string // optional pipeline-slug filter ("" = all pipelines)
}

// ciJobDTO / ciBuildDTO / ciBuildsResponse mirror entire-core's ci-builds
// response DTO (api/corev1: CIJobView / CIBuildView) field for field. This
// endpoint (entiredb#2741) is not yet in this repo's generated coreapi spec, so
// the request is hand-rolled via coreapi.Client.GetJSON and decoded here. Once
// the spec is regenerated to include ci-builds, delete these structs and
// coreBuildSource.fetch's GetJSON call in favour of the generated method.
type ciJobDTO struct {
	BKJobID    string     `json:"bk_job_id"`
	Type       string     `json:"type"`
	Name       string     `json:"name"`
	StepKey    string     `json:"step_key"`
	State      string     `json:"state"`
	ExitStatus *int       `json:"exit_status,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type ciBuildDTO struct {
	BKBuildUUID    string     `json:"bk_build_uuid"`
	BKOrganization string     `json:"bk_organization"`
	BKPipeline     string     `json:"bk_pipeline"`
	BuildNumber    int64      `json:"build_number"`
	State          string     `json:"state"`
	Commit         string     `json:"commit"`
	Branch         string     `json:"branch"`
	Message        string     `json:"message"`
	WebURL         string     `json:"web_url"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	LastEvent      string     `json:"last_event"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Jobs           []ciJobDTO `json:"jobs"`
}

type ciBuildsResponse struct {
	Builds []ciBuildDTO `json:"builds"`
}

// fetch pulls the repo's recent builds, applying the pipeline filter and limit.
// The commit filter the endpoint also supports is left unset — watch has no
// commit input — so an empty commit means "all commits".
func (s *coreBuildSource) fetch(ctx context.Context) ([]ciBuildDTO, error) {
	q := url.Values{}
	if s.pipeline != "" {
		q.Set("pipeline", s.pipeline)
	}
	q.Set("limit", strconv.Itoa(ciBuildsLimit))
	var resp ciBuildsResponse
	if err := s.client.GetJSON(ctx, "repos/"+s.repoID+"/ci-builds", q, &resp); err != nil {
		return nil, err
	}
	return resp.Builds, nil
}

func (s *coreBuildSource) ActiveBuilds(ctx context.Context) ([]buildView, error) {
	builds, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]buildView, 0, len(builds))
	for i := range builds {
		if !isActiveBuildState(builds[i].State) {
			continue
		}
		views = append(views, buildViewFromCore(&builds[i]))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Number > views[j].Number })
	return views, nil
}

func (s *coreBuildSource) Build(ctx context.Context, number int) (buildView, error) {
	builds, err := s.fetch(ctx)
	if err != nil {
		return buildView{}, err
	}
	for i := range builds {
		if int(builds[i].BuildNumber) == number {
			return buildViewFromCore(&builds[i]), nil
		}
	}
	return buildView{}, fmt.Errorf("build #%d not found for this repo%s (it may be older than the %d most recent builds)", number, pipelineSuffix(s.pipeline), ciBuildsLimit)
}

// buildViewFromCore projects a ci-builds DTO onto the neutral view model.
// Waiter jobs (structural barriers with no command) are dropped — they're noise
// in a step tree.
func buildViewFromCore(b *ciBuildDTO) buildView {
	v := buildView{
		Number:  int(b.BuildNumber),
		State:   b.State,
		Branch:  b.Branch,
		Commit:  b.Commit,
		Message: firstLine(b.Message),
		URL:     b.WebURL,
	}
	v.Started, v.HasStarted = fromPtrTime(b.StartedAt)
	v.Finished, v.HasFinished = fromPtrTime(b.FinishedAt)
	for i := range b.Jobs {
		j := &b.Jobs[i]
		if j.Type == "waiter" {
			continue
		}
		sv := stepView{Label: stepLabel(j.Name, j.StepKey, j.Type), Key: j.StepKey, State: j.State, Exit: j.ExitStatus}
		sv.Started, sv.HasStarted = fromPtrTime(j.StartedAt)
		sv.Finished, sv.HasFinished = fromPtrTime(j.FinishedAt)
		v.Steps = append(v.Steps, sv)
	}
	return v
}

// fromPtrTime projects an optional timestamp onto the (time, ok) pair the view
// model uses; a nil or zero time means "not yet started/finished".
func fromPtrTime(t *time.Time) (time.Time, bool) {
	if t == nil || t.IsZero() {
		return time.Time{}, false
	}
	return *t, true
}

func isActiveBuildState(state string) bool {
	for _, s := range activeBuildStates {
		if state == s {
			return true
		}
	}
	return false
}

// pipelineSuffix renders an optional " (pipeline <slug>)" qualifier for
// user-facing messages, or "" when no pipeline filter is set.
func pipelineSuffix(pipeline string) string {
	if pipeline == "" {
		return ""
	}
	return " (pipeline " + pipeline + ")"
}

// firstLine trims a build message to its first line, capped at 72 runes, for a
// tidy single-line header.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len([]rune(s)) > 72 {
		s = string([]rune(s)[:71]) + "…"
	}
	return s
}

// stepLabel picks the best human label for a step from its (name, step_key,
// type), falling back through them so a frame never shows a blank cell.
func stepLabel(name, stepKey, typ string) string {
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	if s := strings.TrimSpace(stepKey); s != "" {
		return s
	}
	if s := strings.TrimSpace(typ); s != "" {
		return "(" + s + ")"
	}
	return "(step)"
}

// isTerminalWriter reports whether w is a character device (a TTY), so the
// watcher can choose in-place redraw vs. plain transition logging.
// Dependency-free: no x/term / go-isatty.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
