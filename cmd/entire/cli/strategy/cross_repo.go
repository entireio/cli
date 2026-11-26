package strategy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/internal/entireclient/userdirs"

	"github.com/go-git/go-git/v6"
)

// Cross-repository linking (global enable only). A session lives in the
// repository its agent started in (its home); with global enable on, the
// commits its agent makes in any other repository link to it too. The home
// state is never written from elsewhere: other repositories find the session
// through a per-user pointer, read the home state read-only, and store the
// checkpoint in their own checkpoint ref.

// crossRepoPointersDir holds one pointer per agent process that started a turn
// with global enable on, named by the hash of its proclive identity.
const crossRepoPointersDir = "session-owners"

// crossRepoInstallsFile lists the repositories Entire installed its git hooks
// in for a session homed elsewhere, for `entire disable --global` to name.
const crossRepoInstallsFile = "cross-repo-installs.json"

// crossRepoPointer is what a commit hook in another repository needs to find
// the session its committing agent belongs to.
type crossRepoPointer struct {
	SessionID      string            `json:"session_id"`
	AgentType      types.AgentType   `json:"agent,omitempty"`
	TranscriptPath string            `json:"transcript_path,omitempty"`
	Owner          proclive.Identity `json:"owner"`
	CommonDir      string            `json:"common_dir"`
	Worktree       string            `json:"worktree"`
	TurnStart      time.Time         `json:"turn_start"`
}

// crossRepoPointerName is the pointer file name for an owner: a hash of every
// identity field Depth matches on, so a recycled PID files elsewhere.
func crossRepoPointerName(owner proclive.Identity) string {
	sum := sha256.Sum256([]byte(owner.Host + "\x00" + owner.Boot + "\x00" + strconv.Itoa(owner.PID) + "\x00" + owner.Start))
	return hex.EncodeToString(sum[:16]) + ".json"
}

// openCrossRepoPointers opens the pointer directory under the user config
// root, creating both when create is set. A missing one is returned unwrapped.
func openCrossRepoPointers(create bool) (*os.Root, error) {
	var parent *os.Root
	var err error
	if create {
		parent, err = userdirs.ConfigRoot()
	} else {
		parent, err = userdirs.ConfigRootForRead()
	}
	if err != nil {
		return nil, err //nolint:wrapcheck // userdirs names the directory; callers classify IsNotExist
	}
	if create {
		if err := osroot.MkdirAllNoSymlink(parent, crossRepoPointersDir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", crossRepoPointersDir, err)
		}
	}
	return osroot.OpenChild(parent, crossRepoPointersDir) //nolint:wrapcheck // callers classify IsNotExist
}

// RecordCrossRepoOwner files this turn's pointer for sessionID, the session
// whose turn just started in this repository, and drops pointers of exited
// owners. Only with global enable on, and only for a session with a recorded
// owner (none on platforms proclive cannot introspect). Best effort.
func RecordCrossRepoOwner(ctx context.Context, sessionID string) {
	if !settings.UserTierEnablesEverywhere(ctx) {
		return
	}
	state, err := LoadSessionState(ctx, sessionID)
	if err != nil || state == nil || state.Owner == nil {
		return
	}
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		return
	}
	worktree, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}
	err = writeCrossRepoPointer(crossRepoPointer{
		SessionID: sessionID, AgentType: state.AgentType, TranscriptPath: state.TranscriptPath,
		Owner: *state.Owner, CommonDir: commonDir, Worktree: worktree, TurnStart: time.Now().UTC(),
	})
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "cross-repo"), "could not record the session owner pointer",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
	gcCrossRepoPointers()
}

func writeCrossRepoPointer(p crossRepoPointer) error {
	return writeCrossRepoPointerAs(crossRepoPointerName(p.Owner), p)
}

func writeCrossRepoPointerAs(name string, p crossRepoPointer) error {
	root, err := openCrossRepoPointers(true)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode pointer: %w", err)
	}
	return jsonutil.WriteFileAtomicIn(root, name, data, 0o600) //nolint:wrapcheck // callers log it
}

// gcCrossRepoPointers removes the pointers of owners proclive reports exited.
// An owner it cannot judge (another host, no introspection) stays.
func gcCrossRepoPointers() {
	root, err := openCrossRepoPointers(false)
	if err != nil {
		return
	}
	defer root.Close()
	entries, err := osroot.ReadDirNoSymlinks(root, ".")
	if err != nil {
		return
	}
	for _, entry := range entries {
		p, ok := readCrossRepoPointer(root, entry.Name())
		if ok && proclive.Check(p.Owner) == proclive.LivenessDead {
			_ = osroot.RemoveNoSymlinks(root, entry.Name()) //nolint:errcheck // the next turn start retries
		}
	}
}

func readCrossRepoPointer(root *os.Root, name string) (crossRepoPointer, bool) {
	var p crossRepoPointer
	data, err := osroot.ReadFileNoFollow(root, name)
	if err != nil || json.Unmarshal(data, &p) != nil || p.SessionID == "" {
		return crossRepoPointer{}, false
	}
	return p, true
}

// findCrossRepoPointer returns the pointer of the nearest ancestor of this
// commit hook that started a session turn, or nil. A pointer counts only when
// the owner it records is that ancestor (ancestry.Depth), so a file name
// collision or a recycled PID never matches.
func findCrossRepoPointer(ctx context.Context) *crossRepoPointer {
	ancestry, ok := commitAncestry(ctx)
	if !ok {
		return nil
	}
	root, err := openCrossRepoPointers(false)
	if err != nil {
		return nil
	}
	defer root.Close()
	for depth, ancestor := range ancestry.Chain() {
		p, ok := readCrossRepoPointer(root, crossRepoPointerName(ancestor))
		if ok && ancestry.Depth(p.Owner) == depth {
			return &p
		}
	}
	return nil
}

// ClearCrossRepoPointers removes every pointer (`entire disable --global`).
func ClearCrossRepoPointers() error {
	parent, err := userdirs.ConfigRootForRead()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err //nolint:wrapcheck // userdirs names the directory
	}
	if err := osroot.RemoveAllNoSymlinks(parent, crossRepoPointersDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", crossRepoPointersDir, err)
	}
	return nil
}

// CrossRepoInstalls returns the repositories Entire installed its git hooks
// in for sessions homed elsewhere, in the order it did.
func CrossRepoInstalls() []string {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		return nil
	}
	data, err := osroot.ReadFileNoFollow(root, crossRepoInstallsFile)
	if err != nil {
		return nil
	}
	var repos []string
	if json.Unmarshal(data, &repos) != nil {
		return nil
	}
	return repos
}

func recordCrossRepoInstall(repoRoot string) error {
	repos := CrossRepoInstalls()
	if slices.Contains(repos, repoRoot) {
		return nil
	}
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return err //nolint:wrapcheck // userdirs names the directory
	}
	data, err := json.Marshal(append(repos, repoRoot))
	if err != nil {
		return fmt.Errorf("encode installs: %w", err)
	}
	return jsonutil.WriteFileAtomicIn(root, crossRepoInstallsFile, data, 0o600) //nolint:wrapcheck // callers log it
}

// crossRepoHome is a session homed in another repository, as a commit here
// sees it: its state, read without changing it, and the home repository's git
// common directory, where the link journal is appended.
type crossRepoHome struct {
	state     *SessionState
	commonDir string
}

// errNotCrossRepo: the session is this repository's own, or not there.
var errNotCrossRepo = errors.New("no session homed in another repository")

// loadCrossRepoHome reads the state of sessionID in the repository whose
// worktree is homeWorktree. It returns errNotCrossRepo when that is this repository
// (sibling worktrees link through the shared store) or the session is not
// there. homeWorktree only selects the repository: git resolves its common
// directory, which the store and journal are opened on.
func loadCrossRepoHome(ctx context.Context, sessionID, homeWorktree string) (*crossRepoHome, error) {
	if !filepath.IsAbs(homeWorktree) {
		return nil, fmt.Errorf("home worktree %q is not absolute", homeWorktree)
	}
	here, err := gitdir.CommonDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve this repository: %w", err)
	}
	home, err := gitdir.CommonDirForWorktree(ctx, homeWorktree)
	if err != nil {
		return nil, fmt.Errorf("resolve the home repository: %w", err)
	}
	if sameDir(here, home) {
		return nil, errNotCrossRepo
	}
	store, err := session.NewStateStoreForWorktree(ctx, homeWorktree)
	if err != nil {
		return nil, err //nolint:wrapcheck // names the worktree
	}
	state, err := store.LoadReadOnly(ctx, sessionID)
	if err != nil {
		return nil, err //nolint:wrapcheck // names the session
	}
	if state == nil {
		return nil, errNotCrossRepo
	}
	return &crossRepoHome{state: state, commonDir: home}, nil
}

// crossRepoCommitSession returns the home of the session whose agent is
// making this commit in a repository other than its home, or nil.
func crossRepoCommitSession(ctx context.Context) *crossRepoHome {
	if !settings.UserTierEnablesEverywhere(ctx) {
		return nil
	}
	p := findCrossRepoPointer(ctx)
	if p == nil {
		return nil
	}
	home, err := loadCrossRepoHome(ctx, p.SessionID, p.Worktree)
	if err != nil {
		if errors.Is(err, errNotCrossRepo) {
			return nil
		}
		logging.Debug(logging.WithComponent(ctx, "cross-repo"), "could not read the committing session's home state",
			slog.String("session_id", p.SessionID), slog.String("error", err.Error()))
		return nil
	}
	return home
}

// editedOverlap reports whether the commit changed any path the session
// edited in this repository.
func editedOverlap(committed map[string]struct{}, edited []string) bool {
	for _, path := range edited {
		if _, ok := committed[filepath.ToSlash(path)]; ok {
			return true
		}
	}
	return false
}

// Link kinds in the home journal.
const (
	linkedByTrailer = "trailer"
	linkedBySweep   = "recorded_link"
)

// writeCrossRepoMarker stores, on this repository's checkpoint ref, a marker
// checkpoint saying the home session made commit sha here: no transcript,
// prompts or tokens, which stay with the session's home. A recorded link
// (the sweep) also names the commit in LinkedCommits, as attach does. The
// link is then appended to the home's journal.
func (s *ManualCommitStrategy) writeCrossRepoMarker(ctx context.Context, repo *git.Repository, cpID id.CheckpointID, home *crossRepoHome, sha string, committed map[string]struct{}, linkedBy string) error {
	worktree, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve this worktree: %w", err)
	}
	repoKey, _ := settings.RepoKey(ctx, worktree)                //nolint:errcheck // empty when unknown
	homeKey, _ := settings.RepoKey(ctx, home.state.WorktreePath) //nolint:errcheck // empty when unknown
	if homeKey == "" {
		homeKey = filepath.ToSlash(filepath.Dir(home.commonDir))
	}
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		return err
	}
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	opts := cpkg.WriteOptions{
		CheckpointID: cpID, SessionID: home.state.SessionID, Strategy: StrategyNameManualCommit,
		Branch: GetCurrentBranchName(repo), FilesTouched: committedFilesExcludingMetadata(committed),
		Agent: home.state.AgentType, AuthorName: authorName, AuthorEmail: authorEmail,
		CrossRepo: &cpkg.CrossRepoMarker{CommitSHA: sha, Repo: repoKey, HomeRepo: homeKey},
	}
	if linkedBy == linkedBySweep {
		opts.LinkedCommits = []cpkg.LinkedCommit{{SHA: sha}}
	}
	if err := store.Write(ctx, condensationSessionWriteRequest(opts)); err != nil {
		return fmt.Errorf("write the marker checkpoint: %w", err)
	}
	here, _ := gitdir.CommonDir(ctx) //nolint:errcheck // a hint only
	link := cpkg.LinkedRepoCommit{RepoKey: repoKey, CommonDir: here, SHA: sha, CheckpointID: cpID.String(), LinkedBy: linkedBy, At: time.Now().UTC()}
	if err := appendCrossRepoLink(home, link); err != nil {
		logging.Warn(logging.WithComponent(ctx, "cross-repo"), "could not record the link in the session's home repository",
			slog.String("session_id", home.state.SessionID), slog.String("error", err.Error()))
	}
	recordCrossRepoLastLink(ctx, crossRepoLastLink{SessionID: home.state.SessionID, HomeRepo: homeKey})
	logging.Info(logging.WithComponent(ctx, "cross-repo"), "linked a commit to a session homed in another repository",
		slog.String("session_id", home.state.SessionID), slog.String("checkpoint_id", cpID.String()),
		slog.String("home_repo", homeKey))
	return nil
}

// crossRepoLinksName is a session's link journal in its home git common dir:
// one JSON line per commit its agent made in another repository. Append-only,
// so a commit elsewhere never rewrites the home's session state.
func crossRepoLinksName(sessionID string) string {
	return "entire-cross-repo-links-" + sessionID + ".jsonl"
}

func appendCrossRepoLink(home *crossRepoHome, link cpkg.LinkedRepoCommit) error {
	root, err := gitdir.OpenAt(home.commonDir)
	if err != nil {
		return fmt.Errorf("open the home git directory: %w", err)
	}
	line, err := json.Marshal(link)
	if err != nil {
		return fmt.Errorf("encode link: %w", err)
	}
	f, err := osroot.OpenFileNoFollow(root, crossRepoLinksName(home.state.SessionID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the link journal: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("append to the link journal: %w", err)
	}
	return f.Close() //nolint:wrapcheck // close of an appended file
}

// CrossRepoLinks returns every commit sessionID's agent made in other
// repositories, from this repository's journal, oldest first.
func CrossRepoLinks(ctx context.Context, sessionID string) []cpkg.LinkedRepoCommit {
	root, err := gitdir.Open(ctx)
	if err != nil {
		return nil
	}
	data, err := osroot.ReadFileNoFollow(root, crossRepoLinksName(sessionID))
	if err != nil {
		return nil
	}
	var links []cpkg.LinkedRepoCommit
	for _, line := range bytes.Split(data, []byte("\n")) {
		var link cpkg.LinkedRepoCommit
		if len(line) > 0 && json.Unmarshal(line, &link) == nil && link.SHA != "" {
			links = append(links, link)
		}
	}
	return links
}

// withLinkedRepos adds a session's cross-repository commits to its next
// checkpoint: those since the previous one, and all of them, and advances the
// state's cursor.
func withLinkedRepos(ctx context.Context, opts *cpkg.WriteOptions, state *SessionState) {
	all := CrossRepoLinks(ctx, state.SessionID)
	if len(all) == 0 {
		return
	}
	start := min(state.CrossRepoLinksCondensed, len(all))
	opts.LinkedRepos = all[start:]
	opts.SessionLinkedRepos = all
	state.CrossRepoLinksCondensed = len(all)
}

// crossRepoLastLinkFile, in a repository's git common dir, names the session
// homed elsewhere that its last marker checkpoint linked to (for status).
const crossRepoLastLinkFile = "entire-cross-repo-session.json"

type crossRepoLastLink struct {
	SessionID string `json:"session_id"`
	HomeRepo  string `json:"home_repo"`
}

func recordCrossRepoLastLink(ctx context.Context, link crossRepoLastLink) {
	root, err := gitdir.Open(ctx)
	if err != nil {
		return
	}
	if data, err := json.Marshal(link); err == nil {
		_ = jsonutil.WriteFileAtomicIn(root, crossRepoLastLinkFile, data, 0o600) //nolint:errcheck // status only
	}
}

// CrossRepoStatus describes cross-repository linking for `entire status`:
// the session homed elsewhere whose live agent links commits here (empty when
// none), and how many other repositories this repository's sessions committed
// in.
func CrossRepoStatus(ctx context.Context) (inboundSession, inboundHome string, outboundRepos int) {
	root, err := gitdir.Open(ctx)
	if err != nil {
		return "", "", 0
	}
	var last crossRepoLastLink
	if data, err := osroot.ReadFileNoFollow(root, crossRepoLastLinkFile); err == nil && json.Unmarshal(data, &last) == nil && liveCrossRepoSession(last.SessionID) {
		inboundSession, inboundHome = last.SessionID, last.HomeRepo
	}
	repos := map[string]bool{}
	entries, _ := osroot.ReadDirNoSymlinks(root, ".") //nolint:errcheck // none on error
	for _, entry := range entries {
		id, ok := strings.CutPrefix(entry.Name(), "entire-cross-repo-links-")
		if !ok {
			continue
		}
		for _, link := range CrossRepoLinks(ctx, strings.TrimSuffix(id, ".jsonl")) {
			repos[link.RepoKey] = true
		}
	}
	return inboundSession, inboundHome, len(repos)
}

// liveCrossRepoSession reports whether a pointer names sessionID and its owner
// is not known to have exited.
func liveCrossRepoSession(sessionID string) bool {
	root, err := openCrossRepoPointers(false)
	if err != nil {
		return false
	}
	defer root.Close()
	entries, err := osroot.ReadDirNoSymlinks(root, ".")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if p, ok := readCrossRepoPointer(root, entry.Name()); ok && p.SessionID == sessionID && proclive.Check(p.Owner) != proclive.LivenessDead {
			return true
		}
	}
	return false
}
