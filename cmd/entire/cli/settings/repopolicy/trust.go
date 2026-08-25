package repopolicy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

const (
	trustLedgerFileName = "trust.json"
	trustLedgerLockWait = 2 * time.Second
)

// TrustGrantLedger tracks only consent keys written by Entire for this
// worktree. It intentionally does not mirror hand-authored user settings.
type TrustGrantLedger struct {
	Version            int      `json:"version"`
	CanonicalWorktree  string   `json:"canonical_worktree"`
	CanonicalGitCommon string   `json:"canonical_git_common"`
	OriginKeys         []string `json:"origin_keys,omitempty"`
	Paths              []string `json:"paths,omitempty"`
}

func trustLedgerPath(repository Repository) string {
	return filepath.Join(registryDir(repository), trustLedgerFileName)
}

// ResolveTrustIdentity derives an exclusive origin or path identity. A
// configured but unparseable origin is an error; path identity is allowed only
// when origin is absent.
func ResolveTrustIdentity(ctx context.Context, repository Repository) (TrustIdentity, error) {
	urls, fetchFound, err := gitremote.GetRemoteURLsInDirIfSet(ctx, repository.WorktreeRoot, "origin")
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("reading origin remote: %w", err)
	}
	pushURLs, pushFound, err := gitremote.GetRemotePushURLsInDirIfSet(ctx, repository.WorktreeRoot, "origin")
	if err != nil {
		return TrustIdentity{}, fmt.Errorf("reading origin pushurl: %w", err)
	}
	urls = append(urls, pushURLs...)
	if fetchFound || pushFound {
		keys := make([]string, 0, len(urls))
		for _, raw := range urls {
			key := NormalizeOrigin(raw)
			if key == "" {
				return TrustIdentity{}, errors.New("configured origin cannot be normalized")
			}
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		if len(keys) == 0 {
			return TrustIdentity{}, errors.New("configured origin has no usable URLs")
		}
		return TrustIdentity{OriginKeys: keys}, nil
	}
	return TrustIdentity{Path: repository.WorktreeRoot}, nil
}

// DecideEgress computes the single trust decision stored in RepoPolicy.
func DecideEgress(ctx context.Context, policy RepoPolicy, global *GlobalConfig, repository Repository) TrustDecision {
	if !policy.Active {
		return TrustDecision{Source: TrustSourceNone, Reason: TrustReasonInactive}
	}
	if policy.ActivationSource == ActivationLocal {
		return TrustDecision{Allowed: true, Source: TrustSourceLocal}
	}
	if global == nil || !global.Enabled {
		return TrustDecision{Source: TrustSourceNone, Reason: TrustReasonInactive}
	}
	identity, err := ResolveTrustIdentity(ctx, repository)
	if err != nil {
		return TrustDecision{Source: TrustSourceNone, Reason: TrustReasonInvalidOrigin}
	}
	decision := TrustDecision{Source: TrustSourceNone, Reason: TrustReasonUntrusted, Identity: identity}
	if global.TrustAll {
		decision.Allowed = true
		decision.Source = TrustSourceAll
		decision.Reason = TrustReasonNone
		return decision
	}
	if identityTrusted(ctx, global, identity) {
		decision.Allowed = true
		decision.Source = TrustSourceRepo
		decision.Reason = TrustReasonNone
	}
	return decision
}

func identityTrusted(ctx context.Context, global *GlobalConfig, identity TrustIdentity) bool {
	if identity.OriginKeyed() {
		for _, key := range identity.OriginKeys {
			if !containsOrigin(global.TrustedOrigins, key) {
				return false
			}
		}
		return true
	}
	matched, err := MatchesExcludePathExact(ctx, global.TrustedPaths, identity.Path)
	return err == nil && matched
}

func containsOrigin(entries []string, key string) bool {
	for _, entry := range entries {
		if CanonicalTrustOrigin(entry) == key {
			return true
		}
	}
	return false
}

// CanonicalTrustOrigin normalizes a hand-edited exact trust key.
func CanonicalTrustOrigin(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// ReadTrustGrantLedger returns the verified machine-local grant history.
func ReadTrustGrantLedger(repository Repository) (TrustGrantLedger, bool, error) {
	if err := validateRepository(repository); err != nil {
		return TrustGrantLedger{}, false, err
	}
	data, err := os.ReadFile(trustLedgerPath(repository))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return TrustGrantLedger{}, false, nil
		}
		return TrustGrantLedger{}, false, fmt.Errorf("reading trust ledger: %w", err)
	}
	var ledger TrustGrantLedger
	if err := decodeStrict(data, &ledger); err != nil {
		return TrustGrantLedger{}, false, fmt.Errorf("parsing trust ledger: %w", err)
	}
	if ledger.Version != recordVersion {
		return TrustGrantLedger{}, false, fmt.Errorf("unsupported trust ledger version %d", ledger.Version)
	}
	if ledger.CanonicalWorktree != repository.WorktreeRoot || ledger.CanonicalGitCommon != repository.GitCommonDir {
		return TrustGrantLedger{}, false, errors.New("trust ledger repository identity mismatch")
	}
	return ledger, true, nil
}

// WriteTrustGrantLedger atomically records CLI-owned consent keys.
func WriteTrustGrantLedger(repository Repository, ledger TrustGrantLedger) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if err := ensureRegistryDir(repository); err != nil {
		return err
	}
	ledger.Version = recordVersion
	ledger.CanonicalWorktree = repository.WorktreeRoot
	ledger.CanonicalGitCommon = repository.GitCommonDir
	data, err := jsonutil.MarshalIndentWithNewline(ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding trust ledger: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(trustLedgerPath(repository), data, 0o600); err != nil {
		return fmt.Errorf("writing trust ledger: %w", err)
	}
	return nil
}

// ModifyTrustGrantLedger serializes a consent mutation with repository
// relocation. It clears the old ownership record before invoking fn, so a
// failed user-settings write can only forget CLI ownership; it can never leave
// stale ownership that a later revoke could apply to a hand-authored grant.
func ModifyTrustGrantLedger(
	ctx context.Context,
	repository Repository,
	fn func(TrustGrantLedger) (TrustGrantLedger, error),
) error {
	release, err := acquireTrustGrantLedgerLock(ctx, repository)
	if err != nil {
		return err
	}
	defer release()

	ledger, _, err := ReadTrustGrantLedger(repository)
	if err != nil {
		return err
	}
	if err := WriteTrustGrantLedger(repository, TrustGrantLedger{}); err != nil {
		return fmt.Errorf("clearing trust ledger ownership: %w", err)
	}
	next, err := fn(ledger)
	if err != nil {
		return err
	}
	if err := WriteTrustGrantLedger(repository, next); err != nil {
		return fmt.Errorf("recording trust ledger ownership: %w", err)
	}
	return nil
}

func acquireTrustGrantLedgerLock(ctx context.Context, repository Repository) (func(), error) {
	if err := validateRepository(repository); err != nil {
		return nil, err
	}
	if err := ensureRegistryDir(repository); err != nil {
		return nil, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, trustLedgerLockWait)
	defer cancel()
	release, err := flock.AcquireContext(lockCtx, trustLedgerPath(repository)+".lock")
	if err != nil {
		return nil, fmt.Errorf("locking trust grant ledger: %w", err)
	}
	return release, nil
}
