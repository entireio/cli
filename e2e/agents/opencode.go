package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type openCodeAgent struct {
	model   string
	timeout time.Duration
}

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "opencode" {
		return
	}
	if _, err := exec.LookPath(openCodeBinary); err != nil {
		return
	}
	model := os.Getenv("E2E_OPENCODE_MODEL")
	if model == "" {
		model = "anthropic/claude-haiku-4-5"
	}
	Register(&openCodeAgent{model: model, timeout: 2 * time.Minute})
}

func (a *openCodeAgent) Name() string               { return "opencode" }
func (a *openCodeAgent) Binary() string             { return openCodeBinary }
func (a *openCodeAgent) EntireAgent() string        { return "opencode" }
func (a *openCodeAgent) PromptPattern() string      { return `(Ask anything|▣)` }
func (a *openCodeAgent) TimeoutMultiplier() float64 { return 2.0 }

func (a *openCodeAgent) IsTransientError(out Output, _ error) bool {
	transientPatterns := []string{
		"overloaded",
		"rate limit",
		"529",
		"503",
		"ECONNRESET",
		"ETIMEDOUT",
		"Token refresh failed",
		"database is locked",
	}
	for _, p := range transientPatterns {
		if strings.Contains(out.Stderr, p) {
			return true
		}
	}
	return false
}

const (
	// openCodeWarmupBudget bounds a single warmup attempt. It sits far above
	// the cost of the trivial model round-trip on purpose: the warmup's job is
	// to pay opencode's remaining first-run costs (global DB migration) once and
	// serially, before ~40 tests start in parallel, so a budget that kills the
	// attempt part-way leaves that work half-done for every test that follows.
	openCodeWarmupBudget = 90 * time.Second

	// openCodeWarmupRetryBudget bounds attempts after the first. Only attempt 1
	// can be paying opencode's genuine first-run costs, so retrying at the full
	// budget just multiplies the wait before we give up and let the tests run
	// anyway: 3 x 90s is over four minutes of blocked CI on the exact path this
	// warmup exists to survive.
	openCodeWarmupRetryBudget = 30 * time.Second

	// openCodeDepsBudget bounds the plugin dependency install. It is generous
	// against a measured ~3s because a cold npm cache on a CI runner is a very
	// different machine from a developer's, and the cost of overrunning it is
	// only that we fall back to opencode installing per-directory itself.
	openCodeDepsBudget = 5 * time.Minute

	// openCodeDepsAttempts caps how many times a process will try to build the
	// tree. See openCodePluginDeps for why it is neither 1 nor unbounded.
	openCodeDepsAttempts = 2

	// openCodePluginPkg is the package opencode pins into a project's generated
	// .opencode/package.json. It pins the version of the RUNNING BINARY, not a
	// range, so the tree has to be rebuilt on every opencode upgrade.
	openCodePluginPkg = "@opencode-ai/plugin"

	// openCodeBinary is the executable name. It is a constant because the
	// dependency tree is resolved from package scope, before any agent instance
	// exists. Name()/EntireAgent() return the agent id and keep their own
	// literals — the two happen to coincide.
	openCodeBinary = "opencode"

	// openCodeSeedGitignore mirrors the .gitignore opencode writes into
	// .opencode itself. It names its own file, so everything we plant — the
	// .gitignore included — stays invisible to git. That matters here beyond
	// tidiness: these repos are the subject of checkpoint assertions, and an
	// untracked 61MB tree in the working set would change what the CLI sees.
	openCodeSeedGitignore = "node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore\n"
)

// openCodeVersionRe matches what `opencode --version` prints. The result names
// a directory, so it is validated rather than trusted.
var openCodeVersionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)

// openCodePluginDeps resolves the pre-built dependency tree, building it on
// first use.
//
// It resolves rather than reading something Bootstrap stashed, because Bootstrap
// and SetupRepo run in DIFFERENT PROCESSES: `go run ./e2e/bootstrap`, then
// `go test ./e2e/tests`. A package variable set by Bootstrap is empty in the
// test process, and because seeding degrades quietly that failure looks like
// success — bootstrap reports a fast warmup while every test still pays the
// install. That is exactly what run 33865617639 did.
//
// A success is cached for the life of the process; a failure is retried, but at
// most openCodeDepsAttempts times in total. Both bounds matter. Caching the
// error — which is what sync.OnceValues does — lets one transient npm blip on
// the very first call disable seeding for every repo in the run, reinstating the
// concurrent-install storm this exists to prevent, for the whole run rather than
// one attempt. Retrying without a cap is the opposite failure: ~40 installs when
// npm is simply absent. The mutex is held across the build, so attempts are
// serial and callers queue behind the one in flight either way.
func openCodePluginDeps() (string, error) {
	openCodeDeps.mu.Lock()
	defer openCodeDeps.mu.Unlock()
	if openCodeDeps.dir != "" {
		return openCodeDeps.dir, nil
	}
	if openCodeDeps.attempts >= openCodeDepsAttempts {
		return "", openCodeDeps.err
	}
	openCodeDeps.attempts++
	openCodeDeps.dir, openCodeDeps.err = buildPluginDeps()
	return openCodeDeps.dir, openCodeDeps.err
}

var openCodeDeps struct {
	mu       sync.Mutex
	dir      string
	err      error
	attempts int
}

// openCodeDepsDir names the shared tree for one opencode version.
//
// Deliberately not under os.UserCacheDir(): the e2e TestMain points
// XDG_CACHE_HOME at the artifact directory, so a tree built there would both
// miss the one Bootstrap built — different process, different cache dir — and
// be uploaded as a ~61MB CI artifact. os.TempDir() is stable across both
// processes, which is the whole requirement.
//
// Keyed by version because opencode pins the plugin to its own binary version,
// so a tree built for an older opencode is one it would discard and reinstall.
func openCodeDepsDir(version string) string {
	return filepath.Join(os.TempDir(), "entire-e2e-opencode-deps-"+version)
}

func (a *openCodeAgent) Bootstrap() error {
	// Build the dependency tree opencode would otherwise install itself, once,
	// so SeedRepo can plant it in every test repo.
	//
	// Whenever a project directory contains a local plugin file — which
	// `entire enable` always writes — opencode generates
	// .opencode/package.json and installs it (27 packages, ~61MB) before it
	// will finish bootstrapping. That install happens inside a phase that logs
	// nothing and has no timeout, so a slow one presents as a silent hang with
	// no output at all; on 2026-09-03 it began exceeding the per-prompt budget
	// in CI and killed half the suite, with 27 of 53 opencode processes dying
	// between "loading path=..." and "all LSPs are disabled".
	//
	// Doing it here turns ~40 unbounded installs hidden inside the agent into
	// one bounded step that reports its own failure.
	//
	// Not a test-only problem. opencode forks that install for every config
	// directory that exists (ConfigPaths.directories) and blocks on all of them
	// whenever any plugin is configured (plugin/index.ts: `if (plugins.length)
	// yield* config.waitForDependencies()`). So a repo pays it once it has both
	// a project-local .opencode directory and a plugin configured anywhere,
	// global or project — and `entire enable` supplies both.
	//
	// The build is triggered by the SeedRepo call below rather than here:
	// buildPluginDeps reports the build and SeedRepo reports a failure, so an
	// explicit call would only print a third message about the same event.
	//
	// opencode also has a first-run DB migration that races with parallel test
	// execution (upstream issue #6935). Run a trivial prompt to force the rest
	// of initialization before tests. It runs in a seeded scratch directory so
	// it is not itself blocked on the install above.
	//
	// Each attempt's duration is reported whether it succeeds or not: this is
	// the only serial, uncontended measurement of opencode startup we take, so
	// it is the cheapest place to see a step change in it from the CI log.
	warmDir, err := os.MkdirTemp("", "opencode-warmup-*")
	if err != nil {
		return fmt.Errorf("create opencode warmup dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(warmDir) }()
	if err := a.SeedRepo(warmDir); err != nil {
		fmt.Fprintf(os.Stderr, "opencode: could not seed warmup dir: %v\n", err)
	}

	for i := range 3 {
		budget := openCodeWarmupBudget
		if i > 0 {
			budget = openCodeWarmupRetryBudget
		}
		start := time.Now()
		// Through RunPrompt so the warmup child is set up exactly like a test's.
		// The difference is not cosmetic: openCodePromptEnv forces PWD, without
		// which opencode resolves a different project root and never loads the
		// plugin — a warmup that skipped it would warm the wrong directory.
		out, err := a.RunPrompt(context.Background(), warmDir, "say hi", WithPromptTimeout(budget))
		elapsed := time.Since(start).Round(time.Millisecond)
		if err == nil {
			fmt.Fprintf(os.Stderr, "opencode warmup succeeded on attempt %d in %s\n", i+1, elapsed)
			return nil
		}
		if i < 2 {
			fmt.Fprintf(os.Stderr, "opencode warmup attempt %d failed after %s: %s\n%s%s\n", i+1, elapsed, err, out.Stdout, out.Stderr)
			time.Sleep(5 * time.Second)
		}
	}
	// Non-fatal: warmup failure shouldn't block tests entirely.
	fmt.Fprintln(os.Stderr, "opencode warmup failed after 3 attempts, proceeding anyway")
	return nil
}

// buildPluginDeps materialises the .opencode dependency tree once and returns
// the directory holding it. See Bootstrap for why it exists.
func buildPluginDeps() (string, error) {
	version, err := openCodeVersion()
	if err != nil {
		return "", err
	}
	dir := openCodeDepsDir(version)
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		return dir, nil
	}

	// Built in a staging directory and renamed into place, so a reader never
	// sees a half-installed tree. Bootstrap normally wins this race and the test
	// process takes the Stat above; if bootstrap was skipped, a loser here
	// adopts the winner's tree rather than failing.
	staging, err := os.MkdirTemp(os.TempDir(), "entire-e2e-opencode-deps-*")
	if err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }() // no-op once renamed away
	pkg := fmt.Sprintf("{\n  \"dependencies\": {\n    %q: %q\n  }\n}\n", openCodePluginPkg, version)
	if err := os.WriteFile(filepath.Join(staging, "package.json"), []byte(pkg), 0o644); err != nil {
		return "", fmt.Errorf("write package.json: %w", err)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), openCodeDepsBudget)
	defer cancel()
	// The npm CLI, deliberately: opencode installs the same tree with
	// @npmcli/arborist in-process, which measured 5m00s against npm's 3s for an
	// identical result. Using npm here is the point of doing it ourselves.
	cmd := exec.CommandContext(ctx, "npm", "install", "--no-audit", "--no-fund", "--loglevel=error")
	cmd.Dir = staging
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("npm install %s@%s: %w\n%s", openCodePluginPkg, version, err, out)
	}
	elapsed := time.Since(start).Round(time.Millisecond)

	if err := os.Rename(staging, dir); err != nil {
		if _, statErr := os.Stat(filepath.Join(dir, "node_modules")); statErr == nil {
			return dir, nil // lost the race; the winner's tree is equivalent
		}
		return "", fmt.Errorf("publish plugin deps to %q: %w", dir, err)
	}
	fmt.Fprintf(os.Stderr, "opencode: built plugin deps for %s in %s at %s\n", version, elapsed, dir)
	return dir, nil
}

// openCodeVersion reports the running opencode's version, which is the version
// its generated package.json will pin.
func openCodeVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, openCodeBinary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("opencode --version: %w", err)
	}
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	version = strings.TrimSpace(version)
	if !openCodeVersionRe.MatchString(version) {
		return "", fmt.Errorf("opencode --version printed %q, which is not a version", version)
	}
	return version, nil
}

// SeedRepo prepares a directory opencode is about to run in: its config file,
// plus the pre-built dependency tree so opencode's own blocking install never
// runs there.
func (a *openCodeAgent) SeedRepo(dir string) error {
	// Written before the tree is resolved, so a repo still gets a usable config
	// when the pre-build failed and opencode falls back to installing for itself.
	//
	// opencode's non-interactive mode auto-rejects the external_directory
	// permission, since there is no user to prompt.
	cfg := `{"$schema": "https://opencode.ai/config.json", "permission": {"external_directory": "allow"}}`
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		cfg = fmt.Sprintf(`{"$schema": "https://opencode.ai/config.json", "permission": {"external_directory": "allow"}, "provider": {"anthropic": {"options": {"apiKey": %q}}}}`, key)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(cfg+"\n"), 0o644); err != nil {
		return fmt.Errorf("write opencode.json: %w", err)
	}

	deps, err := openCodePluginDeps()
	if err != nil {
		// Leave the repo alone and let opencode install for itself.
		fmt.Fprintf(os.Stderr, "opencode: no pre-built plugin deps (%v); %s pays the install\n", err, dir)
		return nil
	}
	target := filepath.Join(dir, ".opencode")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create .opencode: %w", err)
	}
	for _, name := range []string{"package.json", "package-lock.json"} {
		data, err := os.ReadFile(filepath.Join(deps, name))
		if err != nil {
			return fmt.Errorf("read seeded %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(target, name), data, 0o644); err != nil {
			return fmt.Errorf("write seeded %s: %w", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, ".gitignore"), []byte(openCodeSeedGitignore), 0o644); err != nil {
		return fmt.Errorf("write seeded .gitignore: %w", err)
	}
	// node_modules is linked rather than copied: the tree is ~61MB across
	// thousands of small files and ~40 repos exist at once, so copying it would
	// cost about what the install we are avoiding costs. opencode reads through
	// the link and leaves it in place.
	link := filepath.Join(target, "node_modules")
	if err := linkFile(filepath.Join(deps, "node_modules"), link); err != nil {
		// Degrade to opencode installing for itself rather than failing setup.
		// linkFile copies on Windows, which cannot copy a directory, so that
		// platform lands here by design.
		fmt.Fprintf(os.Stderr, "opencode: could not link seeded node_modules (%v); %s pays the install\n", err, dir)
		return nil
	}
	return nil
}

func (a *openCodeAgent) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{}
	for _, o := range opts {
		o(cfg)
	}

	model := a.model
	if cfg.Model != "" {
		model = cfg.Model
	}

	args := []string{"run"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

	timeout := a.timeout
	if envTimeout := os.Getenv("E2E_TIMEOUT"); envTimeout != "" {
		if parsed, err := time.ParseDuration(envTimeout); err == nil {
			timeout = parsed
		}
	}
	// Per-prompt timeout is the most specific override.
	if cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.Binary(), args...)
	cmd.Dir = dir
	cmd.Env = openCodePromptEnv(os.Environ(), dir)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := Output{
		Command: a.Binary() + " " + strings.Join(args, " "),
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			out.ExitCode = exitErr.ExitCode()
		} else {
			out.ExitCode = -1
		}
		return out, err
	}

	return out, nil
}

// openCodePromptEnv builds the child environment for a headless `opencode run`.
// cmd.Dir chdirs the child but does NOT update the inherited PWD env var, which
// still points at the `go test` package dir. opencode (Node) resolves its
// project/worktree root from process.env.PWD, so without forcing PWD to match
// cmd.Dir all file operations land in the wrong repo and the per-repo entire
// plugin never loads. The tmux/interactive path is unaffected because
// `tmux new-session -c dir` already sets PWD correctly.
func openCodePromptEnv(base []string, dir string) []string {
	return append(filterEnv(base, "ENTIRE_TEST_TTY", "PWD"), "PWD="+dir)
}

func (a *openCodeAgent) StartSession(ctx context.Context, dir string) (Session, error) {
	// opencode's TUI occasionally fails to render on CI (empty pane).
	// Retry once if the first attempt produces no output at all.
	var s *TmuxSession
	var lastErr error
	for attempt := range 2 {
		name := fmt.Sprintf("opencode-test-%d", time.Now().UnixNano())
		var err error
		s, err = NewTmuxSession(name, dir, []string{"ENTIRE_TEST_TTY"}, a.Binary(), "--model", a.model)
		if err != nil {
			return nil, err
		}

		// Wait for TUI to be ready (input area with placeholder text).
		// OpenCode's TUI has a large ASCII banner and multiple panels that
		// can take a while to render on CI, plus WaitFor needs 2s settle.
		if _, err := s.WaitFor(`Ask anything`, 60*time.Second); err != nil {
			content := s.Capture()
			_ = s.Close()
			if strings.TrimSpace(content) == "" && attempt == 0 {
				lastErr = err
				continue
			}
			return nil, fmt.Errorf("waiting for startup: %w", err)
		}
		s.stableAtSend = ""
		return s, nil
	}
	return nil, fmt.Errorf("opencode TUI failed to start after retry: %w", lastErr)
}
