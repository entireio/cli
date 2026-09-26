package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// agy's title/statusline hook pipes a state JSON to the configured command on
// every agent state change. The context_window object is the ONLY surface
// where agy exposes token usage — it never appears in transcripts or
// lifecycle hook payloads. AppendStatusSnapshot persists those snapshots so
// the lifecycle can compute per-checkpoint deltas later.
//
// Totals are cumulative per conversation; current_usage is the latest API call.

// statusDirEnv overrides the snapshot cache directory (tests, ops).
const statusDirEnv = "ENTIRE_ANTIGRAVITY_STATUS_DIR"

// statusRetention is how long snapshot files for other conversations are kept.
const statusRetention = 14 * 24 * time.Hour

// statusLockSuffix is appended to a conversation's snapshot file name to form
// the advisory lock file AppendStatusSnapshot serialises on.
const statusLockSuffix = ".lock"

// statusCurrentUsage mirrors context_window.current_usage in the payload.
type statusCurrentUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// statusContextWindow mirrors context_window in the payload.
type statusContextWindow struct {
	TotalInputTokens  int                 `json:"total_input_tokens"`
	TotalOutputTokens int                 `json:"total_output_tokens"`
	ContextWindowSize int                 `json:"context_window_size,omitempty"`
	CurrentUsage      *statusCurrentUsage `json:"current_usage,omitempty"`
}

// statusSnapshot is one persisted line in <conversation_id>.jsonl.
type statusSnapshot struct {
	Timestamp      string              `json:"ts"`
	ConversationID string              `json:"conversation_id"`
	ContextWindow  statusContextWindow `json:"context_window"`
}

// statuslinePayload is the subset of agy's state JSON we consume.
type statuslinePayload struct {
	ConversationID string               `json:"conversation_id"`
	ContextWindow  *statusContextWindow `json:"context_window"`
}

// statusStore is where snapshot files live: a shared *os.Root anchor plus the
// directory name inside it (docs/development/filesystem-safety.md, "The Root
// Anchors"). Every read, append, lock and prune below is a NAME inside this
// root, so a symlink planted at any component is refused instead of followed.
type statusStore struct {
	root *os.Root
	// dir is the directory inside root holding the JSONL files. "" means the
	// root itself is the directory (the override case).
	dir string
}

// statusDefaultDir is the store's location inside the per-user cache root.
// Slash-separated on every platform: names inside an os.Root are slash paths
// by contract, and osroot splits them on "/" only — a filepath.Join here made
// "antigravity\status" a single component on Windows, so the store was never
// created and no snapshot was ever persisted there (trail 444).
const statusDefaultDir = "antigravity/status"

// openStatusStore resolves the store. It honours the ENTIRE_ANTIGRAVITY_STATUS_DIR
// env override (tests, ops), otherwise anchors on userdirs.CacheRoot. userdirs is
// the mandated resolver: it honours $XDG_CACHE_HOME on every platform
// (os.UserCacheDir ignores it on darwin, defeating harness isolation), falls back
// to a throwaway per-process dir under `go test`, and refuses a relative
// override before anything is created. The override is held to the same rule
// (RequireAbsoluteOverride) and opened through the shared registry like every
// other anchor, never as filepath.Dir of the file about to be written.
func openStatusStore() (statusStore, error) {
	if override := os.Getenv(statusDirEnv); override != "" {
		if err := userdirs.RequireAbsoluteOverride(statusDirEnv, override); err != nil {
			return statusStore{}, fmt.Errorf("antigravity status: %w", err)
		}
		if err := userdirs.EnsurePrivateDir(override); err != nil {
			return statusStore{}, fmt.Errorf("antigravity status: %w", err)
		}
		root, err := osroot.Shared(override)
		if err != nil {
			return statusStore{}, fmt.Errorf("antigravity status: open %s: %w", statusDirEnv, err)
		}
		return statusStore{root: root}, nil
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		return statusStore{}, fmt.Errorf("antigravity status: resolve cache dir: %w", err)
	}
	return statusStore{root: root, dir: statusDefaultDir}, nil
}

// dirName is the store directory as a name inside the root ("." for the root).
func (st statusStore) dirName() string {
	if st.dir == "" {
		return "."
	}
	return st.dir
}

// fileName returns the slash-separated name inside the root of a
// conversation's JSONL file. filepath.Base guards against path traversal in
// the conversation ID (it strips either separator on Windows).
func (st statusStore) fileName(conversationID string) string {
	return path.Join(st.dir, filepath.Base(conversationID)+".jsonl")
}

// statusFilePath returns the absolute path of a conversation's snapshot file.
// Diagnostics and tests only: production I/O goes through the root by name.
func statusFilePath(conversationID string) (string, error) {
	st, err := openStatusStore()
	if err != nil {
		return "", err
	}
	return filepath.Join(st.root.Name(), filepath.FromSlash(st.fileName(conversationID))), nil
}

// AppendStatusSnapshot parses an agy state-JSON payload and appends a snapshot
// to the per-conversation JSONL file. The hot path never returns an error for
// malformed input — only for genuine I/O failures.
//
// agy fires the title command on every agent state change and does not
// serialize the invocations, so two tees can run at once. The dedup read and
// the append therefore happen under one advisory lock per conversation
// (<id>.jsonl.lock, next to the file); without it a concurrent tee could append
// between the read and the write and the dedup would miss.
func AppendStatusSnapshot(payload []byte) error {
	var p statuslinePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	if p.ConversationID == "" || p.ContextWindow == nil {
		return nil // missing required fields — silently skip
	}

	// Dedup: compare compact JSON of the new context_window against the last
	// persisted line's context_window.
	newCWBytes, err := json.Marshal(p.ContextWindow)
	if err != nil {
		return nil
	}

	st, err := openStatusStore()
	if err != nil {
		return err
	}
	if st.dir != "" {
		if err := osroot.MkdirAllNoSymlink(st.root, st.dir, 0o750); err != nil {
			return fmt.Errorf("antigravity status: mkdir: %w", err)
		}
	}
	name := st.fileName(p.ConversationID)

	release, err := lockStatusFile(st, name)
	if err != nil {
		return err
	}
	defer release()

	f, err := osroot.OpenFileNoFollow(st.root, name, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("antigravity status: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("antigravity status: stat: %w", err)
	}
	isNew := info.Size() == 0
	if !isNew {
		lastSnap, readErr := readLastSnapshotFrom(f)
		if readErr == nil && lastSnap != nil {
			lastCWBytes, marshalErr := json.Marshal(lastSnap.ContextWindow)
			if marshalErr == nil && bytes.Equal(newCWBytes, lastCWBytes) {
				return nil // duplicate — skip
			}
		}
	}

	snap := statusSnapshot{
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
		ConversationID: p.ConversationID,
		ContextWindow:  *p.ContextWindow,
	}
	line, err := json.Marshal(snap)
	if err != nil {
		return nil
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("antigravity status: write: %w", err)
	}

	// Best-effort prune of stale files for other conversations when we first
	// create the active file (avoids per-append overhead).
	if isNew {
		pruneStaleStatusFiles(st, p.ConversationID)
	}

	return nil
}

// SnapshotTokenBaseline returns the latest persisted snapshot for the
// conversation, or nil if none exists yet. A nil baseline is exact only for a
// genuinely fresh conversation; a resumed conversation whose title-tee shim
// hasn't written a snapshot before the first TurnStart will over-count the
// prior cumulative total on that first tracked turn.
func (a *AntigravityAgent) SnapshotTokenBaseline(ctx context.Context, sessionID string) (json.RawMessage, error) {
	st, err := openStatusStore()
	if err != nil {
		return nil, nil //nolint:nilerr // ditto: an unusable status dir means no baseline
	}
	snap, err := readLastStatusSnapshot(ctx, st, st.fileName(sessionID))
	if err != nil || snap == nil {
		return nil, nil //nolint:nilerr // ditto (missing file, no lines, malformed)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, nil //nolint:nilerr // ditto
	}
	return raw, nil
}

// CalculateTokenUsageSince computes the delta between the baseline snapshot
// and the latest persisted snapshot.
//
// Exact: InputTokens/OutputTokens (cumulative totals minus baseline totals).
// Best-effort: cache fields and APICallCount, derived from the snapshot lines
// appended after the baseline timestamp (the dedup writer appends ~one line
// per API response, but lines can be missed between agent state changes).
func (a *AntigravityAgent) CalculateTokenUsageSince(ctx context.Context, sessionID string, baseline json.RawMessage) (*agent.TokenUsage, error) {
	snaps, err := readStatusSnapshots(ctx, sessionID)
	if err != nil || len(snaps) == 0 {
		return nil, nil //nolint:nilerr,nilnil // no data -> no token counts, never an error
	}

	var base statusSnapshot
	if len(baseline) > 0 {
		_ = json.Unmarshal(baseline, &base) //nolint:errcheck // unparseable baseline -> zero baseline
	}

	latest := snaps[len(snaps)-1]
	usage := &agent.TokenUsage{
		InputTokens:  max(0, latest.ContextWindow.TotalInputTokens-base.ContextWindow.TotalInputTokens),
		OutputTokens: max(0, latest.ContextWindow.TotalOutputTokens-base.ContextWindow.TotalOutputTokens),
	}

	// The strictly-after (.After, not >=) filter is load-bearing for
	// multi-turn correctness: turn N+1's baseline IS turn N's latest snapshot,
	// so excluding the equal-timestamp boundary line prevents re-counting it.
	// Changing this to >= would double-count the boundary line every turn.
	baseTS, baseTSErr := time.Parse(time.RFC3339Nano, base.Timestamp)
	for _, s := range snaps {
		// If baseTS is unparseable we count cache/apicalls over all lines; accepted because input/output remain exact via the totals delta.
		if base.Timestamp != "" && baseTSErr == nil {
			ts, parseErr := time.Parse(time.RFC3339Nano, s.Timestamp)
			if parseErr != nil || !ts.After(baseTS) {
				continue
			}
		}
		usage.APICallCount++
		if cu := s.ContextWindow.CurrentUsage; cu != nil {
			usage.CacheCreationTokens += cu.CacheCreationInputTokens
			usage.CacheReadTokens += cu.CacheReadInputTokens
		}
	}

	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheCreationTokens == 0 && usage.CacheReadTokens == 0 {
		return nil, nil //nolint:nilnil // nothing observed this turn
	}
	return usage, nil
}

// statusTailWindow bounds how many bytes readLastSnapshotFrom reads from the
// end of the file. Snapshot lines are well under 1 KB, so 64 KB always covers
// the final line with huge margin.
const statusTailWindow = 64 * 1024

// snapshotFileExists reports whether a conversation's snapshot file is present
// in the store, without following a symlink at any component. It is checked
// BEFORE a reader takes the conversation lock: flock.AcquireIn creates the lock
// file it opens, so a baseline read at every TurnStart on a conversation that
// never produces a snapshot — or whose snapshot was already pruned — would
// otherwise leave an orphan <id>.jsonl.lock behind for the whole retention
// window (the prune cannot tell such an orphan from a lock a tee has just taken
// while creating its file). The check-then-lock gap is deliberate and cheap: a
// tee creating the file in between costs one turn's baseline, which lands in
// the same "no snapshot yet" degradation these readers already document.
func snapshotFileExists(st statusStore, name string) (bool, error) {
	if _, err := osroot.LstatNoSymlinks(st.root, name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("antigravity status: stat: %w", err)
	}
	return true, nil
}

// readLastStatusSnapshot opens name inside the store and returns its final
// snapshot; a missing file is nil, nil.
func readLastStatusSnapshot(ctx context.Context, st statusStore, name string) (*statusSnapshot, error) {
	if exists, err := snapshotFileExists(st, name); err != nil {
		return nil, err
	} else if !exists {
		return nil, nil //nolint:nilnil // no snapshot yet — and no lock file left behind for it
	}
	release, err := lockStatusFileForRead(ctx, st, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil //nolint:nilnil // no status directory yet means no snapshots yet
		}
		return nil, err
	}
	defer release()
	f, err := osroot.OpenNoFollow(st.root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil //nolint:nilnil // a missing file is "no snapshots yet", not an error
		}
		return nil, fmt.Errorf("antigravity status: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	return readLastSnapshotFrom(f)
}

// lockStatusFile takes the per-conversation advisory lock that
// AppendStatusSnapshot writes under. The readers take it too: agy does not
// serialise its title-command invocations, so a baseline or delta read that
// ran unlocked could observe the last line half-written by a concurrent tee
// and treat the torn JSON as "no snapshot" — a silently dropped token
// baseline for that turn. The lock file is created on first use; a status
// directory that does not exist yet is reported through fs.ErrNotExist.
//
// The writer waits without bound: it IS the tee, its critical section is one
// read and one append, and serialising the appends is the whole point.
func lockStatusFile(st statusStore, name string) (release func(), err error) {
	release, err = flock.AcquireIn(st.root, name+statusLockSuffix)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err //nolint:wrapcheck // preserved so callers can read it as "no snapshots yet"
		}
		return nil, fmt.Errorf("antigravity status: lock: %w", err)
	}
	return release, nil
}

// pruneStaleSnapshot removes one stale conversation's snapshot file, holding
// that conversation's lock if it has one. Reports whether the file is gone.
//
// A lock file beside the snapshot means some tee has been mid-append for this
// conversation, so take that lock — non-blocking — before unlinking, the way
// the orphan-lock loop is careful about its own: a dormant conversation
// resumed between the caller's stat and this unlink is mid-append, and pulling
// the file from under its open fd loses the per-line detail between the
// baseline and now. A held lock means it is alive right now, which is reason
// enough to leave it for the next prune.
//
// No lock file means no tee ever was, because AppendStatusSnapshot takes the
// lock BEFORE creating the .jsonl — the ordering the orphan-lock loop already
// relies on. So absence is evidence here, and the file is unlinked directly.
// The try-lock is not a free probe: flock opens with O_CREATE|O_EXCL, so
// asking for a lock that does not exist CREATES one, and it would be left
// behind for a conversation that is being deleted — an orphan the loop below
// cannot collect this pass (it walks an entries snapshot taken before the lock
// existed) and will not collect on the next one until it has aged past the
// cutoff itself.
func pruneStaleSnapshot(st statusStore, name string) bool {
	target := path.Join(st.dir, name)
	hasLock, err := snapshotFileExists(st, target+statusLockSuffix)
	if err != nil {
		return false
	}
	if hasLock {
		release, locked := tryLockStatusFile(st, target)
		if !locked {
			return false
		}
		defer release()
	}
	return osroot.RemoveNoSymlinks(st.root, target) == nil
}

// tryLockStatusFile takes a conversation's lock only if it is free right now.
// An already-expired deadline selects flock's non-blocking path: one LOCK_NB
// attempt, then the context error rather than a wait. locked is false when
// another process holds it, and for a lock file that cannot be created —
// both mean "do not touch this conversation's snapshots".
func tryLockStatusFile(st statusStore, name string) (release func(), locked bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	release, err := flock.AcquireContextIn(ctx, st.root, name+statusLockSuffix)
	if err != nil {
		return nil, false
	}
	return release, true
}

// statusReadLockTimeout bounds how long a reader waits for the conversation
// lock. A variable so tests can shorten it.
var statusReadLockTimeout = 2 * time.Second

// lockStatusFileForRead is lockStatusFile for the readers, which run inside
// agy's hooks (SnapshotTokenBaseline at PreInvocation, CalculateTokenUsageSince
// at Stop). A hook must not stall behind a tee that hangs while holding the
// lock — the same rule the turn-start session-state lock follows — so the wait
// is bounded, and on timeout the read proceeds unlocked, which is exactly the
// pre-lock behaviour: at worst a torn last line reads as "no snapshot" for one
// turn, against a hook that never returns. The returned release is always
// safe to call.
func lockStatusFileForRead(ctx context.Context, st statusStore, name string) (release func(), err error) {
	acqCtx, cancel := context.WithTimeout(ctx, statusReadLockTimeout)
	defer cancel()
	release, err = flock.AcquireContextIn(acqCtx, st.root, name+statusLockSuffix)
	if err == nil {
		return release, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, err //nolint:wrapcheck // preserved so callers can read it as "no snapshots yet"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		logging.Debug(logging.WithComponent(ctx, "antigravity"),
			"status lock busy; reading token snapshots unlocked",
			slog.String("file", name))
		return func() {}, nil
	}
	return nil, fmt.Errorf("antigravity status: lock: %w", err)
}

// readLastSnapshotFrom returns the snapshot on the final non-empty line of f,
// or nil if the file has no usable line. It reads a bounded tail window instead
// of streaming the whole file: it is shared by the per-fire dedup comparison
// in AppendStatusSnapshot (on the already-open, locked descriptor) and by
// every-TurnStart SnapshotTokenBaseline, and agy fires the title command on
// each agent state change — a front-to-back scan would cost O(file) per fire,
// O(n^2) over a conversation.
func readLastSnapshotFrom(f *os.File) (*statusSnapshot, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("antigravity status: stat: %w", err)
	}

	offset := info.Size() - statusTailWindow
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("antigravity status: read tail: %w", err)
	}

	// When the window starts mid-file, the first chunk may be a partial line —
	// discard through the first newline so only whole lines are considered.
	if offset > 0 {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			return nil, nil //nolint:nilnil // single line larger than the window — treat as no usable snapshot
		}
		buf = buf[nl+1:]
	}

	var lastLine []byte
	for _, line := range bytes.Split(buf, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			lastLine = line
		}
	}
	if len(lastLine) == 0 {
		return nil, nil //nolint:nilnil // no lines yet — caller handles nil gracefully
	}

	var snap statusSnapshot
	if err := json.Unmarshal(lastLine, &snap); err != nil {
		return nil, nil //nolint:nilerr,nilnil // malformed last line — treat as no prior snapshot
	}
	return &snap, nil
}

// readStatusSnapshots reads all valid snapshot lines from the JSONL file for
// the given conversationID. A missing file returns nil, nil (not an error).
func readStatusSnapshots(ctx context.Context, conversationID string) ([]statusSnapshot, error) {
	st, err := openStatusStore()
	if err != nil {
		return nil, err
	}
	name := st.fileName(conversationID)
	if exists, err := snapshotFileExists(st, name); err != nil {
		return nil, err
	} else if !exists {
		return nil, nil // no snapshot yet — and no lock file left behind for it
	}
	release, err := lockStatusFileForRead(ctx, st, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer release()
	f, err := osroot.OpenNoFollow(st.root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("antigravity status: open for read: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var snaps []statusSnapshot
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var snap statusSnapshot
		if err := json.Unmarshal([]byte(line), &snap); err != nil {
			continue // skip malformed lines
		}
		snaps = append(snaps, snap)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("antigravity status: scan: %w", err)
	}
	return snaps, nil
}

// pruneStaleStatusFiles removes other conversations' snapshot files that have
// not been written to within statusRetention, and lock files that are both
// orphaned (no snapshot file beside them) and older than the same cutoff.
//
// A lock file is never pruned while its snapshot file exists: flock does not
// touch mtime, so a lock file is as old as the conversation's first snapshot,
// and a conversation still running past the retention window would otherwise
// have its lock unlinked from under the tee holding it. The next tee would
// create a fresh lock file, lock a different inode, and the dedup read/append
// AppendStatusSnapshot serialises with that lock would race again.
//
// Nor is an orphan pruned on sight: AppendStatusSnapshot takes the lock BEFORE
// it creates the snapshot file, so a prune running from another conversation's
// first append can observe a lock file whose .jsonl does not exist yet. That
// lock is milliseconds old; requiring it to be older than the cutoff leaves it
// alone, while a lock left behind by an earlier prune of its snapshot file is
// at least as old as that file's last write and goes in the same pass.
func pruneStaleStatusFiles(st statusStore, activeConversationID string) {
	activePrefix := filepath.Base(activeConversationID) + ".jsonl"
	entries, err := osroot.ReadDirNoSymlinks(st.root, st.dirName())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-statusRetention)
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			present[entry.Name()] = true
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, activePrefix) || strings.HasSuffix(name, statusLockSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if pruneStaleSnapshot(st, name) {
				delete(present, name)
			}
		}
	}
	// A lock file whose snapshot file is gone (pruned above, or by an earlier
	// run) guards nothing any more — once it is old enough that it cannot be a
	// lock another tee is holding while it creates its snapshot file.
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, statusLockSuffix) || strings.HasPrefix(name, activePrefix) {
			continue
		}
		if present[strings.TrimSuffix(name, statusLockSuffix)] {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = osroot.RemoveNoSymlinks(st.root, path.Join(st.dir, name)) //nolint:errcheck // best-effort prune
	}
}
