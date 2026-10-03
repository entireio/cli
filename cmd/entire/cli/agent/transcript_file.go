package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// ReadTranscriptFile reads a transcript path while preserving the .entire
// boundary for agents whose stable transcript cache lives there (OpenCode and
// Pi). Agent-owned transcript stores remain explicit external paths and retain
// their existing behavior.
func ReadTranscriptFile(filePath string) ([]byte, error) {
	if _, _, underEntire := entiredir.Split(filePath); !underEntire {
		//nolint:gosec,wrapcheck // external agent transcript path is the caller's selected input; os error carries op and path
		return os.ReadFile(filePath)
	}
	abs, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("resolve transcript path %s: %w", filePath, err)
	}
	root, name, err := entiredir.OpenPathForRead(abs)
	if err != nil {
		// Unwrapped when the directory is simply absent. OpenPathForRead
		// already returns an *fs.PathError there, and os.IsNotExist — unlike
		// errors.Is — does not unwrap a %w, so adding context here would tell
		// every caller that tests it (opencode PrepareTranscript,
		// pi GetTranscriptPosition) that a missing .entire is a hard failure.
		if os.IsNotExist(err) {
			return nil, err //nolint:wrapcheck // see comment: os.IsNotExist does not unwrap
		}
		return nil, fmt.Errorf("open %s for transcript %s: %w", paths.EntireDir, filePath, err)
	}
	return entiredir.ReadFile(root, name) //nolint:wrapcheck // preserve os.IsNotExist classification at call sites
}

// ReadTranscriptFileUnderHome reads a transcript without following links below
// an authorized agent home. Callers must establish the home's provenance first.
// An empty home retains legacy behavior; paths in .entire use its own root.
// A nonempty home must be absolute, and other paths must remain beneath it.
func ReadTranscriptFileUnderHome(filePath, agentHome string) ([]byte, error) {
	if agentHome == "" {
		return ReadTranscriptFile(filePath)
	}
	if _, _, underEntire := entiredir.Split(filePath); underEntire {
		return ReadTranscriptFile(filePath)
	}
	store, name, err := transcriptStoreUnderHome(filePath, agentHome)
	if err != nil {
		return nil, err
	}
	return store.ReadFile(name)
}

// OpenTranscriptFileUnderHome opens a transcript for streaming with the same
// boundary as ReadTranscriptFileUnderHome. The caller owns the returned file.
func OpenTranscriptFileUnderHome(filePath, agentHome string) (*os.File, error) {
	if _, _, underEntire := entiredir.Split(filePath); underEntire {
		abs, err := filepath.Abs(filePath)
		if err != nil {
			return nil, fmt.Errorf("resolve transcript path %s: %w", filePath, err)
		}
		root, name, err := entiredir.OpenPathForRead(abs)
		if err != nil {
			return nil, err //nolint:wrapcheck // preserve filesystem errors
		}
		return osroot.OpenNoFollow(root, name) //nolint:wrapcheck // preserve filesystem errors
	}
	if agentHome == "" {
		//nolint:gosec,wrapcheck // explicit legacy agent transcript path
		return os.Open(filePath)
	}
	store, name, err := transcriptStoreUnderHome(filePath, agentHome)
	if err != nil {
		return nil, err
	}
	return store.OpenFile(name)
}

// transcriptStoreUnderHome accepts both the original spelling of a symlinked
// home and its canonical spelling. It never resolves links below the home.
func transcriptStoreUnderHome(filePath, agentHome string) (*SessionStore, string, error) {
	if !filepath.IsAbs(agentHome) {
		return nil, "", fmt.Errorf("agent home must be absolute: %s", agentHome)
	}
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, "", fmt.Errorf("resolve transcript path: %w", err)
	}
	resolvedHome, err := filepath.EvalSymlinks(agentHome)
	if err != nil {
		return nil, "", fmt.Errorf("resolve agent home %s: %w", agentHome, err)
	}
	rel, ok := TranscriptNameUnderHome(absPath, agentHome)
	if !ok {
		rel, ok = TranscriptNameUnderHome(absPath, resolvedHome)
	}
	if !ok {
		return nil, "", fmt.Errorf("transcript %s: %w", filePath, ErrOutsideSessionStore)
	}
	store, err := OpenSessionStoreAt(nil, resolvedHome)
	if err != nil {
		return nil, "", fmt.Errorf("open agent home: %w", err)
	}
	return store, filepath.ToSlash(rel), nil
}

// TranscriptNameUnderHome returns a lexical name beneath home, using Windows
// case rules where applicable. It does not authorize home or inspect symlinks.
// The name keeps path's own spelling: callers join it onto a home and persist
// the result, so only the containment comparison folds case.
func TranscriptNameUnderHome(path, home string) (string, bool) {
	return transcriptNameUnderHome(path, home, runtime.GOOS == hookWrapperOSWindows)
}

func transcriptNameUnderHome(path, home string, foldCase bool) (string, bool) {
	path, home = filepath.Clean(path), filepath.Clean(home)
	if !foldCase {
		rel, err := filepath.Rel(home, path)
		return rel, err == nil && rel != "." && !paths.IsRelativeTraversal(rel)
	}
	rel, err := filepath.Rel(strings.ToLower(home), strings.ToLower(path))
	if err != nil || rel == "." || paths.IsRelativeTraversal(rel) {
		return "", false
	}
	// Lowercasing preserves the component count, so the trailing components
	// of the cleaned original path are rel in its original casing.
	parts := strings.Split(path, string(filepath.Separator))
	n := len(strings.Split(rel, string(filepath.Separator)))
	if n > len(parts) {
		return "", false
	}
	return filepath.Join(parts[len(parts)-n:]...), true
}

// TranscriptReadableUnderHome checks metadata confinement, not permission to
// open the transcript: path lies under home and no existing component below
// home is a symbolic link. Confined reads refuse such links, so a home whose
// transcripts sit behind one (for example a relocated projects directory) is
// not recorded and the session keeps the legacy read protocol. A missing
// component is accepted; later reads still check every component. Discovery
// must additionally open a regular file before selecting this candidate.
func TranscriptReadableUnderHome(path, home string) bool {
	if path == "" || home == "" {
		return false
	}
	store, name, err := transcriptStoreUnderHome(path, home)
	if err != nil {
		return false
	}
	info, err := store.Lstat(name)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	return info.Mode()&os.ModeSymlink == 0
}

// GetTranscriptPositionUnderHome counts a transcript through a confined reader.
// Agents without a reader analyzer and sessions without a home retain their
// existing position implementation. Missing transcripts return zero.
func GetTranscriptPositionUnderHome(analyzer TranscriptAnalyzer, transcriptPath, agentHome string) (int, error) {
	if transcriptPath == "" {
		return 0, nil
	}
	confined, ok := analyzer.(ConfinedTranscriptAnalyzer)
	if !ok || agentHome == "" {
		return analyzer.GetTranscriptPosition(transcriptPath) //nolint:wrapcheck // preserve analyzer errors
	}
	file, err := openTranscriptUnderHome(transcriptPath, agentHome)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer file.Close()
	return confined.GetTranscriptPositionFromReader(file) //nolint:wrapcheck // preserve analyzer errors
}

func openTranscriptUnderHome(path, home string) (*os.File, error) {
	if _, _, underEntire := entiredir.Split(path); underEntire {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve cached transcript: %w", err)
		}
		root, name, err := entiredir.OpenPathForRead(abs)
		if err != nil {
			return nil, fmt.Errorf("open cached transcript: %w", err)
		}
		return osroot.OpenNoFollow(root, name) //nolint:wrapcheck // preserve filesystem errors
	}
	store, name, err := transcriptStoreUnderHome(path, home)
	if err != nil {
		return nil, err
	}
	return store.OpenFile(name)
}

// CountTranscriptLines counts raw JSONL lines using bounded memory. Blank and
// malformed lines count; a final line without a newline also counts.
func CountTranscriptLines(r io.Reader) (int, error) {
	var buf [32 * 1024]byte
	count := 0
	unterminated := false
	for {
		n, err := r.Read(buf[:])
		if n > 0 && n <= len(buf) {
			count += bytes.Count(buf[:n], []byte{'\n'})
			unterminated = !bytes.HasSuffix(buf[:n], []byte{'\n'})
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return 0, fmt.Errorf("count transcript lines: %w", err)
			}
			if unterminated {
				count++
			}
			return count, nil
		}
	}
}

// StatTranscriptFile is the metadata-only counterpart to ReadTranscriptFile.
// Lstat is intentional under .entire: a dangling or redirected transcript link
// is not the cached file whose existence the caller is testing.
func StatTranscriptFile(filePath string) (os.FileInfo, error) {
	if _, _, underEntire := entiredir.Split(filePath); !underEntire {
		//nolint:wrapcheck // external agent transcript path is the caller's selected input; os error carries op and path
		return os.Stat(filePath)
	}
	abs, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("resolve transcript path %s: %w", filePath, err)
	}
	root, name, err := entiredir.OpenPathForRead(abs)
	if err != nil {
		// Unwrapped when the directory is simply absent. OpenPathForRead
		// already returns an *fs.PathError there, and os.IsNotExist — unlike
		// errors.Is — does not unwrap a %w, so adding context here would tell
		// every caller that tests it (opencode PrepareTranscript,
		// pi GetTranscriptPosition) that a missing .entire is a hard failure.
		if os.IsNotExist(err) {
			return nil, err //nolint:wrapcheck // see comment: os.IsNotExist does not unwrap
		}
		return nil, fmt.Errorf("open %s for transcript %s: %w", paths.EntireDir, filePath, err)
	}
	info, err := osroot.LstatNoSymlinks(root, name)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve missing-file classification
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: %w", name, osroot.ErrSymlinkedPath)
	}
	return info, nil
}
