package tokenstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// goosWindows is runtime.GOOS on Windows, where unix permission bits don't
// exist (Go reports synthetic modes) so permission checks are skipped.
const goosWindows = "windows"

// loosePermsWarnW receives the loose-permissions warning. Package-level so
// tests can capture it; production always writes to stderr (matching the
// unlock warning in withFileLock).
var loosePermsWarnW io.Writer = os.Stderr

// fileStore persists credentials as a JSON file on disk.
// The file format is: { "service": { "user": "password" } }
type fileStore struct {
	path string
	// ownsDir reports whether filepath.Dir(path) is a directory Entire
	// chose — the per-user config dir it shares with contexts.json — as
	// opposed to one the user named through PathEnvVar. Only the former is
	// tightened to a private mode. A user-supplied path can be a CI secret
	// mount, a read-only volume, a shared secrets directory, or $HOME
	// itself: chmod-ing it is a side effect nobody asked for when it works,
	// and takes down every Get/Set/Delete when it doesn't.
	ownsDir bool
	mu      sync.Mutex
	// warnedLoosePerms dedupes the loose-permissions warning to once per
	// store instance — effectively once per CLI invocation, since
	// currentBackend caches a single fileStore for the process. Like the
	// rest of the store's state it relies on mu, which every production
	// caller of load (Get/Set/Delete) holds; tests that call load directly
	// are single-goroutine.
	warnedLoosePerms bool
}

// withFileLock runs fn while holding an exclusive flock on f.path + ".lock".
// The lock coordinates across processes; the in-process mu handles goroutines.
func (f *fileStore) withFileLock(fn func() error) error {
	if err := f.ensureDir(); err != nil {
		return err
	}

	fl := flock.New(f.path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked, err := fl.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("acquiring token store lock: %w", err)
	}
	if !locked {
		return errors.New("timeout acquiring token store lock")
	}
	defer func() {
		// Unlock errors are logged but don't fail the operation.
		if unlockErr := fl.Unlock(); unlockErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to unlock token store: %v\n", unlockErr)
		}
	}()

	return fn()
}

// ensureDir makes sure the store file's directory exists. Creating it is
// mandatory — there is nowhere to write otherwise — but re-moding an existing
// one happens only for a directory Entire owns; see fileStore.ownsDir. A
// directory created here is private either way, since it is new and nothing
// else was using it.
func (f *fileStore) ensureDir() error {
	dir := filepath.Dir(f.path)
	if f.ownsDir {
		if err := userdirs.EnsurePrivateDir(dir); err != nil {
			return fmt.Errorf("creating token store directory: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating token store directory: %w", err)
	}
	return nil
}

func (f *fileStore) load() (map[string]map[string]string, error) {
	// The file holds bearer tokens; warn (once per store) when it is
	// readable or writable by group/others. Deliberately a warning, not a
	// refusal: externally provisioned files (CI secret mounts, read-only
	// volumes) often carry modes the user cannot change, a hard refusal
	// would also block the login rewrite that restores 0600, and diagnostic
	// commands must keep working so the user can see their auth state.
	// Files written by save() are always 0600, so this only fires on files
	// created or chmod-ed outside this store. Windows has no unix permission
	// bits — Go reports synthetic modes there — so the check is unix-only.
	if runtime.GOOS != goosWindows && !f.warnedLoosePerms {
		if info, statErr := os.Stat(f.path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			f.warnedLoosePerms = true
			fmt.Fprintf(loosePermsWarnW, "Warning: token store %s is accessible by group/others (mode %04o) and holds bearer tokens; run: chmod 0600 %s\n", f.path, info.Mode().Perm(), f.path)
		}
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]map[string]string), nil
		}
		return nil, fmt.Errorf("reading token store: %w", err)
	}
	var store map[string]map[string]string
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parsing token store: %w", err)
	}
	return store, nil
}

// save writes the store atomically via temp file + rename so a concurrent
// reader never sees a partial JSON document.
func (f *fileStore) save(store map[string]map[string]string) error {
	data, err := json.Marshal(store)
	if err != nil {
		return fmt.Errorf("marshaling token store: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".tokens-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp token store: %w", err)
	}
	tmpName := tmp.Name()
	// Clean up the temp file on any error path.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp token store: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp token store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp token store: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("renaming token store: %w", err)
	}
	return nil
}

func (f *fileStore) Get(service, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var result string
	var resultErr error
	err := f.withFileLock(func() error {
		store, err := f.load()
		if err != nil {
			return err
		}
		if users, ok := store[service]; ok {
			if pass, ok := users[user]; ok {
				result = pass
				return nil
			}
		}
		resultErr = ErrNotFound
		return nil
	})
	if err != nil {
		return "", err
	}
	return result, resultErr
}

func (f *fileStore) Set(service, user, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.withFileLock(func() error {
		store, err := f.load()
		if err != nil {
			return err
		}
		if store[service] == nil {
			store[service] = make(map[string]string)
		}
		store[service][user] = password
		return f.save(store)
	})
}

func (f *fileStore) Delete(service, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var notFound bool
	err := f.withFileLock(func() error {
		store, err := f.load()
		if err != nil {
			return err
		}
		users, ok := store[service]
		if !ok {
			notFound = true
			return nil
		}
		if _, ok := users[user]; !ok {
			notFound = true
			return nil
		}
		delete(users, user)
		if len(users) == 0 {
			delete(store, service)
		}
		return f.save(store)
	})
	if err != nil {
		return err
	}
	if notFound {
		return ErrNotFound
	}
	return nil
}
