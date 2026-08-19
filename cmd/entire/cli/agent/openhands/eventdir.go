package openhands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// configDirName is OpenHands' project config directory.
	configDirName = ".openhands"

	// eventsDirName is the per-conversation event directory.
	eventsDirName = "events"

	// conversationsDirName is the directory holding all conversations.
	conversationsDirName = "conversations"

	// persistenceEnv and conversationsEnv mirror openhands_cli/locations.py.
	persistenceEnv   = "OPENHANDS_PERSISTENCE_DIR"
	conversationsEnv = "OPENHANDS_CONVERSATIONS_DIR"

	// eventFilePattern mirrors EVENT_FILE_PATTERN in
	// openhands/sdk/conversation/persistence_const.py. Reproducing it exactly is
	// what makes the JSONL round trip back to byte-identical filenames.
	eventFilePattern = "event-%05d-%s.json"
)

// eventFileRe mirrors EVENT_NAME_RE, used to order files by index rather than
// by lexical name.
var eventFileRe = regexp.MustCompile(`^event-(\d{5})-([0-9a-fA-F-]{8,})\.json$`)

// conversationsRoot resolves the directory holding all conversations, honouring
// the same environment overrides the CLI does.
func conversationsRoot() (string, error) {
	if dir := os.Getenv(conversationsEnv); dir != "" {
		return dir, nil
	}
	base := os.Getenv(persistenceEnv)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		base = filepath.Join(home, configDirName)
	}
	return filepath.Join(base, conversationsDirName), nil
}

// conversationDirID converts a conversation id to the spelling used for the
// on-disk directory: undashed hex.
//
// OpenHands prints the dashed UUID in its resume hint but names the directory
// with the undashed form, so a hook-supplied id can arrive either way.
func conversationDirID(id string) string {
	return strings.ReplaceAll(id, "-", "")
}

// resumeID converts a conversation id to the dashed UUID form `--resume` wants.
// Ids that are not 32 hex characters are passed through untouched.
func resumeID(id string) string {
	if strings.Contains(id, "-") {
		return id
	}
	if len(id) != 32 || !isHex(id) {
		return id
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", id[0:8], id[8:12], id[12:16], id[16:20], id[20:32])
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// readEventDir serializes an OpenHands event directory to JSONL, one event per
// line in index order.
//
// A missing directory yields no data rather than an error: the hook can fire
// before OpenHands has written its first event.
func readEventDir(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read openhands event dir: %w", err)
	}

	type indexed struct {
		idx  int
		name string
	}
	var files []indexed
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := eventFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			// Skips base_state.json and the .eventlog.lock file.
			continue
		}
		idx, convErr := strconv.Atoi(m[1])
		if convErr != nil {
			continue
		}
		files = append(files, indexed{idx: idx, name: e.Name()})
	}
	// Sort by index, not by name: the zero-padded prefix makes those agree
	// today, but ordering is a correctness property so it is stated explicitly.
	sort.Slice(files, func(i, j int) bool { return files[i].idx < files[j].idx })

	var out bytes.Buffer
	for _, f := range files {
		data, readErr := os.ReadFile(filepath.Join(dir, f.name)) //nolint:gosec // path from a validated dir listing
		if readErr != nil {
			return nil, fmt.Errorf("read openhands event %s: %w", f.name, readErr)
		}
		// Compact so one event occupies exactly one line.
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return nil, fmt.Errorf("compact openhands event %s: %w", f.name, err)
		}
		out.Write(compact.Bytes())
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// writeEventDir expands serialized JSONL back into one file per event.
//
// Filenames are rebuilt from (line index, event id) via eventFilePattern, which
// is what makes the serialization reversible. Events already present are
// replaced; unrelated files such as base_state.json are left alone.
func writeEventDir(dir string, data []byte) error {
	//nolint:gosec // G301: openhands reads this directory
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create openhands event dir: %w", err)
	}

	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	for i, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var ev struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("parse openhands event at line %d: %w", i, err)
		}
		if ev.ID == "" {
			return fmt.Errorf("openhands event at line %d has no id; cannot rebuild its filename", i)
		}
		name := fmt.Sprintf(eventFilePattern, i, ev.ID)
		//nolint:gosec // G306: openhands reads these files
		if err := os.WriteFile(filepath.Join(dir, name), line, 0o644); err != nil {
			return fmt.Errorf("write openhands event %s: %w", name, err)
		}
	}
	return nil
}
