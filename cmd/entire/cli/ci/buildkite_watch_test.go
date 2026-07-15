package ci

import (
	"context"
	"strings"
	"testing"
	"time"
)

// This test file is untagged so the provider-neutral renderer and poll loop are
// covered by a normal `go test` run, not only the internal build.

func TestIsTerminalBuildState(t *testing.T) {
	for _, s := range []string{"passed", "failed", "canceled", "skipped", "not_run", "blocked", "finished"} {
		if !isTerminalBuildState(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range []string{"running", "scheduled", "failing", "canceling"} {
		if isTerminalBuildState(s) {
			t.Errorf("%q should NOT be terminal", s)
		}
	}
}

func TestRenderBuild_StepTree(t *testing.T) {
	start := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	now := start.Add(15 * time.Second)
	v := buildView{
		Number: 12, State: "running", Branch: "main", Commit: "abcdef0123456",
		URL: "https://buildkite.com/entire/canary/builds/12", HasStarted: true, Started: start,
		Steps: []stepView{
			{Label: ":package: build", State: "passed", Exit: new(0), HasStarted: true, Started: start, HasFinished: true, Finished: start.Add(12 * time.Second)},
			{Label: ":test: tests", State: "running", HasStarted: true, Started: start.Add(12 * time.Second)},
			{Label: ":rocket: deploy", State: "scheduled"},
		},
	}
	out := renderBuild(v, now, false)

	for _, want := range []string{
		"Build #12  RUNNING", "main", "abcdef0", "https://buildkite.com/entire/canary/builds/12",
		":package: build", "passed", ":test: tests", "running", ":rocket: deploy", "scheduled",
		"✓", "▶", "·", "15s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderBuild_ShowsFailingExitStatus(t *testing.T) {
	v := buildView{
		Number: 3, State: "failed",
		Steps: []stepView{{Label: "tests", State: "failed", Exit: new(2)}},
	}
	out := renderBuild(v, time.Now(), false)
	if !strings.Contains(out, "✗") || !strings.Contains(out, "exit 2") {
		t.Errorf("expected failure glyph + exit code in:\n%s", out)
	}
}

func TestRenderBuild_EmptyState(t *testing.T) {
	v := buildView{Number: 1, State: "scheduled"}
	out := renderBuild(v, time.Now(), false)
	if !strings.Contains(out, "(no steps yet)") {
		t.Errorf("expected empty-state note in:\n%s", out)
	}
}

func TestRenderBuild_ColorGatesStateWordOnly(t *testing.T) {
	v := buildView{Number: 5, State: "passed", Steps: []stepView{{Label: "build", State: "passed"}}}
	colored := renderBuild(v, time.Now(), true)
	if !strings.Contains(colored, "\x1b[32mPASSED\x1b[0m") {
		t.Errorf("expected colorized state word in:\n%q", colored)
	}
	plain := renderBuild(v, time.Now(), false)
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("plain render must carry no ANSI escapes:\n%q", plain)
	}
}

// fakeSource scripts a sequence of Build() results so we can drive runWatch
// deterministically to a terminal state.
type fakeSource struct {
	active []buildView
	seq    []buildView
	i      int
	calls  int
}

func (f *fakeSource) ActiveBuilds(context.Context) ([]buildView, error) { return f.active, nil }
func (f *fakeSource) Build(context.Context, int) (buildView, error) {
	f.calls++
	v := f.seq[f.i]
	if f.i < len(f.seq)-1 {
		f.i++
	}
	return v, nil
}

func TestRunWatch_StopsAtTerminalAndLogsTransitions(t *testing.T) {
	src := &fakeSource{seq: []buildView{
		{Number: 12, State: "running", Steps: []stepView{{Label: "build", State: "running"}}},
		{Number: 12, State: "running", Steps: []stepView{{Label: "build", State: "passed", Exit: new(0)}}},
		{Number: 12, State: "passed", Steps: []stepView{{Label: "build", State: "passed", Exit: new(0)}}},
	}}
	var out strings.Builder
	clock := func() time.Time { return time.Unix(0, 0) }

	err := runWatch(context.Background(), &out, src, 12, time.Millisecond, clock, false)
	if err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Build #12  PASSED") {
		t.Errorf("expected terminal PASSED frame in:\n%s", got)
	}
	// Non-TTY prints a frame per state transition: running, build-passed, build PASSED = 3.
	if n := strings.Count(got, "Build #12"); n != 3 {
		t.Errorf("frame count = %d, want 3 (one per state transition)", n)
	}
	if src.calls != 3 {
		t.Errorf("Build calls = %d, want 3 (stopped at terminal)", src.calls)
	}
}

func TestRunWatch_NoActiveBuilds(t *testing.T) {
	src := &fakeSource{active: nil}
	var out strings.Builder
	err := runWatch(context.Background(), &out, src, 0, time.Millisecond, nil, false)
	if err == nil || !strings.Contains(err.Error(), "no active builds") {
		t.Fatalf("err = %v, want errNoActiveBuilds", err)
	}
}

func TestRunWatch_PicksMostRecentActive(t *testing.T) {
	src := &fakeSource{
		active: []buildView{{Number: 14, State: "running"}, {Number: 9, State: "running"}},
		seq:    []buildView{{Number: 14, State: "passed", Steps: []stepView{{Label: "x", State: "passed"}}}},
	}
	var out strings.Builder
	if err := runWatch(context.Background(), &out, src, 0, time.Millisecond, nil, false); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	// With >1 active build, runWatch emits a notice naming the count and the
	// chosen (newest, active[0]) build number.
	if !strings.Contains(out.String(), "2 active builds") {
		t.Errorf("expected multi-active notice in:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "#14") {
		t.Errorf("expected to watch newest active build #14 in:\n%s", out.String())
	}
}
