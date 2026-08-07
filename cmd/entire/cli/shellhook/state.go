package shellhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// StatePath returns the absolute path of the throttling state file.
func StatePath() string {
	return filepath.Join(userdirs.Cache(), stateFileName)
}

// LoadState reads the throttling state. A missing file yields empty state.
func LoadState() (*State, error) {
	path := StatePath()
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from userdirs, not user input
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("reading shell hook state: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		// A corrupt cache must not silence the hook forever; start over.
		return &State{}, nil //nolint:nilerr // cache file, not user data
	}
	return &state, nil
}

// SaveState writes the throttling state atomically, pruning it first.
func SaveState(state *State) error {
	if state == nil {
		return errors.New("nil state")
	}
	state.prune(MaxStateEntries)
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encoding shell hook state: %w", err)
	}
	return writeFileAtomic(StatePath(), append(data, '\n'))
}

// IsDismissed reports whether the user silenced warnings for key.
func (s *State) IsDismissed(key string) bool {
	if s == nil {
		return false
	}
	return s.Repos[key].Dismissed
}

// ShouldWarn reports whether a warning for key is due at now.
//
// Dismissed repositories never warn; otherwise the last warning must be at
// least throttle old. A LastWarnedAt in the future (clock skew, a restored
// backup) also warns, so a bad timestamp cannot silence a repo indefinitely.
func (s *State) ShouldWarn(key string, now time.Time, throttle time.Duration) bool {
	if s == nil {
		return true
	}
	repo, ok := s.Repos[key]
	if !ok {
		return true
	}
	if repo.Dismissed {
		return false
	}
	if repo.LastWarnedAt.IsZero() || repo.LastWarnedAt.After(now) {
		return true
	}
	return now.Sub(repo.LastWarnedAt) >= throttle
}

// MarkWarned records that key was warned about at now.
func (s *State) MarkWarned(key string, now time.Time) {
	s.update(key, func(repo *RepoState) { repo.LastWarnedAt = now })
}

// MarkDismissed silences key permanently. It also stamps LastWarnedAt so the
// entry carries prune recency — see RepoState.LastWarnedAt.
func (s *State) MarkDismissed(key string, now time.Time) {
	s.update(key, func(repo *RepoState) {
		repo.Dismissed = true
		repo.LastWarnedAt = now
	})
}

// DismissedCount returns how many repositories the user has silenced.
func (s *State) DismissedCount() int {
	if s == nil {
		return 0
	}
	count := 0
	for _, repo := range s.Repos {
		if repo.Dismissed {
			count++
		}
	}
	return count
}

func (s *State) update(key string, mutate func(*RepoState)) {
	if s == nil || key == "" {
		return
	}
	if s.Repos == nil {
		s.Repos = make(map[string]RepoState)
	}
	repo := s.Repos[key]
	mutate(&repo)
	s.Repos[key] = repo
}

// prune evicts the least recently touched entries until at most maxEntries
// remain, so the cache file cannot grow without bound.
func (s *State) prune(maxEntries int) {
	if s == nil || len(s.Repos) <= maxEntries {
		return
	}
	keys := make([]string, 0, len(s.Repos))
	for key := range s.Repos {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.Repos[keys[i]].LastWarnedAt, s.Repos[keys[j]].LastWarnedAt
		if a.Equal(b) {
			return keys[i] < keys[j] // deterministic tie-break
		}
		return a.Before(b)
	})
	for _, key := range keys[:len(keys)-maxEntries] {
		delete(s.Repos, key)
	}
}
