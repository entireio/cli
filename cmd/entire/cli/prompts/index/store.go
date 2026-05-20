package index

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const (
	IndexDirName  = "prompts"
	IndexFileName = "index.ndjson"
	LockFileName  = "index.lock"
)

var (
	ErrIndexMissing      = errors.New("prompt index not found")
	ErrIndexCorrupt      = errors.New("prompt index is corrupt")
	ErrIndexVersionNewer = errors.New("prompt index was created by a newer version of the CLI")
	ErrIndexEmpty        = errors.New("prompt index is empty")
)

type Store struct {
	repoRoot  string
	indexPath string
	lockPath  string
}

func NewStore(repoRoot string) *Store {
	entireDir := filepath.Join(repoRoot, paths.EntireDir)
	indexDir := filepath.Join(entireDir, IndexDirName)
	return &Store{
		repoRoot:  repoRoot,
		indexPath: filepath.Join(indexDir, IndexFileName),
		lockPath:  filepath.Join(indexDir, LockFileName),
	}
}

func (s *Store) IndexPath() string { return s.indexPath }
func (s *Store) LockPath() string  { return s.lockPath }
func (s *Store) IndexDir() string  { return filepath.Dir(s.indexPath) }

func (s *Store) Exists() bool {
	_, err := os.Stat(s.indexPath)
	return err == nil
}

func (s *Store) Load(_ context.Context) ([]Entry, error) {
	f, err := os.Open(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrIndexMissing
		}
		return nil, fmt.Errorf("opening index file: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	var header Header
	var entries []Entry
	lineNum := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			lineNum++
			continue
		}

		if lineNum == 0 {
			if err := json.Unmarshal([]byte(line), &header); err != nil {
				return nil, fmt.Errorf("%w: header: %w", ErrIndexCorrupt, err)
			}
			if header.Version <= 0 {
				return nil, fmt.Errorf("%w: header: invalid version %d", ErrIndexCorrupt, header.Version)
			}
			if header.Version > CurrentIndexVersion {
				return nil, ErrIndexVersionNewer
			}
		} else {
			var entry Entry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				return nil, fmt.Errorf("%w: line %d: %w", ErrIndexCorrupt, lineNum+1, err)
			}
			entries = append(entries, entry)
		}
		lineNum++
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading index file: %w", err)
	}

	if lineNum == 0 {
		return nil, ErrIndexEmpty
	}

	return entries, nil
}

func (s *Store) AppendEntries(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(s.indexPath), 0o750); err != nil {
		return fmt.Errorf("creating index directory: %w", err)
	}

	lock, err := newLockFile(s.lockPath)
	if err != nil {
		return fmt.Errorf("creating lock: %w", err)
	}

	var lockErr error
	for attempt := range 3 {
		lockErr = lock.TryLock()
		if lockErr == nil {
			break
		}
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	if lockErr != nil {
		return fmt.Errorf("acquiring lock after retries: %w", lockErr)
	}

	defer func() {
		if err := lock.Unlock(); err != nil {
			logging.Warn(nil, "failed to unlock index", "error", err)
		}
	}()

	return s.appendEntriesLine(entries)
}

func (s *Store) appendEntriesLine(entries []Entry) error {
	fi, err := os.Stat(s.indexPath)
	if err != nil || fi.Size() == 0 {
		if err := s.InitIndex(); err != nil {
			return fmt.Errorf("initializing index: %w", err)
		}
	}

	f, err := os.OpenFile(s.indexPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening index for append: %w", err)
	}
	defer func() { _ = f.Close() }()

	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("marshaling entry: %w", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("appending to index: %w", err)
		}
	}
	return nil
}

func (s *Store) InitIndex() error {
	if err := os.MkdirAll(filepath.Dir(s.indexPath), 0o750); err != nil {
		return fmt.Errorf("creating index directory: %w", err)
	}

	header := Header{
		Version:   CurrentIndexVersion,
		CreatedAt: time.Now(),
		RepoRoot:  s.repoRoot,
	}

	data, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshaling header: %w", err)
	}

	if err := os.WriteFile(s.indexPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing index header: %w", err)
	}

	return nil
}

type Stats struct {
	IndexPath       string
	Version         int
	CheckpointCount int
	PromptCount     int
	EmptyCount      int
	FileSize        int64
	LastUpdated     time.Time
	Exists          bool
}

func (s *Store) Stats(_ context.Context) (Stats, error) {
	stats := Stats{
		IndexPath: s.indexPath,
		Exists:    s.Exists(),
	}

	if !stats.Exists {
		return stats, nil
	}

	fi, err := os.Stat(s.indexPath)
	if err == nil {
		stats.FileSize = fi.Size()
		stats.LastUpdated = fi.ModTime()
	}

	entries, err := s.Load(context.Background())
	if err != nil {
		if errors.Is(err, ErrIndexMissing) || errors.Is(err, ErrIndexEmpty) {
			return stats, nil
		}
		return stats, err
	}

	stats.PromptCount = len(entries)

	cpIDs := make(map[string]bool)
	for _, e := range entries {
		cpIDs[e.CheckpointID] = true
	}
	stats.CheckpointCount = len(cpIDs)
	stats.EmptyCount = len(entries) - stats.CheckpointCount

	return stats, nil
}

var checkpointIDPrefixRegex = regexp.MustCompile(`^[0-9a-f]{4,12}`)

func ParseCheckpointIDPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	matches := checkpointIDPrefixRegex.FindString(prefix)
	if len(matches) < 4 {
		return ""
	}
	return matches
}

func FormatFileSize(bytes int64) string {
	if bytes < 1024 {
		return strconv.FormatInt(bytes, 10) + " B"
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	if bytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(bytes)/(1024*1024*1024))
}

type fileLock struct {
	path string
	file *os.File
}

func newLockFile(path string) (*fileLock, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	return &fileLock{path: path}, nil
}

func (l *fileLock) TryLock() error {
	if info, err := os.Stat(l.path); err == nil {
		if time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(l.path)
		}
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating lock file: %w", err)
	}
	l.file = f
	return nil
}

func (l *fileLock) Unlock() error {
	if l.file == nil {
		return nil
	}
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("closing lock file: %w", err)
	}
	l.file = nil
	if err := os.Remove(l.path); err != nil {
		return fmt.Errorf("removing lock file: %w", err)
	}
	return nil
}

func (s *Store) Rebuild() error {
	if err := s.InitIndex(); err != nil {
		return err
	}
	return nil
}
