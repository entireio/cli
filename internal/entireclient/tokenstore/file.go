package tokenstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// fileStore persists credentials as a JSON file on disk.
// The file format is: { "service": { "user": "password" } }
type fileStore struct {
	path string
	mu   sync.Mutex
}

// withFileLock runs fn while holding an exclusive flock on f.path + ".lock".
// The lock coordinates across processes; the in-process mu handles goroutines.
func (f *fileStore) withFileLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		return fmt.Errorf("creating token store directory: %w", err)
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

func (f *fileStore) load() (map[string]map[string]string, error) {
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
