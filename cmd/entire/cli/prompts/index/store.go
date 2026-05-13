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

type IndexStore struct {
	repoRoot  string
	indexPath string
	lockPath  string
}

func NewIndexStore(repoRoot string) *IndexStore {
	entireDir := filepath.Join(repoRoot, paths.EntireDir)
	indexDir := filepath.Join(entireDir, IndexDirName)
	return &IndexStore{
		repoRoot:  repoRoot,
		indexPath: filepath.Join(indexDir, IndexFileName),
		lockPath:  filepath.Join(indexDir, LockFileName),
	}
}

func (s *IndexStore) IndexPath() string { return s.indexPath }
func (s *IndexStore) LockPath() string  { return s.lockPath }
func (s *IndexStore) IndexDir() string   { return filepath.Dir(s.indexPath) }

func (s *IndexStore) Exists() bool {
	_, err := os.Stat(s.indexPath)
	return err == nil
}

func (s *IndexStore) Load(ctx context.Context) (*IndexHeader, []PromptEntry, error) {
	f, err := os.Open(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrIndexMissing
		}
		return nil, nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var header IndexHeader
	var entries []PromptEntry
	lineNum := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			lineNum++
			continue
		}

		if lineNum == 0 {
			if err := json.Unmarshal([]byte(line), &header); err != nil {
				return nil, nil, fmt.Errorf("%w: header: %v", ErrIndexCorrupt, err)
			}
		} else {
			var entry PromptEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				return nil, nil, fmt.Errorf("%w: line %d: %v", ErrIndexCorrupt, lineNum+1, err)
			}
			entries = append(entries, entry)
		}
		lineNum++
	}

	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}

	if lineNum == 0 {
		return nil, nil, ErrIndexEmpty
	}

	return &header, entries, nil
}

func (s *IndexStore) AppendEntries(entries []PromptEntry) error {
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
	defer lock.Unlock()

	if err := lock.TryLock(); err != nil {
		return s.appendEntriesWithRetry(entries, 3)
	}

	return s.appendEntriesLine(entries)
}

func (s *IndexStore) appendEntriesLine(entries []PromptEntry) error {
	f, err := os.OpenFile(s.indexPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening index for append: %w", err)
	}
	defer f.Close()

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

func (s *IndexStore) appendEntriesWithRetry(entries []PromptEntry, maxRetries int) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		time.Sleep(50 * time.Millisecond)

		lock, err := newLockFile(s.lockPath)
		if err != nil {
			lastErr = err
			continue
		}
		defer lock.Unlock()

		if err := lock.TryLock(); err != nil {
			lastErr = err
			continue
		}

		if err := s.appendEntriesLine(entries); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("failed to acquire lock after %d retries: %w", maxRetries, lastErr)
}

func (s *IndexStore) InitIndex() error {
	if err := os.MkdirAll(filepath.Dir(s.indexPath), 0o750); err != nil {
		return fmt.Errorf("creating index directory: %w", err)
	}

	header := IndexHeader{
		Version:   CurrentIndexVersion,
		CreatedAt: time.Now(),
		RepoRoot:  s.repoRoot,
	}

	data, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshaling header: %w", err)
	}

	if err := os.WriteFile(s.indexPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing index header: %w", err)
	}

	return nil
}

type IndexStats struct {
	IndexPath        string
	Version          int
	CheckpointCount  int
	PromptCount      int
	EmptyCount       int
	FileSize         int64
	LastUpdated      time.Time
	Exists           bool
}

func (s *IndexStore) Stats(_ context.Context) (IndexStats, error) {
	stats := IndexStats{
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

	_, entries, err := s.Load(context.Background())
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
		return nil, err
	}
	return &fileLock{path: path}, nil
}

func (l *fileLock) TryLock() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.file = f
	return nil
}

func (l *fileLock) Unlock() error {
	if l.file == nil {
		return nil
	}
	_ = l.file.Close()
	_ = os.Remove(l.path)
	l.file = nil
	return nil
}

func (s *IndexStore) Rebuild() error {
	if err := s.InitIndex(); err != nil {
		return err
	}
	return nil
}