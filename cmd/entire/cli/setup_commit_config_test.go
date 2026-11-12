package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const testEntireGitignore = ".entire/.gitignore"

// newCommitConfigRepo chdirs into a fresh repository with git isolated from
// the developer's global and system config, commits a README unless empty is
// set, and leaves .entire/settings.json untracked.
func newCommitConfigRepo(t *testing.T, empty bool) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := setupTestRepo(t)
	if !empty {
		testutil.WriteFile(t, dir, "README.md", "x")
		testutil.GitAdd(t, dir, "README.md")
		testutil.GitCommit(t, dir, "initial")
	}
	testutil.WriteFile(t, dir, EntireSettingsFile, `{"enabled":true}`)
	return dir
}

type commitConfigPromptFunc = func(context.Context, io.Writer, []string, []string, string, func(io.Writer) error) error

// stubCommitConfig replaces the prompt and, when commit is non-nil, the commit
// for one test.
func stubCommitConfig(t *testing.T, prompt commitConfigPromptFunc, commit func(context.Context, string, []string, string, io.Writer) error) {
	t.Helper()
	origPrompt, origCommit := commitConfigPrompt, commitConfigFiles
	t.Cleanup(func() { commitConfigPrompt, commitConfigFiles = origPrompt, origCommit })
	commitConfigPrompt = prompt
	if commit != nil {
		commitConfigFiles = commit
	}
}

// promptedCommitConfig is a stub prompt that behaves like the real one — the
// list on render, then on yes the commit and its report on the same writer —
// and answers with commit, or fails with err.
type promptedCommitConfig struct {
	render *bytes.Buffer
	commit bool
	err    error

	asked        bool
	errOut       io.Writer
	files        []string
	leftToReview []string
	branch       string
}

func (p *promptedCommitConfig) prompt(_ context.Context, errOut io.Writer, files, leftToReview []string, branch string, commitAndReport func(io.Writer) error) error {
	p.asked, p.errOut, p.files, p.leftToReview, p.branch = true, errOut, files, leftToReview, branch
	printConfigLeftToReview(p.render, leftToReview)
	printConfigToCommit(p.render, files)
	if p.err != nil {
		return p.err
	}
	if !p.commit {
		return nil
	}
	return commitAndReport(p.render)
}

func failOnPrompt(t *testing.T) commitConfigPromptFunc {
	t.Helper()
	return func(context.Context, io.Writer, []string, []string, string, func(io.Writer) error) error {
		t.Error("prompted to commit when no prompt was expected")
		return nil
	}
}

func assertOutput(t *testing.T, out string, want, notWant []string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
	for _, s := range notWant {
		if strings.Contains(out, s) {
			t.Errorf("output unexpectedly contains %q:\n%s", s, out)
		}
	}
}

// afterCleanSetup returns opts as setup hands them to the offer when no shared
// config file had uncommitted changes before the run, so every pending file
// is one setup changed.
func afterCleanSetup(opts EnableOptions) EnableOptions {
	opts.configCleanBefore = make(map[string]struct{})
	for _, file := range sharedConfigFiles() {
		opts.configCleanBefore[file] = struct{}{}
	}
	return opts
}

// beforeSetup returns opts carrying the snapshot setup takes before it writes
// anything.
func beforeSetup(t *testing.T, opts EnableOptions) EnableOptions {
	t.Helper()
	opts.snapshotCleanConfig(context.Background())
	if opts.configCleanBefore == nil {
		t.Fatal("snapshotCleanConfig() recorded nothing")
	}
	return opts
}

func runOfferCommitConfig(t *testing.T, opts EnableOptions) string {
	t.Helper()
	var out bytes.Buffer
	if err := offerCommitConfig(context.Background(), &out, opts); err != nil {
		t.Fatalf("offerCommitConfig() error = %v", err)
	}
	return out.String()
}

// Not parallel: newCommitConfigRepo chdirs and sets environment variables.
func TestOfferCommitConfig_ListsPendingFilesWithoutATerminal(t *testing.T) {
	tests := []struct {
		name    string
		empty   bool
		prepare func(t *testing.T, dir string)
		want    []string
		notWant []string
	}{
		{
			name:    "untracked settings",
			want:    []string{"Make sure to commit these files", "  .entire/settings.json\n"},
			notWant: []string{testEntireGitignore, "Session checkpoints"},
		},
		{
			name: "modified settings and untracked gitignore",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				testutil.GitAdd(t, dir, EntireSettingsFile)
				testutil.GitCommit(t, dir, "settings")
				testutil.WriteFile(t, dir, EntireSettingsFile, `{"enabled":false}`)
				testutil.WriteFile(t, dir, testEntireGitignore, "tmp/\n")
			},
			want: []string{"  .entire/settings.json\n  .entire/.gitignore\n"},
		},
		{
			name: "installed agent hook config",
			prepare: func(t *testing.T, _ string) {
				t.Helper()
				ag, err := agent.Get(agent.AgentNameClaudeCode)
				if err != nil {
					t.Fatalf("get Claude Code agent: %v", err)
				}
				hookAgent, ok := agent.AsHookSupport(ag)
				if !ok {
					t.Fatal("Claude Code agent does not support hooks")
				}
				if _, err := hookAgent.InstallHooks(context.Background(), false); err != nil {
					t.Fatalf("install Claude Code hooks: %v", err)
				}
			},
			want: []string{"  .claude/settings.json\n"},
		},
		{
			// Deselecting an agent rewrites or removes its hook config; the
			// agent is no longer installed, but the change still has to ship.
			name: "deselected agent hook config removed",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				testutil.WriteFile(t, dir, ".cursor/hooks.json", "{}")
				testutil.GitAdd(t, dir, ".cursor/hooks.json")
				testutil.GitCommit(t, dir, "cursor hooks")
				if err := os.Remove(".cursor/hooks.json"); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"  .cursor/hooks.json\n"},
		},
		{
			name: "skill files",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				testutil.WriteFile(t, dir, agentHelpSkillTemplatePath(agent.AgentNameClaudeCode), "skill")
				testutil.WriteFile(t, dir, searchSkillTemplatePath(agent.AgentNameCodex), "skill")
			},
			want: []string{"  .claude/skills/entire/SKILL.md\n", "  .agents/skills/"},
		},
		{
			name: "everything committed",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				testutil.WriteFile(t, dir, testEntireGitignore, "tmp/\n")
				testutil.GitAdd(t, dir, EntireSettingsFile, testEntireGitignore)
				testutil.GitCommit(t, dir, "settings")
			},
			notWant: []string{"commit these files", "Session checkpoints"},
		},
		{
			name:  "empty repository",
			empty: true,
			want:  []string{"Session checkpoints require at least one commit.\nMake sure to commit these files", "  .entire/settings.json\n"},
		},
		{
			name:  "empty repository with the configuration ignored",
			empty: true,
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				testutil.WriteFile(t, dir, ".git/info/exclude", ".entire/\n")
			},
			want:    []string{"Session checkpoints require at least one commit. Make one to get started.\n"},
			notWant: []string{"commit these files"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newCommitConfigRepo(t, tt.empty)
			if tt.prepare != nil {
				tt.prepare(t, dir)
			}
			stubCommitConfig(t, failOnPrompt(t), nil)

			assertOutput(t, runOfferCommitConfig(t, afterCleanSetup(EnableOptions{})), tt.want, tt.notWant)
		})
	}
}

// Not parallel: chdirs and sets environment variables.
func TestOfferCommitConfig_ListsWithoutPromptingWhenToldNotToAsk(t *testing.T) {
	tests := map[string]EnableOptions{
		"--yes":     {Yes: true},
		"agent add": {NoCommitOffer: true},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			newCommitConfigRepo(t, false)
			t.Setenv(interactive.EnvTestTTY, "1")
			stubCommitConfig(t, failOnPrompt(t), nil)

			assertOutput(t, runOfferCommitConfig(t, afterCleanSetup(opts)), []string{"Make sure to commit these files", "  .entire/settings.json\n"}, nil)
		})
	}
}

// Not parallel: chdirs and sets environment variables.
func TestOfferCommitConfig_ListsWithoutPromptingDuringAGitOperation(t *testing.T) {
	dir := newCommitConfigRepo(t, false)
	head := testutil.RunGit(t, dir, "rev-parse", "HEAD")
	testutil.WriteFile(t, dir, ".git/MERGE_HEAD", head)
	t.Setenv(interactive.EnvTestTTY, "1")
	stubCommitConfig(t, failOnPrompt(t), nil)

	assertOutput(t, runOfferCommitConfig(t, afterCleanSetup(EnableOptions{})), []string{"Make sure to commit these files", "  .entire/settings.json\n"}, nil)
}

// Not parallel: chdirs and sets environment variables.
func TestOfferCommitConfig_CommitsOnlyTheConfigFiles(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "repository with commits"
		if empty {
			name = "empty repository"
		}
		t.Run(name, func(t *testing.T) {
			dir := newCommitConfigRepo(t, empty)
			testutil.WriteFile(t, dir, "staged.txt", "unrelated")
			testutil.GitAdd(t, dir, "staged.txt")
			t.Setenv(interactive.EnvTestTTY, "1")
			branch := strings.TrimSpace(testutil.RunGit(t, dir, "symbolic-ref", "--short", "HEAD"))

			p := &promptedCommitConfig{render: &bytes.Buffer{}, commit: true}
			stubCommitConfig(t, p.prompt, nil)
			out := runOfferCommitConfig(t, afterCleanSetup(EnableOptions{}))

			if !slices.Equal(p.files, []string{EntireSettingsFile}) || p.branch != branch {
				t.Errorf("prompt got %v on %q, want [%s] on %q", p.files, p.branch, EntireSettingsFile, branch)
			}
			// The list and the outcome go where the prompt rendered, not to
			// the command's output.
			assertOutput(t, p.render.String(), []string{"  .entire/settings.json\n", "✓ Committed them to " + branch + "\n"}, nil)
			assertOutput(t, out, nil, []string{"Make sure to commit these files"})

			committed := strings.Fields(testutil.RunGit(t, dir, "show", "--name-only", "--format=%s", "HEAD"))
			if !slices.Equal(committed, []string{"Enable", "Entire", EntireSettingsFile}) {
				t.Errorf("HEAD = %v, want the %q commit of %s alone", committed, configCommitMessage, EntireSettingsFile)
			}
			if status := testutil.RunGit(t, dir, "status", "--porcelain", "--", "staged.txt"); !strings.HasPrefix(status, "A  staged.txt") {
				t.Errorf("staged.txt should still be staged and uncommitted, status %q", status)
			}
		})
	}
}

// Not parallel: chdirs and sets environment variables.
func TestOfferCommitConfig_DeclinedAbortedOrFailed(t *testing.T) {
	tests := []struct {
		name      string
		commit    bool
		promptErr error
		commitErr error
		render    []string
		out       []string
	}{
		{name: "declined"},
		{
			name:      "no terminal to prompt on",
			promptErr: errors.New("open prompt terminal: no controlling terminal"),
			out:       []string{"Make sure to commit these files", "  .entire/settings.json\n"},
		},
		{
			name:      "commit failed",
			commit:    true,
			commitErr: errors.New("git commit: exit status 1"),
			render:    []string{"Could not commit them: git commit: exit status 1\n"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newCommitConfigRepo(t, false)
			t.Setenv(interactive.EnvTestTTY, "1")
			p := &promptedCommitConfig{render: &bytes.Buffer{}, commit: tt.commit, err: tt.promptErr}
			stubCommitConfig(t, p.prompt, func(context.Context, string, []string, string, io.Writer) error {
				if !tt.commit {
					t.Error("committed after the prompt was not accepted")
				}
				return tt.commitErr
			})

			out := runOfferCommitConfig(t, afterCleanSetup(EnableOptions{}))
			assertOutput(t, p.render.String(), tt.render, []string{"✓ Committed"})
			assertOutput(t, out, tt.out, []string{"✓ Committed"})
		})
	}
}

// Not parallel: chdirs and sets environment variables.
func TestOfferCommitConfig_InterruptIsAnError(t *testing.T) {
	t.Run("at the question", func(t *testing.T) {
		newCommitConfigRepo(t, false)
		t.Setenv(interactive.EnvTestTTY, "1")
		ctx, cancel := context.WithCancel(context.Background())
		stubCommitConfig(t, func(context.Context, io.Writer, []string, []string, string, func(io.Writer) error) error {
			cancel() // Ctrl+C while the question is up
			return context.Canceled
		}, func(context.Context, string, []string, string, io.Writer) error {
			t.Error("committed after an interrupt")
			return nil
		})

		if err := offerCommitConfig(ctx, io.Discard, afterCleanSetup(EnableOptions{})); !errors.Is(err, context.Canceled) {
			t.Fatalf("offerCommitConfig() error = %v, want context.Canceled", err)
		}
	})
	t.Run("during the commit", func(t *testing.T) {
		newCommitConfigRepo(t, false)
		t.Setenv(interactive.EnvTestTTY, "1")
		ctx, cancel := context.WithCancel(context.Background())
		p := &promptedCommitConfig{render: &bytes.Buffer{}, commit: true}
		stubCommitConfig(t, p.prompt, func(context.Context, string, []string, string, io.Writer) error {
			cancel() // Ctrl+C during a slow pre-commit hook kills git
			return errors.New("git commit: signal: killed")
		})

		if err := offerCommitConfig(ctx, io.Discard, afterCleanSetup(EnableOptions{})); !errors.Is(err, context.Canceled) {
			t.Fatalf("offerCommitConfig() error = %v, want context.Canceled", err)
		}
		assertOutput(t, p.render.String(), nil, []string{"Could not commit them", "✓ Committed"})
	})
}

// Not parallel: chdirs and sets environment variables.
func TestOfferCommitConfig_PromptsOnTheCommandsErrorStream(t *testing.T) {
	newCommitConfigRepo(t, false)
	t.Setenv(interactive.EnvTestTTY, "1")
	p := &promptedCommitConfig{render: &bytes.Buffer{}}
	stubCommitConfig(t, p.prompt, nil)

	var errOut bytes.Buffer
	runOfferCommitConfig(t, afterCleanSetup(EnableOptions{errOut: &errOut}))
	if p.errOut != &errOut {
		t.Errorf("prompt got error stream %v, want the command's", p.errOut)
	}
}

// closeableTerminal is a fallback prompt terminal that records what was
// written to it and fails every write after Close, as a closed tty would.
type closeableTerminal struct {
	t      *testing.T
	buf    bytes.Buffer
	closed bool
}

func (c *closeableTerminal) Write(p []byte) (int, error) {
	if c.closed {
		c.t.Errorf("write to the prompt terminal after it was closed: %q", p)
		return 0, os.ErrClosed
	}
	return c.buf.Write(p)
}

func (c *closeableTerminal) Close() error {
	c.closed = true
	return nil
}

// Not parallel: swaps the terminal opener and sets ACCESSIBLE.
func TestPromptCommitConfig_ListQuestionAndOutcomeShareTheTerminal(t *testing.T) {
	// The command's error stream is redirected (not a terminal), so the form
	// falls back to the controlling terminal. The list must go there with the
	// question, and the commit and its report must reach it before it closes.
	t.Setenv("ACCESSIBLE", "1")
	terminal := &closeableTerminal{t: t}
	prev := openPromptTerminal
	openPromptTerminal = func() (promptTerminal, error) {
		return promptTerminal{in: strings.NewReader("y\n"), out: terminal, close: terminal.Close}, nil
	}
	t.Cleanup(func() { openPromptTerminal = prev })

	var redirected bytes.Buffer
	committed := false
	err := promptCommitConfig(t.Context(), &redirected, []string{EntireSettingsFile, ".claude/settings.json"}, []string{".cursor/hooks.json"}, "main", func(w io.Writer) error {
		committed = true
		_, err := io.WriteString(w, "hook output\n✓ Committed them to main\n")
		return err
	})
	if err != nil {
		t.Fatalf("promptCommitConfig() error = %v", err)
	}
	if !committed {
		t.Error("answering y should commit")
	}
	if !terminal.closed {
		t.Error("the fallback terminal should be closed once the prompt returns")
	}
	assertOutput(t, terminal.buf.String(), []string{"had uncommitted changes before this run", "  .cursor/hooks.json\nMake sure to commit these files", "  .entire/settings.json\n  .claude/settings.json\n", "Commit them to main now?", "hook output\n✓ Committed them to main\n"}, nil)
	if redirected.Len() != 0 {
		t.Errorf("nothing should go to the redirected stream, got:\n%s", redirected.String())
	}
}

// Not parallel: chdirs and sets environment variables.
func TestOfferCommitConfig_LeavesFilesThatWereAlreadyDirtyOutOfTheCommit(t *testing.T) {
	const claudeSettings = ".claude/settings.json"
	const localEdit = `{"env":{"API_KEY":"local"}}`
	trackClaudeSettings := func(t *testing.T, dir string) {
		t.Helper()
		testutil.WriteFile(t, dir, claudeSettings, "{}")
		testutil.GitAdd(t, dir, claudeSettings)
		testutil.GitCommit(t, dir, "claude settings")
	}
	tests := []struct {
		name string
		// before leaves the developer's own uncommitted changes in place.
		before func(t *testing.T, dir string)
		// setup makes the changes enable would, after the snapshot.
		setup      func(t *testing.T, dir string)
		wantReview string
	}{
		{
			name: "unstaged edit to a tracked hook config",
			before: func(t *testing.T, dir string) {
				t.Helper()
				trackClaudeSettings(t, dir)
				testutil.WriteFile(t, dir, claudeSettings, localEdit)
			},
			wantReview: claudeSettings,
		},
		{
			name: "unstaged edit to a hook config setup then rewrites",
			before: func(t *testing.T, dir string) {
				t.Helper()
				trackClaudeSettings(t, dir)
				testutil.WriteFile(t, dir, claudeSettings, localEdit)
			},
			setup: func(t *testing.T, dir string) {
				t.Helper()
				testutil.WriteFile(t, dir, claudeSettings, `{"env":{"API_KEY":"local"},"hooks":{}}`)
			},
			wantReview: claudeSettings,
		},
		{
			name: "untracked hook config of an agent that is not enabled",
			before: func(t *testing.T, dir string) {
				t.Helper()
				testutil.WriteFile(t, dir, ".cursor/hooks.json", "{}")
			},
			wantReview: ".cursor/hooks.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newCommitConfigRepo(t, false)
			if err := os.Remove(EntireSettingsFile); err != nil {
				t.Fatal(err)
			}
			tt.before(t, dir)
			opts := beforeSetup(t, EnableOptions{})
			testutil.WriteFile(t, dir, EntireSettingsFile, `{"enabled":true}`)
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			t.Setenv(interactive.EnvTestTTY, "1")
			p := &promptedCommitConfig{render: &bytes.Buffer{}, commit: true}
			stubCommitConfig(t, p.prompt, nil)

			runOfferCommitConfig(t, opts)

			if !slices.Equal(p.files, []string{EntireSettingsFile}) {
				t.Errorf("offered to commit %v, want only %s", p.files, EntireSettingsFile)
			}
			if !slices.Equal(p.leftToReview, []string{tt.wantReview}) {
				t.Errorf("left to review %v, want only %s", p.leftToReview, tt.wantReview)
			}
			assertOutput(t, p.render.String(), []string{"had uncommitted changes before this run", "  " + tt.wantReview + "\n"}, nil)
			committed := strings.Fields(testutil.RunGit(t, dir, "show", "--name-only", "--format=", "HEAD"))
			if !slices.Equal(committed, []string{EntireSettingsFile}) {
				t.Errorf("HEAD committed %v, want %s alone", committed, EntireSettingsFile)
			}
			if status := testutil.RunGit(t, dir, "status", "--porcelain", "--", tt.wantReview); strings.TrimSpace(status) == "" {
				t.Errorf("%s should still be uncommitted", tt.wantReview)
			}
		})
	}
}

// Not parallel: chdirs and sets environment variables.
func TestOfferCommitConfig_ListsWithoutPromptingWhenNothingIsSafeToCommit(t *testing.T) {
	tests := []struct {
		name string
		opts func(t *testing.T) EnableOptions
		want []string
	}{
		{
			// The settings file was already untracked before this run.
			name: "this run changed nothing",
			opts: func(t *testing.T) EnableOptions { t.Helper(); return beforeSetup(t, EnableOptions{}) },
			want: []string{"had uncommitted changes before this run", "  .entire/settings.json\n"},
		},
		{
			name: "no snapshot was taken",
			opts: func(*testing.T) EnableOptions { return EnableOptions{} },
			want: []string{"had uncommitted changes before this run", "  .entire/settings.json\n"},
		},
		{
			name: "--local",
			opts: func(*testing.T) EnableOptions { return afterCleanSetup(EnableOptions{UseLocalSettings: true}) },
			want: []string{"Make sure to commit these files", "  .entire/settings.json\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newCommitConfigRepo(t, false)
			t.Setenv(interactive.EnvTestTTY, "1")
			stubCommitConfig(t, failOnPrompt(t), nil)

			assertOutput(t, runOfferCommitConfig(t, tt.opts(t)), tt.want, nil)
		})
	}
}

// Not parallel: swaps the terminal opener and sets ACCESSIBLE.
func TestPromptCommitConfig_EnterCommits(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	terminal := &closeableTerminal{t: t}
	prev := openPromptTerminal
	openPromptTerminal = func() (promptTerminal, error) {
		return promptTerminal{in: strings.NewReader("\n"), out: terminal, close: terminal.Close}, nil
	}
	t.Cleanup(func() { openPromptTerminal = prev })

	committed := false
	err := promptCommitConfig(t.Context(), &bytes.Buffer{}, []string{EntireSettingsFile}, nil, "main", func(io.Writer) error {
		committed = true
		return nil
	})
	if err != nil {
		t.Fatalf("promptCommitConfig() error = %v", err)
	}
	if !committed {
		t.Error("Enter should commit: every offered file is one setup changed")
	}
}

// Not parallel: swaps the terminal opener and sets ACCESSIBLE.
func TestPromptCommitConfig_AbortIsReportedOnTheTerminal(t *testing.T) {
	// Esc/Ctrl+C inside the form is an answer: "Commit cancelled." goes to the
	// fallback terminal the question was on, before it closes, and nothing is
	// committed. Accessible mode reads Ctrl+C as invalid input rather than an
	// abort, so an inherited ACCESSIBLE would fail this for the wrong reason.
	t.Setenv("ACCESSIBLE", "")
	terminal := &closeableTerminal{t: t}
	prev := openPromptTerminal
	openPromptTerminal = func() (promptTerminal, error) {
		return promptTerminal{in: strings.NewReader("\x03"), out: terminal, close: terminal.Close}, nil
	}
	t.Cleanup(func() { openPromptTerminal = prev })

	err := promptCommitConfig(t.Context(), &bytes.Buffer{}, []string{EntireSettingsFile}, nil, "main", func(io.Writer) error {
		t.Error("committed after an abort")
		return nil
	})
	if err != nil {
		t.Fatalf("promptCommitConfig() error = %v, want nil for an abort", err)
	}
	if !terminal.closed {
		t.Error("the fallback terminal should be closed once the prompt returns")
	}
	assertOutput(t, terminal.buf.String(), []string{"Commit cancelled.\n"}, nil)
}

// Not parallel: chdirs and sets environment variables.
func TestRunEnableOnConfiguredRepo_ListsPendingConfigWithoutOffering(t *testing.T) {
	tests := []struct {
		name     string
		settings string
		want     string
	}{
		{name: "already enabled", settings: testSettingsEnabled, want: "Entire is already enabled."},
		{name: "re-enabled", settings: testSettingsDisabled, want: "Entire is now enabled."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newCommitConfigRepo(t, false)
			t.Setenv(interactive.EnvTestTTY, "1")
			// An earlier enable whose offer was declined leaves everything
			// setup writes in place, uncommitted.
			writeSettings(t, testSettingsEnabled)
			declined := &promptedCommitConfig{render: &bytes.Buffer{}}
			stubCommitConfig(t, declined.prompt, nil)
			earlier := newEnableCmd()
			earlier.SetOut(io.Discard)
			if err := runEnableOnConfiguredRepo(context.Background(), earlier, EnableOptions{}); err != nil {
				t.Fatalf("earlier runEnableOnConfiguredRepo() error = %v", err)
			}

			writeSettings(t, tt.settings)
			stubCommitConfig(t, failOnPrompt(t), nil)
			cmd := newEnableCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := runEnableOnConfiguredRepo(context.Background(), cmd, EnableOptions{}); err != nil {
				t.Fatalf("runEnableOnConfiguredRepo() error = %v", err)
			}
			// Everything pending was already uncommitted before this run, so
			// it is the developer's to review, not this run's to commit.
			assertOutput(t, out.String(), []string{tt.want, "had uncommitted changes before this run", "  .entire/settings.json\n"}, []string{"Make sure to commit these files"})
		})
	}
}

// Not parallel: chdirs and sets environment variables.
func TestSetupAgentHooksNonInteractive_OffersOnlyWhatItChanged(t *testing.T) {
	const claudeSettings = ".claude/settings.json"
	dir := newCommitConfigRepo(t, false)
	if err := os.Remove(EntireSettingsFile); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, dir, claudeSettings, "{}")
	testutil.GitAdd(t, dir, claudeSettings)
	testutil.GitCommit(t, dir, "claude settings")
	testutil.WriteFile(t, dir, claudeSettings, `{"env":{"API_KEY":"local"}}`)
	t.Setenv(interactive.EnvTestTTY, "1")
	p := &promptedCommitConfig{render: &bytes.Buffer{}}
	stubCommitConfig(t, p.prompt, nil)

	ag, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Fatalf("get Claude Code agent: %v", err)
	}
	if err := setupAgentHooksNonInteractive(context.Background(), io.Discard, ag, EnableOptions{}); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive() error = %v", err)
	}
	if !p.asked {
		t.Fatal("expected the commit offer for the files setup created")
	}
	if !slices.Contains(p.files, EntireSettingsFile) || slices.Contains(p.files, claudeSettings) {
		t.Errorf("offered to commit %v, want %s and not %s", p.files, EntireSettingsFile, claudeSettings)
	}
	if !slices.Contains(p.leftToReview, claudeSettings) {
		t.Errorf("left to review %v, want %s", p.leftToReview, claudeSettings)
	}
}
