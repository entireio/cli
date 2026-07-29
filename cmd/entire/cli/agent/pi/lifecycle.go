package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// Hook names — these match Pi's native event names exactly (snake_case),
// because the embedded TypeScript extension forwards `pi.on(<event>)` events
// directly. Keeping the names identical avoids a translation layer in the
// extension.
const (
	HookNameSessionStart     = "session_start"
	HookNameBeforeAgentStart = "before_agent_start"
	HookNameAgentEnd         = "agent_end"
	HookNameSessionShutdown  = "session_shutdown"
)

// HookNames returns the verbs registered as `entire hooks pi <name>`.
func (a *PiAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameBeforeAgentStart,
		HookNameAgentEnd,
		HookNameSessionShutdown,
	}
}

// GetSupportedHooks maps Pi's native events to normalised lifecycle types.
//
//   - session_start       → SessionStart
//   - before_agent_start  → TurnStart
//   - agent_end           → TurnEnd
//   - session_shutdown    → (cleanup-only, no lifecycle event — see ParseHookEvent)
func (a *PiAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookUserPromptSubmit,
		agent.HookStop,
	}
}

// Compile-time assertion that Pi can inject context into the model.
var _ agent.ContextInjector = (*PiAgent)(nil)

// InjectionEvent reports that Pi injects model context at TurnStart
// (before_agent_start) — the only Pi event where the embedded extension can
// return a message that Pi stores in the session and sends to the LLM.
func (a *PiAgent) InjectionEvent() agent.EventType { return agent.TurnStart }

// RenderContextInjection emits a {"inject_context":"..."} envelope on stdout.
// The embedded extension (entire_extension.ts) reads it from the
// before_agent_start hook's stdout and returns it to Pi as a hidden persistent
// message. An empty Text renders nothing.
func (a *PiAgent) RenderContextInjection(inj agent.ContextInjection) ([]byte, error) {
	if strings.TrimSpace(inj.Text) == "" {
		return nil, nil
	}
	b, err := json.Marshal(struct {
		InjectContext string `json:"inject_context"`
	}{InjectContext: inj.Text})
	if err != nil {
		return nil, fmt.Errorf("marshal pi context injection: %w", err)
	}
	return append(b, '\n'), nil
}

// piHookPayload is the JSON the embedded TypeScript extension pipes to
// `entire hooks pi <event>` on stdin.
type piHookPayload struct {
	Type        string              `json:"type"`
	Cwd         string              `json:"cwd,omitempty"`
	SessionFile string              `json:"session_file,omitempty"`
	SessionID   string              `json:"session_id,omitempty"`
	Prompt      string              `json:"prompt,omitempty"`
	SkillEvents []piSkillEventInput `json:"skill_events,omitempty"`
}

type piSkillEventInput struct {
	SkillName  string `json:"skill_name"`
	Invocation string `json:"invocation"`
	Timestamp  string `json:"timestamp,omitempty"`
}

// piSkillEvents converts the Pi extension's live skill-invocation reports into
// agent.SkillEvents. This is Pi's only skill-capture path. PiAgent intentionally
// does NOT implement agent.SkillEventExtractor: a transcript extractor would
// double-count these live events at condensation (see
// TestPiAgent_UsesLiveSkillCaptureNotTranscriptExtraction).
func piSkillEvents(in []piSkillEventInput) []agent.SkillEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]agent.SkillEvent, 0, len(in))
	for i, ev := range in {
		skillName := strings.TrimSpace(ev.SkillName)
		if skillName == "" {
			continue
		}
		invocation := strings.TrimSpace(ev.Invocation)
		if invocation == "" {
			invocation = "/skill:" + skillName
		}
		id := ""
		if ev.Timestamp != "" {
			id = fmt.Sprintf("pi-skill-%s-%s-%d", skillName, ev.Timestamp, i)
		}
		out = append(out, agent.SkillEvent{
			ID:        id,
			EventType: agent.SkillEventTypePromptInvocation,
			Skill: agent.SkillEventSkill{
				Name: skillName,
			},
			Source: agent.SkillEventSource{
				Agent:      string(agent.AgentNamePi),
				Signal:     agent.SkillSignalPiInputSlashCommand,
				Confidence: agent.SkillConfidenceExplicit,
			},
			Timestamp: ev.Timestamp,
			Native: map[string]string{
				"command": invocation,
			},
			Collapse: agent.SkillEventCollapse{
				Target:           agent.SkillCollapseTargetUserMessage,
				Label:            invocation,
				DefaultCollapsed: true,
			},
		})
	}
	return out
}

// ParseHookEvent translates a Pi hook invocation into a normalised lifecycle
// event. Implements agent.HookSupport.
func (a *PiAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	// Stream one JSON value rather than io.ReadAll so the hook never blocks
	// waiting for stdin EOF that some agents don't send on Windows (issue #1398).
	parsed, err := agent.ReadAndParseHookInput[piHookPayload](stdin)
	if err != nil {
		return nil, err
	}
	payload := *parsed

	identity := parsePiSessionIdentity(payload.SessionFile)
	sessionID := payload.SessionID
	if sessionID == "" {
		sessionID = identity.SessionID
	}
	// subagentID is non-empty only for Pi subagent runs, whose transcript path
	// carries <parent>/<sub>/run-N identity that filepath.Base would otherwise
	// collapse to the role-named "session" leaf (issue #1870).
	subagentID := identity.SubagentID

	now := time.Now()

	switch hookName {
	case HookNameSessionStart:
		cacheSessionID(ctx, sessionID)
		return &agent.Event{
			Type:      agent.SessionStart,
			SessionID: sessionID,
			Timestamp: now,
		}, nil

	case HookNameBeforeAgentStart:
		if subagentID != "" {
			// A Pi subagent run is starting. Route it through the subagent
			// mechanism (issue #1870) so pre-task state is captured and the run
			// is attributed to the parent session rather than a phantom
			// "session"-named session. No context injection: that is for the
			// parent turn only.
			return &agent.Event{
				Type:       agent.SubagentStart,
				SessionID:  sessionID,
				SubagentID: subagentID,
				ToolUseID:  subagentID,
				// SessionRef is the parent conversation on both Pi subagent
				// events (Start/End). The Start handler only logs it and
				// snapshots pre-task git state, so the value is informational
				// here; keeping it identical to End avoids a confusing mismatch.
				SessionRef: derivePiParentTranscript(payload.SessionFile),
				Timestamp:  now,
			}, nil
		}
		// Pi emits before_agent_start with a fully-populated session ID, but
		// we cache it anyway to support the agent_end fallback below.
		if sessionID == "" {
			sessionID = readCachedSessionID(ctx)
		} else {
			cacheSessionID(ctx, sessionID)
		}
		// Provide the live Pi session file as SessionRef so state.TranscriptPath
		// is populated before any mid-turn commits. Without this, the
		// post-commit hook cannot condense when no shadow branch exists yet.
		return &agent.Event{
			Type:        agent.TurnStart,
			SessionID:   sessionID,
			SessionRef:  payload.SessionFile,
			Prompt:      payload.Prompt,
			Timestamp:   now,
			SkillEvents: piSkillEvents(payload.SkillEvents),
		}, nil

	case HookNameAgentEnd:
		if sessionID == "" {
			sessionID = readCachedSessionID(ctx)
		}
		if subagentID != "" {
			// A Pi subagent run ended. Stage its transcript under the run's own
			// identity (never the shared "session.json"), attach it to the
			// parent session as a subagent transcript, and route as SubagentEnd
			// so distinct runs no longer overwrite one another (issue #1870).
			subagentRef := captureTranscript(ctx, subagentID, payload.SessionFile)
			return &agent.Event{
				Type:       agent.SubagentEnd,
				SessionID:  sessionID,
				SubagentID: subagentID,
				ToolUseID:  subagentID,
				// SessionRef points at the raw parent transcript, not the staged
				// .entire/tmp/pi/<parentUUID>.json copy: the parent's agent_end
				// (which stages that copy) fires last, after every subagent run,
				// so the staged copy does not exist yet here. The raw path still
				// exists mid-session and is read best-effort at checkpoint-write
				// time, so the parent content lands in the shadow tree then.
				SessionRef:             derivePiParentTranscript(payload.SessionFile),
				SubagentTranscriptPath: subagentRef,
				Model:                  extractModelFromPiSessionFile(subagentRef),
				Timestamp:              now,
			}, nil
		}
		// Capture the Pi JSONL into <repo>/.entire/tmp/pi/<id>.json so the
		// strategy has a stable transcript reference even if the user later
		// deletes Pi sessions. The pi/ subdir avoids colliding with paths
		// other agents (or test harnesses) stage under .entire/tmp/.
		sessionRef := captureTranscript(ctx, sessionID, payload.SessionFile)
		return &agent.Event{
			Type:       agent.TurnEnd,
			SessionID:  sessionID,
			SessionRef: sessionRef,
			Model:      extractModelFromPiSessionFile(sessionRef),
			Timestamp:  now,
		}, nil

	case HookNameSessionShutdown:
		// Cleanup-only: clear the cached session ID. We intentionally do NOT
		// emit SessionEnd here.
		//
		// Pi fires session_shutdown and agent_end on session teardown, and the
		// TypeScript extension dispatches both via separate `entire hooks pi …`
		// child processes (execFile is non-blocking). Child-process startup
		// ordering then decides which event reaches the lifecycle dispatcher
		// first; if session_shutdown wins, an emitted SessionEnd transitions
		// the session to "ended" before agent_end can save the linkable
		// checkpoint, leaving prepare-commit-msg with no session to attach a
		// trailer to and the user's commit unlinked.
		//
		// agent_end is the source of truth for "turn complete" (and, for Pi,
		// effectively "session over" for any single-turn `pi -p` invocation).
		// SessionEnd is left for the framework to derive from idle timeout or
		// the next SessionStart's stale-state cleanup.
		clearCachedSessionID(ctx)
		return nil, nil //nolint:nilnil // intentional: cleanup-only, no lifecycle event

	default:
		// Unknown / future hooks have no lifecycle significance.
		return nil, nil //nolint:nilnil // unknown hook = no lifecycle event (acceptable)
	}
}

// --- session ID cache ---
//
// Pi's `before_agent_start` event sometimes fires before `session_start` has
// completed cacheing the session ID (race during early extension load), and
// `agent_end` may fire after Pi has torn down its session manager. We cache
// the active session ID at session_start time so subsequent hooks can recover
// it.

const activeSessionFile = "pi-active-session"

// piHookCacheSubdir is the subdirectory under .entire/tmp/ where hook
// flow caches the active-session ID file and the agent_end transcript
// snapshot. Agent-specific (not just .entire/tmp/) so other agents'
// integration tests and tooling don't shadow each other under the cache
// root.
const piHookCacheSubdir = "pi"

// resolveSessionDir returns the per-repo hook cache directory used by
// cacheSessionID / readCachedSessionID / clearCachedSessionID and
// captureTranscript.
//
// This is intentionally distinct from PiAgent.GetSessionDir, which
// points at Pi's native session store (~/.pi/agent/sessions/...) so
// cold attach can resolve transcripts that were never hook-captured.
// The cache here is hook-internal and only reachable via Pi hooks
// firing; the framework records the cached path as SessionRef in
// checkpoint metadata, so subsequent operations on hooked sessions go
// through the recorded path rather than re-resolving via GetSessionDir.
func resolveSessionDir(ctx context.Context) string {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		//nolint:forbidigo // fallback when no git repo (tests run outside repos)
		wd, wdErr := os.Getwd()
		if wdErr != nil {
			return filepath.Join(paths.EntireTmpDir, piHookCacheSubdir)
		}
		root = wd
	}
	return filepath.Join(root, paths.EntireTmpDir, piHookCacheSubdir)
}

func cacheSessionID(ctx context.Context, id string) {
	if id == "" {
		return
	}
	dir := resolveSessionDir(ctx)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		logging.Debug(ctx, "pi: cache session id mkdir", slog.String("err", err.Error()))
		return
	}

	if err := os.WriteFile(filepath.Join(dir, activeSessionFile), []byte(id), 0o600); err != nil {
		logging.Debug(ctx, "pi: cache session id write", slog.String("err", err.Error()))
	}
}

func extractModelFromPiSessionFile(path string) string {
	if path == "" {
		return ""
	}
	//nolint:gosec // path comes from Pi's hook payload or our captured transcript path
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	model, err := (&PiAgent{}).ExtractModel(data)
	if err != nil {
		return ""
	}
	return model
}

func readCachedSessionID(ctx context.Context) string {
	dir := resolveSessionDir(ctx)
	//nolint:gosec // path constructed from validated repo root
	data, err := os.ReadFile(filepath.Join(dir, activeSessionFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func clearCachedSessionID(ctx context.Context) {
	dir := resolveSessionDir(ctx)
	_ = os.Remove(filepath.Join(dir, activeSessionFile))
}

// captureTranscript copies the Pi JSONL session file to
// <repo>/.entire/tmp/pi/<id>.json so Entire has a stable transcript
// reference. Returns the path to the cached file, or "" if either input is
// missing. The pi/ namespace under .entire/tmp/ is intentional — see
// GetSessionDir / piHookCacheSubdir for the rationale.
func captureTranscript(ctx context.Context, sessionID, piSessionFile string) string {
	if sessionID == "" || piSessionFile == "" {
		return ""
	}
	// sessionID comes from the hook payload (or the locally cached active
	// session) and is used to build dst below, before the lifecycle dispatcher
	// validates it. Validate here at the choke point so an unsafe ID cannot
	// write the transcript outside the cache directory; "" signals no capture.
	if err := validation.ValidateSessionID(sessionID); err != nil {
		logging.Warn(ctx, "pi: refusing to capture transcript for unsafe session ID",
			slog.String("session_id", sessionID), slog.String("err", err.Error()))
		return ""
	}
	dir := resolveSessionDir(ctx)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		logging.Warn(ctx, "pi: capture transcript mkdir failed",
			slog.String("dir", dir), slog.String("err", err.Error()))
		return ""
	}
	dst := filepath.Join(dir, sessionID+".json")
	//nolint:gosec // G703: piSessionFile from trusted Pi extension stdin payload
	data, err := os.ReadFile(piSessionFile)
	if err != nil {
		logging.Warn(ctx, "pi: capture transcript read failed",
			slog.String("src", piSessionFile), slog.String("err", err.Error()))
		return ""
	}
	//nolint:gosec // G703: dst is sessionID (validated above) under .entire/tmp/pi
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		logging.Warn(ctx, "pi: capture transcript write failed",
			slog.String("dst", dst), slog.String("err", err.Error()))
		return ""
	}
	return dst
}

// piSubagentLeaf is the fixed basename Pi gives every subagent run transcript.
// The run's identity lives in the surrounding directory segments, not this
// leaf — the bug in issue #1870 was collapsing the path to this literal.
const piSubagentLeaf = "session.jsonl"

// piSessionIdentity is the identity resolved from a Pi transcript path. For a
// parent session, SessionID is the session UUID and SubagentID is empty. For a
// subagent run, SessionID rolls up to the parent UUID and SubagentID names the
// run (<sub>-run-N) so runs no longer collide on the role-named "session" leaf.
type piSessionIdentity struct {
	SessionID  string
	SubagentID string
}

// parsePiSessionIdentity resolves a Pi session_file path into its identity.
// Subagent runs live at <...>/<timestamp>_<uuid>/<sub>/run-N/session.jsonl;
// parents at <...>/<timestamp>_<uuid>.jsonl.
func parsePiSessionIdentity(p string) piSessionIdentity {
	if p == "" {
		return piSessionIdentity{}
	}
	if filepath.Base(p) == piSubagentLeaf {
		if idty, ok := parsePiSubagentPath(p); ok {
			return idty
		}
	}
	return piSessionIdentity{SessionID: parsePiParentID(filepath.Base(p))}
}

// parsePiParentID extracts the UUID from a parent transcript basename
// (<timestamp>_<uuid>.jsonl → <uuid>), falling back to the basename without
// extension when there is no underscore to split on.
func parsePiParentID(base string) string {
	base = strings.TrimSuffix(base, ".jsonl")
	if i := strings.LastIndex(base, "_"); i >= 0 {
		return base[i+1:]
	}
	return base
}

// parsePiSubagentPath resolves <...>/<timestamp>_<uuid>/<sub>/run-N/session.jsonl
// into the parent UUID and a distinct, path-safe run identity. ok is false when
// the surrounding segments don't match that shape or yield unsafe identifiers,
// so the caller falls back to parent-style parsing.
func parsePiSubagentPath(p string) (piSessionIdentity, bool) {
	runDir := filepath.Dir(p)                        // <...>/<seg>/<sub>/run-N
	subDir := filepath.Dir(runDir)                   // <...>/<seg>/<sub>
	parentSeg := filepath.Base(filepath.Dir(subDir)) // <timestamp>_<uuid>
	run := filepath.Base(runDir)
	sub := filepath.Base(subDir)
	if run == "." || sub == "." || parentSeg == "." || parentSeg == string(filepath.Separator) {
		return piSessionIdentity{}, false
	}
	parentID := parsePiParentID(parentSeg)
	if parentID == "" || parentID == parentSeg {
		// No <timestamp>_<uuid> underscore split — not the expected shape.
		return piSessionIdentity{}, false
	}
	subagentID := sub + "-" + run
	// subagentID feeds two consumers with different rules: the dispatcher
	// (ValidateAgentID) and captureTranscript (the stricter ValidateSessionID,
	// which also rejects a leading dash). Require both so an exotic <sub> dir
	// can't parse-pass here and then silently fail to stage its transcript.
	if validation.ValidateSessionID(parentID) != nil ||
		validation.ValidateAgentID(subagentID) != nil ||
		validation.ValidateSessionID(subagentID) != nil {
		return piSessionIdentity{}, false
	}
	return piSessionIdentity{SessionID: parentID, SubagentID: subagentID}, true
}

// extractSessionIDFromPath returns the Pi session identity's session ID: the
// parent UUID for both parent and subagent transcript paths.
func extractSessionIDFromPath(p string) string {
	return parsePiSessionIdentity(p).SessionID
}

// derivePiParentTranscript maps a subagent run path
// <...>/<seg>/<sub>/run-N/session.jsonl to the parent transcript <...>/<seg>.jsonl,
// so a subagent checkpoint can reference the parent conversation. Returns "" if
// the path is not a subagent run.
func derivePiParentTranscript(p string) string {
	if p == "" || filepath.Base(p) != piSubagentLeaf {
		return ""
	}
	parentSegDir := filepath.Dir(filepath.Dir(filepath.Dir(p))) // <...>/<seg>
	if parentSegDir == "." || parentSegDir == string(filepath.Separator) {
		return ""
	}
	return parentSegDir + ".jsonl"
}
