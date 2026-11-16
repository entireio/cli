package settings

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// CrossRepoCommitsFile is the path inside the git common dir that holds the
// clone's opt-in to linking commits its agent sessions make in other
// repositories. Linking copies the session's transcript into the other
// repository's checkpoints, so the choice belongs to the developer of the
// clone the transcript comes from: it lives in the common dir, where nothing
// a clone or a pull brings in can set it, and outside settings for the same
// reason as CheckpointPushPolicyFile.
const CrossRepoCommitsFile = "entire/cross-repo-commits"

const (
	crossRepoCommitsOn  = "on"
	crossRepoCommitsOff = "off"
)

// ParseCrossRepoCommits accepts exactly "on" or "off".
func ParseCrossRepoCommits(value string) (bool, error) {
	switch value {
	case crossRepoCommitsOn:
		return true, nil
	case crossRepoCommitsOff:
		return false, nil
	default:
		return false, fmt.Errorf("cross-repo commits %q is not one of %q, %q", value, crossRepoCommitsOn, crossRepoCommitsOff)
	}
}

// LoadCrossRepoCommitsAt reports whether the clone whose git common dir is
// commonDir opted in. Anything but a readable "on" is off: a missing file,
// a symlink, an unreadable file, or a value this binary does not recognize.
func LoadCrossRepoCommitsAt(commonDir string) bool {
	root, name, filePath, err := cloneFileRoot(commonDir, CrossRepoCommitsFile)
	if err != nil {
		return false
	}
	data, err := readConfinedIn(root, name, filePath)
	if err != nil {
		return false
	}
	on, err := ParseCrossRepoCommits(strings.TrimSuffix(string(data), "\n"))
	return err == nil && on
}

// SaveCrossRepoCommits stores the opt-in for the clone ctx resolves to.
func SaveCrossRepoCommits(ctx context.Context, on bool) error {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return fmt.Errorf("save cross-repo commits: resolve git common dir: %w", err)
	}
	return saveCrossRepoCommitsAt(commonDir, on)
}

func saveCrossRepoCommitsAt(commonDir string, on bool) error {
	value := crossRepoCommitsOff
	if on {
		value = crossRepoCommitsOn
	}
	if _, err := writeCloneFile(commonDir, CrossRepoCommitsFile, []byte(value+"\n"), false); err != nil {
		return fmt.Errorf("save cross-repo commits: %w", err)
	}
	return nil
}
