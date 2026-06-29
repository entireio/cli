package agentimport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// cursorImporter imports Cursor transcripts. Cursor uses the same JSONL line
// format as Claude Code (role-tagged), so it reuses the shared user-prompt
// detection and content extraction. Cursor records neither model nor token
// usage, so imported turns carry an empty model and nil tokens.
type cursorImporter struct{}

func (cursorImporter) Name() string { return string(agent.AgentNameCursor) }

func (cursorImporter) AgentType() types.AgentType { return agent.AgentTypeCursor }

// Discover returns Cursor transcript files for the repo modified within the
// lookback window. Cursor stores sessions either flat (<dir>/<id>.jsonl) or
// nested (<dir>/<id>/<id>.jsonl, the IDE layout); both are discovered.
func (cursorImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir, err := resolveDir(repoRoot, overridePath, "cursor", (&cursor.CursorAgent{}).GetSessionDir)
	if err != nil {
		return nil, err
	}
	return discoverSessionFiles(dir, now, sessionFilter, func(dir string, e os.DirEntry) (string, string, bool) {
		id, path := cursorSessionFile(dir, e)
		return id, path, path != ""
	})
}

// cursorSessionFile maps a directory entry to a (sessionID, transcript path),
// resolving both the flat and nested Cursor layouts. Returns an empty path for
// entries that are not Cursor transcripts.
func cursorSessionFile(dir string, e os.DirEntry) (sessionID, path string) {
	if e.IsDir() {
		nested := filepath.Join(dir, e.Name(), e.Name()+".jsonl")
		if _, err := os.Stat(nested); err == nil {
			return e.Name(), nested
		}
		return "", ""
	}
	if !strings.HasSuffix(e.Name(), ".jsonl") {
		return "", ""
	}
	return strings.TrimSuffix(e.Name(), ".jsonl"), filepath.Join(dir, e.Name())
}

// SplitTurns produces one Turn per user-prompt line, bounded by the next. It
// reuses the package's shared JSONL helpers; Cursor carries no token usage or
// model, so those fields are left zero.
//
// Real Cursor lines carry only role + message — there is no per-turn uuid or
// timestamp (see cursor/AGENT.md). The append-only line index is the stable
// turn key (as the Codex importer does), so each prompt yields a distinct
// checkpoint ID instead of colliding on an empty UUID and dropping every turn
// after the first. The timestamp falls back to the transcript file's modtime
// (as the Factory/Gemini importers do).
func (cursorImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	var createdAt time.Time
	if info, statErr := os.Stat(sf.Path); statErr == nil {
		createdAt = info.ModTime()
	}
	return splitLineTurns(splitRawLines(full), isUserPromptLine,
		func(rawLines [][]byte, start, _ int, _ []byte) (*Turn, error) {
			var rec struct {
				Message json.RawMessage `json:"message"`
			}
			if err := json.Unmarshal(rawLines[start], &rec); err != nil {
				//nolint:nilerr // skip defensively; the line already parsed in isUserPromptLine
				return nil, nil
			}
			return &Turn{
				UUID:      strconv.Itoa(start),
				Prompt:    transcript.ExtractUserContent(rec.Message),
				CreatedAt: createdAt,
			}, nil
		})
}
