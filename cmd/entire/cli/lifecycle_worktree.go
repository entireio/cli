package cli

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// capturePathEvents are the lifecycle events whose handlers read or write
// repo-relative state for the turn — the ones that must follow the agent into a
// linked worktree.
//
// SessionStart and SessionEnd are deliberately absent. They spawn detached
// children (__sweep_sessions, __refresh_trail_enablement) that take a worktree
// root as an explicit argument, and which root those should receive is a
// separate decision from where a turn's capture lands. Leaving them on the
// process working directory keeps this change to the capture path, which is
// where the defect is.
//
// SubagentEnd is absent for a different reason: no agent populates Event.CWD on
// its subagent-stop hook today, so listing it would be inert. Subagent task
// records resolve their transcripts from the agent's own session directory
// rather than from the repo, so they need their own analysis before following
// the turn's root — adding them here on the assumption that it is the same
// problem would be a guess.
func isCapturePathEvent(t agent.EventType) bool {
	switch t {
	case agent.TurnStart, agent.TurnEnd, agent.Compaction, agent.ToolUse:
		return true
	case agent.SessionStart, agent.SessionEnd, agent.SubagentStart, agent.SubagentEnd, agent.ModelUpdate:
		return false
	default:
		return false
	}
}

// withEventWorktree points repo-relative resolution at the worktree the agent
// was actually working in, when the event names one and it is safe to follow.
//
// Everything a turn touches — the metadata directory it writes full.jsonl into,
// .entire/tmp, the git status scan, the repository SaveStep opens, and the
// ephemeral store's own tree build — resolves through paths.WorktreeRoot. Hook
// processes inherit whatever directory the agent launched them from, which for
// an agent working in a linked worktree is not the worktree at all on some
// hosts and is the worktree on others. Deriving it from the event instead makes
// the outcome the same either way.
//
// SETTINGS ARE PINNED, NOT MOVED. settingsAbsPaths resolves through
// paths.AbsPath, so moving the path root would also move which
// .entire/settings.json governs redaction, scanner-engine selection and the OPF
// command trust gate. A worktree branched before those settings were committed
// — or any worktree at all in a repo enabled with `entire enable --local`,
// whose settings.local.json is gitignored and so can never be in a fresh
// checkout — would silently fall back to DEFAULT redaction, because
// settings.Load returns defaults for a missing file rather than refusing. The
// admission gate (settings.IsSetUpAndEnabled in hook_registry.go) runs before
// dispatch and so cannot catch it. Pinning settings to the pre-move root is
// therefore not tidiness; it is the difference between scanning content with
// the rules the repo chose and scanning it with whatever the defaults are.
//
// Returns ctx unchanged when the event names no directory, when the directory
// is not part of this repository, or when either root cannot be resolved. Every
// such case leaves today's behaviour in place, which is the correct failure
// direction for a hook: capture something against the process working
// directory rather than nothing.
//
// THE OVERRIDE MUST NOT REACH TRANSCRIPT RESOLUTION, and two properties keep it
// away. Capture reads transcripts from the payload (event.SessionRef) or from
// the stored path — strategy's resolveTranscriptPath re-resolves against
// filepath.Dir(state.TranscriptPath), never a repo root — while it is restore
// and attach that derive them from one, via GetSessionDir. That matters because
// GetSessionDir slugs the repo path into ~/.claude/projects/<slug>: a session
// that started in the main checkout keeps its transcript under the main slug
// even after Claude enters a worktree, so resolving it against the worktree
// would silently name a directory that does not exist.
//
// The second property is where this call sits. Agents that compute a missing
// transcript path do it while PARSING the hook input, before dispatch — see
// cursor's resolveTranscriptRef, which calls paths.WorktreeRoot and
// GetSessionDir from parseTurnStart/parseTurnEnd/parseSessionEnd. Moving this
// override earlier (into the hook registry, say) would put those resolutions
// under it and break exactly as described above. Placement after parsing is
// load-bearing, not incidental.
func withEventWorktree(ctx context.Context, event *agent.Event) context.Context {
	if event == nil || event.CWD == "" || !isCapturePathEvent(event.Type) {
		return ctx
	}

	logCtx := logging.WithComponent(ctx, "lifecycle")

	sessionRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// No root to pin settings to, so no safe way to move the path root.
		logging.Debug(logCtx, "event worktree: process worktree root unresolved, staying put",
			slog.String("event_cwd", event.CWD),
			slog.String("error", err.Error()))
		return ctx
	}

	eventRoot, ok := resolveEventWorktreeRoot(ctx, event.CWD, sessionRoot)
	if !ok {
		return ctx
	}
	if eventRoot == filepath.Clean(sessionRoot) {
		// Already there; setting the override would be a no-op that only makes
		// the resolution harder to reason about.
		return ctx
	}

	logging.Debug(logCtx, "event worktree: following the agent into a linked worktree",
		slog.String("from", sessionRoot),
		slog.String("to", eventRoot))

	// Order is not load-bearing (the two keys are independent) but both are
	// required: paths follows the turn, settings stay with the session.
	ctx = settings.WithWorktreeRoot(ctx, sessionRoot)
	return paths.WithWorktreeRoot(ctx, eventRoot)
}

// resolveEventWorktreeRoot accepts the event's working directory as a worktree
// root, and refuses it unless it belongs to the same repository as sessionRoot.
//
// The gate is the git COMMON dir, not the worktree root or the git dir: linked
// worktrees of one repository share a common dir and separate repositories never
// do, which is exactly the boundary worth allowing across. It matters because
// event.CWD arrives in a hook payload — an agent that was told to work somewhere
// else reports that honestly — and the root it produces decides where a
// transcript is written and which repository is inspected.
//
// Both sides go through gitrepo.ResolveWorktreeMetadata, which reads the
// filesystem and runs no git subprocess, so following a turn into a worktree
// costs nothing per hook. That also means eventCWD must BE a worktree root, not
// merely inside one: ResolveWorktreeMetadata does not discover a root, and
// discovery is deliberately not reintroduced here — walking up for a .git entry
// is the search paths.resolveWorktreeRoot documents removing, and asking git
// would put a second unaudited metadata query on every turn. Claude Code's cwd
// is the worktree root when Claude enters a worktree, which is the case this
// exists for; an agent that then cds into a subdirectory is simply not followed
// and captures exactly as it does today.
func resolveEventWorktreeRoot(ctx context.Context, eventCWD, sessionRoot string) (string, bool) {
	logCtx := logging.WithComponent(ctx, "lifecycle")

	eventMeta, err := gitrepo.ResolveWorktreeMetadata(eventCWD)
	if err != nil {
		logging.Debug(logCtx, "event worktree: cwd is not a worktree root, staying put",
			slog.String("event_cwd", eventCWD),
			slog.String("error", err.Error()))
		return "", false
	}
	sessionMeta, err := gitrepo.ResolveWorktreeMetadata(sessionRoot)
	if err != nil {
		logging.Debug(logCtx, "event worktree: session root metadata unresolved, staying put",
			slog.String("session_root", sessionRoot),
			slog.String("error", err.Error()))
		return "", false
	}

	if !sameCommonDir(eventMeta.CommonDir, sessionMeta.CommonDir) {
		logging.Warn(logCtx, "event worktree: cwd belongs to a different repository, ignoring it",
			slog.String("event_cwd", eventCWD),
			slog.String("event_common_dir", eventMeta.CommonDir),
			slog.String("session_common_dir", sessionMeta.CommonDir))
		return "", false
	}

	return filepath.Clean(eventCWD), true
}

// sameCommonDir compares two common directories through EvalSymlinks, because
// the two sides reach this function by different routes — one from the event's
// payload, one from the process — and on macOS the same directory is spelled
// /var/... or /private/var/... depending on which. A lexical comparison would
// read one repository as two.
func sameCommonDir(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}
