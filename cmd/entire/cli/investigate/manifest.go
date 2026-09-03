package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const manifestsSubdirName = "manifests"

// LocalManifest is the persisted record of one `entire investigate` run for
// local findings browsing. Written to <git-common-dir>/entire-investigations/
// manifests/<timestamp>-<run-id>.json after each run terminates.
//
// The schema is intentionally narrower than RunState: this file is what
// `entire investigate --findings` reads to render the picker, so it carries
// only what a human (or `entire status`) needs to identify a past run, not the
// state needed to resume one.
type LocalManifest struct {
	// RunID is the 12-hex-char investigation run identifier.
	RunID string `json:"run_id"`

	// Topic is the human-readable subject of the investigation.
	Topic string `json:"topic"`

	// Slug is the filesystem-safe form of Topic, derived via SlugifyTopic.
	Slug string `json:"slug"`

	// StartingSHA is the git commit SHA that was HEAD when the
	// investigation started.
	StartingSHA string `json:"starting_sha"`

	// WorktreePath is the absolute path to the worktree the run executed
	// in. Empty when the run was not associated with a specific
	// worktree.
	WorktreePath string `json:"worktree_path,omitempty"`

	// FindingsDoc is the absolute path to the findings document the run
	// produced. Always absolute — callers (writeRunManifest in particular)
	// must resolve repo-relative paths before populating this field. It is
	// display-only (printed by `entire investigate --findings`): `show` and
	// `fix` never read it back directly, since it is untrusted data decoded
	// from a JSON file on disk — both instead resolve RunID (the validated
	// part of the manifest) as a name inside the investigation store via
	// ReadRunFindings. The on-disk file is removed for terminal outcomes
	// (Quorum/Stalled) once FindingsContent has been captured — the path
	// remains here for resumable runs (Paused/Cancelled) where the file
	// still lives in the per-run directory.
	FindingsDoc string `json:"findings_doc,omitempty"`

	// FindingsContent embeds the final findings.md content as of run
	// end. Populated on terminal outcomes (Quorum/Stalled) so the
	// findings survive after the per-run directory is cleaned up. Empty
	// on Paused/Cancelled — those runs are resumable and the file lives
	// on disk in the per-run directory at FindingsDoc.
	FindingsContent string `json:"findings_content,omitempty"`

	// Agents is the ordered list of agent names that participated in
	// the run.
	Agents []string `json:"agents"`

	// Outcome is the terminal outcome of the run. One of: "quorum",
	// "stalled", "paused", "cancelled".
	Outcome string `json:"outcome"`

	// StancesByAgent records the LAST stance each agent expressed in
	// the run, keyed by agent name. Empty when the run terminated
	// without any stances being recorded.
	StancesByAgent map[string]string `json:"stances_by_agent,omitempty"`

	// StartedAt is when the run was initiated.
	StartedAt time.Time `json:"started_at"`

	// EndedAt is when the run terminated.
	EndedAt time.Time `json:"ended_at"`
}

// LocalManifestStore wraps the directory that holds persisted LocalManifest
// JSON files for one repository.
type LocalManifestStore struct {
	// dir is the absolute manifests directory, kept for messages. All I/O is a
	// name inside root.
	dir     string
	parent  string
	dirName string
}

// NewLocalManifestStore creates a LocalManifestStore rooted at
// <git-common-dir>/entire-investigations/manifests.
func NewLocalManifestStore(ctx context.Context) (*LocalManifestStore, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("get worktree root: %w", err)
	}
	metadata, err := gitrepo.ResolveWorktreeMetadata(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("get git common dir: %w", err)
	}
	return &LocalManifestStore{
		dir:     filepath.Join(metadata.CommonDir, InvestigationsDirName, manifestsSubdirName),
		parent:  metadata.CommonDir,
		dirName: InvestigationsDirName + "/" + manifestsSubdirName,
	}, nil
}

// NewLocalManifestStoreWithDir creates a LocalManifestStore rooted at dir.
// Useful for tests that do not want to depend on a real git repository.
func NewLocalManifestStoreWithDir(dir string) *LocalManifestStore {
	return &LocalManifestStore{
		dir:     dir,
		parent:  filepath.Dir(dir),
		dirName: filepath.Base(dir),
	}
}

// root returns the shared *os.Root over the directory the manifests sit inside.
func (s *LocalManifestStore) root() (*os.Root, error) {
	return gitdir.OpenAt(s.parent) //nolint:wrapcheck // gitdir names the directory and returns a missing one unwrapped for os.IsNotExist
}

// name renders a manifest file as a name relative to root.
func (s *LocalManifestStore) name(file string) string {
	return s.dirName + "/" + file
}

// Write persists m to the manifests directory using a deterministic filename
// derived from m.StartedAt and m.RunID. Existing files are overwritten — the
// timestamp+run-id combination is unique by construction (each run has a fresh
// run ID and a different start time).
func (s *LocalManifestStore) Write(ctx context.Context, m LocalManifest) error {
	_ = ctx // Reserved for future use.

	if err := validateRunID(m.RunID); err != nil {
		return fmt.Errorf("invalid run ID: %w", err)
	}
	if m.StartedAt.IsZero() {
		return errors.New("manifest StartedAt is required")
	}

	root, err := s.root()
	if err != nil {
		return fmt.Errorf("open investigations store: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, s.dirName, 0o750); err != nil {
		return fmt.Errorf("create investigations manifests dir: %w", err)
	}

	data, err := jsonutil.MarshalIndentWithNewline(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	// 0o600: manifests embed the user-supplied investigation prompt and
	// (on terminal outcomes) the final findings body. Matches the mode
	// state.go uses for state.json.
	if err := jsonutil.WriteFileAtomicIn(root, s.name(manifestFilename(m)), data, 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// List returns every manifest in the store sorted newest first by StartedAt.
// A missing directory is treated as an empty list (nil, nil) — useful for
// callers that want to render `--findings` even when no investigation has
// ever been run in this repo.
func (s *LocalManifestStore) List(ctx context.Context) ([]LocalManifest, error) {
	_ = ctx // Reserved for future use.

	root, err := s.root()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open investigations store: %w", err)
	}
	entries, err := osroot.ReadDirNoSymlinks(root, s.dirName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read investigations manifests dir: %w", err)
	}

	manifests := make([]LocalManifest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		b, readErr := osroot.ReadFileNoFollow(root, s.name(name))
		if readErr != nil {
			return nil, fmt.Errorf("read manifest %s: %w", name, readErr)
		}
		var m LocalManifest
		if err := json.Unmarshal(b, &m); err != nil {
			// Skip files we can't decode — they may be stale or
			// from a future schema. Listing must keep working.
			continue
		}
		// Skip manifests whose RunID is not a valid 12-hex id. A planted
		// manifest with e.g. "run_id":"../../.." would otherwise flow
		// through clean → RunDir → os.RemoveAll as a path-traversal target.
		// Validate at the source so no downstream consumer can be tricked.
		if err := validateRunID(m.RunID); err != nil {
			continue
		}
		manifests = append(manifests, m)
	}
	sort.SliceStable(manifests, func(i, j int) bool {
		return manifests[i].StartedAt.After(manifests[j].StartedAt)
	})
	return manifests, nil
}

// FindByRunID returns the manifest whose RunID equals runID. The bool
// reports whether a match was found; when false the returned manifest is
// the zero value. Returns an error only when the underlying directory read
// itself fails.
//
// Filenames are <timestamp>-<runID>.json, so the lookup is a single Glob +
// one file read rather than scanning every manifest in the directory.
func (s *LocalManifestStore) FindByRunID(ctx context.Context, runID string) (LocalManifest, bool, error) {
	_ = ctx
	if err := validateRunID(runID); err != nil {
		return LocalManifest{}, false, fmt.Errorf("invalid run ID: %w", err)
	}
	root, err := s.root()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LocalManifest{}, false, nil
		}
		return LocalManifest{}, false, fmt.Errorf("open investigations store: %w", err)
	}
	// A directory scan with a literal suffix match rather than filepath.Glob:
	// os.Root has no glob, and a literal match is the safer primitive anyway —
	// runID is validated above, but a pattern would let metacharacters in a
	// future caller's id match unrelated manifests.
	entries, err := osroot.ReadDirNoSymlinks(root, s.dirName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LocalManifest{}, false, nil
		}
		return LocalManifest{}, false, fmt.Errorf("read investigations manifests dir: %w", err)
	}
	suffix := "-" + runID + ".json"
	match := ""
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			match = e.Name()
			break
		}
	}
	if match == "" {
		return LocalManifest{}, false, nil
	}
	b, err := osroot.ReadFileNoFollow(root, s.name(match))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LocalManifest{}, false, nil
		}
		return LocalManifest{}, false, fmt.Errorf("read manifest %s: %w", match, err)
	}
	var m LocalManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return LocalManifest{}, false, fmt.Errorf("decode manifest %s: %w", match, err)
	}
	return m, true, nil
}

// ResolveByRunID matches a (possibly partial) run ID against the supplied
// manifest list. Exact match wins; otherwise unique-prefix wins. Returns
// a slice (always length 1 on success) so callers handle the not-found
// and ambiguous cases via the error.
//
// The shape mirrors what show and clean both want: an exact 12-hex match
// resolves O(1), a unique prefix expands to exactly one manifest, and any
// other case produces a user-readable error listing the candidates.
func ResolveByRunID(manifests []LocalManifest, runID string) ([]LocalManifest, error) {
	for _, m := range manifests {
		// Never match (and thus never delete via) a manifest whose RunID is
		// invalid — defense-in-depth in case a caller passes an unfiltered
		// list. List() already drops these.
		if validateRunID(m.RunID) != nil {
			continue
		}
		if m.RunID == runID {
			return []LocalManifest{m}, nil
		}
	}
	var prefixMatches []LocalManifest
	for _, m := range manifests {
		if validateRunID(m.RunID) != nil {
			continue
		}
		if strings.HasPrefix(m.RunID, runID) {
			prefixMatches = append(prefixMatches, m)
		}
	}
	switch len(prefixMatches) {
	case 0:
		return nil, fmt.Errorf("no investigation found with run id or prefix %q", runID)
	case 1:
		return prefixMatches, nil
	default:
		return nil, ambiguousRunIDError(prefixMatches, runID)
	}
}

// ambiguousRunIDError formats a list of candidate run ids for the user
// to choose from. When runID is empty, the header asks the user to pass a
// run id; otherwise it reports the ambiguous prefix.
func ambiguousRunIDError(candidates []LocalManifest, runID string) error {
	sorted := append([]LocalManifest(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].StartedAt.After(sorted[j].StartedAt)
	})
	var b strings.Builder
	if runID == "" {
		b.WriteString("multiple investigations available — pass a run id (or unique prefix):\n")
	} else {
		fmt.Fprintf(&b, "ambiguous run id prefix %q matches multiple investigations:\n", runID)
	}
	for _, m := range sorted {
		fmt.Fprintf(&b, "  %s  %s\n", m.RunID, m.Topic)
	}
	return errors.New(strings.TrimRight(b.String(), "\n"))
}

// Latest returns the most recent manifest in the store, identified by the
// lexicographically largest filename (filenames are <timestamp>-<runID>.json
// where the timestamp prefix sorts chronologically). The bool reports
// whether the store has any manifests; when false the returned manifest is
// the zero value. Avoids reading every manifest just to pick the newest one.
func (s *LocalManifestStore) Latest(ctx context.Context) (LocalManifest, bool, error) {
	_ = ctx
	root, err := s.root()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LocalManifest{}, false, nil
		}
		return LocalManifest{}, false, fmt.Errorf("open investigations store: %w", err)
	}
	entries, err := osroot.ReadDirNoSymlinks(root, s.dirName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LocalManifest{}, false, nil
		}
		return LocalManifest{}, false, fmt.Errorf("read investigations manifests dir: %w", err)
	}
	var latest string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		if name > latest {
			latest = name
		}
	}
	if latest == "" {
		return LocalManifest{}, false, nil
	}
	b, err := osroot.ReadFileNoFollow(root, s.name(latest))
	if err != nil {
		return LocalManifest{}, false, fmt.Errorf("read manifest %s: %w", latest, err)
	}
	var m LocalManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return LocalManifest{}, false, fmt.Errorf("decode manifest %s: %w", latest, err)
	}
	return m, true, nil
}

// manifestFilename returns the on-disk filename for m. Format:
// <timestamp>-<run-id>.json, where timestamp is the UTC StartedAt formatted
// as 20060102T150405. The timestamp prefix sorts manifests
// chronologically by directory listing, making `ls` output match List's
// newest-first ordering by simple reverse.
func manifestFilename(m LocalManifest) string {
	stamp := m.StartedAt.UTC().Format("20060102T150405")
	return stamp + "-" + m.RunID + ".json"
}

// Remove deletes m's manifest file through the store's root.
//
// The filename is derived from m.StartedAt and m.RunID, and m was decoded from
// a file on disk, so resolving it as a name inside the root rather than joining
// it into an absolute path is what keeps a planted manifest from choosing what
// gets deleted. List already drops manifests whose RunID fails validateRunID;
// this is the layer underneath that.
//
// A manifest that is not there is not an error: clean converges when run
// against a partially removed state.
func (s *LocalManifestStore) Remove(m LocalManifest) error {
	root, err := s.root()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open investigations store: %w", err)
	}
	if err := osroot.RemoveNoSymlinks(root, s.name(manifestFilename(m))); err != nil {
		return fmt.Errorf("remove manifest: %w", err)
	}
	return nil
}

// PathFor returns the on-disk path of the manifest file for m. The path
// is computed deterministically from m.StartedAt + m.RunID (the same
// inputs Write uses to choose its destination), so callers can use this
// to delete a manifest record without scanning the directory.
func (s *LocalManifestStore) PathFor(m LocalManifest) string {
	return filepath.Join(s.dir, manifestFilename(m))
}
