package agentimport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// resolveDir returns overridePath when set, otherwise the agent's session
// directory for the repo. getDir is the agent's GetSessionDir method.
func resolveDir(repoRoot, overridePath, agentName string, getDir func(string) (string, error)) (string, error) {
	if overridePath != "" {
		return overridePath, nil
	}
	dir, err := getDir(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve %s session dir: %w", agentName, err)
	}
	return dir, nil
}

// sessionResolver maps a directory entry under dir to a discovered session's
// (sessionID, transcript path). ok=false skips the entry — it is not a
// transcript this importer should import (wrong extension/layout, or rejected
// by an importer-specific predicate such as a repo match).
type sessionResolver func(dir string, e os.DirEntry) (sessionID, path string, ok bool)

// discoverSessionFiles lists transcripts in dir using the discovery rules every
// flat-directory importer shares: skip entries the resolver rejects, apply the
// session-ID filter, drop transcripts older than the lookback window (by the
// transcript file's modtime), and sort by path. A missing dir yields no
// sessions (not an error).
//
// codex does not use this — its sessions live in a recursively-walked,
// session_meta-filtered tree rather than a flat directory.
func discoverSessionFiles(dir string, now time.Time, sessionFilter []string, resolve sessionResolver) ([]SessionFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session dir: %w", err)
	}
	cutoff := now.AddDate(0, 0, -LookbackDays)
	var out []SessionFile
	for _, e := range entries {
		sessionID, path, ok := resolve(dir, e)
		if !ok {
			continue
		}
		if len(sessionFilter) > 0 && !slices.Contains(sessionFilter, sessionID) {
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil || info.ModTime().Before(cutoff) {
			continue
		}
		out = append(out, SessionFile{Path: path, SessionID: sessionID})
	}
	slices.SortFunc(out, func(a, b SessionFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// identitySessionID uses the file stem verbatim as the session ID — the common
// case for agents that name transcripts <sessionID><ext>.
func identitySessionID(stem string) string { return stem }

// jsonlSessionResolver returns a sessionResolver for the common flat layout:
// one <stem><ext> file per session. deriveID maps the file stem to the session
// ID (identity for most agents; pi derives a UUID suffix). Directories and
// non-matching extensions are skipped.
func jsonlSessionResolver(ext string, deriveID func(stem string) string) sessionResolver {
	return func(dir string, e os.DirEntry) (string, string, bool) {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			return "", "", false
		}
		stem := strings.TrimSuffix(e.Name(), ext)
		return deriveID(stem), filepath.Join(dir, e.Name()), true
	}
}

// repoScopedJSONLResolver is jsonlSessionResolver restricted to transcripts
// whose recorded cwd places them in repoRoot. Claude Code, Pi, and Factory
// derive their per-project directory from a lossy path encoding (e.g. both
// /w/acme/foo-bar and /w/acme-foo/bar become -w-acme-foo-bar), so one
// directory can hold sessions from several repositories; the directory alone
// does not prove a transcript belongs to this repo.
func repoScopedJSONLResolver(ext string, deriveID func(stem string) string, repoRoot string) sessionResolver {
	resolve := jsonlSessionResolver(ext, deriveID)
	return func(dir string, e os.DirEntry) (string, string, bool) {
		sessionID, path, ok := resolve(dir, e)
		if !ok || !repoMatches(firstRecordedCwd(path), repoRoot) {
			return "", "", false
		}
		return sessionID, path, true
	}
}

// firstRecordedCwd returns the first non-empty top-level "cwd" field in a JSONL
// transcript, or "" when none is found or the file is unreadable (callers then
// treat the transcript as not belonging to the repo). Claude records cwd on
// every message line; Pi and Factory record it on their leading session line.
// Lines are read with bufio.Reader rather than bufio.Scanner because transcript
// lines have no size bound.
func firstRecordedCwd(path string) string {
	f, err := os.Open(path) //nolint:gosec // path discovered under the configured session dir
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if len(line) > 0 && json.Unmarshal(line, &rec) == nil && rec.Cwd != "" {
			return rec.Cwd
		}
		if readErr != nil {
			return ""
		}
	}
}
