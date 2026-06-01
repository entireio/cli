package pi

import (
	"context"
	"encoding/json"
	"errors"
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
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read pi hook input: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("empty pi hook input")
	}

	var payload piHookPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse pi hook payload: %w", err)
	}

	sessionID := payload.SessionID
	if sessionID == "" {
		sessionID = extractSessionIDFromPath(payload.SessionFile)
	}

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
		// Capture the Pi JSONL into <repo>/.entire/tmp/pi/<id>.json so the
		// strategy has a stable transcript reference even if the user later
		// deletes Pi sessions. The pi/ subdir avoids colliding with paths
		// other agents (or test harnesses) stage under .entire/tmp/.
		sessionRef := captureTranscript(ctx, sessionID, payload.SessionFile)
		return &agent.Event{
			Type:       agent.TurnEnd,
			SessionID:  sessionID,
			SessionRef: sessionRef,
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
	//nolint:gosec // G703: dst constructed from validated session ID inside .entire/tmp
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		logging.Warn(ctx, "pi: capture transcript write failed",
			slog.String("dst", dst), slog.String("err", err.Error()))
		return ""
	}
	return dst
}

// extractSessionIDFromPath extracts the UUID from a Pi session filename.
// Pattern: <timestamp>_<uuid>.jsonl → returns <uuid>
// Falls back to the basename without extension if the pattern doesn't match.
func extractSessionIDFromPath(p string) string {
	if p == "" {
		return ""
	}
	base := filepath.Base(p)
	base = strings.TrimSuffix(base, ".jsonl")
	if i := strings.LastIndex(base, "_"); i >= 0 {
		return base[i+1:]
	}
	return base
}
