package strategy

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// priorAICheckpointsLookback bounds how many commits back the
// missed-opportunity signal scans for AI checkpoint history. One bounded git
// subprocess regardless of repo size.
const priorAICheckpointsLookback = 50

// searchProbe records whether the session consulted Entire's history search
// AND how that answer was obtained. The source travels with the boolean all the
// way to the payload because the two carry different information: "we looked and
// found nothing" and "we cannot look at this transcript" are both `used=false`,
// and a ratio computed over their union is a confident number over a population
// it silently cannot measure.
type searchProbe struct {
	used   bool
	source string
}

// measured reports whether used is a real measurement rather than "we could not
// look".
//
// Deliberately an allowlist over the known-measured sources, not `source !=
// searchSourceUnsupported`. The zero value of searchProbe has source "", which a
// denylist admits — so any path that condenses without ever running the probe
// would ship used_search=false with a blank label: the fabricated negative this
// whole tri-state exists to prevent, wearing no label to reveal itself. An
// allowlist makes forgetting to set the probe fail safe instead.
func (p searchProbe) measured() bool {
	switch p.source {
	case searchSourceNone, searchSourceCommand, searchSourceSubagent:
		return true
	default:
		return false
	}
}

// label is the value sent as used_search_source. It maps the zero value onto
// unsupported so the property honours its documented contract of always being
// one of the four named sources.
func (p searchProbe) label() string {
	if p.source == "" {
		return searchSourceUnsupported
	}
	return p.source
}

// Sources for searchProbe.source. Low cardinality on purpose — this is a
// PostHog property, and a new probe method should get a new label rather than
// being folded into an existing one.
const (
	// searchSourceUnsupported: this agent's transcript has no tool-call view,
	// so the question is unanswerable. used is false, and MUST NOT be read as
	// "did not search".
	searchSourceUnsupported = "unsupported"
	// searchSourceNone: the transcript was walked and no invocation was found.
	// This is the only trustworthy false.
	searchSourceNone = "none"
	// searchSourceCommand: a shell tool ran the command directly.
	searchSourceCommand = "command"
	// searchSourceSubagent: the entire-search subagent was dispatched.
	searchSourceSubagent = "subagent"
)

// entireSearchHints is the byte-level prefilter handed to the scanner. It is a
// performance filter, so every string entireSearchCommandPattern or the
// subagent check can accept must contain one of these LITERALLY — which is why
// the pattern below spells the internal separators as single spaces instead of
// \s+. TestSearchHintsCoverPattern pins the relationship; loosening the pattern
// without extending the hints is a silent false negative, not a slow path.
//
//nolint:gochecknoglobals // immutable lookup table, built once.
var entireSearchHints = [][]byte{
	[]byte("entire search"),
	[]byte("entire checkpoint search"),
	[]byte(EntireSearchSubagentName),
}

// entireSearchCommandPattern matches `entire search` / `entire checkpoint
// search` in COMMAND position — at the start of a command, or after a shell
// separator, tolerating leading env assignments and a path prefix. Position is
// the whole point: it accepts `cd sub && entire search "x" --json` and
// `/usr/local/bin/entire search x`, while rejecting the mentions that made the
// old substring probe ~18x false-positive, because in every one of them the
// phrase sits inside an argument rather than at a command boundary —
// `grep -rn "entire search" cmd/`, `git commit -m "... entire search ..."`.
//
// The regex itself is quote-blind, so it MUST only ever see commands that have
// been through sanitizeShellCommandForMatching: without that, a separator
// inside a quoted argument reads as a command boundary and the phrase after it
// as a command — `git commit -m "wip; entire search notes"`,
// `rg "foo|entire search bar" .`, and a heredoc writing the search skill's own
// body all matched. detectSearchUsage is the only caller and applies it.
//
// Known residual false negatives, and they are the safer direction: `xargs
// entire search`, `for f in …; do entire search; done` reached through a loop
// variable, an invocation inside backticks or a quoted `$(…)` (the sanitizer
// strips quoted spans whole), and anything behind a wrapper script. They report
// `none`, so the missed-opportunity rate reads as an upper bound.
//
//nolint:gochecknoglobals // compiled once, read-only.
var entireSearchCommandPattern = regexp.MustCompile(
	`(?:^|[;&|(\n])\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)*(?:[\w./-]*/)?entire (?:checkpoint )?search(?:\s|$)`)

// commandInvokesEntireSearch is THE command matcher: sanitize, then match.
// detectSearchUsage and the pinning tests both go through it so the sanitizer
// cannot be bypassed on one side and not the other.
func commandInvokesEntireSearch(cmd string) bool {
	return entireSearchCommandPattern.MatchString(sanitizeShellCommandForMatching(cmd))
}

// sanitizeShellCommandForMatching prepares a raw shell command for
// entireSearchCommandPattern by blanking the spans whose content must not be
// read as shell structure: single-quoted strings, double-quoted strings, and
// heredoc bodies. Each blanked span becomes a single NUL byte — NUL is in
// neither the pattern's separator class nor any of its literals, so a
// replacement can neither fabricate a command boundary nor join two fragments
// into a phrase that was never contiguous in the input. Every non-NUL byte of
// the output is copied verbatim, in order, from the input, which is what keeps
// the hint-prefilter contract intact: any match found in the sanitized text
// exists literally in the raw command.
//
// This is a matcher aid, not a shell parser. It understands just enough —
// backslash escapes outside quotes, `\"` inside double quotes, `<<`/`<<-`
// heredocs with optionally quoted delimiters (which must start with a letter
// or underscore, so `$((1<<2))` is not misread as one) — to kill the
// false-positive classes review actually measured. Where it errs, it errs
// toward blanking too much, which is a false NEGATIVE for the probe: the safer
// direction, per entireSearchCommandPattern's doc.
func sanitizeShellCommandForMatching(cmd string) string {
	var b strings.Builder
	b.Grow(len(cmd))
	var pendingHeredocs []string
	for i := 0; i < len(cmd); {
		switch c := cmd[i]; c {
		case '\\':
			// An escaped character is literal text; copy the pair so the
			// escaped byte can't be read as a quote or separator opener.
			b.WriteByte(c)
			if i+1 < len(cmd) {
				b.WriteByte(cmd[i+1])
				i += 2
			} else {
				i++
			}
		case '\'':
			if end := strings.IndexByte(cmd[i+1:], '\''); end >= 0 {
				i += end + 2
			} else {
				i = len(cmd) // unterminated: blank to end
			}
			b.WriteByte(0)
		case '"':
			j := i + 1
			for j < len(cmd) && cmd[j] != '"' {
				if cmd[j] == '\\' {
					j++ // skip the escaped byte too
				}
				j++
			}
			if j < len(cmd) {
				i = j + 1
			} else {
				i = len(cmd) // unterminated: blank to end
			}
			b.WriteByte(0)
		case '<':
			if delim, next, ok := parseHeredocStart(cmd, i); ok {
				pendingHeredocs = append(pendingHeredocs, delim)
				b.WriteString(cmd[i:next])
				i = next
			} else {
				b.WriteByte(c)
				i++
			}
		case '\n':
			b.WriteByte(c)
			i++
			// The lines that follow are heredoc bodies, not commands. Blank
			// each pending body through its delimiter line, keeping a newline
			// in its place so a real command on the line AFTER the body still
			// sits on a command boundary.
			for _, delim := range pendingHeredocs {
				i = skipHeredocBody(cmd, i, delim)
				b.WriteByte(0)
				b.WriteByte('\n')
			}
			pendingHeredocs = pendingHeredocs[:0]
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// parseHeredocStart reports whether cmd[i:] opens a heredoc (`<<` or `<<-`,
// not the `<<<` here-string), returning the delimiter word and the offset just
// past it. The delimiter must start with a letter or underscore so arithmetic
// like `$((1<<2))` is not misread as a heredoc.
func parseHeredocStart(cmd string, i int) (delim string, next int, ok bool) {
	if i+1 >= len(cmd) || cmd[i+1] != '<' || (i+2 < len(cmd) && cmd[i+2] == '<') {
		return "", 0, false
	}
	j := i + 2
	if j < len(cmd) && cmd[j] == '-' {
		j++
	}
	for j < len(cmd) && (cmd[j] == ' ' || cmd[j] == '\t') {
		j++
	}
	var quote byte
	if j < len(cmd) && (cmd[j] == '\'' || cmd[j] == '"') {
		quote = cmd[j]
		j++
	}
	start := j
	for j < len(cmd) && (isShellWordByte(cmd[j]) || (j > start && cmd[j] >= '0' && cmd[j] <= '9')) {
		j++
	}
	delim = cmd[start:j]
	if delim == "" || !isShellWordByte(delim[0]) {
		return "", 0, false
	}
	if quote != 0 && j < len(cmd) && cmd[j] == quote {
		j++
	}
	return delim, j, true
}

func isShellWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// skipHeredocBody returns the offset just past the line matching delim
// (tolerating the leading tabs `<<-` allows), or len(cmd) when the body is
// unterminated.
func skipHeredocBody(cmd string, i int, delim string) int {
	for i < len(cmd) {
		line := cmd[i:]
		next := len(cmd)
		if eol := strings.IndexByte(line, '\n'); eol >= 0 {
			line = line[:eol]
			next = i + eol + 1
		}
		i = next
		if strings.TrimLeft(line, "\t") == delim {
			return i
		}
	}
	return i
}

// EntireSearchSubagentName is the search skill name setup_search_skill.go
// (package cli) scaffolds into <agent>/skills/entire-search/SKILL.md for every
// agent. Exported so the installer and its tests pin their scaffolded paths
// and template bodies against the value this probe matches — strategy cannot
// import cli, so the constant lives here and cli reaches down.
//
// The subagent-dispatch match below exists for the artifact this feature
// shipped as before it became a skill: a dispatchable subagent under this
// name (.claude/agents/entire-search.md and Codex/Gemini equivalents). Those
// installs linger — the installer only removes them on the next
// --search-skill run — and a subagent's own `entire search` Bash call is
// written to a SEPARATE transcript file that condensation never reads, so
// dropping the dispatch match reports "did not search" for exactly the
// sessions still on the old artifact. Skill-based sessions run the command in
// the main transcript and surface through the command match instead — but
// only where the command match runs at all: ScanToolInvocations requires
// ToolInvocationScanner, which Claude Code alone implements, so the other
// agents' sessions report searchSourceUnsupported regardless of skill use.
const EntireSearchSubagentName = "entire-search"

// detectSearchUsage reports whether the session consulted Entire's history
// search, and how it knows.
//
// Structural by construction: it asks the agent for recorded tool invocations
// rather than scanning bytes, so a transcript that merely *mentions* the command
// no longer counts. Entire installs the artifacts that made that distinction
// urgent — setup_search_skill.go embeds `entire search --json` in the search
// skill's own body, and investigate/prompt.go injects it into every investigate
// prompt — so the old probe fired on sessions that had only read Entire's own
// text.
//
// An empty transcript reports unsupported, not none: seeing nothing because
// there is nothing to see is not evidence that the session did not search.
func detectSearchUsage(ag agent.Agent, transcriptData []byte) searchProbe {
	source := searchSourceNone
	found, supported := agent.ScanToolInvocations(ag, transcriptData, entireSearchHints, func(inv agent.ToolInvocation) bool {
		// Exact, not EqualFold: the hint prefilter that decides whether this
		// line is parsed at all is case-sensitive bytes.Contains, so a
		// case-insensitive matcher here would accept a spelling the prefilter
		// already discarded — a silent false negative, which is exactly the
		// hazard ToolInvocationScanner's doc warns callers about.
		if strings.TrimSpace(inv.SubagentType) == EntireSearchSubagentName {
			source = searchSourceSubagent
			return true
		}
		if inv.Command != "" && commandInvokesEntireSearch(inv.Command) {
			source = searchSourceCommand
			return true
		}
		return false
	})
	if !supported {
		return searchProbe{used: false, source: searchSourceUnsupported}
	}
	return searchProbe{used: found, source: source}
}

// priorAICommitFiles returns the repo-root-relative paths touched by recent
// commits carrying an Entire checkpoint trailer, walking history from commit —
// the commit being reported on — and excluding that commit itself (--skip=1: it
// is not its own prior history). Anchoring on the explicit hash rather than
// ambient HEAD matters twice over: the probe runs after the session gate
// releases, so HEAD may have moved (a chained hook amending, a concurrent
// commit), and hooks inherit git's exported GIT_DIR/GIT_WORK_TREE, which
// override -C's repo selection — gitrepo.EnvWithoutRepoOverrides scrubs those, same
// as this package's other `git -C` call site. Paths match git log --name-only
// output, i.e. the same form as SessionState.FilesTouched.
//
// ok=false means the probe COULD NOT run (git unavailable, cancelled ctx, a
// log invocation that errors) — the caller must treat that as unmeasured, not
// as "no prior history": a fabricated false here deflates the very miss rate
// the event exists to measure, the same conflation the used_search tri-state
// prevents. ok=true with a nil set is the measured "no prior history".
//
// A set rather than a predicate because the answer is commit-scoped and
// identical for every session in the commit — only the intersection differs —
// which is what lets commitCondensedEmitter run the subprocess once.
//
// -z keeps names unquoted and NUL-terminated, so non-ASCII paths (which git
// would otherwise emit quoted, e.g. "caf\303\251.go") still match their
// FilesTouched form, and names containing newlines survive parsing.
//
// Merge commits contribute nothing, and that is deliberate rather than a gap.
// --name-only emits no names for a merge, and the obvious fix
// (--diff-merges=first-parent) would make a "Merge origin/main into X" commit
// attribute every file it merged in, inflating prior_ai_history and therefore
// the miss rate. A merge's content is already attributed to the individual
// trailer-carrying commits it brings in, which sit in this window on their own
// account. Squash merges are single-parent, so their files do appear.
func priorAICommitFiles(ctx context.Context, repoRoot, commit string) (result map[string]struct{}, ok bool) {
	// Each record starts with an empty field: -z NUL-terminates the format
	// output, and %x00 prefixes it, so splitting the whole output on NUL yields
	// ["", "<hash>\n<body>", "\n<file>", "<file>", …] per commit. A commit
	// message cannot contain NUL, so the record marker rests on nothing about
	// message content — the defect in the %x1e/%x1f framing this replaces,
	// where either control character in a body split a record early and dropped
	// a real trailer. %H guarantees the post-marker field is non-empty, so an
	// empty-message commit cannot produce two adjacent markers and mis-frame
	// the next record.
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "log", "-z",
		"--skip=1", "-n", strconv.Itoa(priorAICheckpointsLookback),
		"--name-only", "--format=%x00%H%n%B", commit)
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var files map[string]struct{}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		if fields[i] != "" {
			continue // not a record marker; consumed below
		}
		i++
		if i >= len(fields) {
			break
		}
		isCheckpoint := false
		if _, ok := trailers.ParseCheckpoint(fields[i]); ok {
			isCheckpoint = true
		}
		// Consume this record's file list, whether or not we want it, so the
		// outer loop resumes on the next marker rather than mid-record.
		for i+1 < len(fields) && fields[i+1] != "" {
			i++
			if !isCheckpoint {
				continue
			}
			// The diff section opens with the newline separating it from the
			// format block; strip it from the segment carrying it.
			name := strings.TrimPrefix(fields[i], "\n")
			if name == "" {
				continue
			}
			if files == nil {
				files = make(map[string]struct{})
			}
			files[name] = struct{}{}
		}
	}
	return files, true
}

// commitCondensedSignal captures, while the session gate is held, the few
// state/result fields the condensed-checkpoint signal needs. Everything
// expensive — the env/settings gates, the git-log density probe, machine-ID
// lookup, and the detached-process spawn — runs later in
// commitCondensedEmitter.emit, after MutateSessionState has released the gate,
// matching the skill-event telemetry pattern.
type commitCondensedSignal struct {
	agentType    types.AgentType
	searchProbe  searchProbe
	filesTouched []string
}

// newCommitCondensedSignal snapshots the signal inputs for a successful
// condensation, or nil when there is nothing to report. Cheap and I/O-free by
// design — it and the gated search probe are the only parts of this signal
// that run under the session gate.
//
// filesTouched is re-intersected against committedFiles rather than trusting
// result.FilesTouched: filterFilesTouched deliberately leaves the session's
// list whole when the commit changed no files (--allow-empty, or file
// detection failing upstream), which is correct for checkpoint metadata but
// would make files_committed count files the commit never landed — and
// prior_ai_history intersect on them. The intersection is also what copies the
// slice out from under the caller's later state mutations.
//
// Commit-scoped by construction: the sole caller is condenseAndUpdateState,
// reached only from postCommitProcessSessionLocked. That is a scoping decision,
// not an oversight. The payload describes a commit — files_committed counts its
// files, and prior_ai_history's git-log probe passes --skip=1 precisely to
// exclude the commit just made — and it feeds the ratio "commits that landed
// AI-dense files without consulting search". The other condensation paths
// (CondenseSessionByID via doctor, CondenseAndMarkFullyCondensed at session
// end) condense real checkpoints but have no commit: --skip=1 would exclude an
// unrelated HEAD, and the resulting rows would be indistinguishable from
// genuine misses, inflating the denominator instead of completing it. Covering
// them means a trigger discriminator plus nullable commit-scoped fields — a
// change to what the metric means, not a bug fix.
func newCommitCondensedSignal(state *SessionState, result *CondenseResult, committedFiles map[string]struct{}) *commitCondensedSignal {
	if result == nil || result.Skipped || state == nil {
		return nil
	}
	// At-most-once per checkpoint: an amend re-condenses the same trailer
	// checkpoint ID, and emitting again would count one logical commit twice.
	// The ledger write rides the session-state save that already gates the
	// emit. Guarded on a real ID so unit fixtures with a zero result don't
	// dedupe against the field's zero value.
	if result.CheckpointID != id.EmptyCheckpointID {
		ckpt := result.CheckpointID.String()
		if state.CommitCondensedSignalCheckpointID == ckpt {
			return nil
		}
		state.CommitCondensedSignalCheckpointID = ckpt
	}
	files := make([]string, 0, len(result.FilesTouched))
	for _, f := range result.FilesTouched {
		if _, ok := committedFiles[f]; ok {
			files = append(files, f)
		}
	}
	return &commitCondensedSignal{
		agentType:    state.AgentType,
		searchProbe:  result.SearchProbe,
		filesTouched: files,
	}
}

// commitCondensedEmitter emits the cli_commit_condensed signal for each session
// of one commit, memoizing everything that is commit-scoped rather than
// session-scoped: the telemetry gate (settings load) and the git-log scan of
// prior AI checkpoint history.
//
// The scan was per session, and its cost is almost entirely process spawn:
// measured 13.8ms p50, flat across a 60x range of output size, against 8.7ms
// for `git --version` alone. Its output is identical for every session in the
// commit — only the per-session intersection differs — so paying it per session
// was paying for spawns, not work.
//
// Everything resolves on FIRST USE: a commit where no session condenses runs
// neither settings.Load nor git log, and an opted-out user never reaches the
// probe. The gate has two resolution points sharing one memo — searchProbeAllowed,
// called during condensation so a closed gate skips the transcript scan
// entirely, and emit, for the send decision — so whichever runs first pays the
// single settings load. Do not front-run the gate by resolving anything in the
// constructor.
//
// Not safe for concurrent use: PostCommit's session loop is sequential.
type commitCondensedEmitter struct {
	repoRoot string
	// commit is the hash of the commit PostCommit is reporting on; the
	// prior-history probe walks from it explicitly rather than from ambient
	// HEAD. See priorAICommitFiles.
	commit string

	gateResolved bool
	// settings is non-nil only when the gate is open; s.Enabled is needed at
	// send time, which is why the loaded value is retained rather than reduced
	// to a bool.
	settings *settings.EntireSettings

	probed     bool
	priorFiles map[string]struct{}
	// probeOK records whether the memoized probe run actually measured
	// anything; false makes every session's prior_ai_history unmeasured
	// (omitted) rather than a fabricated false.
	probeOK bool

	// probeFn is a test seam, like emitSkillTelemetry, so tests can count probe
	// invocations without spawning git.
	probeFn func(ctx context.Context, repoRoot, commit string) (map[string]struct{}, bool)
}

// newCommitCondensedEmitter returns an emitter for one commit. repoRoot is the
// worktree root PostCommit already resolved, which is what the per-session emit
// used to recompute; commit is the hash of the commit being reported on.
func newCommitCondensedEmitter(repoRoot, commit string) *commitCondensedEmitter {
	return &commitCondensedEmitter{repoRoot: repoRoot, commit: commit, probeFn: priorAICommitFiles}
}

// searchProbeAllowed reports whether the search-usage transcript scan should
// run at all, resolving the same memoized gate emit uses. Condensation calls
// this (through condenseOpts.searchProbeAllowed) BEFORE scanning, so an
// opted-out or not-opted-in user never pays a full-transcript pass for a
// payload that would be dropped at emit — and the two non-emitting condensation
// paths (doctor repair, session-end leftovers), which pass no gate, never scan.
func (e *commitCondensedEmitter) searchProbeAllowed(ctx context.Context) bool {
	if e == nil || telemetry.IsEnvOptedOut() {
		return false
	}
	_, ok := e.allowed(ctx)
	return ok
}

// emit sends the content-free adoption signal for one condensed checkpoint: did
// the session consult search, and did the files it committed already carry AI
// checkpoint history? Together these give the "commits that landed AI-dense
// files without consulting search" ratio that raw command counts cannot.
//
// Gated on the env opt-out and then the opt-in telemetry setting before any
// probe work, and best-effort throughout: the PostHog call happens in a
// detached child and never blocks the hook. Call it AFTER the surrounding
// MutateSessionState returns, never inside its closure.
func (e *commitCondensedEmitter) emit(ctx context.Context, sig *commitCondensedSignal) {
	if e == nil || sig == nil {
		return
	}
	if telemetry.IsEnvOptedOut() {
		return
	}
	s, ok := e.allowed(ctx)
	if !ok {
		return
	}
	priorAIHistory := e.priorAITouched(ctx, sig.filesTouched)
	// Report the registry key ("claude-code"), not the display name stored in
	// state.AgentType ("Claude Code"), so the agent property lines up with the
	// skill and command events. Unknown agent types fall back to the stored
	// string rather than dropping the signal.
	agentName := string(sig.agentType)
	if ag, agErr := agent.GetByAgentType(sig.agentType); agErr == nil && ag != nil {
		agentName = string(ag.Name())
	}
	// Send used_search only when it was actually measurable. An unsupported
	// transcript sends the source alone, so a consumer's `used_search = false`
	// filter excludes those rows instead of counting them as "did not search" —
	// a missing PostHog property is not false.
	var usedSearch *bool
	if sig.searchProbe.measured() {
		used := sig.searchProbe.used
		usedSearch = &used
	}
	emitCommitCondensed(telemetry.CommitCondensedSignal{
		Agent:            agentName,
		UsedSearch:       usedSearch,
		UsedSearchSource: sig.searchProbe.label(),
		PriorAIHistory:   priorAIHistory,
		FilesCommitted:   len(sig.filesTouched),
	}, s.Enabled, versioninfo.Version)
}

// allowed resolves the opt-in telemetry gate at most once per commit.
//
// IsTelemetryEnabled is #2023's helper, extracted precisely to stop the
// hand-rolled `s.Telemetry == nil || !*s.Telemetry` being copied a fourth time.
func (e *commitCondensedEmitter) allowed(ctx context.Context) (*settings.EntireSettings, bool) {
	if !e.gateResolved {
		e.gateResolved = true
		if s, err := settings.Load(ctx); err == nil && s.IsTelemetryEnabled() {
			e.settings = s
		}
	}
	return e.settings, e.settings != nil
}

// priorAITouched intersects one session's committed files against the commit's
// prior-history set, running the git-log scan at most once. Nil means the
// probe could not run and the payload must omit the property; a commit that
// landed no files is a measured false without probing at all.
func (e *commitCondensedEmitter) priorAITouched(ctx context.Context, files []string) *bool {
	measured := func(v bool) *bool { return &v }
	if len(files) == 0 {
		return measured(false)
	}
	if !e.probed {
		e.probed = true
		e.priorFiles, e.probeOK = e.probeFn(ctx, e.repoRoot, e.commit)
	}
	if !e.probeOK {
		return nil
	}
	for _, f := range files {
		if _, hit := e.priorFiles[f]; hit {
			return measured(true)
		}
	}
	return measured(false)
}

// emitCommitCondensed is the send step, separated from the gating above so
// tests can assert what the gate lets through without a PostHog client.
//
//nolint:gochecknoglobals // test seam, set and restored by in-package tests.
var emitCommitCondensed = telemetry.TrackCommitCondensedDetached
