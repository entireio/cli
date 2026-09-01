package agent

import (
	"fmt"
	"os"
	"path/filepath"

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
