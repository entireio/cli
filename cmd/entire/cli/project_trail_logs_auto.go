package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

const autoLogCommand = "__stream_trail_session"
const autoLogLifetime = 55 * time.Minute

var spawnAutoLogProcess = execx.SpawnDetached

// Hooks hand off only the exact session ID, never a guessed current session or
// raw transcript. There is no network work on the hook's critical path.
func maybeStreamTrailSession(ctx context.Context, ag agent.Agent, event *agent.Event) {
	if event.Type != agent.TurnStart && event.Type != agent.TurnEnd && event.Type != agent.ToolUse {
		return
	}
	startAutoLogSession(ctx, ag, event.SessionID, false)
}

// Creating/linking work from an agent can associate a session mid-turn. Never
// use the resolver's worktree fallback to start sharing someone else's session.
func maybeStreamCallingTrailSession(cmd *cobra.Command, branch string) {
	if trailRepoFlag(cmd) != "" || branch == "" {
		return
	}
	current, err := GetCurrentBranch(cmd.Context())
	if err != nil || current != branch {
		return
	}
	caller := strategy.ResolveCallerSession(cmd.Context())
	if !caller.Tracked || !caller.Resolution.IsCaller() {
		return
	}
	ag, err := agent.GetByAgentType(caller.AgentType)
	if err != nil {
		return
	}
	startAutoLogSession(cmd.Context(), ag, caller.SessionID, true)
}

func startAutoLogSession(ctx context.Context, ag agent.Agent, id string, linked bool) {
	if !supportedLocalTranscriptAgent(string(ag.Name())) || id == "" {
		return
	}
	if !settings.IsSetUpAndEnabled(ctx) {
		return
	}
	// This is local-only: do not spawn workers for unconfigured repositories
	// or logged-out sessions (including isolated hook-test subprocesses).
	scope, err := currentTrailEnablementScope(ctx)
	if err != nil || !scope.Supported || scope.AuthKey == "" {
		return
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}
	state, err := strategy.LoadSessionState(ctx, id)
	if err != nil || !autoLogOwnsState(state, root, ag) || state.IsEnded() {
		return
	}
	common, name, err := autoLogFiles(ctx, id)
	if err != nil {
		return
	}
	release, err := tryAutoLogLock(ctx, common, name)
	if err != nil {
		return
	} // Already publishing (or cannot prove exclusive ownership).
	release()
	if cursor, exists, err := loadAutoLogCursor(common, name); err != nil || (!linked && exists && time.Now().Before(cursor.RetryAfter)) {
		return
	}
	// The child takes the same lock: concurrent hooks can race to spawn, but
	// never to upload. SpawnDetached is a no-op in in-process tests.
	spawnAutoLogProcess(root, autoLogCommand, id)
}

func autoLogOwnsState(state *session.State, root string, ag agent.Agent) bool {
	return state != nil && state.AgentType == ag.Type() && !state.Kind.IsImported() &&
		state.TranscriptPath != "" && filepath.Clean(state.WorktreePath) == filepath.Clean(root)
}

func newAutoTrailSessionLogsCmd() *cobra.Command {
	return &cobra.Command{Use: autoLogCommand + " <session-id>", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ensureLogger(cmd)
			ctx, cancel := context.WithTimeout(cmd.Context(), autoLogLifetime)
			defer cancel()
			if err := runAutoTrailSessionLogs(ctx, args[0]); err != nil {
				// Do not log native transcript paths, content, or credential-bearing errors.
				logging.Warn(ctx, "automatic Trail log stream stopped", slog.String("session_id", args[0]))
				return err
			}
			return nil
		}}
}

// The lock and cursor live in the git common dir, not checkpoint metadata.
// Only offsets/identity are persisted, never raw or redacted log content.
func autoLogFiles(ctx context.Context, id string) (*os.Root, string, error) {
	if err := validation.ValidateSessionID(id); err != nil {
		return nil, "", fmt.Errorf("stream session ID: %w", err)
	}
	root, err := gitdir.Open(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("stream git directory: %w", err)
	}
	const dir = "entire-live-logs"
	if err := osroot.MkdirAllNoSymlink(root, dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("stream state directory: %w", err)
	}
	return root, fmt.Sprintf("%s/%x", dir, sha256.Sum256([]byte(id))), nil
}

func tryAutoLogLock(ctx context.Context, root *os.Root, name string) (func(), error) {
	lockCtx, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	return flock.AcquireContextIn(lockCtx, root, name+".lock") //nolint:wrapcheck // caller treats lock contention as an existing publisher
}

type autoLogCursor struct {
	Scope      string    `json:"scope"`
	Transcript string    `json:"transcript"`
	Offset     int64     `json:"offset"`
	Stream     string    `json:"stream,omitempty"`
	RetryAfter time.Time `json:"retry_after,omitempty"`
}

func loadAutoLogCursor(root *os.Root, name string) (autoLogCursor, bool, error) {
	data, err := osroot.ReadFileNoFollow(root, name+".json")
	if errors.Is(err, os.ErrNotExist) {
		return autoLogCursor{}, false, nil
	}
	if err != nil {
		return autoLogCursor{}, false, fmt.Errorf("read stream cursor: %w", err)
	}
	var cursor autoLogCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Offset < 0 {
		return cursor, false, errors.New("invalid stream cursor")
	}
	return cursor, true, nil
}

func saveAutoLogCursor(root *os.Root, name string, cursor autoLogCursor) error {
	data, err := json.Marshal(cursor)
	if err != nil {
		return fmt.Errorf("encode stream cursor: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, name+".json", data, 0o600); err != nil {
		return fmt.Errorf("save stream cursor: %w", err)
	}
	return nil
}

type autoLogTarget struct {
	Scope    string
	Path     string
	Branch   string
	Identity trailEnablementScope
}

// Resolve addressed branch work, not a project collection or an editor-backed
// detail. Its parent is authoritative; an absent parent is NOT a repo fallback.
func resolveAutoLogTarget(ctx context.Context) (autoLogTarget, error) {
	scope, err := currentTrailEnablementScope(ctx)
	if err != nil || !scope.Supported || scope.AuthKey == "" {
		return autoLogTarget{}, errors.New("no authenticated Trail scope")
	}
	branch, err := GetCurrentBranch(ctx)
	if err != nil || branch == "" {
		return autoLogTarget{}, errors.New("no attached branch")
	}
	client, repoID, err := newTrailAPIClient(ctx, false, scope.Forge, scope.Owner, scope.Repo)
	if err != nil {
		return autoLogTarget{}, fmt.Errorf("resolve stream repository: %w", err)
	}
	base, err := trailRepoBasePath(scope.Forge, scope.Owner, scope.Repo, repoID)
	if err != nil {
		return autoLogTarget{}, err
	}
	work, err := findTrailByBranchAtPath(ctx, client, base, branch)
	if err != nil {
		return autoLogTarget{}, err
	}
	if work == nil || work.Parent == nil || work.Branch != branch || !looksLikeULID(work.ID) {
		return autoLogTarget{}, errors.New("branch has no linked project change")
	}
	target, err := projectTrailTargetForReference(*work.Parent)
	if err != nil || target == nil || target.TrailID == "" {
		return autoLogTarget{}, errors.New("branch has no valid project Trail")
	}
	// Bind both the destination and the publisher's auth/data-origin identity.
	key, err := json.Marshal([]string{scope.RepoKey, scope.AuthKey, scope.APIBase, branch, work.ID, target.ProjectID, target.path()})
	if err != nil {
		return autoLogTarget{}, fmt.Errorf("encode stream scope: %w", err)
	}
	return autoLogTarget{Scope: fmt.Sprintf("%x", sha256.Sum256(key)), Path: target.path(), Branch: branch, Identity: scope}, nil
}

func runAutoTrailSessionLogs(ctx context.Context, id string) error {
	root, name, err := autoLogFiles(ctx, id)
	if err != nil {
		return err
	}
	release, err := tryAutoLogLock(ctx, root, name)
	if err != nil {
		return nil
	} // Another hook already started this session's publisher.
	defer release()
	if !settings.IsSetUpAndEnabled(ctx) {
		return nil
	}
	worktree, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("stream worktree: %w", err)
	}
	states, err := session.NewStateStore(ctx)
	if err != nil {
		return fmt.Errorf("stream session store: %w", err)
	}
	state, err := states.Load(ctx, id)
	if err != nil || state == nil {
		return errors.New("stream session is unavailable")
	}
	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil || !supportedLocalTranscriptAgent(string(ag.Name())) {
		return errors.New("unsupported stream agent")
	}
	if !autoLogOwnsState(state, worktree, ag) || state.IsEnded() || state.OwnerExited() {
		return nil
	}
	store, err := agent.OpenSessionStore(ag, worktree)
	if err != nil {
		return fmt.Errorf("stream transcript store: %w", err)
	}
	transcript, err := store.Name(state.TranscriptPath)
	if err != nil {
		return fmt.Errorf("stream transcript outside agent store: %w", err)
	}
	file, err := store.OpenRead(transcript)
	if err != nil {
		return fmt.Errorf("stream transcript: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stream transcript stat: %w", err)
	}
	cursor, exists, err := loadAutoLogCursor(root, name)
	if err != nil {
		return err
	}
	lookupCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	target, targetErr := resolveAutoLogTarget(lookupCtx)
	stop()
	offset := autoLogStartOffset(cursor, exists, target.Scope, state.TranscriptPath, info.Size())
	cursor = autoLogCursor{Scope: target.Scope, Transcript: state.TranscriptPath, Offset: offset, Stream: ulid.Make().String(), RetryAfter: time.Now().Add(30 * time.Second)}
	// Remember the boundary even when unlinked, so a later link cannot replay
	// private work done before association. A changed association starts at EOF.
	if err := saveAutoLogCursor(root, name, cursor); err != nil {
		return err
	}
	if targetErr != nil {
		return targetErr
	}
	if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
		return fmt.Errorf("configure stream redaction: %w", err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	check := func() (bool, error) {
		current, err := states.Load(streamCtx, id)
		if err != nil || !autoLogOwnsState(current, worktree, ag) || current.TranscriptPath != state.TranscriptPath {
			return false, errors.New("stream session identity changed")
		}
		if !settings.IsSetUpAndEnabled(streamCtx) {
			return false, errors.New("session capture disabled")
		}
		branch, err := GetCurrentBranch(streamCtx)
		if err != nil || branch != target.Branch {
			return false, errors.New("stream branch changed")
		}
		identity, err := currentTrailEnablementScope(streamCtx)
		if err != nil || identity != target.Identity {
			return false, errors.New("stream repository or login changed")
		}
		return current.IsEnded() || current.OwnerExited(), nil
	}
	readerCtx, stopInput := context.WithCancel(streamCtx)
	defer stopInput()
	reader := &autoLogReader{ctx: readerCtx, file: file, offset: offset, initial: info, skipPartial: autoLogPartialLine(file, offset),
		stat: func() (os.FileInfo, error) { return store.Lstat(transcript) }, check: check,
		save: func(pos int64) error { cursor.Offset = pos; return saveAutoLogCursor(root, name, cursor) }, interval: time.Second}
	var input io.Reader = reader
	if offset > 0 {
		input = io.MultiReader(autoLogFormatHeader(string(ag.Name())), reader)
	}
	logging.Info(ctx, "automatically streaming session to Trail audience", slog.String("session_id", id), slog.String("stream_id", cursor.Stream), slog.String("trail_path", target.Path))
	send := func(sendCtx context.Context, records []localLogRecord) error {
		if _, err := check(); err != nil {
			cancel()
			return err
		}
		// Recheck the parent and local identity on EVERY request, retries included.
		// Same-branch unlink/reparent and login switches must not reuse admission.
		client := func(c context.Context) (*api.Client, error) {
			if _, err := check(); err != nil {
				cancel()
				return nil, err
			}
			current, err := resolveAutoLogTarget(c)
			if err != nil || current.Scope != target.Scope {
				cancel()
				return nil, errors.New("stream association could not be revalidated")
			}
			return newProjectLogClient(c, false)
		}
		path := target.Path + "/logs/" + cursor.Stream
		if offset > 0 {
			if records[0].Sequence == 1 {
				gap := []localLogRecord{{Sequence: 1, Kind: localLogKindGap, Text: "Automatic capture resumed here; earlier transcript records are not replayed."}}
				if err := appendLocalLogBatch(sendCtx, path, cursor.Stream, gap, client); err != nil {
					cancel()
					return err
				}
			}
			records = append([]localLogRecord(nil), records...)
			for i := range records {
				records[i].Sequence++
			}
		}
		if err := appendLocalLogBatch(sendCtx, path, cursor.Stream, records, client); err != nil {
			cancel()
			return err
		}
		return nil
	}
	return publishLocalTranscriptCapture(streamCtx, input, io.Discard, string(ag.Name()), localLogFilters(), send, stopInput)
}

func autoLogStartOffset(previous autoLogCursor, exists bool, scope, transcript string, size int64) int64 {
	if exists {
		if previous.Scope == scope && scope != "" && previous.Transcript == transcript && previous.Offset <= size {
			return previous.Offset
		}
		return size
	}
	if scope == "" || size > localTranscriptMaxBytes {
		return size
	}
	return 0
}

// Explicit format markers select the existing converters on a resumed suffix.
// They carry no invented conversation content or identity/model metadata.
func autoLogFormatHeader(name string) io.Reader {
	switch name {
	case "pi":
		return strings.NewReader("{\"type\":\"session\",\"version\":3}\n")
	case "codex":
		return strings.NewReader("{\"type\":\"session_meta\",\"payload\":{}}\n")
	default:
		return strings.NewReader("")
	}
}
