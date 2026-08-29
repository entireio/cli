package strategy

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// Real Claude Code JSONL lines. The shapes matter: the old substring probe was
// tested against invented ones ({"tool":"Bash","command":...}) that no agent
// writes, which is part of how an 18-to-1 false-positive rate went unnoticed.
const (
	fixtureBashSearch = `{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"entire search \"retry backoff\" --json"}}]}}
`
	fixtureBashCheckpointSearch = `{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"entire checkpoint search foo"}}]}}
`
	fixtureBashChainedSearch = `{"type":"assistant","uuid":"a3","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"cd sub && entire search foo --json"}}]}}
`
	fixtureSubagentDispatch = `{"type":"assistant","uuid":"a4","message":{"content":[{"type":"tool_use","id":"t4","name":"Agent","input":{"subagent_type":"entire-search","prompt":"find prior work on retries"}}]}}
`
	fixtureBashGrepMention = `{"type":"assistant","uuid":"a5","message":{"content":[{"type":"tool_use","id":"t5","name":"Bash","input":{"command":"grep -rn \"entire search\" cmd/"}}]}}
`
	fixtureBashCommitMessage = `{"type":"assistant","uuid":"a6","message":{"content":[{"type":"tool_use","id":"t6","name":"Bash","input":{"command":"git commit -m \"mention entire search here\""}}]}}
`
	fixtureToolResultOutput = `{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"t9","content":"$ entire search foo\nno results"}]}}
`
	fixtureAssistantTextMention = `{"type":"assistant","uuid":"a7","message":{"content":[{"type":"text","text":"You could run entire search to find that."}]}}
`
	fixtureWriteSkillBody = `{"type":"assistant","uuid":"a8","message":{"content":[{"type":"tool_use","id":"t8","name":"Write","input":{"file_path":".claude/agents/entire-search.md","content":"Your only history-search mechanism is the entire search --json command."}}]}}
`
	fixtureInvestigatePrompt = `{"type":"user","uuid":"u2","message":{"content":"Run entire search \"<phrase from the symptom>\" --json to find prior work."}}
`
	fixtureUnrelatedBash = `{"type":"assistant","uuid":"a9","message":{"content":[{"type":"tool_use","id":"t7","name":"Bash","input":{"command":"entire status"}}]}}
`
	// A non-assistant envelope whose message decodes into tool_use-shaped
	// content. Only assistant envelopes carry live tool calls; the walker must
	// apply the same gate as the package's other tool_use consumers.
	fixtureNonAssistantToolUse = `{"type":"summary","uuid":"s1","message":{"content":[{"type":"tool_use","id":"tx","name":"Bash","input":{"command":"entire search foo"}}]}}
`
)

func TestDetectSearchUsage_ClaudeCode(t *testing.T) {
	t.Parallel()

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}

	tests := []struct {
		name       string
		transcript string
		want       searchProbe
	}{
		{"bash invocation", fixtureBashSearch, searchProbe{used: true, source: searchSourceCommand}},
		{"checkpoint search alias", fixtureBashCheckpointSearch, searchProbe{used: true, source: searchSourceCommand}},
		{"after a shell separator", fixtureBashChainedSearch, searchProbe{used: true, source: searchSourceCommand}},
		{"entire-search subagent dispatch", fixtureSubagentDispatch, searchProbe{used: true, source: searchSourceSubagent}},

		// The false-positive classes the substring probe could not tell apart
		// from a real invocation. Each of these is a documented source: Entire's
		// own search skill body, the investigate prompt, or a session reading
		// this repository.
		{"grep for the phrase", fixtureBashGrepMention, searchProbe{used: false, source: searchSourceNone}},
		{"phrase inside a commit message", fixtureBashCommitMessage, searchProbe{used: false, source: searchSourceNone}},
		{"phrase in command output", fixtureToolResultOutput, searchProbe{used: false, source: searchSourceNone}},
		{"phrase in assistant prose", fixtureAssistantTextMention, searchProbe{used: false, source: searchSourceNone}},
		{"scaffolded skill body being written", fixtureWriteSkillBody, searchProbe{used: false, source: searchSourceNone}},
		{"injected investigate prompt", fixtureInvestigatePrompt, searchProbe{used: false, source: searchSourceNone}},

		{"unrelated entire command", fixtureUnrelatedBash, searchProbe{used: false, source: searchSourceNone}},
		{"tool_use outside an assistant envelope", fixtureNonAssistantToolUse, searchProbe{used: false, source: searchSourceNone}},
		{"phrase absent", `{"type":"assistant","uuid":"a0","message":{"content":[]}}`, searchProbe{used: false, source: searchSourceNone}},

		// Not "none": seeing nothing because there is nothing to see is a
		// different fact from having looked.
		{"empty transcript", "", searchProbe{used: false, source: searchSourceUnsupported}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := detectSearchUsage(ag, []byte(tt.transcript)); got != tt.want {
				t.Errorf("detectSearchUsage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestDetectSearchUsage_MixedTranscriptFindsTheInvocation guards the walk
// itself: the accepting line must be found among rejected ones, in either
// order, so a prefilter miss cannot masquerade as "did not search".
func TestDetectSearchUsage_MixedTranscriptFindsTheInvocation(t *testing.T) {
	t.Parallel()

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}
	noise := fixtureToolResultOutput + fixtureAssistantTextMention + fixtureWriteSkillBody
	for _, tt := range []struct {
		name       string
		transcript string
	}{
		{"invocation last", noise + fixtureBashSearch},
		{"invocation first", fixtureBashSearch + noise},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := searchProbe{used: true, source: searchSourceCommand}
			if got := detectSearchUsage(ag, []byte(tt.transcript)); got != want {
				t.Errorf("detectSearchUsage() = %+v, want %+v", got, want)
			}
		})
	}
}

// TestDetectSearchUsage_UnprobeableAgentsReportUnsupported is the test that
// keeps the metric honest, and it is the case the review finding's suggested
// fix would have gotten wrong. These agents are fed a transcript that DOES
// contain a real invocation: reporting used=false with source=none would be a
// fabricated data point indistinguishable in aggregate from a real miss.
//
// These agents are unprobeable because no ToolInvocationScanner walker exists
// for them yet — not because one is impossible. Cursor in particular shares
// the tool_use JSONL shape and could implement one; see the interface doc in
// agent/tool_invocations.go for what blocks it.
func TestDetectSearchUsage_UnprobeableAgentsReportUnsupported(t *testing.T) {
	t.Parallel()

	for _, agentType := range []types.AgentType{agent.AgentTypeCursor, agent.AgentTypePi, agent.AgentTypeCopilotCLI} {
		t.Run(string(agentType), func(t *testing.T) {
			t.Parallel()
			ag, err := agent.GetByAgentType(agentType)
			if err != nil {
				t.Fatalf("GetByAgentType(%s) error: %v", agentType, err)
			}
			want := searchProbe{used: false, source: searchSourceUnsupported}
			if got := detectSearchUsage(ag, []byte(fixtureBashSearch)); got != want {
				t.Errorf("detectSearchUsage(%s) = %+v, want %+v", agentType, got, want)
			}
		})
	}
}

func TestDetectSearchUsage_NilAgentReportsUnsupported(t *testing.T) {
	t.Parallel()
	want := searchProbe{used: false, source: searchSourceUnsupported}
	if got := detectSearchUsage(nil, []byte(fixtureBashSearch)); got != want {
		t.Errorf("detectSearchUsage(nil) = %+v, want %+v", got, want)
	}
}

// TestSearchHintsCoverPattern pins the contract that makes the scanner's byte
// prefilter a performance filter rather than a correctness one: every string
// the matchers accept must contain one of the hints literally. Loosen the
// pattern's internal spacing to \s+ and this fails — which is the point, since
// the failure mode is otherwise a silent false negative.
func TestSearchHintsCoverPattern(t *testing.T) {
	t.Parallel()

	accepted := []string{
		"entire search foo",
		"entire checkpoint search foo",
		"cd sub && entire search foo",
		"ENTIRE_X=1 entire search foo",
		"/usr/local/bin/entire search foo",
		"echo hi; entire search foo",
		"$(entire search foo)",
		`entire search "two words"`,
		// A here-string is not a heredoc opener; the command after it is live
		// shell. The sanitizer misread `<<< x` as a heredoc before it moved
		// to stringutil, which made this a false negative.
		"cat <<< x; entire search foo",
	}
	for _, cmd := range accepted {
		if !commandInvokesEntireSearch(cmd) {
			t.Errorf("commandInvokesEntireSearch must accept %q", cmd)
			continue
		}
		if !hintedBySearchHints(cmd) {
			t.Errorf("accepted command %q contains no entireSearchHints literal; the scanner would never parse its line", cmd)
		}
	}
	if !hintedBySearchHints(EntireSearchSubagentName) {
		t.Errorf("subagent name %q contains no entireSearchHints literal", EntireSearchSubagentName)
	}
	// The prefilter is case-sensitive bytes.Contains, so the subagent matcher
	// must be case-sensitive too. A case-insensitive matcher would accept
	// spellings the prefilter discards before the line is ever parsed.
	for _, variant := range []string{"Entire-Search", "ENTIRE-SEARCH", "Entire-search"} {
		if hintedBySearchHints(variant) {
			t.Errorf("hint list unexpectedly covers %q; the case-sensitivity test below is moot", variant)
		}
		if strings.TrimSpace(variant) == EntireSearchSubagentName {
			t.Errorf("subagent matcher accepts %q, which the case-sensitive hint prefilter would discard first", variant)
		}
	}

	rejected := []string{
		"grep -rn \"entire search\" cmd/",
		"git commit -m \"mention entire search here\"",
		"entire status",
		"entire searchfoo",
		// Separators inside quoted arguments are text, not command
		// boundaries. Each of these matched before sanitization was added —
		// the quote-blind regex read the `;`/`|`/newline inside the argument
		// as a boundary and the phrase after it as a command.
		`git commit -m "wip; entire search notes"`,
		`rg "foo|entire search bar" .`,
		"echo \"a\nentire search b\"",
		`git commit -m 'wip; entire search notes'`,
		// A heredoc body is data — this is an agent WRITING the search skill
		// (whose body contains the command at line start) via cat, the
		// scaffolded-artifact false-positive class review measured.
		"cat > .claude/agents/entire-search.md <<'EOF'\nentire search --json \"x\"\nEOF",
		"cat > notes.md <<DOC\nentire search --json\nDOC\necho done",
	}
	for _, cmd := range rejected {
		if commandInvokesEntireSearch(cmd) {
			t.Errorf("commandInvokesEntireSearch must reject %q", cmd)
		}
	}

	// The sanitizer must not eat commands AFTER a heredoc: the body ends at
	// its delimiter line, and what follows is live shell again.
	if !commandInvokesEntireSearch("cat > notes.md <<DOC\nsome text\nDOC\nentire search foo") {
		t.Error("commandInvokesEntireSearch must accept an invocation on the line after a heredoc body ends")
	}
	// Arithmetic << must not be misread as a heredoc opener that swallows the
	// rest of the command.
	if !commandInvokesEntireSearch("echo $((1<<2))\nentire search foo") {
		t.Error("commandInvokesEntireSearch must accept an invocation after arithmetic <<")
	}
}

func hintedBySearchHints(s string) bool {
	for _, hint := range entireSearchHints {
		if bytes.Contains([]byte(s), hint) {
			return true
		}
	}
	return false
}

func TestNewCommitCondensedSignal_CarriesSearchProbe(t *testing.T) {
	t.Parallel()

	probe := searchProbe{used: true, source: searchSourceSubagent}
	sig := newCommitCondensedSignal(
		&SessionState{AgentType: agent.AgentTypeClaudeCode},
		&CondenseResult{SearchProbe: probe, FilesTouched: []string{"a.go"}},
		map[string]struct{}{"a.go": {}},
	)
	if sig == nil {
		t.Fatal("newCommitCondensedSignal returned nil")
	}
	if sig.searchProbe != probe {
		t.Errorf("searchProbe = %+v, want %+v", sig.searchProbe, probe)
	}
	if len(sig.filesTouched) != 1 || sig.filesTouched[0] != "a.go" {
		t.Errorf("filesTouched = %v, want [a.go]", sig.filesTouched)
	}
}

// TestNewCommitCondensedSignal_ScopesFilesToCommit pins the payload's
// commit-scoping against the one upstream path that deliberately does not
// narrow: filterFilesTouched leaves the session's whole FilesTouched list when
// the commit changed no files (--allow-empty, or file detection failing).
// files_committed and the prior_ai_history intersection must never count files
// the commit did not land.
func TestNewCommitCondensedSignal_ScopesFilesToCommit(t *testing.T) {
	t.Parallel()

	state := &SessionState{AgentType: agent.AgentTypeClaudeCode}
	result := &CondenseResult{FilesTouched: []string{"a.go", "b.go", "c.go"}}

	if sig := newCommitCondensedSignal(state, result, nil); sig == nil {
		t.Fatal("newCommitCondensedSignal returned nil")
	} else if len(sig.filesTouched) != 0 {
		t.Errorf("empty commit: filesTouched = %v, want none — the commit landed no files", sig.filesTouched)
	}

	sig := newCommitCondensedSignal(state, result, map[string]struct{}{"b.go": {}, "unrelated.go": {}})
	if sig == nil {
		t.Fatal("newCommitCondensedSignal returned nil")
	}
	if len(sig.filesTouched) != 1 || sig.filesTouched[0] != "b.go" {
		t.Errorf("filesTouched = %v, want [b.go]", sig.filesTouched)
	}
}

// hasFile is the intersection the emitter performs per session, spelled out so
// the set-returning probe can be asserted directly.
func hasFile(files map[string]struct{}, name string) bool {
	_, ok := files[name]
	return ok
}

// probeOrFail runs priorAICommitFiles and fails the test if the probe itself
// could not run — these tests assert the measured result, not measurability.
func probeOrFail(t *testing.T, repoRoot, commit string) map[string]struct{} {
	t.Helper()
	files, ok := priorAICommitFiles(t.Context(), repoRoot, commit)
	if !ok {
		t.Fatal("priorAICommitFiles reported the probe could not run")
	}
	return files
}

func TestPriorAICommitFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)

	// Commit 1: an AI checkpoint commit touching ai.txt.
	testutil.WriteFile(t, tmpDir, "ai.txt", "ai content")
	testutil.GitAdd(t, tmpDir, "ai.txt")
	cpID := id.MustCheckpointID("abcdef123456")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("ai change", cpID))

	// Commit 2: a plain human commit touching human.txt.
	testutil.WriteFile(t, tmpDir, "human.txt", "human content")
	testutil.GitAdd(t, tmpDir, "human.txt")
	testutil.GitCommit(t, tmpDir, "human change")

	// Commit 3: HEAD — the commit that was "just created"; --skip=1 must
	// exclude it, so its files never count as prior history.
	testutil.WriteFile(t, tmpDir, "head.txt", "head content")
	testutil.GitAdd(t, tmpDir, "head.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("head change", cpID))

	files := probeOrFail(t, tmpDir, "HEAD")

	if !hasFile(files, "ai.txt") {
		t.Error("ai.txt was touched by a prior checkpoint commit; want present")
	}
	if hasFile(files, "human.txt") {
		t.Error("human.txt was only touched by a human commit; want absent")
	}
	if hasFile(files, "head.txt") {
		t.Error("head.txt was only touched by the just-created HEAD commit; want absent")
	}
	if files, ok := priorAICommitFiles(t.Context(), t.TempDir(), "HEAD"); ok {
		t.Errorf("not a git repository; the probe must report itself unmeasured, got ok=true with %v", files)
	}
}

// TestPriorAICommitFiles_AnchorsOnExplicitCommit pins the two properties that
// keep the probe describing the commit PostCommit is reporting on rather than
// whatever the process environment says HEAD is:
//   - the walk starts at the explicit hash, so a HEAD that moved between the
//     commit and the (post-gate) probe changes nothing;
//   - GIT_DIR/GIT_WORK_TREE exported to hooks are scrubbed, so -C selects the
//     repo — an inherited GIT_DIR would override it and walk another repo.
func TestPriorAICommitFiles_AnchorsOnExplicitCommit(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)

	testutil.WriteFile(t, tmpDir, "ai.txt", "ai content")
	testutil.GitAdd(t, tmpDir, "ai.txt")
	cpID := id.MustCheckpointID("abcdef123456")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("ai change", cpID))

	// The commit being reported on.
	testutil.WriteFile(t, tmpDir, "reported.txt", "reported")
	testutil.GitAdd(t, tmpDir, "reported.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("reported change", cpID))
	reported := strings.TrimSpace(testutil.RunGit(t, tmpDir, "rev-parse", "HEAD"))

	// HEAD moves on before the probe runs (it fires after the session gate
	// releases): a later checkpoint commit that must count neither itself nor
	// shift --skip=1 onto the wrong commit.
	testutil.WriteFile(t, tmpDir, "later.txt", "later")
	testutil.GitAdd(t, tmpDir, "later.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("later change", cpID))

	// A hook environment pointing git somewhere else entirely; the scrub must
	// keep -C authoritative.
	otherRepo := t.TempDir()
	testutil.InitRepo(t, otherRepo)
	testutil.WriteFile(t, otherRepo, "other.txt", "other")
	testutil.GitAdd(t, otherRepo, "other.txt")
	testutil.GitCommit(t, otherRepo, "other")
	t.Setenv("GIT_DIR", filepath.Join(otherRepo, ".git"))
	t.Setenv("GIT_WORK_TREE", otherRepo)

	files := probeOrFail(t, tmpDir, reported)
	if !hasFile(files, "ai.txt") {
		t.Error("ai.txt precedes the reported commit and carries a trailer; want present")
	}
	if hasFile(files, "reported.txt") {
		t.Error("the reported commit is not its own prior history; want absent")
	}
	if hasFile(files, "later.txt") {
		t.Error("a commit made after the reported one (HEAD moved on) must not count; want absent")
	}
}

func TestPriorAICommitFiles_NonASCIIPath(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)

	// Without -z git quotes non-ASCII names in --name-only output
	// ("caf\303\251.go"), which can never match the unquoted FilesTouched
	// form — a systematic false negative this test pins down.
	testutil.WriteFile(t, tmpDir, "café.go", "package main")
	testutil.GitAdd(t, tmpDir, "café.go")
	cpID := id.MustCheckpointID("abcdef123456")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("ai change", cpID))

	// HEAD commit that --skip=1 excludes.
	testutil.WriteFile(t, tmpDir, "head.txt", "head content")
	testutil.GitAdd(t, tmpDir, "head.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("head change", cpID))

	if !hasFile(probeOrFail(t, tmpDir, "HEAD"), "café.go") {
		t.Error("café.go was touched by a prior checkpoint commit; want present")
	}
}

// TestPriorAICommitFiles_ControlCharsInBody is the regression test for the
// %x1e/%x1f framing: those delimiters came out of the raw %B, so either
// character in a commit body split a record early and dropped the real
// Entire-Checkpoint trailer with it — making prior_ai_history read false and
// suppressing a genuine miss. The NUL-anchored framing cannot be broken by
// message content, because a commit message cannot contain NUL.
func TestPriorAICommitFiles_ControlCharsInBody(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)

	testutil.WriteFile(t, tmpDir, "hostile.txt", "content")
	testutil.GitAdd(t, tmpDir, "hostile.txt")
	cpID := id.MustCheckpointID("abcdef123456")
	// Both delimiters, plus a trailer-lookalike line placed before the real
	// trailer, so an early split would look plausible rather than empty.
	body := "subject\n\nbody with \x1e and \x1f inside\nEntire-Checkpoint: not-an-id\n"
	testutil.GitCommit(t, tmpDir, body+"\n"+trailers.FormatCheckpoint("", cpID))

	testutil.WriteFile(t, tmpDir, "head.txt", "head content")
	testutil.GitAdd(t, tmpDir, "head.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("head change", cpID))

	if !hasFile(probeOrFail(t, tmpDir, "HEAD"), "hostile.txt") {
		t.Error("a commit body containing \\x1e/\\x1f must not hide its checkpoint trailer")
	}
}

// TestPriorAICommitFiles_EmptyMessageCommit pins why the format carries %H: an
// empty body would otherwise put two adjacent empty fields in the output and
// mis-frame the following record.
func TestPriorAICommitFiles_EmptyMessageCommit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)

	testutil.WriteFile(t, tmpDir, "ai.txt", "ai content")
	testutil.GitAdd(t, tmpDir, "ai.txt")
	cpID := id.MustCheckpointID("abcdef123456")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("ai change", cpID))

	// An empty-message commit between the checkpoint commit and HEAD.
	testutil.WriteFile(t, tmpDir, "empty.txt", "empty")
	testutil.GitAdd(t, tmpDir, "empty.txt")
	testutil.GitCommit(t, tmpDir, "")

	testutil.WriteFile(t, tmpDir, "head.txt", "head content")
	testutil.GitAdd(t, tmpDir, "head.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("head change", cpID))

	files := probeOrFail(t, tmpDir, "HEAD")
	if !hasFile(files, "ai.txt") {
		t.Error("an empty-message commit must not mis-frame the records after it")
	}
	if hasFile(files, "empty.txt") {
		t.Error("the empty-message commit carries no trailer; want absent")
	}
}

// TestPriorAICommitFiles_MergeCommitContributesNothing pins the deliberate
// decision documented on priorAICommitFiles, so a future "fix" has to argue
// with a test rather than silently inflate prior_ai_history.
func TestPriorAICommitFiles_MergeCommitContributesNothing(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "base.txt", "base")
	testutil.GitAdd(t, tmpDir, "base.txt")
	testutil.GitCommit(t, tmpDir, "base")

	branch := testutil.RunGit(t, tmpDir, "rev-parse", "--abbrev-ref", "HEAD")
	branch = strings.TrimSpace(branch)

	testutil.RunGit(t, tmpDir, "checkout", "-b", "side")
	testutil.WriteFile(t, tmpDir, "side.txt", "side")
	testutil.GitAdd(t, tmpDir, "side.txt")
	testutil.GitCommit(t, tmpDir, "side change")

	testutil.RunGit(t, tmpDir, "checkout", branch)
	cpID := id.MustCheckpointID("abcdef123456")
	// A merge commit carrying a checkpoint trailer — the shape 51 of the last
	// 300 merges on main have.
	testutil.RunGit(t, tmpDir, "merge", "--no-ff", "-m",
		trailers.FormatCheckpoint("merge side", cpID), "side")

	testutil.WriteFile(t, tmpDir, "head.txt", "head")
	testutil.GitAdd(t, tmpDir, "head.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("head change", cpID))

	if hasFile(probeOrFail(t, tmpDir, "HEAD"), "side.txt") {
		t.Error("a merge commit must contribute no files: its content is already " +
			"attributed to the commits it brings in, and first-parent diffs would " +
			"inflate prior_ai_history")
	}
}

// TestCommitCondensedEmitter_ProbeFailureOmitsPriorAIHistory pins the
// unmeasured half of prior_ai_history: when the git-log probe cannot run,
// the payload omits the property (nil) instead of fabricating a measured
// false — the same treatment used_search gets, applied to the other half of
// the same ratio. A commit that landed no files, by contrast, is a measured
// false and never even probes.
func TestCommitCondensedEmitter_ProbeFailureOmitsPriorAIHistory(t *testing.T) {
	writeTelemetrySettings(t, "true")

	var got []telemetry.CommitCondensedSignal
	restore := emitCommitCondensed
	emitCommitCondensed = func(sig telemetry.CommitCondensedSignal, _ bool, _ string) { got = append(got, sig) }
	t.Cleanup(func() { emitCommitCondensed = restore })

	probeCalls := 0
	e := newCommitCondensedEmitter(t.TempDir(), "HEAD")
	e.probeFn = func(context.Context, string, string) (map[string]struct{}, bool) {
		probeCalls++
		return nil, false // the probe itself failed
	}

	e.emit(t.Context(), &commitCondensedSignal{
		agentType:    agent.AgentTypeClaudeCode,
		searchProbe:  searchProbe{used: false, source: searchSourceNone},
		filesTouched: []string{"a.go"},
	})
	e.emit(t.Context(), &commitCondensedSignal{
		agentType:   agent.AgentTypeClaudeCode,
		searchProbe: searchProbe{used: false, source: searchSourceNone},
		// No files: measured false, no probe needed.
	})

	if len(got) != 2 {
		t.Fatalf("sent %d events, want 2 — a failed probe withholds the property, not the event", len(got))
	}
	if got[0].PriorAIHistory != nil {
		t.Errorf("PriorAIHistory = %v, want nil when the probe could not run", *got[0].PriorAIHistory)
	}
	if got[1].PriorAIHistory == nil || *got[1].PriorAIHistory {
		t.Errorf("PriorAIHistory = %v, want measured false for a commit that landed no files", got[1].PriorAIHistory)
	}
	if probeCalls != 1 {
		t.Errorf("probe ran %d times, want 1 — the failure is memoized and the no-files session never probes", probeCalls)
	}
}

// TestNewCommitCondensedSignal_DedupesAmendRecondensation pins the at-most-once
// ledger: `git commit --amend` re-runs PostCommit with the same trailer
// checkpoint ID and an ACTIVE session re-condenses unconditionally, so without
// the ledger one logical commit is emitted (and counted) twice.
func TestNewCommitCondensedSignal_DedupesAmendRecondensation(t *testing.T) {
	t.Parallel()

	state := &SessionState{AgentType: agent.AgentTypeClaudeCode}
	committed := map[string]struct{}{"a.go": {}}
	result := &CondenseResult{
		CheckpointID: id.MustCheckpointID("abcdef123456"),
		FilesTouched: []string{"a.go"},
	}

	if sig := newCommitCondensedSignal(state, result, committed); sig == nil {
		t.Fatal("first condensation of a checkpoint must produce a signal")
	}
	if sig := newCommitCondensedSignal(state, result, committed); sig != nil {
		t.Error("re-condensing the same checkpoint (amend) must not produce a second signal")
	}

	next := &CondenseResult{
		CheckpointID: id.MustCheckpointID("fedcba654321"),
		FilesTouched: []string{"a.go"},
	}
	if sig := newCommitCondensedSignal(state, next, committed); sig == nil {
		t.Error("a new checkpoint ID is a new commit and must produce a signal")
	}
}

// TestCommitCondensedEmitter_SearchProbeGate pins the gate condensation
// consults before scanning a transcript: open only when telemetry is opted in,
// closed for disabled settings and for a nil emitter (the ungated condensation
// paths). This is what keeps the full-transcript search probe off every path
// whose result would be discarded.
func TestCommitCondensedEmitter_SearchProbeGate(t *testing.T) {
	writeTelemetrySettings(t, "true")
	e := newCommitCondensedEmitter(t.TempDir(), "HEAD")
	if !e.searchProbeAllowed(t.Context()) {
		t.Error("gate must open when telemetry is enabled")
	}
}

func TestCommitCondensedEmitter_SearchProbeGateClosed(t *testing.T) {
	writeTelemetrySettings(t, "false")
	e := newCommitCondensedEmitter(t.TempDir(), "HEAD")
	if e.searchProbeAllowed(t.Context()) {
		t.Error("gate must stay closed when telemetry is disabled")
	}
	var nilEmitter *commitCondensedEmitter
	if nilEmitter.searchProbeAllowed(t.Context()) {
		t.Error("a nil emitter must report the gate closed")
	}
}

func TestCommitCondensedEmitter_ProbesAndGatesOnce(t *testing.T) {
	writeTelemetrySettings(t, "true")

	probeCalls := 0
	sends := 0
	restore := emitCommitCondensed
	emitCommitCondensed = func(telemetry.CommitCondensedSignal, bool, string) { sends++ }
	t.Cleanup(func() { emitCommitCondensed = restore })

	e := newCommitCondensedEmitter(t.TempDir(), "HEAD")
	e.probeFn = func(context.Context, string, string) (map[string]struct{}, bool) {
		probeCalls++
		return map[string]struct{}{"shared.go": {}}, true
	}

	for _, files := range [][]string{{"shared.go"}, {"other.go"}, {"shared.go", "other.go"}} {
		e.emit(t.Context(), &commitCondensedSignal{
			agentType:    agent.AgentTypeClaudeCode,
			searchProbe:  searchProbe{used: true, source: searchSourceCommand},
			filesTouched: files,
		})
	}

	if probeCalls != 1 {
		t.Errorf("probe ran %d times across 3 sessions, want 1 — the scan is commit-scoped", probeCalls)
	}
	if sends != 3 {
		t.Errorf("sent %d events, want 3", sends)
	}
}

// TestCommitCondensedEmitter_NilSignalCostsNothing pins the laziness that makes
// a commit which condenses nothing free: no settings read, no subprocess.
func TestCommitCondensedEmitter_NilSignalCostsNothing(t *testing.T) {
	t.Parallel()

	probeCalls := 0
	e := newCommitCondensedEmitter(t.TempDir(), "HEAD")
	e.probeFn = func(context.Context, string, string) (map[string]struct{}, bool) {
		probeCalls++
		return nil, true
	}
	e.emit(t.Context(), nil)

	if probeCalls != 0 {
		t.Errorf("probe ran %d times for a nil signal, want 0", probeCalls)
	}
	if e.gateResolved {
		t.Error("a nil signal must not resolve the telemetry gate")
	}
}

// TestCommitCondensedEmitter_OptedOutNeverProbes keeps the doc comment's
// promise: the gate runs in front of the probe, so an opted-out user never pays
// for the git-log scan.
func TestCommitCondensedEmitter_OptedOutNeverProbes(t *testing.T) {
	for _, tt := range []struct {
		name      string
		telemetry string
	}{
		{"explicit opt-out", "false"},
		{"key absent (telemetry is opt-in)", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writeTelemetrySettings(t, tt.telemetry)

			probeCalls, sends := 0, 0
			restore := emitCommitCondensed
			emitCommitCondensed = func(telemetry.CommitCondensedSignal, bool, string) { sends++ }
			t.Cleanup(func() { emitCommitCondensed = restore })

			e := newCommitCondensedEmitter(t.TempDir(), "HEAD")
			e.probeFn = func(context.Context, string, string) (map[string]struct{}, bool) {
				probeCalls++
				return nil, true
			}
			e.emit(t.Context(), &commitCondensedSignal{
				agentType:    agent.AgentTypeClaudeCode,
				filesTouched: []string{"a.go"},
			})

			if probeCalls != 0 {
				t.Errorf("probe ran %d times while opted out, want 0", probeCalls)
			}
			if sends != 0 {
				t.Errorf("sent %d events while opted out, want 0", sends)
			}
		})
	}
}

// TestCommitCondensedEmitter_OmitsUsedSearchWhenUnsupported pins the mapping
// from the probe's tri-state onto the payload's nullable field.
func TestCommitCondensedEmitter_OmitsUsedSearchWhenUnsupported(t *testing.T) {
	writeTelemetrySettings(t, "true")

	var got telemetry.CommitCondensedSignal
	restore := emitCommitCondensed
	emitCommitCondensed = func(sig telemetry.CommitCondensedSignal, _ bool, _ string) { got = sig }
	t.Cleanup(func() { emitCommitCondensed = restore })

	e := newCommitCondensedEmitter(t.TempDir(), "HEAD")
	e.probeFn = func(context.Context, string, string) (map[string]struct{}, bool) { return nil, true }
	e.emit(t.Context(), &commitCondensedSignal{
		agentType:    agent.AgentTypeClaudeCode,
		searchProbe:  searchProbe{used: false, source: searchSourceUnsupported},
		filesTouched: []string{"a.go"},
	})

	if got.UsedSearch != nil {
		t.Errorf("UsedSearch = %v, want nil so the property is omitted", *got.UsedSearch)
	}
	if got.UsedSearchSource != searchSourceUnsupported {
		t.Errorf("UsedSearchSource = %q, want %q", got.UsedSearchSource, searchSourceUnsupported)
	}
}

// TestSearchProbe_ZeroValueIsNotAMeasurement is the regression test for the
// fabricated negative: a probe that was never run must never present itself as
// a measured "did not search", and must still carry one of the four documented
// source labels.
func TestSearchProbe_ZeroValueIsNotAMeasurement(t *testing.T) {
	t.Parallel()

	var zero searchProbe
	if zero.measured() {
		t.Error("the zero-value probe reports itself as measured; used_search would ship a fabricated false")
	}
	if zero.label() != searchSourceUnsupported {
		t.Errorf("zero-value label = %q, want %q — used_search_source must always be one of the four sources",
			zero.label(), searchSourceUnsupported)
	}

	// An unrecognised source is treated the same way. measured() is an
	// allowlist precisely so a future source added without updating it fails
	// safe rather than leaking a measured false.
	unknown := searchProbe{used: true, source: "some-future-source"}
	if unknown.measured() {
		t.Error("an unrecognised source reports as measured; measured() must be an allowlist")
	}

	for _, p := range []searchProbe{
		{source: searchSourceNone},
		{used: true, source: searchSourceCommand},
		{used: true, source: searchSourceSubagent},
	} {
		if !p.measured() {
			t.Errorf("source %q must count as measured", p.source)
		}
		if p.label() != p.source {
			t.Errorf("label() = %q, want %q", p.label(), p.source)
		}
	}
}

// TestCommitCondensedEmitter_ZeroProbeOmitsUsedSearch covers the path the
// finding named: a session with no readable transcript still condenses on
// FilesTouched or task records alone, so the signal it carries must not read as
// a measured negative.
func TestCommitCondensedEmitter_ZeroProbeOmitsUsedSearch(t *testing.T) {
	writeTelemetrySettings(t, "true")

	var got telemetry.CommitCondensedSignal
	restore := emitCommitCondensed
	emitCommitCondensed = func(sig telemetry.CommitCondensedSignal, _ bool, _ string) { got = sig }
	t.Cleanup(func() { emitCommitCondensed = restore })

	e := newCommitCondensedEmitter(t.TempDir(), "HEAD")
	e.probeFn = func(context.Context, string, string) (map[string]struct{}, bool) { return nil, true }
	// searchProbe left at its zero value, as a no-transcript condensation
	// produced before the probe was made unconditional.
	e.emit(t.Context(), &commitCondensedSignal{
		agentType:    agent.AgentTypeClaudeCode,
		filesTouched: []string{"a.go"},
	})

	if got.UsedSearch != nil {
		t.Errorf("UsedSearch = %v, want nil so the property is omitted rather than a fabricated false", *got.UsedSearch)
	}
	if got.UsedSearchSource != searchSourceUnsupported {
		t.Errorf("UsedSearchSource = %q, want %q — never the empty string", got.UsedSearchSource, searchSourceUnsupported)
	}
}
