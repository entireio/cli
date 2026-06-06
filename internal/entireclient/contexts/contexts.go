// Package contexts is the kubectl-style context store for the Entire CLIs.
//
// One on-disk file at $ENTIRE_CONFIG_DIR/contexts.json (default
// ~/.config/entire/contexts.json) holds:
//
//   - a list of named contexts, each pairing a core URL, principal handle,
//     and OS-keychain slot where the access + refresh tokens live;
//   - current_context, the active login: the preferred identity for cluster
//     operations and the default for direct CLI commands not tied to a
//     cluster.
//
// File invariants: 0600, atomic temp+rename, exclusive flock under load.
// Both CLIs share the same file so a login from either is visible to the
// other.
package contexts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/gofrs/flock"
)

// Context is a single kubectl-style entry: which core to talk to, as
// whom, and where the credentials are stored.
type Context struct {
	// Name is the user-facing identifier. Defaults to the issuer host on
	// auto-creation; overridable via login --name.
	Name string `json:"name"`
	// CoreURL is the JWT issuer URL — what STS exchanges hit. Set from
	// the access token's signed iss claim, not the typed login URL.
	CoreURL string `json:"core_url"`
	// Handle is the principal handle returned from /api/auth/token.
	Handle string `json:"handle"`
	// KeychainService is the OS-keyring slot where the access token is
	// filed; the refresh token lives at KeychainService+":refresh".
	KeychainService string `json:"keychain_service"`
}

// File is the on-disk shape of contexts.json.
type File struct {
	// CurrentContext is the active login; preferred identity for cluster
	// operations (used when its CoreURL is eligible for the target cluster)
	// and the default for direct CLI commands. Empty until the first login.
	CurrentContext string `json:"current_context,omitempty"`
	// Contexts is the list of stored credentials. Order is preserved on
	// disk so list output stays stable across saves.
	Contexts []*Context `json:"contexts,omitempty"`
}

// FilePath returns $configDir/contexts.json after ensuring the directory
// exists with 0700 perms.
func FilePath(configDir string) (string, error) {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return "", fmt.Errorf("create config dir: %w", err)
	}
	return filepath.Join(configDir, "contexts.json"), nil
}

// DefaultConfigDir is $ENTIRE_CONFIG_DIR if set, else ~/.config/entire.
func DefaultConfigDir() string {
	if dir := os.Getenv("ENTIRE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "entire")
}

// Load reads contexts.json under configDir, returning an empty *File
// when the file doesn't exist yet (a fresh user). Holds an exclusive
// flock for the duration of the read.
func Load(configDir string) (*File, error) {
	path, err := FilePath(configDir)
	if err != nil {
		return nil, err
	}
	unlock, err := lockFile(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return readNoLock(path)
}

// Save writes f to contexts.json atomically (temp+rename) under an
// exclusive flock.
func Save(configDir string, f *File) error {
	path, err := FilePath(configDir)
	if err != nil {
		return err
	}
	unlock, err := lockFile(path)
	if err != nil {
		return err
	}
	defer unlock()
	return writeNoLock(path, f)
}

// ContextsForIssuer returns every context whose CoreURL matches issuer
// after trimming. Used by CLI flows that prompt the operator when
// multiple sessions are stored against the same core (logout, entiredb's
// admin/repo prompts). Order matches the on-disk order so prompt
// numbering is stable across saves.
func (f *File) ContextsForIssuer(issuer string) []*Context {
	if f == nil {
		return nil
	}
	want := trimURL(issuer)
	var out []*Context
	for _, c := range f.Contexts {
		if trimURL(c.CoreURL) == want {
			out = append(out, c)
		}
	}
	return out
}

func trimURL(u string) string {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
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

// Upsert replaces the context with matching Name, or appends. Sets the
// current context when there isn't one already (first login).
func (f *File) Upsert(c *Context) {
	if c == nil || c.Name == "" {
		return
	}
	for i, existing := range f.Contexts {
		if existing.Name == c.Name {
			f.Contexts[i] = c
			if f.CurrentContext == "" {
				f.CurrentContext = c.Name
			}
			return
		}
	}
	f.Contexts = append(f.Contexts, c)
	if f.CurrentContext == "" {
		f.CurrentContext = c.Name
	}
}

// Delete drops the context with the given name. If it was the current
// context, current_context is cleared — never reassigned to another
// context, so deleting your active login never silently switches you to a
// different identity.
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

// Modify atomically applies fn to contexts.json under a single
// exclusive flock — load, mutate, write all happen with the lock held.
// Use this for any read-modify-write sequence; calling Load and Save
// separately releases the lock between them and races concurrent
// writers (e.g. a parallel login recording a context).
//
// fn returns (changed, err). When changed is false the file isn't
// rewritten — useful for idempotent operations that often have
// nothing to do. When err is non-nil the change is discarded.
func Modify(configDir string, fn func(*File) (changed bool, err error)) error {
	path, err := FilePath(configDir)
	if err != nil {
		return err
	}
	unlock, err := lockFile(path)
	if err != nil {
		return err
	}
	defer unlock()

	f, err := readNoLock(path)
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
	return writeNoLock(path, f)
}

func lockFile(path string) (func(), error) {
	lockPath := path + ".lock"
	fl := flock.New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked, err := fl.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquire contexts lock: %w", err)
	}
	if !locked {
		return nil, errors.New("timeout acquiring lock on contexts file")
	}
	return func() {
		if unlockErr := fl.Unlock(); unlockErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to unlock contexts file: %v\n", unlockErr)
		}
	}, nil
}

func readNoLock(path string) (*File, error) {
	// #nosec G304 -- path comes from ENTIRE_CONFIG_DIR or the user's home,
	// the same trust boundary credentials.go runs under.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, fmt.Errorf("read contexts file: %w", err)
	}
	if len(data) == 0 {
		return &File{}, nil
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse contexts file: %w", err)
	}
	return &f, nil
}

func writeNoLock(path string, f *File) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal contexts: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".contexts.json.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp contexts file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if tmp != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}
	defer cleanup()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp contexts file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp contexts file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp contexts file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return fmt.Errorf("chmod temp contexts file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp contexts file: %w", err)
	}
	tmp = nil
	return nil
}
