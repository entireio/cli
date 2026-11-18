package bbcache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

const (
	// fetchFallbackTTL is how long a fetch fallback record stays valid. The
	// fetch spec sets 24 hours: long enough that a lasting fault does not
	// fail every fetch, short enough that the repo opts in again soon.
	fetchFallbackTTL = 24 * time.Hour
	// fallbackDir holds the fetch fallback records in the per-user cache.
	fallbackDir = "bb-fetch-fallback"
	// fallbackVersion is the format version of a fetch fallback record.
	fallbackVersion = 1
	// maxFallbackReason bounds the stored reason.
	maxFallbackReason = 1 << 10
)

// FetchFallback is a valid fetch fallback record: until Until, a fetch of
// the repo does not opt in to chunked fetch, and gets whole blobs.
type FetchFallback struct {
	Until time.Time
	// Reason is the failure that set the record.
	Reason string
}

// FallbackRecord is the fetch fallback record of one repo. It is in the
// per-user cache, not in a clone, keyed like the Store by the server URL
// and the repo path. So a clone that failed and was removed still leaves
// the record, and every clone of the repo on the machine obeys it.
type FallbackRecord struct {
	root *os.Root
	name string
	now  func() time.Time
}

type fallbackFile struct {
	Version int       `json:"version"`
	SetAt   time.Time `json:"set_at"`
	Reason  string    `json:"reason"`
}

// OpenFallbackRecord opens the fetch fallback record of the repo at
// repoPath on serverURL in the per-user cache, and creates its directory.
func OpenFallbackRecord(serverURL, repoPath string) (*FallbackRecord, error) {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return nil, fmt.Errorf("bbcache: open the user cache: %w", err)
	}
	return openFallbackRecord(root, serverURL, repoPath)
}

func openFallbackRecord(root *os.Root, serverURL, repoPath string) (*FallbackRecord, error) {
	if serverURL == "" || strings.Trim(repoPath, "/") == "" {
		return nil, errors.New("bbcache: server URL and repo path are required")
	}
	if err := osroot.MkdirAllNoSymlink(root, fallbackDir, 0o700); err != nil {
		return nil, fmt.Errorf("bbcache: create %s: %w", fallbackDir, err)
	}
	return &FallbackRecord{
		root: root,
		name: fallbackDir + "/" + repoKey(serverURL, repoPath) + ".json",
		now:  time.Now,
	}, nil
}

// Set records that the fetches of the repo in the next 24 hours go without
// chunked fetch. A fetch calls it after a failure in the recipe path. A new
// record replaces the old one.
func (r *FallbackRecord) Set(reason string) error {
	if len(reason) > maxFallbackReason {
		reason = reason[:maxFallbackReason]
	}
	data, err := json.Marshal(fallbackFile{Version: fallbackVersion, SetAt: r.now().UTC(), Reason: reason})
	if err != nil {
		return fmt.Errorf("bbcache: encode fetch fallback: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(r.root, r.name, data, 0o600); err != nil {
		return fmt.Errorf("bbcache: write fetch fallback: %w", err)
	}
	return nil
}

// Get returns the record when it is valid. It removes a record that has
// expired or does not decode. A record from the future means that the clock
// moved back. Its age is unknown, so it is not valid either.
func (r *FallbackRecord) Get() (FetchFallback, bool) {
	data, err := osroot.ReadFileNoFollow(r.root, r.name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			debuglog.Printf("bbcache: read fetch fallback: %v", err)
		}
		return FetchFallback{}, false
	}
	var f fallbackFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != fallbackVersion {
		debuglog.Printf("bbcache: dropping fetch fallback: version %d, %v", f.Version, err)
		r.removeUnchanged(data)
		return FetchFallback{}, false
	}
	age := r.now().Sub(f.SetAt)
	if age < 0 || age >= fetchFallbackTTL {
		r.removeUnchanged(data)
		return FetchFallback{}, false
	}
	return FetchFallback{Until: f.SetAt.Add(fetchFallbackTTL), Reason: f.Reason}, true
}

// removeUnchanged removes the record when it still holds data. Another
// process can write a new record after the read; that record stays.
func (r *FallbackRecord) removeUnchanged(data []byte) {
	again, err := osroot.ReadFileNoFollow(r.root, r.name)
	if err != nil || !bytes.Equal(again, data) {
		return
	}
	if err := osroot.RemoveNoSymlinks(r.root, r.name); err != nil && !errors.Is(err, os.ErrNotExist) {
		debuglog.Printf("bbcache: remove fetch fallback: %v", err)
	}
}
