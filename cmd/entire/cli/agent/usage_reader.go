package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
)

// CumulativeTokenInput identifies the session whose running token total is read.
type CumulativeTokenInput struct {
	SessionID      string
	TranscriptPath string
	// Subagents and Home are the session's child inventory and agent home, for
	// children not discoverable from the transcript (Codex).
	Subagents []SubagentReference
	Home      string
}

// CumulativeObservation is one reading of a session's running totals.
type CumulativeObservation struct {
	// Total is the main agent's usage since the session began, without
	// SubagentTokens; Children each subagent's, by a stable child ID (a child
	// can vanish when its file is deleted). nil means no data. InputTokens
	// excludes cache, OutputTokens includes reasoning.
	Total    *TokenUsage
	Children map[string]*TokenUsage
	// Cursor lets the next reading continue where this one stopped.
	Cursor json.RawMessage
	// Restarted reports that the main transcript was rewritten (a resume
	// restoring an older copy) and read again from its start. A lower Total is
	// a rewrite only then; otherwise it is a stale read.
	Restarted bool
}

// UsageFile is one file a UsageCounter reads.
type UsageFile struct {
	Path string
	// ID names what the file holds when its path can change (a Claude Code
	// Workflow agent found later at its direct location); default Path. A new
	// path for an ID is read from its start, after Reset.
	ID string
	// Whole marks a file read whole when its size or last bytes change: one
	// rewritten rather than appended (OpenCode's export, Droid's
	// settings.json), or one whose total is all the counter keeps (a Claude
	// Code subagent). It never reports a restart.
	Whole bool
	// NoRestart keeps a rewrite of the main transcript from being reported,
	// for a total that also rests on a file that does not shrink with it.
	NoRestart bool
}

// UsageCounter is an agent's running token count. ReadCumulativeUsage owns
// the files (only appended, complete lines; rewrites detected); the counter
// owns what the lines mean. It is saved between readings as JSON: state in
// exported fields, settings (a merge policy) set by NewUsageCounter.
type UsageCounter interface {
	// Files returns the files to read, the main transcript first. It is called
	// again after they were read, so it can name files their lines revealed.
	Files(in CumulativeTokenInput) []UsageFile
	// Add folds complete lines appended to path in; firstLine is the first
	// one's zero-based line number, for records identified by position.
	Add(path string, data []byte, firstLine int)
	// Reset forgets what was read from path: it is read again from its start.
	Reset(path string)
	// Totals returns the main agent's total and each child's (Codex reads its
	// children from the subagent inventory here).
	Totals(ctx context.Context, in CumulativeTokenInput) (total *TokenUsage, children map[string]*TokenUsage)
}

// UsageCounterSource is an agent whose session totals ReadCumulativeUsage can
// read. Built-in agents only.
type UsageCounterSource interface {
	Agent

	// NewUsageCounter returns an empty counter.
	NewUsageCounter() UsageCounter
}

// usageCursorVersion changes with the cursor's meaning; another version's
// cursor is ignored.
const usageCursorVersion = 3

// usageCursorCheckBytes is how many bytes before a file's offset a cursor
// fingerprints to detect a rewrite.
const usageCursorCheckBytes = 512

// maxUsageFilePasses bounds how many rounds of newly named files one reading
// follows (Claude Code subagents a subagent launched).
const maxUsageFilePasses = 8

type usageCursor struct {
	Version int                      `json:"v"`
	Agent   string                   `json:"agent"`
	Files   map[string]usageFileMark `json:"files,omitempty"`
	State   json.RawMessage          `json:"state,omitempty"`
}

// usageFileMark is how far a file was read: where, the end of the last
// complete line consumed, how many lines, and a fingerprint of the bytes
// before that end.
type usageFileMark struct {
	Path   string `json:"p"`
	Offset int    `json:"o"`
	Lines  int    `json:"l,omitempty"`
	Check  string `json:"c,omitempty"`
}

// ReadCumulativeUsage reads a session's running totals, continuing from
// cursor (an earlier observation's Cursor, or nil). ok is false for an agent
// with no UsageCounterSource. An unreadable main transcript is an error; an
// unreadable file beside it is skipped.
func ReadCumulativeUsage(ctx context.Context, ag Agent, in CumulativeTokenInput, cursor json.RawMessage) (obs CumulativeObservation, ok bool, err error) {
	src, isSource := AsUsageCounterSource(ag)
	if !isSource {
		return CumulativeObservation{}, false, nil
	}
	if in.TranscriptPath == "" {
		return CumulativeObservation{}, true, errors.New("no transcript path")
	}
	counter := src.NewUsageCounter()
	var cur usageCursor
	known := len(cursor) > 0 && json.Unmarshal(cursor, &cur) == nil &&
		cur.Version == usageCursorVersion && cur.Agent == string(ag.Name())
	if known && len(cur.State) > 0 && json.Unmarshal(cur.State, counter) != nil {
		known = false
		counter = src.NewUsageCounter()
	}
	if !known {
		cur = usageCursor{}
	}

	// A file this reading does not reach keeps its mark: the counter still
	// holds what was read from it, so reading it again from its start later
	// would count it twice.
	marks := maps.Clone(cur.Files)
	if marks == nil {
		marks = make(map[string]usageFileMark)
	}
	read := make(map[string]bool)
	for pass := range maxUsageFilePasses {
		progressed := false
		for i, f := range counter.Files(in) {
			id := f.ID
			if id == "" {
				id = f.Path
			}
			if read[id] {
				continue
			}
			read[id] = true
			progressed = true
			main := pass == 0 && i == 0
			mark, restarted, readErr := readUsageFile(counter, f, cur.Files[id], main)
			if readErr != nil {
				if main {
					return CumulativeObservation{}, true, readErr
				}
				continue // not written yet, or already cleaned up
			}
			marks[id] = mark
			obs.Restarted = obs.Restarted || (main && restarted && !f.NoRestart)
		}
		if !progressed {
			break
		}
	}

	state, err := json.Marshal(counter)
	if err != nil {
		return CumulativeObservation{}, true, fmt.Errorf("encode token counter: %w", err)
	}
	next, err := json.Marshal(usageCursor{Version: usageCursorVersion, Agent: string(ag.Name()), Files: marks, State: state})
	if err != nil {
		return CumulativeObservation{}, true, fmt.Errorf("encode token cursor: %w", err)
	}
	obs.Total, obs.Children = counter.Totals(ctx, in)
	obs.Cursor = next
	return obs, true, nil
}

// readUsageFile feeds counter what f gained since prev. restarted reports a
// file that no longer begins with what prev consumed; a cursor that consumed
// nothing cannot tell a rewrite from growth, and a whole file never restarts.
func readUsageFile(counter UsageCounter, f UsageFile, prev usageFileMark, main bool) (usageFileMark, bool, error) {
	if prev.Path != f.Path {
		// Read before, at another path or never: start over.
		if prev.Path != "" {
			counter.Reset(f.Path)
		}
		prev = usageFileMark{}
	}
	if f.Whole {
		if prev.Offset > 0 {
			// Unchanged when its size and last bytes are: skip the read.
			tail, size, err := readUsageBytes(f.Path, max(prev.Offset-usageCursorCheckBytes, 0), main)
			if err != nil {
				return usageFileMark{}, false, err
			}
			if size == prev.Offset && fingerprint(tail) == prev.Check {
				return prev, false, nil
			}
		}
		data, _, err := readUsageBytes(f.Path, 0, main)
		if err != nil {
			return usageFileMark{}, false, err
		}
		counter.Reset(f.Path)
		counter.Add(f.Path, data, 0)
		return usageFileMark{Path: f.Path, Offset: len(data), Check: fingerprint(data[max(len(data)-usageCursorCheckBytes, 0):])}, false, nil
	}

	start := max(prev.Offset-usageCursorCheckBytes, 0)
	data, size, err := readUsageBytes(f.Path, start, main)
	if err != nil {
		return usageFileMark{}, false, err
	}
	restarted := false
	if prev.Offset > 0 && (size < prev.Offset || fingerprint(data[:prev.Offset-start]) != prev.Check) {
		if data, _, err = readUsageBytes(f.Path, 0, main); err != nil {
			return usageFileMark{}, false, err
		}
		restarted = true
		counter.Reset(f.Path)
		prev, start = usageFileMark{}, 0
	}
	appended := data[prev.Offset-start:]
	end := bytes.LastIndexByte(appended, '\n') + 1 // a line still being written waits
	if end == 0 {
		prev.Path = f.Path
		return prev, restarted, nil
	}
	counter.Add(f.Path, appended[:end], prev.Lines)
	offset := prev.Offset + end
	return usageFileMark{
		Path:   f.Path,
		Offset: offset,
		Lines:  prev.Lines + bytes.Count(appended[:end], []byte("\n")),
		Check:  fingerprint(data[max(offset-usageCursorCheckBytes, start)-start : offset-start]),
	}, restarted, nil
}

// readUsageBytes returns path's bytes from offset to its end, and its size.
// Only a regular file is opened, so a FIFO cannot block the hook. A file a
// counter derived rather than the session's own transcript (main) must not
// be a symlink either: it could lead out of the agent's store. Transcripts
// under .entire go through its confined reader.
func readUsageBytes(path string, offset int, main bool) ([]byte, int, error) {
	if _, _, underEntire := entiredir.Split(path); underEntire {
		data, err := ReadTranscriptFile(path)
		if err != nil {
			return nil, 0, err
		}
		return data[min(offset, len(data)):], len(data), nil
	}
	stat := os.Stat
	if !main {
		stat = os.Lstat
	}
	info, err := stat(path)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	file, err := os.Open(path) //nolint:gosec // an agent transcript or a file beside it, checked regular above
	if err != nil {
		return nil, 0, err //nolint:wrapcheck // the os error names the path
	}
	defer file.Close()
	size := int(info.Size())
	if offset >= size {
		return nil, size, nil
	}
	data := make([]byte, size-offset)
	n, err := file.ReadAt(data, int64(offset))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return data[:n], offset + n, nil
}

func fingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// ForEachLine calls fn for every non-blank line in data, with its zero-based
// line number in the file given data's first line number.
func ForEachLine(data []byte, firstLine int, fn func(lineNo int, line []byte)) {
	for i, line := range bytes.Split(data, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			fn(firstLine+i, line)
		}
	}
}
