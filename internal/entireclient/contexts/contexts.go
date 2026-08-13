// Package contexts stores the CLI's current login metadata.
//
// The metadata is kept in the same credential backend as the tokens (the OS
// keychain by default), under one fixed logical entry per Entire config dir.
// contexts.json is read only once to migrate existing installations.
//
// Context and File remain as compatibility shapes while consumers move to the
// single-login model; production writes contain at most one context.
package contexts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/gofrs/flock"
)

// Context is the legacy-compatible shape of the current login.
type Context struct {
	// Name is retained only to decode and migrate the former context format.
	Name string `json:"name"`
	// CoreURL is the JWT issuer URL — what STS exchanges hit. Set from
	// the access token's signed iss claim, not the typed login URL.
	CoreURL string `json:"core_url"`
	// Handle is the principal handle returned from /api/auth/token.
	Handle string `json:"handle"`
	// KeychainService is the OS-keyring slot where the access token is
	// filed; the refresh token lives at KeychainService+":refresh".
	KeychainService string `json:"keychain_service"`
	// JurisdictionAudiences lists the audiences this context has a jurisdiction
	// (data-plane) access token filed for, trailing-slash-trimmed; each lives at
	// tokenstore.JurisdictionService(audience), also keyed by Handle.
	JurisdictionAudiences []string `json:"jurisdiction_audiences,omitempty"`
}

// File is the legacy-compatible serialized shape stored in the credential
// backend. New writes contain exactly one current login.
type File struct {
	// Version identifies the keychain metadata format. Legacy contexts.json
	// files omit it and are treated as version zero during migration.
	Version int `json:"version,omitempty"`
	// CurrentContext points to the sole login for legacy format compatibility.
	CurrentContext string `json:"current_context,omitempty"`
	// Contexts contains zero or one login in all new writes.
	Contexts []*Context `json:"contexts,omitempty"`
}

const (
	loginMetadataService = "entire-login"
	loginMetadataVersion = 1
)

// FilePath returns the former contexts.json path. It is retained only for the
// one-time migration and can be removed after the compatibility window.
func FilePath(configDir string) (string, error) {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return "", fmt.Errorf("create config dir: %w", err)
	}
	return filepath.Join(configDir, "contexts.json"), nil
}

// metadataAccount scopes the fixed "current" entry to ENTIRE_CONFIG_DIR. This
// preserves config-dir isolation for development, CI, and parallel test runs
// while giving normal installations one stable keychain item.
func metadataAccount(configDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(configDir)))
	return "current:" + hex.EncodeToString(sum[:8])
}

// Load reads the current login metadata from the credential backend. A missing
// entry means logged out. Existing contexts.json data is migrated on first use.
func Load(configDir string) (*File, error) {
	path, err := FilePath(configDir)
	if err != nil {
		return nil, err
	}
	unlock, err := lockFile(filepath.Join(configDir, "login"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	return readNoLock(configDir, path)
}

// Save writes login metadata to the credential backend under its fixed entry.
func Save(configDir string, f *File) error {
	path, err := FilePath(configDir)
	if err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(configDir, "login"))
	if err != nil {
		return err
	}
	defer unlock()
	return writeNoLock(configDir, path, f)
}

// Find returns the context with the given name, or nil.
func (f *File) Find(name string) *Context {
	if f == nil || name == "" {
		return nil
	}
	for _, c := range f.Contexts {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Delete clears the matching current login.
func (f *File) Delete(name string) {
	if f == nil || name == "" {
		return
	}
	idx := slices.IndexFunc(f.Contexts, func(c *Context) bool { return c.Name == name })
	if idx >= 0 {
		f.Contexts = slices.Delete(f.Contexts, idx, idx+1)
	}
	if f.CurrentContext == name {
		f.CurrentContext = ""
	}
}

// Modify atomically applies fn to the keychain metadata under a single
// exclusive flock — load, mutate, write all happen with the lock held.
// Use this for any read-modify-write sequence; calling Load and Save
// separately releases the lock between them and races concurrent
// writers (e.g. a parallel login and logout).
//
// fn returns (changed, err). When changed is false the file isn't
// rewritten — useful for idempotent operations that often have
// nothing to do. When err is non-nil the change is discarded.
func Modify(configDir string, fn func(*File) (changed bool, err error)) error {
	path, err := FilePath(configDir)
	if err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(configDir, "login"))
	if err != nil {
		return err
	}
	defer unlock()

	f, err := readNoLock(configDir, path)
	if err != nil {
		return err
	}
	changed, err := fn(f)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return writeNoLock(configDir, path, f)
}

func lockFile(path string) (func(), error) {
	lockPath := path + ".lock"
	fl := flock.New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked, err := fl.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquire login lock: %w", err)
	}
	if !locked {
		return nil, errors.New("timeout acquiring login lock")
	}
	return func() {
		if unlockErr := fl.Unlock(); unlockErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to unlock login store: %v\n", unlockErr)
		}
	}, nil
}

func readNoLock(configDir, legacyPath string) (*File, error) {
	encoded, err := tokenstore.Get(loginMetadataService, metadataAccount(configDir))
	if err == nil {
		var f File
		if jsonErr := json.Unmarshal([]byte(encoded), &f); jsonErr != nil {
			return nil, fmt.Errorf("parse login metadata: %w", jsonErr)
		}
		if f.Version > loginMetadataVersion {
			return nil, fmt.Errorf("login metadata version %d is newer than this CLI supports", f.Version)
		}
		return &f, nil
	}
	if !errors.Is(err, tokenstore.ErrNotFound) {
		return nil, fmt.Errorf("read login metadata: %w", err)
	}

	// One-time migration from contexts.json. Only the active login survives;
	// the old multi-login list is intentionally collapsed.
	legacy, err := readLegacyFile(legacyPath)
	if err != nil {
		return nil, err
	}
	current := legacy.Find(legacy.CurrentContext)
	if current == nil {
		_ = os.Remove(legacyPath)
		return &File{}, nil
	}
	// Old non-current logins are no longer addressable. Remove their access
	// and refresh credentials while their metadata is still available.
	currentAudiences := make(map[string]bool, len(current.JurisdictionAudiences))
	for _, audience := range current.JurisdictionAudiences {
		currentAudiences[audience] = true
	}
	for _, old := range legacy.Contexts {
		if old == nil || old.Name == current.Name || old.Handle == "" {
			continue
		}
		if old.KeychainService != "" {
			_ = tokenstore.Delete(tokenstore.RefreshService(old.KeychainService), old.Handle) //nolint:errcheck // best-effort migration cleanup
			_ = tokenstore.Delete(old.KeychainService, old.Handle)                            //nolint:errcheck // best-effort migration cleanup
		}
		for _, audience := range old.JurisdictionAudiences {
			if audience == "" || (old.Handle == current.Handle && currentAudiences[audience]) {
				continue
			}
			_ = tokenstore.Delete(tokenstore.JurisdictionService(audience), old.Handle) //nolint:errcheck // best-effort migration cleanup
		}
	}
	migrated := &File{Version: loginMetadataVersion, CurrentContext: current.Name, Contexts: []*Context{current}}
	if err := writeCredential(configDir, migrated); err != nil {
		return nil, err
	}
	if err := os.Remove(legacyPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove migrated contexts file: %w", err)
	}
	return migrated, nil
}

func readLegacyFile(path string) (*File, error) {
	// #nosec G304 -- path comes from ENTIRE_CONFIG_DIR or the user's home.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, fmt.Errorf("read legacy contexts file: %w", err)
	}
	if len(data) == 0 {
		return &File{}, nil
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse legacy contexts file: %w", err)
	}
	return &f, nil
}

func writeNoLock(configDir, legacyPath string, f *File) error {
	if err := writeCredential(configDir, f); err != nil {
		return err
	}
	if err := os.Remove(legacyPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove legacy contexts file: %w", err)
	}
	return nil
}

func writeCredential(configDir string, f *File) error {
	if f == nil || len(f.Contexts) == 0 || f.CurrentContext == "" {
		if err := tokenstore.Delete(loginMetadataService, metadataAccount(configDir)); err != nil && !errors.Is(err, tokenstore.ErrNotFound) {
			return fmt.Errorf("delete login metadata: %w", err)
		}
		return nil
	}
	current := f.Find(f.CurrentContext)
	if current == nil {
		return errors.New("current login metadata points to a missing entry")
	}
	// Enforce the single-login invariant even when a legacy-shaped caller
	// supplies more than one entry.
	stored := &File{Version: loginMetadataVersion, CurrentContext: current.Name, Contexts: []*Context{current}}
	data, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("marshal login metadata: %w", err)
	}
	if err := tokenstore.Set(loginMetadataService, metadataAccount(configDir), string(data)); err != nil {
		return fmt.Errorf("store login metadata: %w", err)
	}
	return nil
}
