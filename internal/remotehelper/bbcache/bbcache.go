// Package bbcache keeps git-remote-entire's local state for client-chunked
// pushes and fetches of one repo: the xorbs that it built, the complete
// xorbs that it downloaded, and what the server said about each xorb. A
// push uses it to skip uploads that an earlier push did, and to resume after
// a failed push. A fetch rebuilds blobs from its complete xorbs.
//
// The store lives in the git common dir at
// <common dir>/entire-xorbs/<repo key>/, with the repo key derived from the
// server URL and the repo path. Deduplication on the server is per repo, so a
// record or xorb of one repo is never used for another. Every file is a
// cache: a missing, short or stale file only costs a new upload or download,
// so eviction and `entire clean` are always safe. A push, and a fetch
// session that opted in to chunked recipes, trims the store to its size cap
// once, when it ends.
//
// The fetch fallback record of a repo is not in the store. FallbackRecord
// keeps it in the per-user cache under the same repo key, so it outlives a
// clone that failed.
package bbcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

const (
	// stagedTrust is how long a staged record stays valid after the upload.
	// The server's lifecycle rule deletes a staged xorb after 1 day, so 12
	// hours leaves a margin.
	stagedTrust = 12 * time.Hour
	// defaultMaxBytes is the default size cap of the files of one repo's
	// store.
	defaultMaxBytes int64 = 1 << 30
)

const (
	xorbsDir    = "xorbs"
	completeDir = "complete"
	recordsDir  = "records"
	// staleTemp is the age after which eviction removes a temp file that a
	// killed writer left. A younger one can belong to a write in progress.
	staleTemp = time.Hour
	// recentComplete is the time since its last use for which eviction
	// keeps a complete xorb. A fetch in another process can be reading it,
	// and the pins of that process are not visible here.
	recentComplete = 30 * time.Minute
	recordVersion  = 1
)

var subdirs = []string{xorbsDir, completeDir, recordsDir}

// Record is what the server last said about a xorb.
type Record struct {
	// UploadedAt is the time of the upload or dedup answer that set State.
	// A dedup answer of StateStaged keeps the time of the upload.
	UploadedAt time.Time
	State      bbclient.State
	Chunks     []bbclient.FooterChunk
}

// Store is the local xorb store of one repo on one server. It is safe for
// concurrent use, also by several processes: every write is a rename, and a
// file that disappears is a cache miss.
type Store struct {
	root *os.Root
	// base is the name of the repo's directory inside root.
	base     string
	maxBytes int64
	now      func() time.Time

	// rename renames a file within a directory. Tests replace it.
	rename func(dir *os.Root, oldname, newname string) error

	mu sync.Mutex
	// pinned holds the files that this Store wrote, and the complete xorbs
	// that the fetch which opened it needs. Trim keeps them: the push may
	// still upload them again, and the fetch may still read them.
	pinned map[string]bool
}

// option configures a Store.
type option func(*Store)

// withMaxBytes sets the size cap of the store's files. Trim removes the
// least recently used files above it.
func withMaxBytes(n int64) option {
	return func(s *Store) { s.maxBytes = n }
}

// repoKey returns the directory name of the store for the repo at repoPath
// on serverURL: the SHA-256 of both, in hex. A trailing slash on serverURL
// and the slashes around repoPath do not change the key.
func repoKey(serverURL, repoPath string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(serverURL, "/") + "\n/" + strings.Trim(repoPath, "/")))
	return hex.EncodeToString(sum[:])
}

// Open opens the store of the repo at repoPath on serverURL in the git
// common dir commonDir, and creates its directories with mode 0700.
func Open(commonDir, serverURL, repoPath string, opts ...option) (*Store, error) {
	if serverURL == "" || strings.Trim(repoPath, "/") == "" {
		return nil, errors.New("bbcache: server URL and repo path are required")
	}
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return nil, fmt.Errorf("bbcache: open git common dir: %w", err)
	}
	s := &Store{
		root:     root,
		base:     gitdir.XorbStoreDirName + "/" + repoKey(serverURL, repoPath),
		maxBytes: defaultMaxBytes,
		now:      time.Now,
		rename:   (*os.Root).Rename,
		pinned:   map[string]bool{},
	}
	for _, opt := range opts {
		opt(s)
	}
	for _, sub := range subdirs {
		if err := osroot.MkdirAllNoSymlink(root, s.base+"/"+sub, 0o700); err != nil {
			return nil, fmt.Errorf("bbcache: create %s: %w", filepath.Join(commonDir, s.base, sub), err)
		}
	}
	return s, nil
}

func (s *Store) name(sub, file string) string {
	return s.base + "/" + sub + "/" + file
}

// PutXorb stores the network form of x.
func (s *Store) PutXorb(x *bbchunk.Xorb) error {
	if err := s.write(s.name(xorbsDir, x.Hash.String()), x.Network); err != nil {
		return fmt.Errorf("bbcache: write xorb %s: %w", x.Hash, err)
	}
	return nil
}

// Xorb returns the stored xorb h. It hashes the stored bytes again, and it
// removes a file that does not hash to h. It reports false for a missing or
// removed xorb.
func (s *Store) Xorb(h xethash.Hash) (*bbchunk.Xorb, bool) {
	name := s.name(xorbsDir, h.String())
	body, ok := s.read(name)
	if !ok {
		return nil, false
	}
	footer, err := xorb.VerifyNetwork(body, h)
	if err != nil {
		debuglog.Printf("bbcache: dropping xorb %s: %v", h, err)
		s.remove(name)
		return nil, false
	}
	return &bbchunk.Xorb{Hash: h, Network: body, Footer: (*bbchunk.Footer)(footer)}, true
}

type recordFile struct {
	Version    int            `json:"version"`
	UploadedAt time.Time      `json:"uploaded_at"`
	State      bbclient.State `json:"state"`
	Chunks     []chunkFile    `json:"chunks"`
}

type chunkFile struct {
	Hash        string `json:"hash"`
	UnpackedEnd uint64 `json:"unpacked_end"`
	StoredEnd   uint64 `json:"stored_end"`
}

// RecordUpload records the server's answer to an upload.
func (s *Store) RecordUpload(r *bbclient.UploadResult) error {
	return s.putRecord(r.Xorb, Record{UploadedAt: s.now(), State: r.State, Chunks: r.Chunks})
}

// RecordDedup replaces the records of the xorbs in a dedup answer. A missing
// xorb loses its record. A staged xorb keeps the upload time of its record,
// and gets no record without one: only an upload starts the server's expiry.
// A permanent xorb keeps the footer of its record, since the answer gives a
// state only. A xorb that an anchor hit names gets a permanent record with
// the footer from the answer.
func (s *Store) RecordDedup(res *bbclient.DedupResult) error {
	var errs []error
	answered := make(map[xethash.Hash]bool, len(res.Xorbs))
	for _, xs := range res.Xorbs {
		answered[xs.Xorb] = true
		if xs.State == bbclient.StateMissing {
			errs = append(errs, s.Forget(xs.Xorb))
			continue
		}
		old, ok := s.readRecord(xs.Xorb)
		if !ok {
			// Without a record there is no footer, and for a staged xorb no
			// upload time.
			if chunks, anchored := res.Footers[xs.Xorb]; anchored && xs.State == bbclient.StatePermanent {
				errs = append(errs, s.putRecord(xs.Xorb, Record{UploadedAt: s.now(), State: xs.State, Chunks: chunks}))
			}
			continue
		}
		old.State = xs.State
		errs = append(errs, s.putRecord(xs.Xorb, old))
	}
	for h, chunks := range res.Footers {
		if answered[h] {
			continue
		}
		rec := Record{UploadedAt: s.now(), State: bbclient.StatePermanent, Chunks: chunks}
		if old, ok := s.readRecord(h); ok {
			rec.UploadedAt = old.UploadedAt
		}
		errs = append(errs, s.putRecord(h, rec))
	}
	return errors.Join(errs...)
}

func (s *Store) putRecord(h xethash.Hash, r Record) error {
	f := recordFile{Version: recordVersion, UploadedAt: r.UploadedAt.UTC(), State: r.State, Chunks: make([]chunkFile, len(r.Chunks))}
	for i, c := range r.Chunks {
		f.Chunks[i] = chunkFile{Hash: c.Hash.String(), UnpackedEnd: c.UnpackedEnd, StoredEnd: c.StoredEnd}
	}
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("bbcache: encode record of xorb %s: %w", h, err)
	}
	if err := s.write(s.name(recordsDir, h.String()+".json"), data); err != nil {
		return fmt.Errorf("bbcache: write record of xorb %s: %w", h, err)
	}
	return nil
}

// Record returns the record of xorb h when it is still valid: a permanent
// record until Forget, a staged one for stagedTrust after its upload.
func (s *Store) Record(h xethash.Hash) (Record, bool) {
	r, ok := s.readRecord(h)
	if !ok || !s.trusted(r) {
		return Record{}, false
	}
	return r, true
}

// TrustedXorbs returns every xorb with a valid record, for the xorbs list of
// a dedup query. It removes the records that are no longer valid.
func (s *Store) TrustedXorbs() []xethash.Hash {
	entries, err := osroot.ReadDirNoSymlinks(s.root, s.base+"/"+recordsDir)
	if err != nil {
		debuglog.Printf("bbcache: list records: %v", err)
		return nil
	}
	var out []xethash.Hash
	for _, e := range entries {
		text, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || jsonutil.IsTempName(e.Name()) {
			continue
		}
		h, err := xethash.Parse(text)
		if err != nil || h.String() != text {
			continue
		}
		r, ok := s.readRecord(h)
		if !ok {
			continue
		}
		if !s.trusted(r) {
			s.remove(s.name(recordsDir, e.Name()))
			continue
		}
		out = append(out, h)
	}
	return out
}

// Forget removes the record of xorb h. A push calls it when the server
// refuses with "missing xorb" for h. The stored xorb stays, so the client
// can upload it again.
func (s *Store) Forget(h xethash.Hash) error {
	if err := osroot.RemoveNoSymlinks(s.root, s.name(recordsDir, h.String()+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("bbcache: forget xorb %s: %w", h, err)
	}
	return nil
}

func (s *Store) trusted(r Record) bool {
	switch r.State {
	case bbclient.StatePermanent:
		return true
	case bbclient.StateStaged:
		// A record from the future means the clock moved back. Its age is
		// unknown, so it is not trusted.
		age := s.now().Sub(r.UploadedAt)
		return age >= 0 && age < stagedTrust
	case bbclient.StateMissing:
		return false
	default:
		return false
	}
}

// readRecord reads the record of xorb h. It removes a record that does not
// decode.
func (s *Store) readRecord(h xethash.Hash) (Record, bool) {
	name := s.name(recordsDir, h.String()+".json")
	data, ok := s.read(name)
	if !ok {
		return Record{}, false
	}
	r, err := decodeRecord(data)
	if err != nil {
		debuglog.Printf("bbcache: dropping record of xorb %s: %v", h, err)
		s.remove(name)
		return Record{}, false
	}
	return r, true
}

func decodeRecord(data []byte) (Record, error) {
	var f recordFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Record{}, fmt.Errorf("decode: %w", err)
	}
	if f.Version != recordVersion {
		return Record{}, fmt.Errorf("version %d, want %d", f.Version, recordVersion)
	}
	r := Record{UploadedAt: f.UploadedAt, State: f.State, Chunks: make([]bbclient.FooterChunk, len(f.Chunks))}
	for i, c := range f.Chunks {
		h, err := xethash.Parse(c.Hash)
		if err != nil {
			return Record{}, fmt.Errorf("chunk %d: %w", i, err)
		}
		r.Chunks[i] = bbclient.FooterChunk{Hash: h, UnpackedEnd: c.UnpackedEnd, StoredEnd: c.StoredEnd}
	}
	return r, nil
}

// write pins a file, then replaces it atomically. Trim holds mu while it
// checks pins and removes files, so a file pinned before its rename is never
// removed by this Store.
func (s *Store) write(name string, data []byte) error {
	s.pin(name)
	if err := jsonutil.WriteFileAtomicIn(s.root, name, data, 0o600); err != nil {
		return err //nolint:wrapcheck // callers name the file
	}
	s.touch(name)
	return nil
}

func (s *Store) pin(name string) {
	s.mu.Lock()
	s.pinned[name] = true
	s.mu.Unlock()
}

// read reads a file and marks it as used for eviction. Any failure is a
// miss.
func (s *Store) read(name string) ([]byte, bool) {
	data, err := osroot.ReadFileNoFollow(s.root, name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			debuglog.Printf("bbcache: read %s: %v", name, err)
		}
		return nil, false
	}
	s.touch(name)
	return data, true
}

// touch sets the modification time, which eviction reads as the last use,
// to the store's clock.
func (s *Store) touch(name string) {
	now := s.now()
	if err := s.root.Chtimes(name, now, now); err != nil {
		debuglog.Printf("bbcache: touch %s: %v", name, err)
	}
}

func (s *Store) remove(name string) {
	if err := osroot.RemoveNoSymlinks(s.root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		debuglog.Printf("bbcache: remove %s: %v", name, err)
	}
}

type storedFile struct {
	name  string
	size  int64
	mtime time.Time
	// recent is set for a complete xorb that eviction keeps.
	recent bool
}

// Trim removes the least recently used files until the store's files fit in
// its size cap. It keeps the files that this Store pinned, so a push that
// writes more than the cap goes over it until the next push trims. It also
// keeps every complete xorb used in the last 30 minutes, which another
// process can be reading. It removes temp files older than staleTemp. A
// push calls Trim once, when it ends. Trim is best effort: a failure leaves
// the store over its cap until the next push.
func (s *Store) Trim() {
	now := s.now()
	var files []storedFile
	var total int64
	for _, sub := range subdirs {
		entries, err := osroot.ReadDirNoSymlinks(s.root, s.base+"/"+sub)
		if err != nil {
			debuglog.Printf("bbcache: list %s: %v", sub, err)
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			name := s.name(sub, e.Name())
			if jsonutil.IsTempName(e.Name()) {
				if now.Sub(info.ModTime()) > staleTemp {
					s.remove(name)
				}
				continue
			}
			recent := sub == completeDir && now.Sub(info.ModTime()) < recentComplete
			files = append(files, storedFile{name: name, size: info.Size(), mtime: info.ModTime(), recent: recent})
			total += info.Size()
		}
	}
	if total <= s.maxBytes {
		return
	}
	slices.SortFunc(files, func(a, b storedFile) int { return a.mtime.Compare(b.mtime) })
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range files {
		if total <= s.maxBytes {
			break
		}
		if s.pinned[f.name] || f.recent {
			continue
		}
		s.remove(f.name)
		total -= f.size
	}
}
