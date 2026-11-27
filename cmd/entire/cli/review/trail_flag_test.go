package review_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// trailFlagRun is one `entire review --trail` invocation against a stub
// reviewer that reports one finding.
type trailFlagRun struct {
	events    []reviewtypes.Event // nil reports one finding
	resolved  []string
	posted    *review.TrailReport
	started   bool
	out       string // stdout
	errOut    string // stderr
	err       error
	reviewCfg reviewtypes.RunConfig
	withDeps  func(*review.Deps) // adjusts the deps before the run, if set
	duringRun func()             // runs while the reviewer runs, if set
}

type trailFlagReviewer struct {
	run *trailFlagRun
}

func (r trailFlagReviewer) Name() string { return "claude-code" }
func (r trailFlagReviewer) Start(_ context.Context, cfg reviewtypes.RunConfig) (reviewtypes.Process, error) {
	r.run.started = true
	r.run.reviewCfg = cfg
	if r.run.duringRun != nil {
		r.run.duringRun()
	}
	events := r.run.events
	if events == nil {
		events = []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.AssistantText{Text: "- [high] f.txt:1 a real bug"},
			reviewtypes.Finished{Success: true},
		}
	}
	return &scriptedDispatchProcess{events: events}, nil
}

// runTrailFlagReview runs `entire review <args>` in the current repository with
// a single claude-code reviewer. resolveErr fails --trail resolution; postErr
// fails posting.
func runTrailFlagReview(t *testing.T, resolveErr, postErr error, args ...string) *trailFlagRun {
	t.Helper()
	return runTrailFlagReviewWith(t, &trailFlagRun{}, resolveErr, postErr, args...)
}

func runTrailFlagReviewWith(t *testing.T, run *trailFlagRun, resolveErr, postErr error, args ...string) *trailFlagRun {
	t.Helper()
	deps := review.Deps{
		GetAgentsWithHooksInstalled: func(context.Context) []types.AgentName {
			return []types.AgentName{"claude-code"}
		},
		NewSilentError:          func(err error) error { return err },
		HeadHasReviewCheckpoint: func(context.Context) (bool, string) { return false, "" },
		ReviewerFor: func(string) reviewtypes.AgentReviewer {
			return trailFlagReviewer{run: run}
		},
		PostReviewToTrail: func(context.Context, io.Writer, string, review.TrailReport) error {
			t.Error("--trail must not post to the current branch's trail")
			return nil
		},
		ResolveTrail: func(_ *cobra.Command, selector string) (review.TrailPoster, error) {
			run.resolved = append(run.resolved, selector)
			if run.started {
				t.Error("--trail was resolved after the reviewer started")
			}
			if resolveErr != nil {
				return nil, resolveErr
			}
			return func(_ context.Context, w io.Writer, _ string, report review.TrailReport) error {
				run.posted = &report
				if postErr != nil {
					return postErr
				}
				_, err := io.WriteString(w, "Posted the review verdict to trail #42.\n")
				return err
			}, nil
		},
	}
	if run.withDeps != nil {
		run.withDeps(&deps)
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd := review.NewCommand(deps)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(args)
	run.err = cmd.Execute()
	run.out, run.errOut = out.String(), errOut.String()
	return run
}

// --trail posts to the named trail even when the profile keeps reviews local
// and the checkout is detached, hands the poster the reviewed commit, and
// resolves the trail before any reviewer runs. Not parallel: changes CWD.
func TestRunReview_TrailFlagPostsToNamedTrail(t *testing.T) {
	setupCmdTestRepo(t)
	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{"claude-code": {Prompt: "review"}}); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(testutil.RunGit(t, ".", "rev-parse", "HEAD"))
	testutil.RunGit(t, ".", "checkout", "--detach", "HEAD")

	run := runTrailFlagReview(t, nil, nil, "general", "--trail", " 42 ")
	if run.err != nil {
		t.Fatalf("review: %v\n%s", run.err, run.out)
	}
	if len(run.resolved) != 1 || run.resolved[0] != "42" {
		t.Fatalf("resolved %q, want one trimmed selector 42", run.resolved)
	}
	if run.posted == nil {
		t.Fatalf("nothing was posted:\n%s", run.out)
	}
	if run.posted.HeadSHA != head {
		t.Errorf("posted HeadSHA = %q, want the reviewed commit %q", run.posted.HeadSHA, head)
	}
	if posted := run.posted.Prose + fmt.Sprint(run.posted.Comments); !strings.Contains(posted, "a real bug") {
		t.Errorf("posted report %+v does not carry the reviewer's finding", *run.posted)
	}
	if !run.reviewCfg.TrailFindingsJSON {
		t.Error("the reviewer was not asked for the trail findings line, though --trail posts its output")
	}
}

// A --trail run fails when its findings are not posted, unlike a profile's
// output: trail, so a script can tell a posted review from a local one; a
// selector that names no trail fails before the agents start.
func TestRunReview_TrailFlagFailsWhenNotPosted(t *testing.T) {
	setupCmdTestRepo(t)
	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{"claude-code": {Prompt: "review"}}); err != nil {
		t.Fatal(err)
	}

	run := runTrailFlagReview(t, nil, errors.New("the trail's head is abc, not the reviewed commit"), "general", "--trail", "42")
	if run.err == nil {
		t.Fatalf("a --trail review whose post failed exited zero:\n%s", run.out)
	}
	// The run's error is silent, so its explanation must reach stderr.
	if !strings.Contains(run.errOut, "Could not post the review to the trail: the trail's head is abc") {
		t.Errorf("stderr does not say why nothing was posted:\n%s", run.errOut)
	}

	run = runTrailFlagReview(t, errors.New("no trail #999"), nil, "general", "--trail", "999")
	if run.err == nil || !strings.Contains(run.err.Error(), "--trail 999: no trail #999") {
		t.Fatalf("err = %v, want the resolution error", run.err)
	}
	if run.started {
		t.Error("the reviewer started though --trail named no trail")
	}
}

// --trail only means something for a review run, and an empty value would
// silently fall back to the current branch.
func TestRunReview_TrailFlagRejectsModesWithoutAReview(t *testing.T) {
	setupCmdTestRepo(t)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--trail", "", "general"}, "--trail cannot be empty"},
		{[]string{"--trail", "42", "--findings"}, "--trail applies to a review run"},
		{[]string{"--trail", "42", "--list"}, "--trail applies to a review run"},
		{[]string{"--trail", "42", "--show-config"}, "--trail applies to a review run"},
	} {
		run := runTrailFlagReview(t, nil, nil, tt.args...)
		if run.err == nil || !strings.Contains(run.err.Error(), tt.want) {
			t.Errorf("%v: err = %v, want %q", tt.args, run.err, tt.want)
		}
		if len(run.resolved) != 0 || run.started {
			t.Errorf("%v: resolved %v, started %v; want neither", tt.args, run.resolved, run.started)
		}
	}
}

// A --trail review refuses a worktree with uncommitted changes, which the
// reviewers would read but the reviewed commit does not contain, before it
// resolves the trail or starts a reviewer: an untracked file even when git
// status hides untracked files, and an edit, deletion or type change of a file
// git is told to treat as unchanged, compared the way git status would
// compare it, and edits in a submodule whose changes git status ignores. A
// skip-worktree file a sparse checkout left out is no edit.
// Not parallel: changes CWD.
func TestRunReview_TrailFlagRefusesUncommittedChanges(t *testing.T) {
	for _, tt := range []struct {
		name  string
		dirty func(t *testing.T)
		want  string // empty: the review runs and posts
	}{
		{"untracked file", func(t *testing.T) {
			t.Helper()
			testutil.WriteFile(t, ".", "scratch.txt", "not committed")
		}, "uncommitted changes (1 paths)"},
		{"untracked file that git status hides", func(t *testing.T) {
			t.Helper()
			testutil.RunGit(t, ".", "config", "status.showUntrackedFiles", "no")
			testutil.WriteFile(t, ".", "scratch.txt", "not committed")
		}, "uncommitted changes (1 paths)"},
		{"edit to an assume-unchanged file", func(t *testing.T) {
			t.Helper()
			testutil.RunGit(t, ".", "update-index", "--assume-unchanged", "f.txt")
			testutil.WriteFile(t, ".", "f.txt", "edited")
		}, "uncommitted changes (1 paths, 1 of them marked assume-unchanged or skip-worktree)"},
		{"deleted assume-unchanged file", func(t *testing.T) {
			t.Helper()
			testutil.RunGit(t, ".", "update-index", "--assume-unchanged", "f.txt")
			if err := os.Remove("f.txt"); err != nil {
				t.Fatal(err)
			}
		}, "uncommitted changes (1 paths, 1 of them marked assume-unchanged or skip-worktree)"},
		{"assume-unchanged file replaced by a symlink", func(t *testing.T) {
			t.Helper()
			testutil.RunGit(t, ".", "update-index", "--assume-unchanged", "f.txt")
			if err := os.Remove("f.txt"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("elsewhere.txt", "f.txt"); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}, "uncommitted changes (1 paths, 1 of them marked assume-unchanged or skip-worktree)"},
		{"assume-unchanged file committed with CRLF under text=auto", func(t *testing.T) {
			t.Helper()
			// The blob keeps its CRLF, so hashing the file with today's
			// attributes would not give the committed object.
			testutil.RunGit(t, ".", "config", "core.autocrlf", "false")
			testutil.WriteFile(t, ".", "crlf.txt", "a\r\nb\r\n")
			testutil.RunGit(t, ".", "add", "crlf.txt")
			testutil.RunGit(t, ".", "commit", "-m", "a CRLF file")
			if blob := testutil.RunGit(t, ".", "cat-file", "-p", "HEAD:crlf.txt"); !strings.Contains(blob, "\r") {
				t.Fatalf("committed blob %q lost its CRLF", blob)
			}
			testutil.WriteFile(t, ".", ".gitattributes", "* text=auto\n")
			testutil.RunGit(t, ".", "add", ".gitattributes")
			testutil.RunGit(t, ".", "commit", "-m", "normalize line endings")
			testutil.RunGit(t, ".", "update-index", "--assume-unchanged", "crlf.txt")
		}, ""},
		{"staged and edited assume-unchanged file", func(t *testing.T) {
			t.Helper()
			testutil.WriteFile(t, ".", "f.txt", "staged")
			testutil.GitAdd(t, ".", "f.txt")
			testutil.RunGit(t, ".", "update-index", "--assume-unchanged", "f.txt")
			testutil.WriteFile(t, ".", "f.txt", "edited again")
		}, "uncommitted changes (1 paths)"},
		{"edit inside a submodule whose changes git status ignores", func(t *testing.T) {
			t.Helper()
			addTrailTestSubmodule(t)
			testutil.RunGit(t, ".", "config", "-f", ".gitmodules", "submodule.sub.ignore", "all")
			testutil.RunGit(t, ".", "add", ".gitmodules")
			testutil.RunGit(t, ".", "commit", "-m", "ignore the submodule")
			testutil.WriteFile(t, "sub", "lib.txt", "edited")
			requireStatusHides(t, "--porcelain")
		}, "uncommitted changes (1 paths)"},
		{"edit inside a submodule whose gitlink is marked assume-unchanged", func(t *testing.T) {
			t.Helper()
			addTrailTestSubmodule(t)
			testutil.RunGit(t, ".", "update-index", "--assume-unchanged", "sub")
			testutil.WriteFile(t, "sub", "lib.txt", "edited")
			requireStatusHides(t, "--porcelain", "--ignore-submodules=none")
		}, "uncommitted changes (1 paths, 1 of them marked assume-unchanged or skip-worktree)"},
		{"edit a submodule's own index marks assume-unchanged", func(t *testing.T) {
			t.Helper()
			addTrailTestSubmodule(t)
			testutil.RunGit(t, "sub", "update-index", "--assume-unchanged", "lib.txt")
			testutil.WriteFile(t, "sub", "lib.txt", "edited")
			requireStatusHides(t, "--porcelain", "--ignore-submodules=none")
		}, "uncommitted changes (1 paths, 1 of them marked assume-unchanged or skip-worktree)"},
		{"skip-worktree file left out of a sparse checkout", func(t *testing.T) {
			t.Helper()
			testutil.RunGit(t, ".", "update-index", "--skip-worktree", "f.txt")
			if err := os.Remove("f.txt"); err != nil {
				t.Fatal(err)
			}
		}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupCmdTestRepo(t)
			if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{"claude-code": {Prompt: "review"}}); err != nil {
				t.Fatal(err)
			}
			tt.dirty(t)

			checkWorktreeRefusal(t, runTrailFlagReview(t, nil, nil, "general", "--trail", "42"), tt.want)
		})
	}
}

// addTrailTestSubmodule commits a submodule at sub, holding lib.txt, to the
// repository in the current directory.
func addTrailTestSubmodule(t *testing.T) {
	t.Helper()
	source := t.TempDir()
	testutil.InitRepo(t, source)
	testutil.WriteFile(t, source, "lib.txt", "x")
	testutil.RunGit(t, source, "add", "lib.txt")
	testutil.RunGit(t, source, "commit", "-m", "lib")
	testutil.RunGit(t, ".", "-c", "protocol.file.allow=always", "submodule", "add", source, "sub")
	testutil.RunGit(t, ".", "commit", "-m", "add a submodule")
}

// requireStatusHides fails the test when git status, run with args, reports
// the change, so the case would check nothing.
func requireStatusHides(t *testing.T, args ...string) {
	t.Helper()
	if status := testutil.RunGit(t, ".", append([]string{"--no-optional-locks", "status"}, args...)...); status != "" {
		t.Fatalf("git status reports the change, so this checks nothing:\n%s", status)
	}
}

// checkWorktreeRefusal checks that run was refused with want before it
// resolved the trail or started a reviewer, or with want empty, that it
// posted.
func checkWorktreeRefusal(t *testing.T, run *trailFlagRun, want string) {
	t.Helper()
	if want == "" {
		if run.err != nil || run.posted == nil {
			t.Fatalf("err = %v, posted %v; want a posted review\n%s", run.err, run.posted != nil, run.errOut)
		}
		return
	}
	if run.err == nil || !strings.Contains(run.err.Error(), want) {
		t.Fatalf("err = %v, want %q", run.err, want)
	}
	if len(run.resolved) != 0 || run.started {
		t.Errorf("resolved %v, started %v; want neither", run.resolved, run.started)
	}
}

// A --trail run rechecks the worktree when the review ends: an edit or a
// commit made while the reviewers ran means a finding can describe content
// the reviewed commit does not hold, and a clean result can miss what it
// does, so nothing is posted and the run fails. Not parallel: changes CWD.
func TestRunReview_TrailFlagRechecksTheWorktreeBeforePosting(t *testing.T) {
	edited := func(t *testing.T) {
		t.Helper()
		testutil.WriteFile(t, ".", "f.txt", "edited while the review ran")
	}
	const editedWant = "the worktree gained uncommitted changes (1 paths) while the review ran"
	clean := []reviewtypes.Event{
		reviewtypes.Started{},
		reviewtypes.AssistantText{Text: `{"summary":"No issues","comments":[]}`},
		reviewtypes.Finished{Success: true},
	}
	for _, tt := range []struct {
		name    string
		events  []reviewtypes.Event // nil reports one finding
		change  func(t *testing.T)
		want    string
		because string // what stderr says before want
	}{
		{"tracked file edited", nil, edited, editedWant, "Nothing was posted to the trail: "},
		{"new commit", nil, func(t *testing.T) {
			t.Helper()
			testutil.RunGit(t, ".", "commit", "--allow-empty", "-q", "-m", "during the review")
		}, "HEAD moved from ", "Nothing was posted to the trail: "},
		{"tracked file edited during a clean review", clean, edited, editedWant,
			"The review found nothing to report, but it did not review the commit alone: "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupCmdTestRepo(t)
			if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{"claude-code": {Prompt: "review"}}); err != nil {
				t.Fatal(err)
			}
			run := runTrailFlagReviewWith(t, &trailFlagRun{events: tt.events, duringRun: func() { tt.change(t) }}, nil, nil, "general", "--trail", "42")
			if run.err == nil || !strings.Contains(run.err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q\n%s", run.err, tt.want, run.errOut)
			}
			if run.posted != nil {
				t.Errorf("posted %+v, want nothing", *run.posted)
			}
			if !strings.Contains(run.errOut, tt.because+tt.want) {
				t.Errorf("stderr does not say why the run failed:\n%s", run.errOut)
			}
		})
	}
}

// A first --trail run with no review profile uses the default one without
// saving it, so the checkout the reviewers read stays as committed, tracked
// settings included. Not parallel: changes CWD.
func TestRunReview_TrailFlagFirstRunLeavesTheSettingsAlone(t *testing.T) {
	setupCmdTestRepo(t)
	testutil.WriteFile(t, ".", ".entire/settings.json", "{}\n")
	testutil.GitAdd(t, ".", ".entire/settings.json")
	testutil.GitCommit(t, ".", "track the settings")

	run := runTrailFlagReview(t, nil, nil, "general", "--trail", "42")
	if run.err != nil || run.posted == nil {
		t.Fatalf("err = %v, posted %v; want a posted review\n%s\n%s", run.err, run.posted != nil, run.out, run.errOut)
	}
	if !strings.Contains(run.out, "No review profiles found; using default") || !strings.Contains(run.out, "It is not saved") {
		t.Errorf("the run did not say it uses the default profile unsaved:\n%s", run.out)
	}
	if status := testutil.RunGit(t, ".", "--no-optional-locks", "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Errorf("the run changed the checkout:\n%s", status)
	}
}

// A first --target --trail run writes nothing either: its target can be this
// very checkout. It passes the default profile it used to the re-run in the
// worktree instead, so the re-run runs the reviewers the trust gate
// inspected. Not parallel: changes CWD.
func TestRunReview_TrailFlagFirstTargetRunPassesTheProfileToTheReRun(t *testing.T) {
	setupCmdTestRepo(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(testutil.RunGit(t, root, "rev-parse", "HEAD"))
	var childEnv []string
	started := false
	run := runTrailFlagReviewWith(t, &trailFlagRun{withDeps: func(deps *review.Deps) {
		deps.ResolveTarget = func(context.Context, io.Writer, io.Writer, string) (review.ResolvedTarget, error) {
			return review.ResolvedTarget{Branch: "feature", HeadSHA: head, ExistingWorktree: root}, nil
		}
		deps.CheckoutTarget = func(context.Context, io.Writer, io.Writer, review.ResolvedTarget, bool) (review.TargetWorktree, error) {
			return review.TargetWorktree{Path: root}, nil
		}
		deps.InspectTrust = func(context.Context, review.TrustSource, []review.TrustAgent) (review.TrustInventory, error) {
			return review.TrustInventory{}, nil
		}
		deps.RunInWorktree = func(_ context.Context, _ string, _, env []string, _ io.Reader, _, _ io.Writer) error {
			started, childEnv = true, env
			return nil
		}
	}}, nil, nil, "general", "--target", "feature", "--trail", "42", "--trust-target", head)
	if run.err != nil || !started {
		t.Fatalf("err = %v, re-run started = %v; want the re-run started\n%s\n%s", run.err, started, run.out, run.errOut)
	}
	var passed struct {
		Name    string                       `json:"name"`
		Profile settings.ReviewProfileConfig `json:"profile"`
	}
	for _, entry := range childEnv {
		if raw, ok := strings.CutPrefix(entry, "ENTIRE_REVIEW_TARGET_PROFILE="); ok {
			if err := json.Unmarshal([]byte(raw), &passed); err != nil {
				t.Fatalf("the passed profile does not decode: %v\n%s", err, raw)
			}
		}
	}
	if _, ok := passed.Profile.Agents["claude-code"]; passed.Name != "general" || !ok {
		t.Errorf("the re-run was not passed the default profile; env = %q", childEnv)
	}
	if status := testutil.RunGit(t, root, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Errorf("the run changed the checkout it targets:\n%s", status)
	}
}

// The re-run in the target worktree uses the profile its parent passed when
// none is saved. Not parallel: changes CWD and the environment.
func TestRunReview_TrailFlagTargetReRunUsesThePassedProfile(t *testing.T) {
	setupCmdTestRepo(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"name": "general", "profile": settings.ReviewProfileConfig{
		Agents: map[string]settings.ReviewConfig{"claude-code": {Prompt: "review"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_REVIEW_FINDINGS_WORKTREE", root)
	t.Setenv("ENTIRE_REVIEW_TARGET_PROFILE", string(raw))

	run := runTrailFlagReview(t, nil, nil, "general", "--trail", "42")
	if run.err != nil || run.posted == nil {
		t.Fatalf("err = %v, posted %v; want a posted review\n%s\n%s", run.err, run.posted != nil, run.out, run.errOut)
	}
	if strings.Contains(run.out, "No review profiles found") {
		t.Errorf("the re-run built its own default profile:\n%s", run.out)
	}
	if _, set := os.LookupEnv("ENTIRE_REVIEW_TARGET_PROFILE"); set {
		t.Error("the passed profile was left in the environment for the reviewers")
	}
}

// A re-run in a target worktree that was passed no profile and finds none
// refuses, rather than build a default from the branch's own hooks, which the
// parent's trust gate never inspected. Not parallel: changes CWD and the
// environment.
func TestRunReview_TrailFlagTargetReRunWithoutAProfileRefuses(t *testing.T) {
	setupCmdTestRepo(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_REVIEW_FINDINGS_WORKTREE", root)

	run := runTrailFlagReview(t, nil, nil, "general", "--trail", "42")
	if run.err == nil || !strings.Contains(run.errOut, "was given no review profile") {
		t.Fatalf("err = %v, want the refusal\n%s\n%s", run.err, run.out, run.errOut)
	}
	if run.started || run.posted != nil {
		t.Error("a reviewer ran without a profile from the parent")
	}
}

// A reviewer that says nothing at all is not a clean review: a --trail run
// fails instead of reporting nothing to post. Not parallel: changes CWD.
func TestRunReview_TrailFlagFailsOnAnEmptyReview(t *testing.T) {
	setupCmdTestRepo(t)
	if err := seedReviewConfig(context.Background(), map[string]settings.ReviewConfig{"claude-code": {Prompt: "review"}}); err != nil {
		t.Fatal(err)
	}
	run := runTrailFlagReviewWith(t, &trailFlagRun{events: []reviewtypes.Event{
		reviewtypes.Started{}, reviewtypes.Finished{Success: true},
	}}, nil, nil, "general", "--trail", "42")
	if run.err == nil || !strings.Contains(run.err.Error(), "the review produced no output") {
		t.Fatalf("err = %v, want the empty-review failure\n%s", run.err, run.out)
	}
	if run.posted != nil {
		t.Error("an empty review was posted")
	}
}
