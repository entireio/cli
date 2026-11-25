package pi

import (
	"bufio"
	"encoding/json"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// headerLineMax bounds the read of a session file's first line, Pi's header.
const headerLineMax = 64 << 10

// HeaderSessionID returns the session ID in the header of Pi's session file
// at sessionFile, or "" when it can't be read and trusted.
//
// The hook identifies a Pi session by its file name (see
// extractSessionIDFromPath), and that stays the ID Entire records. But Pi
// keeps the file name it is given with --session and writes its own ID in
// the header, so for a session `entire stream pi` started the two differ,
// and the stream is known by the header's. This lets the turn-start context
// recognise the agent's own live session by either.
//
// Only the first line is read, through Pi's session store (the file must lie
// inside it, no symbolic link in any component, a regular file), and only
// its ID is kept.
func (a *PiAgent) HeaderSessionID(sessionFile string) string {
	if sessionFile == "" {
		return ""
	}
	for _, dir := range a.headerStoreDirs() {
		store, err := agent.OpenSessionStoreAt(a, dir)
		if err != nil {
			continue
		}
		name, err := store.Name(sessionFile)
		if err != nil {
			continue
		}
		f, err := store.Open(name)
		if err != nil {
			return ""
		}
		id := readHeaderSessionID(f)
		_ = f.Close()
		return id
	}
	return ""
}

// headerStoreDirs is where Pi's session files may lie: the test override,
// then the base of every project's session folder.
func (a *PiAgent) headerStoreDirs() []string {
	var dirs []string
	if dir := os.Getenv(piSessionDirEnvVar); dir != "" {
		dirs = append(dirs, dir)
	}
	if dir, err := a.GetSessionBaseDir(); err == nil && dir != "" {
		dirs = append(dirs, dir)
	}
	return dirs
}

// readHeaderSessionID reads r's first line, at most headerLineMax bytes, as
// Pi's session header and returns its ID if it is a valid session ID.
func readHeaderSessionID(r io.Reader) string {
	line, err := bufio.NewReaderSize(io.LimitReader(r, headerLineMax), headerLineMax).ReadSlice('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		return ""
	}
	var h struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal(line, &h) != nil || h.Type != "session" || validation.ValidateSessionID(h.ID) != nil {
		return ""
	}
	return h.ID
}
