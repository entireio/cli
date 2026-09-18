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

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// FileName is the basename of the user-global settings file.
const FileName = "settings.json"

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

// blockRedaction is the one top-level block this package decodes. It is
// decoded strictly (DisallowUnknownFields) and fails the load closed on an
// unknown key, because it names an executable and an older binary must not
// guess at it. A top-level key absent from blocks belongs to another owner —
// the settings package's `preferences` and `repos`, or a newer binary's
// addition — and is kept verbatim for round-tripping.
//
// Adding a block: a field on UserSettings, an entry in blocks, nothing else.
const blockRedaction = "redaction"

// block is the decode/encode pair for one known block. decode receives the raw
// block (never null — decodeStrict maps null to an absent block); encode
// reports the value to write and whether it is set.
type block struct {
	decode func(us *UserSettings, raw json.RawMessage) error
	encode func(us *UserSettings) (value any, set bool)
}

var blocks = map[string]block{
	blockRedaction: {
		decode: func(us *UserSettings, raw json.RawMessage) error {
			v, err := decodeStrict[RedactionConfig](raw)
			if err == nil && v != nil {
				err = v.validate()
			}
			us.Redaction = v
			return err
		},
		encode: func(us *UserSettings) (any, bool) { return us.Redaction, us.Redaction != nil },
	},
}

// RedactionConfig is the machine-local half of redaction configuration.
type RedactionConfig struct {
	OpenAIPrivacyFilter *OPFConfig `json:"openai_privacy_filter,omitempty"`
}

// OPFConfig holds the machine-local OpenAI Privacy Filter configuration.
//
// Command is the reason this block exists: it becomes argv[0] of an exec at
// pre-push, and this file is the only root that is the developer's by
// construction — no repository can deliver content here — so it is the only
// place the command is honored without an ownership probe. TimeoutSeconds and
// PromptDefault are ordinary configuration that layers between the project
// file and the per-worktree local file; their zero values ("" and 0) mean "not
// set here", the same as omitting the key — 0 is not a way to reset the
// timeout.
type OPFConfig struct {
	Command        string `json:"command,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	PromptDefault  string `json:"prompt_default,omitempty"`
}

func (c *RedactionConfig) validate() error {
	opf := c.OpenAIPrivacyFilter
	if opf == nil {
		return nil
	}
	return ValidateOPFRunSettings(opf.TimeoutSeconds, opf.PromptDefault)
}

// OPF prompt_default values, duplicated from settings rather than imported:
// settings imports this package, so the dependency cannot run the other way.
// TestOPFPromptDefaultsMatchSettings pins the two lists together.
const (
	opfPromptAsk    = "ask"
	opfPromptNever  = "never"
	opfPromptAlways = "always"
)

// ValidateOPFRunSettings checks the OPF keys that may appear in any settings
// tier (timeout_seconds, prompt_default). It lives here, in the leaf package,
// so the user-file decoder and settings' own validation enforce one policy
// with one set of messages.
func ValidateOPFRunSettings(timeoutSeconds int, promptDefault string) error {
	if timeoutSeconds < 0 {
		return fmt.Errorf("openai_privacy_filter.timeout_seconds must be greater than or equal to 0 (got %d)", timeoutSeconds)
	}
	switch promptDefault {
	case "", opfPromptAsk, opfPromptNever, opfPromptAlways:
		return nil
	default:
		return fmt.Errorf("openai_privacy_filter.prompt_default must be one of %q, %q, %q (got %q)",
			opfPromptAsk, opfPromptNever, opfPromptAlways, promptDefault)
	}
}

// UserSettings is the decoded user-global settings file.
//
// MarshalJSON has a VALUE receiver so that both a UserSettings and a
// *UserSettings encode through it; a pointer receiver would silently fall back
// to struct-field encoding for a value, dropping every preserved block.
//
//nolint:recvcheck // MarshalJSON is deliberately a value receiver; see above.
type UserSettings struct {
	Redaction *RedactionConfig `json:"redaction,omitempty"`

	// extra holds every top-level block this package does not decode: the
	// preference blocks the settings package owns (`preferences`, `repos` —
	// their types live there; read them with Block) and any block a newer
	// binary wrote. All are preserved across read-modify-write so a newer
	// binary's settings survive an older one's write. This is also what lets
	// this file coexist with the separate global-activation work, which adds
	// a `global` block this binary neither reads nor disturbs.
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

// SetBlock stores a raw top-level block, for a writer in the settings package
// that owns a block this package does not decode. A nil raw removes it.
func (us *UserSettings) SetBlock(key string, raw json.RawMessage) {
	if us == nil {
		return
	}
	if raw == nil {
		delete(us.extra, key)
		return
	}
	if us.extra == nil {
		us.extra = make(map[string]json.RawMessage, 1)
	}
	us.extra[key] = raw
}

// OPF returns the machine-local OPF configuration, or nil when the redaction
// block or its openai_privacy_filter entry is absent.
func (us *UserSettings) OPF() *OPFConfig {
	if us == nil || us.Redaction == nil {
		return nil
	}
	return us.Redaction.OpenAIPrivacyFilter
}

// decodeStrict decodes raw into a T, rejecting unknown keys. A JSON null is
// "unset" and yields a nil pointer, exactly like an absent block.
func decodeStrict[T any](raw json.RawMessage) (*T, error) {
	if IsJSONNull(raw) {
		return nil, nil //nolint:nilnil // nil is the documented "block unset" value, not an error
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var v T
	if err := decoder.Decode(&v); err != nil {
		return nil, err //nolint:wrapcheck // the caller prefixes the block name
	}
	return &v, nil
}

// IsJSONNull reports whether raw is the JSON null literal. Exported because
// the settings package applies the same "null means unset" rule to the blocks
// it decodes itself.
func IsJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// UnmarshalJSON decodes the file with per-block strictness: each block in
// blocks is strict (an unknown key inside it is an error — an older binary
// must fail closed rather than misread an executable name it does not
// understand), while unknown top-level blocks are kept verbatim.
func (us *UserSettings) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("user settings: %w", err)
	}
	*us = UserSettings{}
	for key, value := range raw {
		known, ok := blocks[key]
		if !ok {
			if us.extra == nil {
				us.extra = make(map[string]json.RawMessage, len(raw))
			}
			us.extra[key] = value
			continue
		}
		if err := known.decode(us, value); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

// MarshalJSON writes every known block that is set plus every preserved
// unknown block. Value receiver on purpose — see the UserSettings comment.
func (us UserSettings) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(us.extra)+len(blocks))
	for key, raw := range us.extra {
		out[key] = raw
	}
	for key, known := range blocks {
		value, set := known.encode(&us)
		if !set {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encoding %s block: %w", key, err)
		}
		out[key] = raw
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encoding user settings: %w", err)
	}
	return data, nil
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
	path, err := resolvePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is resolved by the userdirs trust boundary
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &UserSettings{}, nil
		}
		return nil, fmt.Errorf("reading user settings: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var settings UserSettings
	if err := decoder.Decode(&settings); err != nil {
		return nil, fmt.Errorf("parsing user settings: %w", err)
	}
	// A second value after the first means the file is not one JSON object —
	// concatenated writes, or an editor that appended. Reading only the first
	// would silently honor half a configuration.
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("parsing user settings: multiple JSON values")
		}
		return nil, fmt.Errorf("parsing user settings: trailing data: %w", err)
	}
	return &settings, nil
}

// Modify performs the only supported read-modify-write operation for user
// settings. A cross-process lock prevents concurrent writers from losing one
// another's changes — the file is machine-wide, so writers in different
// repositories genuinely race.
func Modify(ctx context.Context, fn func(*UserSettings) error) error {
	path, err := resolvePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	release, err := flock.Acquire(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock user settings: %w", err)
	}
	defer release()
	settings, err := Load(ctx)
	if err != nil {
		return err
	}
	if err := fn(settings); err != nil {
		return err
	}
	return persist(path, settings)
}

func persist(path string, settings *UserSettings) error {
	data, err := jsonutil.MarshalIndentWithNewline(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding user settings: %w", err)
	}
	if err := writeAtomicThroughSymlink(path, data, 0o600); err != nil {
		return fmt.Errorf("writing user settings: %w", err)
	}
	return nil
}

// writeAtomicThroughSymlink is jsonutil.WriteFileAtomic's behaviour with one
// deliberate difference: a symlink AT the destination is resolved first, so
// the write lands on the link's target rather than replacing the link.
//
// This is the opposite of the rule for .entire and the git hooks directory,
// and the reason is that the trust direction is reversed. There, a link is
// something a repository might have planted, so replacing it is the safe
// answer. Here the file is the user's own config, and ~/.config symlinked into
// a dotfiles checkout (chezmoi, stow, yadm) is an ordinary arrangement among
// exactly the people who run coding agents — the same population
// allow_symlinked_agent_dirs exists for. Replacing that link would silently
// detach the file from the dotfiles repo the user manages it in.
//
// A dangling or non-symlink destination falls through to the plain path.
func writeAtomicThroughSymlink(path string, data []byte, perm fs.FileMode) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	return jsonutil.WriteFileAtomic(target, data, perm) //nolint:wrapcheck // caller names the file
}

// NormalizeOrigin reduces a git remote URL to lowercase host/owner/repo form.
// This is the key shape `repos` entries use.
func NormalizeOrigin(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	info, err := gitremote.ParseURL(rawURL)
	if err != nil || info == nil || info.Owner == "" || info.Repo == "" {
		return ""
	}
	host := info.CanonicalHost()
	if host == "" {
		return ""
	}
	return strings.ToLower(host + "/" + info.Owner + "/" + info.Repo)
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
	keyCommon, err := gitdir.CommonDirForWorktree(ctx, expanded)
	if err != nil {
		return false
	}
	ourCommon, err := gitdir.CommonDirForWorktree(ctx, worktreeRoot)
	if err != nil {
		return false
	}
	return pathsEquivalent(keyCommon, ourCommon)
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
