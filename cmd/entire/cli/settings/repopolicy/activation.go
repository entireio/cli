package repopolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

const (
	recordVersion      = 1
	activationFileName = "activation.json"
)

// WorktreeRegistryRelative is the single shared Git-common subpath for
// machine-local activation, route records, and git-common runtime data.
const WorktreeRegistryRelative = "entire/worktree"

var errInvalidActivationSettings = errors.New("invalid activation settings")

// ActivationState is the machine-local activation choice for one worktree.
type ActivationState string

const (
	ActivationAbsent   ActivationState = ""
	ActivationEnabled  ActivationState = "enabled"
	ActivationDisabled ActivationState = "disabled"
)

// ActivationRecord is the versioned machine-local activation marker.
type ActivationRecord struct {
	Version            int             `json:"version"`
	State              ActivationState `json:"state"`
	CanonicalWorktree  string          `json:"canonical_worktree"`
	CanonicalGitCommon string          `json:"canonical_git_common"`
}

func registryDir(repository Repository) string {
	return WorktreeRegistryDir(repository.GitCommonDir, repository.WorktreeKey)
}

// WorktreeRegistryDir constructs one worktree's machine-local registry path.
func WorktreeRegistryDir(gitCommonDir, worktreeKey string) string {
	return filepath.Join(gitCommonDir, filepath.FromSlash(WorktreeRegistryRelative), worktreeKey)
}

func activationPath(repository Repository) string {
	return filepath.Join(registryDir(repository), activationFileName)
}

func validateRepository(repository Repository) error {
	if repository.WorktreeRoot == "" || repository.GitCommonDir == "" || repository.WorktreeKey == "" {
		return errors.New("repository identity is incomplete")
	}
	if canonicalPath(repository.WorktreeRoot) != repository.WorktreeRoot || canonicalPath(repository.GitCommonDir) != repository.GitCommonDir {
		return errors.New("repository identity is not canonical")
	}
	return nil
}

// ReadLocalActivation reads and verifies this worktree's activation marker.
// A missing record returns ActivationAbsent. Every other invalid shape fails
// closed with an error.
func ReadLocalActivation(repository Repository) (ActivationState, error) {
	if err := validateRepository(repository); err != nil {
		return ActivationAbsent, err
	}
	data, err := os.ReadFile(activationPath(repository))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ActivationAbsent, nil
		}
		return ActivationAbsent, fmt.Errorf("reading activation record: %w", err)
	}
	var record ActivationRecord
	if err := decodeStrict(data, &record); err != nil {
		return ActivationAbsent, fmt.Errorf("parsing activation record: %w", err)
	}
	if record.Version != recordVersion {
		return ActivationAbsent, fmt.Errorf("unsupported activation record version %d", record.Version)
	}
	if record.State != ActivationEnabled && record.State != ActivationDisabled {
		return ActivationAbsent, fmt.Errorf("invalid activation state %q", record.State)
	}
	if record.CanonicalWorktree != repository.WorktreeRoot || record.CanonicalGitCommon != repository.GitCommonDir {
		return ActivationAbsent, errors.New("activation record repository identity mismatch")
	}
	return record.State, nil
}

// SetLocalActivation resolves the current repository and records an explicit
// activation choice. Enabled is the authorized recovery from a corrupt record;
// disabled refuses to overwrite an unverifiable existing record.
func SetLocalActivation(ctx context.Context, state ActivationState) error {
	repository, err := ResolveRepository(ctx)
	if err != nil {
		return err
	}
	return setLocalActivation(repository, state)
}

// SetLocalActivationForRepository is the explicit-repository form used by
// setup flows and tests that must not depend on process cwd.
func SetLocalActivationForRepository(repository Repository, state ActivationState) error {
	return setLocalActivation(repository, state)
}

func setLocalActivation(repository Repository, state ActivationState) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if state != ActivationEnabled && state != ActivationDisabled {
		return fmt.Errorf("invalid activation state %q", state)
	}
	if state == ActivationDisabled {
		if _, err := ReadLocalActivation(repository); err != nil {
			return fmt.Errorf("refusing to replace invalid activation record: %w", err)
		}
	}
	if err := ensureRegistryDir(repository); err != nil {
		return err
	}
	record := ActivationRecord{
		Version:            recordVersion,
		State:              state,
		CanonicalWorktree:  repository.WorktreeRoot,
		CanonicalGitCommon: repository.GitCommonDir,
	}
	data, err := jsonutil.MarshalIndentWithNewline(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding activation record: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(activationPath(repository), data, 0o600); err != nil {
		return fmt.Errorf("writing activation record: %w", err)
	}
	return nil
}

func ensureRegistryDir(repository Repository) error {
	dir := registryDir(repository)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating repository policy registry: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // registry directories require owner-only traversal
		return fmt.Errorf("securing repository policy registry: %w", err)
	}
	return nil
}

// BootstrapLegacyActivation records local activation only when legacy enabled
// settings are paired with exact machine-local evidence for this worktree.
func BootstrapLegacyActivation(ctx context.Context, repository Repository) (bool, error) {
	state, err := ReadLocalActivation(repository)
	if err != nil {
		return false, err
	}
	if state == ActivationEnabled {
		return true, nil
	}
	if state == ActivationDisabled {
		return false, nil
	}
	enabled, err := effectiveEnabledSetting(ctx, repository)
	if err != nil || enabled == nil || !*enabled {
		return false, err
	}
	evidence, err := hasExactSessionEvidence(repository)
	if err != nil {
		return false, err
	}
	if !evidence {
		evidence, err = hasRecognizedMetadataEvidence(ctx, repository)
		if err != nil {
			return false, err
		}
	}
	if !evidence {
		return false, nil
	}
	if err := setLocalActivation(repository, ActivationEnabled); err != nil {
		return false, err
	}
	return true, nil
}

func enabledFromSettingsFile(path string) (*bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // caller passes a provenance-verified canonical worktree path
	if err != nil {
		return nil, fmt.Errorf("%w: reading settings: %w", errInvalidActivationSettings, err)
	}
	var raw map[string]json.RawMessage
	if err := decodeStrict(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: parsing settings: %w", errInvalidActivationSettings, err)
	}
	value, ok := raw["enabled"]
	if !ok {
		return nil, nil //nolint:nilnil // nil means the optional enabled field is absent
	}
	var enabled bool
	if err := json.Unmarshal(value, &enabled); err != nil {
		return nil, fmt.Errorf("%w: enabled must be a boolean", errInvalidActivationSettings)
	}
	return &enabled, nil
}

func effectiveEnabledSetting(ctx context.Context, repository Repository) (*bool, error) {
	provenance, err := VerifySettingsProvenance(ctx, repository)
	if err != nil {
		return nil, err
	}
	var effective *bool
	for _, candidate := range []struct {
		path     string
		verified bool
	}{
		{path: provenance.ProjectPath, verified: provenance.ProjectVerified},
		{path: provenance.LocalPath, verified: provenance.LocalVerified},
	} {
		if !candidate.verified {
			continue
		}
		value, err := enabledFromSettingsFile(candidate.path)
		if err != nil {
			return nil, err
		}
		if value != nil {
			effective = value
		}
	}
	return effective, nil
}

type legacySessionRecord struct {
	WorktreePath string          `json:"worktree_path"`
	WorktreeID   string          `json:"worktree_id"`
	EndedAt      json.RawMessage `json:"ended_at"`
	Phase        string          `json:"phase"`
}

func hasExactSessionEvidence(repository Repository) (bool, error) {
	dir := filepath.Join(repository.GitCommonDir, "entire-sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("reading legacy session records: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // entry is from Git-owned state directory
		if readErr != nil {
			continue
		}
		var record legacySessionRecord
		if json.Unmarshal(data, &record) != nil || !isLegacyActivePhase(record.Phase) || hasNonNullJSON(record.EndedAt) {
			continue
		}
		if canonicalPath(record.WorktreePath) == repository.WorktreeRoot && record.WorktreeID == repository.WorktreeID {
			return true, nil
		}
	}
	return false, nil
}

func isLegacyActivePhase(phase string) bool {
	return phase == "active" || phase == "active_committed"
}

func hasNonNullJSON(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

var legacyMetadataFiles = map[string]struct{}{
	"prompt.txt":       {},
	"full.jsonl":       {},
	"full.log":         {},
	"transcript.jsonl": {},
	"metadata.json":    {},
	"checkpoint.json":  {},
	"content_hash.txt": {},
}

func hasRecognizedMetadataEvidence(ctx context.Context, repository Repository) (bool, error) {
	metadataDir := filepath.Join(repository.WorktreeRoot, ".entire", "metadata") // entire-join-ok: legacy provenance must inspect the literal worktree, never the runtime route
	if err := verifyPathComponents(repository.WorktreeRoot, filepath.FromSlash(".entire/metadata"), true); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, nil
	}
	sessions, err := os.ReadDir(metadataDir)
	if err != nil {
		return false, fmt.Errorf("reading legacy metadata: %w", err)
	}
	for _, sessionEntry := range sessions {
		if !sessionEntry.IsDir() || sessionEntry.Type()&os.ModeSymlink != 0 {
			continue
		}
		sessionRel := filepath.Join(".entire", "metadata", sessionEntry.Name())
		if err := verifyPathComponents(repository.WorktreeRoot, sessionRel, true); err != nil {
			continue
		}
		files, readErr := os.ReadDir(filepath.Join(repository.WorktreeRoot, sessionRel))
		if readErr != nil {
			continue
		}
		for _, file := range files {
			if _, recognized := legacyMetadataFiles[file.Name()]; !recognized || file.IsDir() || file.Type()&os.ModeSymlink != 0 {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(sessionRel, file.Name()))
			if err := verifyPathComponents(repository.WorktreeRoot, filepath.FromSlash(rel), false); err != nil {
				continue
			}
			tracked, trackErr := gitPathTracked(ctx, repository.WorktreeRoot, rel)
			if trackErr != nil {
				return false, trackErr
			}
			if !tracked {
				return true, nil
			}
		}
	}
	return false, nil
}

func gitPathTracked(ctx context.Context, root, rel string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "--error-unmatch", "--", rel)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("checking runtime evidence ownership: %w", err)
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decoding JSON object: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return fmt.Errorf("decoding trailing JSON value: %w", err)
	}
	return nil
}

func verifyPathComponents(root, rel string, finalDir bool) error {
	if !filepath.IsAbs(root) || filepath.IsAbs(rel) {
		return errors.New("path provenance requires absolute root and relative path")
	}
	current := root
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return errors.New("invalid worktree-relative path")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspecting path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %s is a symlink", filepath.Join(parts[:i+1]...))
		}
		isFinal := i == len(parts)-1
		if (!isFinal || finalDir) && !info.IsDir() {
			return fmt.Errorf("path component %s is not a directory", filepath.Join(parts[:i+1]...))
		}
		if isFinal && !finalDir && !info.Mode().IsRegular() {
			return fmt.Errorf("path component %s is not a regular file", filepath.Join(parts[:i+1]...))
		}
	}
	return nil
}
