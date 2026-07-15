package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// kvStore is a tiny string→string key/value store backed by a single JSON file
// in the plugin's data dir (entire.kv). It is loaded lazily on first access and
// written through on every set. It is only ever touched under the Registry
// mutex (via the plugin's Lua state), so it needs no internal locking.
type kvStore struct {
	dir    string
	path   string
	loaded bool
	data   map[string]string
}

// newKVStore returns a kvStore rooted at the plugin's data dir. The dir/file
// are created lazily on first write so a plugin that never persists state
// leaves nothing behind.
func newKVStore(dataDir string) *kvStore {
	return &kvStore{
		dir:  dataDir,
		path: filepath.Join(dataDir, "kv.json"),
		data: map[string]string{},
	}
}

func (k *kvStore) ensureLoaded() error {
	if k.loaded {
		return nil
	}
	k.loaded = true
	data, err := os.ReadFile(k.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read plugin kv store: %w", err)
	}
	parsed := map[string]string{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("parse plugin kv store: %w", err)
	}
	k.data = parsed
	return nil
}

// get returns the value for key and whether it was present.
func (k *kvStore) get(key string) (string, bool, error) {
	if err := k.ensureLoaded(); err != nil {
		return "", false, err
	}
	v, ok := k.data[key]
	return v, ok, nil
}

// set stores value under key and flushes the store to disk.
func (k *kvStore) set(key, value string) error {
	if err := k.ensureLoaded(); err != nil {
		return err
	}
	k.data[key] = value
	return k.flush()
}

// del removes key and flushes the store.
func (k *kvStore) del(key string) error {
	if err := k.ensureLoaded(); err != nil {
		return err
	}
	if _, ok := k.data[key]; !ok {
		return nil
	}
	delete(k.data, key)
	return k.flush()
}

func (k *kvStore) flush() error {
	if err := os.MkdirAll(k.dir, 0o750); err != nil {
		return fmt.Errorf("create plugin data dir: %w", err)
	}
	out, err := json.MarshalIndent(k.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal plugin kv store: %w", err)
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write plugin kv store: %w", err)
	}
	if err := os.Rename(tmp, k.path); err != nil {
		return fmt.Errorf("commit plugin kv store: %w", err)
	}
	return nil
}
