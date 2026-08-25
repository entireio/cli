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
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/gitpath"
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
	return ensureRegistryDirWithSync(repository, jsonutil.SyncDir)
}

func ensureRegistryDirWithSync(repository Repository, syncDir func(string) error) error {
	parent := repository.GitCommonDir
	for _, name := range []string{"entire", "worktree", repository.WorktreeKey} {
		dir := filepath.Join(parent, name)
		err := os.Mkdir(dir, 0o700)
		switch {
		case err == nil:
			if err := syncDir(parent); err != nil {
				return fmt.Errorf("syncing repository policy registry parent: %w", err)
			}
		case errors.Is(err, fs.ErrExist):
			info, statErr := os.Lstat(dir)
			if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("verifying repository policy registry: %w", errUnverifiedPath)
			}
		default:
			return fmt.Errorf("creating repository policy registry: %w", err)
		}
		if err != nil {
			if syncErr := syncDir(parent); syncErr != nil {
				return fmt.Errorf("syncing repository policy registry parent: %w", syncErr)
			}
		}
		parent = dir
	}
	if err := os.Chmod(registryDir(repository), 0o700); err != nil { //nolint:gosec // registry directories require owner-only traversal
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

// RebindMovedRepository repairs identity-bound machine-local records after the
// repository directory itself moves. Recovery requires an active session tied
// to the old worktree identity and the old path must no longer exist, so a
// copied registry cannot activate another checkout.
func RebindMovedRepository(ctx context.Context, repository Repository) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("rebinding moved repository: %w", err)
	}
	if err := validateRepository(repository); err != nil {
		return false, err
	}
	release, err := acquireTrustGrantLedgerLock(ctx, repository)
	if err != nil {
		return false, fmt.Errorf("locking repository relocation: %w", err)
	}
	defer release()
	return rebindMovedRepositoryLocked(repository)
}

func rebindMovedRepositoryLocked(repository Repository) (bool, error) {
	var activation ActivationRecord
	activationFound, err := readOptionalPolicyRecord(activationPath(repository), &activation)
	if err != nil {
		return false, fmt.Errorf("reading activation record for relocation: %w", err)
	}
	var route RuntimeRoute
	routeFound, err := readOptionalPolicyRecord(routePath(repository), &route)
	if err != nil {
		return false, fmt.Errorf("reading runtime route for relocation: %w", err)
	}
	if !activationFound && !routeFound {
		return false, nil
	}
	if activationFound {
		if activation.Version != recordVersion || activation.State != ActivationEnabled {
			return false, nil
		}
	}
	if routeFound && (route.Version != recordVersion || (route.Layout != RuntimeWorktree && route.Layout != RuntimeGitCommon)) {
		return false, nil
	}

	oldWorktree, oldGitCommon := "", ""
	for _, identity := range [][2]string{
		{activation.CanonicalWorktree, activation.CanonicalGitCommon},
		{route.CanonicalWorktree, route.CanonicalGitCommon},
	} {
		if identity[0] == "" && identity[1] == "" {
			continue
		}
		if identity[0] == repository.WorktreeRoot && identity[1] == repository.GitCommonDir {
			continue
		}
		if oldWorktree == "" {
			oldWorktree, oldGitCommon = filepath.Clean(identity[0]), filepath.Clean(identity[1])
			continue
		}
		if filepath.Clean(identity[0]) != oldWorktree || filepath.Clean(identity[1]) != oldGitCommon {
			return false, errors.New("repository relocation records disagree on prior identity")
		}
	}
	if oldWorktree == "" || !filepath.IsAbs(oldWorktree) || !filepath.IsAbs(oldGitCommon) {
		return false, nil
	}
	if exists, statErr := policyPathExists(oldWorktree); statErr != nil || exists {
		return false, statErr
	}
	if oldGitCommon != repository.GitCommonDir {
		if exists, statErr := policyPathExists(oldGitCommon); statErr != nil || exists {
			return false, statErr
		}
	}
	evidence, err := hasActiveSessionEvidence(repository, oldWorktree, repository.WorktreeID, func(string) verifiedReadHooks {
		return verifiedReadHooks{}
	})
	if err != nil || !evidence {
		return false, err
	}

	var setup SetupRecord
	setupFound, err := readOptionalPolicyRecord(setupPath(repository), &setup)
	if err != nil {
		return false, fmt.Errorf("reading setup record for relocation: %w", err)
	}
	var ledger TrustGrantLedger
	ledgerFound, err := readOptionalPolicyRecord(trustLedgerPath(repository), &ledger)
	if err != nil {
		return false, fmt.Errorf("reading trust ledger for relocation: %w", err)
	}
	for name, identity := range map[string][2]string{
		"setup record": {setup.CanonicalWorktree, setup.CanonicalGitCommon},
		"trust ledger": {ledger.CanonicalWorktree, ledger.CanonicalGitCommon},
	} {
		if (name == "setup record" && !setupFound) || (name == "trust ledger" && !ledgerFound) {
			continue
		}
		if identity != [2]string{oldWorktree, oldGitCommon} && identity != [2]string{repository.WorktreeRoot, repository.GitCommonDir} {
			return false, fmt.Errorf("%s does not match repository relocation identity", name)
		}
	}
	if setupFound && (setup.Version != recordVersion || setup.GitHooksSpec < 0 || setup.PrimaryRefSpec < 0) {
		return false, errors.New("invalid setup record during repository relocation")
	}
	if ledgerFound && ledger.Version != recordVersion {
		return false, errors.New("invalid trust ledger during repository relocation")
	}

	if routeFound {
		route.CanonicalWorktree = repository.WorktreeRoot
		route.CanonicalGitCommon = repository.GitCommonDir
		if err := writePolicyRecord(routePath(repository), route); err != nil {
			return false, fmt.Errorf("rebinding runtime route: %w", err)
		}
	}
	if setupFound {
		setup.CanonicalWorktree = repository.WorktreeRoot
		setup.CanonicalGitCommon = repository.GitCommonDir
		if err := writePolicyRecord(setupPath(repository), setup); err != nil {
			return false, fmt.Errorf("rebinding setup record: %w", err)
		}
	}
	if ledgerFound {
		ledger.CanonicalWorktree = repository.WorktreeRoot
		ledger.CanonicalGitCommon = repository.GitCommonDir
		if err := writePolicyRecord(trustLedgerPath(repository), ledger); err != nil {
			return false, fmt.Errorf("rebinding trust ledger: %w", err)
		}
	}
	if activationFound {
		if err := setLocalActivation(repository, ActivationEnabled); err != nil {
			return false, fmt.Errorf("rebinding activation record: %w", err)
		}
	}
	return true, nil
}

func readOptionalPolicyRecord(path string, target any) (bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // callers provide package-owned registry paths, never user-supplied paths
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read policy record: %w", err)
	}
	if err := decodeStrict(data, target); err != nil {
		return false, err
	}
	return true, nil
}

func writePolicyRecord(path string, value any) error {
	data, err := jsonutil.MarshalIndentWithNewline(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode policy record: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write policy record: %w", err)
	}
	return nil
}

func policyPathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("inspect prior repository path: %w", err)
}

func enabledFromSettingsData(data []byte) (*bool, error) {
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
	return effectiveEnabledSettingWithOptions(ctx, repository, defaultSettingsProvenanceOptions())
}

func effectiveEnabledSettingWithOptions(
	ctx context.Context,
	repository Repository,
	options settingsProvenanceOptions,
) (*bool, error) {
	provenance, err := verifySettingsProvenanceWithOptions(ctx, repository, options)
	if err != nil {
		return nil, err
	}
	var effective *bool
	for _, candidate := range []struct {
		data     []byte
		verified bool
	}{
		{data: provenance.ProjectData, verified: provenance.ProjectVerified},
		{
			data: provenance.LocalData,
			verified: provenance.LocalPathSafe &&
				provenance.LocalOwnership == SettingsOwnershipUntracked,
		},
	} {
		if !candidate.verified {
			continue
		}
		value, err := enabledFromSettingsData(candidate.data)
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
	return hasExactSessionEvidenceWithHooks(repository, func(string) verifiedReadHooks { return verifiedReadHooks{} })
}

func hasExactSessionEvidenceWithHooks(repository Repository, hooks func(string) verifiedReadHooks) (bool, error) {
	return hasActiveSessionEvidence(repository, repository.WorktreeRoot, repository.WorktreeID, hooks)
}

func hasActiveSessionEvidence(
	repository Repository,
	worktreeRoot string,
	worktreeID string,
	hooks func(string) verifiedReadHooks,
) (bool, error) {
	root, err := os.OpenRoot(repository.GitCommonDir)
	if err != nil {
		return false, fmt.Errorf("opening Git common root: %w", err)
	}
	defer root.Close()
	if _, err := lstatRootPath(root, "entire-sessions", true); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, nil
	}
	dir, err := root.Open("entire-sessions")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("reading legacy session records: %w", err)
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return false, fmt.Errorf("reading legacy session records: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		rel := filepath.Join("entire-sessions", entry.Name())
		data, readErr := readVerifiedRegular(root, rel, nil, hooks(rel))
		if readErr != nil {
			continue
		}
		var record legacySessionRecord
		if json.Unmarshal(data, &record) != nil || !isLegacyActivePhase(record.Phase) || hasNonNullJSON(record.EndedAt) {
			continue
		}
		if canonicalPath(record.WorktreePath) == canonicalPath(worktreeRoot) && record.WorktreeID == worktreeID {
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

const (
	legacyMetadataGitBudget  = 500 * time.Millisecond
	legacyMetadataScanBudget = 250 * time.Millisecond
)

var errLegacyMetadataBudget = errors.New("legacy metadata scan budget exceeded")

type legacyMetadataOptions struct {
	loadTracked func(context.Context, Repository) (map[string]struct{}, error)
	now         func() time.Time
	scanBudget  time.Duration
	readHooks   func(string) verifiedReadHooks
}

func defaultLegacyMetadataOptions() legacyMetadataOptions {
	return legacyMetadataOptions{
		loadTracked: loadTrackedMetadataPaths,
		now:         time.Now,
		scanBudget:  legacyMetadataScanBudget,
		readHooks:   func(string) verifiedReadHooks { return verifiedReadHooks{} },
	}
}

func hasRecognizedMetadataEvidence(ctx context.Context, repository Repository) (bool, error) {
	return hasRecognizedMetadataEvidenceWithOptions(ctx, repository, defaultLegacyMetadataOptions())
}

func hasRecognizedMetadataEvidenceWithOptions(ctx context.Context, repository Repository, options legacyMetadataOptions) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("scanning legacy metadata: %w", err)
	}
	root, err := os.OpenRoot(repository.WorktreeRoot)
	if err != nil {
		return false, fmt.Errorf("opening worktree root: %w", err)
	}
	defer root.Close()
	metadataRel := filepath.FromSlash(".entire/metadata")
	if _, err := lstatRootPath(root, metadataRel, true); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("scanning legacy metadata: %w", err)
	}
	tracked, err := options.loadTracked(ctx, repository)
	if err != nil {
		return false, err
	}
	started := options.now()
	budgetExpired := func() bool { return options.now().Sub(started) > options.scanBudget }
	if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
		return false, err
	}
	dir, err := root.Open(metadataRel)
	if err != nil {
		return false, fmt.Errorf("reading legacy metadata: %w", err)
	}
	defer dir.Close()
	for {
		if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
			return false, err
		}
		sessions, readErr := dir.ReadDir(32)
		if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
			return false, err
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, fmt.Errorf("reading legacy metadata: %w", readErr)
		}
		for _, sessionEntry := range sessions {
			if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
				return false, err
			}
			if !sessionEntry.IsDir() || sessionEntry.Type()&os.ModeSymlink != 0 {
				continue
			}
			sessionRel := filepath.Join(".entire", "metadata", sessionEntry.Name())
			found, evidenceErr := recognizedMetadataInSession(ctx, root, sessionRel, tracked, options, budgetExpired)
			if evidenceErr != nil || found {
				return found, evidenceErr
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return false, nil
}

// legacyMetadataScanCheckpoint makes the filesystem budget and cancellation
// cooperative between syscalls. It cannot interrupt a filesystem call that is
// already blocked.
func legacyMetadataScanCheckpoint(ctx context.Context, budgetExpired func() bool) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scanning legacy metadata: %w", err)
	}
	if budgetExpired() {
		return errLegacyMetadataBudget
	}
	return nil
}

func recognizedMetadataInSession(
	ctx context.Context,
	root *os.Root,
	sessionRel string,
	tracked map[string]struct{},
	options legacyMetadataOptions,
	budgetExpired func() bool,
) (bool, error) {
	if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
		return false, err
	}
	if _, err := lstatRootPath(root, sessionRel, true); err != nil {
		return false, nil
	}
	if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
		return false, err
	}
	sessionDir, err := root.Open(sessionRel)
	if err != nil {
		return false, nil
	}
	defer sessionDir.Close()
	for {
		if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
			return false, err
		}
		files, readErr := sessionDir.ReadDir(32)
		if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
			return false, err
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, nil
		}
		for _, file := range files {
			if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
				return false, err
			}
			if _, recognized := legacyMetadataFiles[file.Name()]; !recognized || file.IsDir() || file.Type()&os.ModeSymlink != 0 {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(sessionRel, file.Name()))
			if readErr := verifyRegular(root, filepath.FromSlash(rel), options.readHooks(rel)); readErr != nil {
				continue
			}
			if err := legacyMetadataScanCheckpoint(ctx, budgetExpired); err != nil {
				return false, err
			}
			if _, isTracked := tracked[gitpath.CanonicalKey(rel)]; !isTracked {
				return true, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return false, nil
}

func loadTrackedMetadataPaths(ctx context.Context, repository Repository) (map[string]struct{}, error) {
	gitCtx, cancel := context.WithTimeout(ctx, legacyMetadataGitBudget)
	defer cancel()
	cmd := exec.CommandContext(gitCtx, "git", "-C", repository.WorktreeRoot, "ls-files", "-z")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("loading tracked runtime evidence: %w", err)
	}
	tracked := make(map[string]struct{})
	for _, name := range bytes.Split(output, []byte{0}) {
		if len(name) != 0 {
			tracked[gitpath.CanonicalKey(filepath.ToSlash(string(name)))] = struct{}{}
		}
	}
	return tracked, nil
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
