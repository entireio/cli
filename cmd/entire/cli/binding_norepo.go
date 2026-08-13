// binding_norepo.go is the evidence-only hook path for a cwd outside any git
// repo. Launching an agent from a parent directory above sibling repos
// (~/dev/acme — #1098's loudest scenario) fires every hook with a non-repo
// cwd, where checkpoint capture is impossible; the session's evidence can
// still name the repos it touched. This path parses the event, extracts file
// paths, and records foreign repos in the machine-level session record —
// nothing else: no session state, no checkpoints, no logging.Init, no repo
// writes.
package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// recordNoRepoEvidence is the no-repo counterpart of the in-repo binding tap:
// best-effort, evidence-only, and crash-proof — once user-level hooks exist,
// it runs on EVERY hook in EVERY non-repo cwd, so it must be cheap and can
// never block the agent. For non-evidence verbs the added cost over the old
// silent bail is one registry map lookup plus one stdin parse (~270ns + 2
// allocs measured): the verb→event-type mapping is derivable from hookName,
// but no agent exposes it pre-parse, and the saving would be negligible since
// the high-frequency verbs are evidence-bearing and must parse anyway.
//
// logging.Init never ran here (initHookLogging no-ops outside a repo), so only
// logging.Debug may be used: pre-Init it is a silent no-op, while
// Info/Warn/Error would leak raw slog text onto the agent's stderr.
func recordNoRepoEvidence(ctx context.Context, agentName types.AgentName, hookName string, stdin io.Reader) {
	logCtx := logging.WithComponent(ctx, "binding")
	defer func() {
		if r := recover(); r != nil {
			logging.Debug(logCtx, "no-repo evidence path panicked; evidence dropped, agent unaffected",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
		}
	}()

	ag, err := agent.Get(agentName)
	if err != nil {
		return
	}
	// Deliberately no shouldSkipForwardedHook here: it needs a repo root. A
	// forwarded hook (e.g. a Cursor-forwarded claude event) records evidence
	// under the parsing agent's type — harmless for evidence, and the record's
	// meta is first-write-only.
	handler, ok := agent.AsHookSupport(ag)
	if !ok {
		return
	}
	event, parseErr := handler.ParseHookEvent(ctx, hookName, stdin)
	if parseErr != nil || event == nil {
		return
	}
	// Require a real session ID: an empty ID has nothing to key the record on,
	// and the tap's unknownSessionID guard only covers its own entry point —
	// the turn-end evidence+cursor write below bypasses it.
	if event.SessionID == "" || event.SessionID == unknownSessionID {
		return
	}

	meta := binding.SessionMeta{
		AgentType:      string(ag.Type()),
		TranscriptPath: event.SessionRef,
		LaunchRoot:     "", // no repo at the hook cwd
	}

	// Non-evidence events are listed explicitly (not folded into the default)
	// so the exhaustive linter forces a review when a new EventType is added —
	// a new evidence-bearing type must be routed here deliberately.
	switch event.Type {
	case agent.ToolUse:
		// currentWorktreeRoot="" — there is no current repo, so every resolved
		// repo is foreign and the same-worktree skip can never match.
		recordForeignEvidence(ctx, event.SessionID, meta, "", absoluteToolUsePaths(event))
	case agent.TurnEnd:
		if event.SessionRef == "" {
			return
		}
		scan, ok := scanTranscriptForeign(logCtx, ag, event)
		if !ok {
			return
		}
		// currentWorktreeRoot="" — there is no current repo, so every resolved
		// repo is foreign and the same-worktree skip can never match.
		evs := resolveForeignRepos(logCtx, event.SessionID, "", scan.files)
		// Evidence and cursor commit in ONE locked mutation. Advancing the
		// cursor in its own earlier write let a failed evidence write (lock
		// timeout, transient I/O, killed hook) leave the cursor past
		// transcript lines whose evidence never persisted — silently skipped
		// forever. A failure here writes neither, so the next turn-end
		// rescans the same span; nothing was recorded, so the retry cannot
		// double-count.
		if err := binding.RecordEvidenceAndAdvanceCursor(logCtx, event.SessionID, meta, evs, scan.nextCursor, scan.reset); err != nil {
			logging.Debug(logCtx, "no-repo evidence: failed to record transcript scan",
				slog.String("session_id", event.SessionID),
				slog.String("error", err.Error()))
		}
	case agent.SessionStart, agent.TurnStart, agent.Compaction, agent.SessionEnd,
		agent.SubagentStart, agent.SubagentEnd, agent.ModelUpdate:
		// No path evidence; the session-start notice UX is a separate slice.
	default:
	}
}

// absoluteToolUsePaths flattens a ToolUse event's path payloads to absolute
// paths. Relative paths join onto the event's CWD — the tool's OWN working
// directory, which the payload carries precisely so relative paths can be
// resolved — and are skipped when it is empty (no join base means any guess
// could bind the wrong repo).
func absoluteToolUsePaths(event *agent.Event) []string {
	groups := [][]string{event.ModifiedFiles, event.NewFiles, event.DeletedFiles}
	var out []string
	for _, group := range groups {
		for _, p := range group {
			switch {
			case p == "":
			case filepath.IsAbs(p):
				out = append(out, p)
			case event.CWD != "":
				out = append(out, filepath.Join(event.CWD, p))
			}
		}
	}
	return out
}

// transcriptScan is one completed transcript scan: the foreign-path evidence
// found in the newly scanned span, plus the cursor position the caller must
// commit ATOMICALLY with that evidence (binding.RecordEvidenceAndAdvanceCursor).
type transcriptScan struct {
	files      []string // absolute foreign paths extracted from the scanned span
	nextCursor int      // extractor-native position the scan reached
	reset      bool     // transcript truncated/rotated: cursor may regress
}

// scanTranscriptForeign extracts modified-file evidence from the turn's new
// transcript span and returns only ABSOLUTE paths: transcript-level extraction
// has no safe join base — the hook's cwd is the launch dir, but tool calls in
// the transcript may have run after the agent cd'd elsewhere (unlike
// per-ToolUse payloads, whose CWD is the tool's own) — so dropping relative
// paths is the only non-guessing option.
//
// The scan cursor lives in the machine session record and is EXTRACTOR-NATIVE:
// lines for Claude Code's JSONL, a message index for Gemini CLI's single-JSON
// transcript (its extractor interprets startOffset as a message index and
// returns a message count). Cursor advancement means each transcript unit is
// scanned at most once across the session, so the tap's per-turn bounds
// compose with a per-unit-once guarantee. This function only READS: the
// returned nextCursor is committed by the caller together with the evidence
// derived from the scan — never in a separate earlier write, which would let
// a failed evidence write skip the scanned span forever. ok=false means
// nothing was scanned (unreadable/unextractable transcript, no extractor):
// the cursor must not move, so the span is retried next turn. On a
// successful scan the cursor advances even when no paths were found, to keep
// repeat scans cheap for chatty no-repo sessions — consequence: every no-repo
// session with a turn-end creates a cursor-only record on its first Stop,
// even if it never touches a repo (retention for these is part of slice 2's
// parked retention story).
func scanTranscriptForeign(logCtx context.Context, ag agent.Agent, event *agent.Event) (transcriptScan, bool) {
	cursor := 0
	rec, err := binding.LoadRecord(logCtx, event.SessionID)
	switch {
	case err != nil:
		// Degrade to a full scan.
		logging.Debug(logCtx, "no-repo evidence: session record unreadable, rescanning from 0",
			slog.String("session_id", event.SessionID),
			slog.String("error", err.Error()))
	case rec != nil:
		cursor = rec.LastScannedTranscriptCursor
	}

	var files []string
	var nextCursor int
	truncated := false
	if subagentExtractor, ok := agent.AsSubagentAwareExtractor(ag); ok {
		// Claude-shaped JSONL: read once, count complete lines ourselves.
		data, readErr := os.ReadFile(event.SessionRef)
		if readErr != nil {
			return transcriptScan{}, false
		}
		// No +1 for a trailing partial line — transcript.SliceFromLine skips
		// exactly N complete lines, so an uncounted partial line is included
		// in this scan and re-scanned once completed: never skipped, at most
		// one line re-extracted.
		nextCursor = bytes.Count(data, []byte("\n"))
		if nextCursor < cursor {
			cursor = 0
			truncated = true
		}
		var exErr error
		files, exErr = subagentExtractor.ExtractAllModifiedFiles(data, cursor,
			paths.SubagentsDir(filepath.Dir(event.SessionRef), event.SessionID))
		if exErr != nil {
			// Don't advance past unextracted content — retry next turn.
			logging.Debug(logCtx, "no-repo evidence: transcript extraction failed",
				slog.String("session_id", event.SessionID),
				slog.String("error", exErr.Error()))
			return transcriptScan{}, false
		}
	} else if analyzer, ok := agent.AsTranscriptAnalyzer(ag); ok {
		// The extractor reads the file itself and its RETURNED position is the
		// next cursor, in whatever unit the agent uses.
		var exErr error
		files, nextCursor, exErr = analyzer.ExtractModifiedFilesFromOffset(event.SessionRef, cursor)
		if exErr == nil && nextCursor < cursor {
			// Truncated/rotated in the extractor's own coordinates: one
			// immediate rescan from 0.
			truncated = true
			files, nextCursor, exErr = analyzer.ExtractModifiedFilesFromOffset(event.SessionRef, 0)
		}
		if exErr != nil {
			logging.Debug(logCtx, "no-repo evidence: transcript extraction failed",
				slog.String("session_id", event.SessionID),
				slog.String("error", exErr.Error()))
			return transcriptScan{}, false
		}
	} else {
		return transcriptScan{}, false
	}

	absolute := make([]string, 0, len(files))
	for _, f := range files {
		if filepath.IsAbs(f) {
			absolute = append(absolute, f)
		}
	}
	return transcriptScan{files: absolute, nextCursor: nextCursor, reset: truncated}, true
}
