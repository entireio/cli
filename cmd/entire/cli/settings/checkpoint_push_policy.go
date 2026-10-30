package settings

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// CheckpointPushPolicyFile is the path inside the git common dir that holds
// the clone's checkpoint push policy. It lives in the common dir because every
// worktree shares one push queue, and outside settings so a committed file can
// never loosen it. It is its own file rather than a clone preference because
// released binaries rewrite preferences.json without the keys they do not
// model, which would silently turn manual back into automatic.
const CheckpointPushPolicyFile = "entire/checkpoint-push-policy"

// CheckpointPushPolicy is the clone's choice of when checkpoints upload.
type CheckpointPushPolicy string

const (
	// CheckpointPushPolicyAutomatic uploads queued checkpoints on git push.
	CheckpointPushPolicyAutomatic CheckpointPushPolicy = "automatic"
	// CheckpointPushPolicyManual uploads only what `entire checkpoint push` names.
	CheckpointPushPolicyManual CheckpointPushPolicy = "manual"
)

// ParseCheckpointPushPolicy accepts exactly "automatic" or "manual".
func ParseCheckpointPushPolicy(value string) (CheckpointPushPolicy, error) {
	switch policy := CheckpointPushPolicy(value); policy {
	case CheckpointPushPolicyAutomatic, CheckpointPushPolicyManual:
		return policy, nil
	default:
		return "", fmt.Errorf("checkpoint push policy %q is not one of %q, %q",
			value, CheckpointPushPolicyAutomatic, CheckpointPushPolicyManual)
	}
}

// LoadCheckpointPushPolicy reads the policy of the clone ctx resolves to.
func LoadCheckpointPushPolicy(ctx context.Context) (CheckpointPushPolicy, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return "", fmt.Errorf("read checkpoint push policy: resolve git common dir: %w", err)
	}
	return LoadCheckpointPushPolicyAt(commonDir)
}

// LoadCheckpointPushPolicyAt reads the policy of the clone whose git common
// dir is commonDir. A missing file means automatic. Anything else this binary
// does not recognize is an error so callers can fail closed: it may be a
// stricter policy written by a newer binary.
func LoadCheckpointPushPolicyAt(commonDir string) (CheckpointPushPolicy, error) {
	root, name, filePath, err := checkpointPushPolicyRoot(commonDir)
	if err != nil {
		return "", fmt.Errorf("read checkpoint push policy: %w", err)
	}
	data, err := readConfinedIn(root, name, filePath)
	if errors.Is(err, fs.ErrNotExist) {
		return CheckpointPushPolicyAutomatic, nil
	}
	if err != nil {
		return "", fmt.Errorf("read checkpoint push policy: %w", err)
	}
	policy, err := ParseCheckpointPushPolicy(strings.TrimSuffix(string(data), "\n"))
	if err != nil {
		return "", fmt.Errorf("read checkpoint push policy from %s: %w", filePath, err)
	}
	return policy, nil
}

// SaveCheckpointPushPolicy stores the policy for the clone ctx resolves to.
func SaveCheckpointPushPolicy(ctx context.Context, policy CheckpointPushPolicy) error {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return fmt.Errorf("save checkpoint push policy: resolve git common dir: %w", err)
	}
	return saveCheckpointPushPolicyAt(commonDir, policy)
}

// SaveCheckpointPushPolicyIfUnset stores the policy unless the clone already
// has one, and reports whether it did. A choice made while the caller waited,
// for example on a question, wins.
func SaveCheckpointPushPolicyIfUnset(ctx context.Context, policy CheckpointPushPolicy) (bool, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return false, fmt.Errorf("save checkpoint push policy: resolve git common dir: %w", err)
	}
	return saveCheckpointPushPolicyIfUnsetAt(commonDir, policy)
}

// CheckpointPushPolicyStored reports whether the clone has chosen a policy,
// which a stored automatic counts as and the default does not.
func CheckpointPushPolicyStored(ctx context.Context) (bool, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return false, fmt.Errorf("read checkpoint push policy: resolve git common dir: %w", err)
	}
	return checkpointPushPolicyStoredAt(commonDir)
}

func checkpointPushPolicyStoredAt(commonDir string) (bool, error) {
	root, name, _, err := checkpointPushPolicyRoot(commonDir)
	if err != nil {
		return false, fmt.Errorf("read checkpoint push policy: %w", err)
	}
	if _, err := root.Lstat(name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read checkpoint push policy: %w", err)
	}
	return true, nil
}

func saveCheckpointPushPolicyAt(commonDir string, policy CheckpointPushPolicy) error {
	_, err := writeCheckpointPushPolicy(commonDir, policy, false)
	return err
}

func saveCheckpointPushPolicyIfUnsetAt(commonDir string, policy CheckpointPushPolicy) (bool, error) {
	return writeCheckpointPushPolicy(commonDir, policy, true)
}

func writeCheckpointPushPolicy(commonDir string, policy CheckpointPushPolicy, onlyIfUnset bool) (bool, error) {
	if _, err := ParseCheckpointPushPolicy(string(policy)); err != nil {
		return false, err
	}
	root, name, _, err := checkpointPushPolicyRoot(commonDir)
	if err != nil {
		return false, fmt.Errorf("save checkpoint push policy: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, path.Dir(name), 0o750); err != nil {
		return false, fmt.Errorf("save checkpoint push policy: create directory: %w", err)
	}
	release, err := flock.AcquireIn(root, name+".lock")
	if err != nil {
		return false, fmt.Errorf("save checkpoint push policy: lock: %w", err)
	}
	defer release()
	if onlyIfUnset {
		stored, err := checkpointPushPolicyStoredAt(commonDir)
		if err != nil || stored {
			return false, err
		}
	}
	if err := jsonutil.WriteFileAtomicIn(root, name, []byte(string(policy)+"\n"), 0o644); err != nil {
		return false, fmt.Errorf("save checkpoint push policy: %w", err)
	}
	return true, nil
}

func checkpointPushPolicyRoot(commonDir string) (*os.Root, string, string, error) {
	filePath := filepath.Join(commonDir, CheckpointPushPolicyFile)
	root, name, err := gitdir.OpenPathIn(commonDir, filePath)
	if err != nil {
		return nil, "", "", fmt.Errorf("open git common dir: %w", err)
	}
	return root, name, filePath, nil
}
