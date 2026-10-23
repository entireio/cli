// Package usersettings owns the user-global settings file,
// ~/.config/entire/settings.json.
//
// It exists as a leaf package, separate from settings, for one reason: the
// preference blocks this file carries are typed by the settings package, and
// settings cannot import a package that imports it. The split the branch this
// was lifted from drew is preserved — this package decodes only the blocks
// whose contents it owns and hands every other top-level block back as raw
// JSON through Block, which is how settings reads `preferences` and `repos`.
//
// Why the file is here at all. Every other settings tier lives somewhere a
// repository can reach: .entire/settings.json is committed, and
// .entire/settings.local.json sits in the working tree, which is what a clone
// materializes. Proving that a value in those files is the developer's own
// therefore has to be done at load time, by probing git — the apparatus in
// settings/opf_command_trust.go, which has been defeated four separate ways.
// A path under the user's config directory cannot be delivered by cloning at
// all, so the same property holds structurally and needs no probe. That is the
// whole reason the OPF command may be honored from here directly.
package usersettings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// FileName is the basename of the user-global settings file.
const FileName = "settings.json"

// GlobalEnableKey is the top-level key `entire enable --global` sets: true
// makes every repository behave as if `entire enable` had run there.
const GlobalEnableKey = "global_enable"

// caseInsensitivePaths mirrors the platforms whose filesystems fold case by
// default. Used only to compare a `repos` path key against a worktree root;
// getting it wrong costs an unmatched preference entry, never a grant.
var caseInsensitivePaths = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// Path returns the absolute user settings path for diagnostics, or "" when the
// config directory cannot be resolved.
//
// Deliberately NOT falling back to the unchecked plain-string config-dir
// accessor for a path to print. That resolver cannot report a rejected
// $ENTIRE_CONFIG_DIR override, which is why every caller of it has to be
// entered in the userDirConsumers ledger and argued for. The argument here
// would have been "diagnostics only, never creates" — true today and one edit
// away from false. Returning "" instead makes the file safe by construction,
// which is the shape that ledger says it wants to shrink toward, and costs
// nothing: the only thing an unchecked path could add to a message is the
// directory the user already gave us and that we already rejected.
func Path() string {
	path, err := resolvePath()
	if err != nil {
		return ""
	}
	return path
}

func resolvePath() (string, error) {
	configDir, err := userdirs.ConfigDirChecked()
	if err != nil {
		return "", fmt.Errorf("resolving user settings directory: %w", err)
	}
	return filepath.Join(configDir, FileName), nil
}

// UserSettings is the decoded user-global settings file.
type UserSettings struct {
	// extra holds every top-level block this package does not decode: the
	// blocks the settings package owns (`preferences`, `repos`,
	// `global_enable` — read them with Block) and any block a newer binary
	// wrote. SetGlobalEnable, the only writer, keeps all of them.
	extra map[string]json.RawMessage
}

// Block returns a top-level block this package does not decode, as raw JSON.
// The settings package reads its `preferences` and `repos` blocks this way.
func (us *UserSettings) Block(key string) (json.RawMessage, bool) {
	if us == nil {
		return nil, false
	}
	raw, ok := us.extra[key]
	return raw, ok
}

// IsJSONNull reports whether raw is the JSON null literal. Exported because
// the settings package applies the same "null means unset" rule to the blocks
// it decodes itself.
func IsJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// UnmarshalJSON keeps every top-level block verbatim.
//
// This package decodes none of them itself. The preference blocks are typed by
// the settings package, which cannot be imported from here, and the blocks a
// newer binary writes are not ours to interpret — so all of them are kept
// verbatim and Block hands them to their owner. A block whose contents this
// binary must fail closed on (an executable name, say) would need a strict
// decoder registered here; there is none yet, and adding one before there is a
// consumer would be surface with nothing behind it.
func (us *UserSettings) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("user settings: %w", err)
	}
	*us = UserSettings{}
	for key, value := range raw {
		if us.extra == nil {
			us.extra = make(map[string]json.RawMessage, len(raw))
		}
		us.extra[key] = value
	}
	return nil
}

// Load decodes user-global settings with per-block strictness. A missing file
// is an unconfigured tier, not an error; malformed JSON, or an unknown key
// inside a decoded block, returns an error so callers can fail closed.
//
// Note what this does NOT need: a git repository, or `git` on $PATH. Every
// other settings path in the CLI resolves through paths.WorktreeRoot, which
// shells out to `git rev-parse --show-toplevel`. The machine-wide tier is
// readable when none of that is available, which is why the OPF command is
// more reliably found here than in the file it is moving from.
func Load(_ context.Context) (*UserSettings, error) {
	data, err := readFile()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &UserSettings{}, nil
		}
		return nil, err
	}
	return decode(data)
}

// decode parses the file's contents. A second value after the first means the
// file is not one JSON object — concatenated writes, or an editor that
// appended. Reading only the first would silently honor half a configuration,
// and writing it back would discard the rest.
func decode(data []byte) (*UserSettings, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var settings UserSettings
	if err := decoder.Decode(&settings); err != nil {
		return nil, fmt.Errorf("parsing user settings: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("parsing user settings: multiple JSON values")
		}
		return nil, fmt.Errorf("parsing user settings: trailing data: %w", err)
	}
	return &settings, nil
}

// SetGlobalEnable writes GlobalEnableKey into the user settings file, keeping
// every other block, and returns the file's path. It is the file's only
// writer, used by `entire enable --global` and `entire disable --global`.
//
// The read and the write happen under one lock and through one root, so two
// concurrent writers cannot lose each other's change and a symlink swapped in
// between cannot redirect the write. A file that does not parse is refused
// rather than replaced.
func SetGlobalEnable(_ context.Context, enabled bool) (string, error) {
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return "", fmt.Errorf("opening user settings directory: %w", err)
	}
	path := filepath.Join(root.Name(), FileName)
	release, err := flock.AcquireIn(root, FileName+".lock")
	if err != nil {
		return path, fmt.Errorf("locking user settings %s: %w", path, err)
	}
	defer release()

	settings := &UserSettings{}
	data, err := osroot.ReadFileNoFollow(root, FileName)
	switch {
	case err == nil:
		if settings, err = decode(data); err != nil {
			return path, fmt.Errorf("%s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
	case errors.Is(err, osroot.ErrSymlinkedPath):
		return path, fmt.Errorf("user settings %s is a symbolic link; it must be a regular file: %w", path, err)
	default:
		return path, fmt.Errorf("reading user settings %s: %w", path, err)
	}

	blocks := settings.extra
	if blocks == nil {
		blocks = map[string]json.RawMessage{}
	}
	value, err := json.Marshal(enabled)
	if err != nil {
		return path, fmt.Errorf("encoding %s: %w", GlobalEnableKey, err)
	}
	blocks[GlobalEnableKey] = value
	out, err := jsonutil.MarshalIndentWithNewline(blocks, "", "  ")
	if err != nil {
		return path, fmt.Errorf("encoding user settings: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, FileName, out, 0o600); err != nil {
		return path, fmt.Errorf("writing user settings %s: %w", path, err)
	}
	return path, nil
}

// readFile reads the settings file through the per-user config root, without
// creating the directory. A symlinked file is refused, as a symlinked
// .entire/settings.local.json is: nothing documents an exception for this
// file, and following the link would read whatever it names.
func readFile() ([]byte, error) {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err //nolint:wrapcheck // the caller classifies a missing directory as an unconfigured tier
		}
		return nil, fmt.Errorf("opening user settings directory: %w", err)
	}
	data, err := osroot.ReadFileNoFollow(root, FileName)
	if err == nil {
		return data, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, err //nolint:wrapcheck // the caller classifies a missing file as an unconfigured tier
	}
	path := filepath.Join(root.Name(), FileName)
	if errors.Is(err, osroot.ErrSymlinkedPath) {
		if target, linkErr := os.Readlink(path); linkErr == nil {
			return nil, fmt.Errorf("user settings %s is a symbolic link to %s; it must be a regular file: %w", path, target, err)
		}
		return nil, fmt.Errorf("user settings %s is a symbolic link; it must be a regular file: %w", path, err)
	}
	return nil, fmt.Errorf("reading user settings %s: %w", path, err)
}

// NormalizeOrigin reduces a git remote URL to the lowercase key shape `repos`
// entries use: forge/owner/repo for recognized Entire forge namespaces, or
// canonical-host/owner/repo for everything else.
func NormalizeOrigin(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	info, err := gitremote.ParseURL(rawURL)
	if err != nil || info == nil || info.Owner == "" || info.Repo == "" {
		return ""
	}
	prefix := info.Forge
	if !gitremote.IsForgePathToken(prefix) {
		prefix = info.CanonicalHost()
	}
	if prefix == "" {
		return ""
	}
	return strings.ToLower(prefix + "/" + info.Owner + "/" + info.Repo)
}

// OriginKeys returns the normalized origin keys for a worktree, derived from
// every fetch and push URL of `origin`.
//
// present distinguishes "this repository has no origin" (false, nil error —
// the caller falls back to matching by path) from "origin exists but could not
// be read or normalized" (an error). Collapsing the two would let an
// unreadable remote config read as an unconfigured repository.
func OriginKeys(ctx context.Context, worktreeRoot string) (keys []string, present bool, err error) {
	// The environment is scrubbed because this runs on the hook path:
	// IsSetUpAndEnabled reaches here, git exports GIT_DIR and GIT_WORK_TREE to
	// its hooks, and those outrank cmd.Dir. An unscrubbed child would read the
	// hook's repository and match another repository's `repos` entry.
	env := gitrepo.EnvWithoutRepoOverrides()
	fetchURLs, fetchFound, err := gitremote.GetRemoteURLsInDirIfSet(ctx, worktreeRoot, env, "origin")
	if err != nil {
		return nil, false, fmt.Errorf("reading origin remote: %w", err)
	}
	pushURLs, pushFound, err := gitremote.GetRemotePushURLsInDirIfSet(ctx, worktreeRoot, env, "origin")
	if err != nil {
		return nil, false, fmt.Errorf("reading origin pushurl: %w", err)
	}
	if !fetchFound && !pushFound {
		return nil, false, nil
	}
	for _, origin := range append(fetchURLs, pushURLs...) {
		normalized := NormalizeOrigin(origin)
		if normalized == "" {
			return nil, true, errors.New("origin remote cannot be normalized")
		}
		if !slices.Contains(keys, normalized) {
			keys = append(keys, normalized)
		}
	}
	return keys, true, nil
}

// ExpandTilde expands a `repos` path key to an absolute, slash-separated path.
// A relative path is refused rather than resolved against the process working
// directory, which would make one key mean different repositories depending on
// where the command ran.
func ExpandTilde(pattern string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", nil
	}
	if pattern == "~" || strings.HasPrefix(pattern, "~/") || strings.HasPrefix(pattern, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding ~: %w", err)
		}
		suffix := strings.TrimPrefix(pattern, "~")
		suffix = strings.TrimLeft(strings.ReplaceAll(suffix, `\`, "/"), "/")
		return filepath.ToSlash(filepath.Join(home, filepath.FromSlash(suffix))), nil
	}
	if strings.HasPrefix(pattern, "~") {
		return "", errors.New("unsupported ~user form")
	}
	if !filepath.IsAbs(pattern) {
		return "", errors.New("path key must be absolute (a drive-rooted path on Windows)")
	}
	return filepath.ToSlash(filepath.Clean(pattern)), nil
}

// PathNamesThisClone reports whether a `repos` path key names this repository,
// matching any worktree of the clone rather than only the one it spells.
//
// A path key exists for a repository with no usable origin, and every worktree
// of a clone has a DIFFERENT path — so comparing paths alone made a path-keyed
// entry apply to exactly one worktree and leave every other one unconfigured.
// That is the same per-worktree divergence this whole tier exists to remove,
// reintroduced through the fallback meant to cover repositories that cannot
// use the primary key.
//
// So the path comparison is tried first, because it is free, and a miss falls
// through to comparing git common directories: every worktree of one clone
// shares one, and no two clones do. A key that is not a repository at all
// fails that lookup and is simply not a match.
func PathNamesThisClone(ctx context.Context, key, worktreeRoot string) bool {
	if PathIsRoot(key, worktreeRoot) {
		return true
	}
	expanded, err := ExpandTilde(key)
	if err != nil || expanded == "" {
		return false
	}
	keyCommon, err := commonDirCached(ctx, expanded)
	if err != nil {
		return false
	}
	ourCommon, err := commonDirCached(ctx, worktreeRoot)
	if err != nil {
		return false
	}
	return pathsEquivalent(keyCommon, ourCommon)
}

// commonDirByPath memoizes gitdir.CommonDirForPath for the process lifetime.
// settings.Load runs several times per hook and checks every path-keyed
// `repos` entry each time, so without it N such entries cost 2N git
// subprocesses per Load. Only successes are cached: a failure may be a
// directory that becomes a repository later in a long-lived process.
var (
	commonDirMu     sync.Mutex
	commonDirByPath = map[string]string{}
)

// ClearCommonDirCache drops the memoized common directories. Tests that
// create or remove repositories at a reused path within one process call it.
func ClearCommonDirCache() {
	commonDirMu.Lock()
	defer commonDirMu.Unlock()
	clear(commonDirByPath)
}

func commonDirCached(ctx context.Context, dir string) (string, error) {
	key := filepath.Clean(dir)
	commonDirMu.Lock()
	cached, ok := commonDirByPath[key]
	commonDirMu.Unlock()
	if ok {
		return cached, nil
	}
	common, err := gitdir.CommonDirForPath(ctx, key)
	if err != nil {
		return "", err //nolint:wrapcheck // the caller treats any failure as "not a match"
	}
	commonDirMu.Lock()
	commonDirByPath[key] = common
	commonDirMu.Unlock()
	return common, nil
}

func pathsEquivalent(a, b string) bool {
	aForms, bForms := pathForms(a), pathForms(b)
	if caseInsensitivePaths {
		foldForms(aForms)
		foldForms(bForms)
	}
	for _, x := range aForms {
		if slices.Contains(bForms, x) {
			return true
		}
	}
	return false
}

// PathIsRoot reports whether a `repos` path key names this worktree root
// exactly. Both sides are compared in raw and symlink-resolved form, because
// the key and the root can reach the same directory by different spellings — a
// worktree under a symlinked parent is the ordinary case on macOS (/tmp) and
// in dotfile-managed checkouts.
func PathIsRoot(key, worktreeRoot string) bool {
	expanded, err := ExpandTilde(key)
	if err != nil || expanded == "" {
		return false
	}
	keyForms := pathForms(expanded)
	rootForms := pathForms(worktreeRoot)
	if caseInsensitivePaths {
		foldForms(keyForms)
		foldForms(rootForms)
	}
	for _, k := range keyForms {
		if slices.Contains(rootForms, k) {
			return true
		}
	}
	return false
}

func pathForms(path string) []string {
	forms := []string{filepath.ToSlash(filepath.Clean(path))}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if slashed := filepath.ToSlash(resolved); slashed != forms[0] {
			forms = append(forms, slashed)
		}
	}
	return forms
}

func foldForms(forms []string) {
	for i := range forms {
		forms[i] = strings.ToLower(forms[i])
	}
}
