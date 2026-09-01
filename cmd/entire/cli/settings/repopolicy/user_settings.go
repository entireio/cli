package repopolicy

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
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// UserSettingsFileName is the basename of the user-global settings file.
const UserSettingsFileName = "settings.json"

var caseInsensitivePaths = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// UserSettingsPath returns the absolute user settings path.
func UserSettingsPath() string {
	path, err := resolveUserSettingsPath()
	if err != nil {
		return filepath.Join(userdirs.Config(), UserSettingsFileName)
	}
	return path
}

func resolveUserSettingsPath() (string, error) {
	configDir, err := userdirs.ResolveConfig()
	if err != nil {
		return "", fmt.Errorf("resolving user settings directory: %w", err)
	}
	return filepath.Join(configDir, UserSettingsFileName), nil
}

// Top-level blocks this binary interprets. Every entry is decoded strictly
// (DisallowUnknownFields); what a decode error costs differs per block:
//
//   - fatal (`global`): the whole load fails. The block records consent, and
//     a binary must never guess at consent it cannot parse — the tier goes
//     off machine-wide and `entire doctor` says so.
//   - non-fatal (`redaction`): the block alone is dropped. Dropping it is
//     already fail-closed for exec — no command is honored from a block that
//     did not decode — so a purely personal OPF typo must not switch
//     tracking off machine-wide. The raw bytes stay preserved in extra so a
//     read-modify-write (`entire trust`) cannot destroy the user's content,
//     and the reason is recorded for the surfaces (status, hooks, doctor).
//
// A top-level key absent from this table belongs to a newer binary and is
// kept verbatim for round-tripping (see UserSettings).
//
// Adding a block: a field on UserSettings, an entry here, nothing else.
const (
	userSettingsGlobalKey    = "global"
	userSettingsRedactionKey = "redaction"
)

// userSettingsBlock is the decode/encode pair for one known block. decode
// receives the raw block (never null — decodeStrictBlock maps null to an
// absent block) and must leave us untouched on error; encode reports the
// value to write and whether it is set.
type userSettingsBlock struct {
	decode func(us *UserSettings, raw json.RawMessage) error
	encode func(us *UserSettings) (value any, set bool)
	// fatal: a decode error fails the whole file instead of dropping the block.
	fatal bool
}

var userSettingsBlocks = map[string]userSettingsBlock{
	userSettingsGlobalKey: {
		decode: func(us *UserSettings, raw json.RawMessage) error {
			v, err := decodeStrictBlock[GlobalConfig](raw)
			if err != nil {
				return err
			}
			us.Global = v
			return nil
		},
		encode: func(us *UserSettings) (any, bool) { return us.Global, us.Global != nil },
		fatal:  true,
	},
	userSettingsRedactionKey: {
		decode: func(us *UserSettings, raw json.RawMessage) error {
			v, err := decodeStrictBlock[UserRedactionConfig](raw)
			if err != nil {
				return err
			}
			if v != nil {
				if err := v.validate(); err != nil {
					return err
				}
			}
			us.Redaction = v
			return nil
		},
		encode: func(us *UserSettings) (any, bool) { return us.Redaction, us.Redaction != nil },
	},
}

// decodeStrictBlock decodes raw into a T, rejecting unknown keys. A JSON null
// is "unset" and yields a nil pointer, exactly like an absent block.
func decodeStrictBlock[T any](raw json.RawMessage) (*T, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
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

// UnmarshalJSON decodes the user settings file with per-block strictness: each
// block in userSettingsBlocks is strict (an unknown key inside it is an
// error), a fatal block's error fails the file, a non-fatal block's error
// drops that block alone — bytes preserved, reason recorded — and unknown
// top-level blocks are kept verbatim for round-tripping. See the
// userSettingsBlocks comment for which block gets which treatment and why.
func (us *UserSettings) UnmarshalJSON(data []byte) error {
	var blocks map[string]json.RawMessage
	if err := json.Unmarshal(data, &blocks); err != nil {
		return fmt.Errorf("user settings: %w", err)
	}
	*us = UserSettings{}
	for key, raw := range blocks {
		block, known := userSettingsBlocks[key]
		if !known {
			us.preserveRaw(key, raw, len(blocks))
			continue
		}
		err := block.decode(us, raw)
		if err == nil {
			continue
		}
		if block.fatal {
			return fmt.Errorf("%s: %w", key, err)
		}
		// Dropped, not fatal: keep the user's bytes so a read-modify-write
		// cannot delete them, and record why for the reporting surfaces.
		us.preserveRaw(key, raw, len(blocks))
		if us.blockErrs == nil {
			us.blockErrs = make(map[string]string, 1)
		}
		us.blockErrs[key] = err.Error()
	}
	return nil
}

func (us *UserSettings) preserveRaw(key string, raw json.RawMessage, sizeHint int) {
	if us.extra == nil {
		us.extra = make(map[string]json.RawMessage, sizeHint)
	}
	us.extra[key] = raw
}

// RedactionError reports why the redaction block was dropped during decode,
// or "" when it decoded (or was absent). The dropped block's bytes are still
// in the file; nothing from it is honored until the user fixes it.
func (us *UserSettings) RedactionError() string {
	if us == nil {
		return ""
	}
	return us.blockErrs[userSettingsRedactionKey]
}

// MarshalJSON writes every known block that is set plus every preserved
// unknown block. Value receiver on purpose — see the UserSettings type comment.
func (us UserSettings) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(us.extra)+len(userSettingsBlocks))
	for key, raw := range us.extra {
		out[key] = raw
	}
	for key, block := range userSettingsBlocks {
		value, set := block.encode(&us)
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

// LoadUserSettings decodes user-global settings with per-block strictness
// (UnmarshalJSON). A missing file is an unconfigured tier; malformed JSON, or
// an unknown key inside the `global` block, returns an error so callers can
// fail closed.
func LoadUserSettings(_ context.Context) (*UserSettings, error) {
	path, err := resolveUserSettingsPath()
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
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("parsing user settings: multiple JSON values")
		}
		return nil, fmt.Errorf("parsing user settings: trailing data: %w", err)
	}
	return &settings, nil
}

// ModifyUserSettings performs the only supported read-modify-write operation
// for user settings. A cross-process lock prevents concurrent consent writers
// from losing one another's changes.
func ModifyUserSettings(ctx context.Context, fn func(*UserSettings) error) error {
	path, err := resolveUserSettingsPath()
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
	settings, err := LoadUserSettings(ctx)
	if err != nil {
		return err
	}
	if err := fn(settings); err != nil {
		return err
	}
	return persistUserSettings(path, settings)
}

func persistUserSettings(path string, settings *UserSettings) error {
	data, err := jsonutil.MarshalIndentWithNewline(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding user settings: %w", err)
	}
	if err := jsonutil.WriteFileAtomicFollowingSymlinks(path, data, 0o600); err != nil {
		return fmt.Errorf("writing user settings: %w", err)
	}
	return nil
}

// NormalizeOrigin reduces a Git remote URL to lowercase host/owner/repo form.
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

// ExpandTilde expands and validates an exclusion/trust path.
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
		return "", errors.New("pattern must be an absolute path (a drive-rooted path on Windows)")
	}
	return filepath.ToSlash(filepath.Clean(pattern)), nil
}

func splitGlobPrefix(pattern string) (prefix, rest string) {
	segments := strings.Split(pattern, "/")
	for i, segment := range segments {
		if strings.ContainsAny(segment, "*?[{") {
			return strings.Join(segments[:i], "/"), strings.Join(segments[i:], "/")
		}
	}
	return pattern, ""
}

func resolveGlobPrefixSymlinks(expanded string) string {
	prefix, rest := splitGlobPrefix(expanded)
	if prefix == "" || prefix == "/" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(filepath.FromSlash(prefix))
	if err != nil {
		return ""
	}
	slashed := filepath.ToSlash(resolved)
	if slashed == prefix {
		return ""
	}
	if rest != "" {
		return slashed + "/" + rest
	}
	return slashed
}

func checkExcludePathPattern(pattern string) (string, error) {
	expanded, err := ExpandTilde(pattern)
	if err != nil {
		return "", err
	}
	if expanded == "" {
		return "", nil
	}
	if !doublestar.ValidatePattern(expanded) {
		return "", errors.New("invalid glob")
	}
	return expanded, nil
}

// MatchesExcludePath reports whether a root matches an exclusion glob.
func MatchesExcludePath(ctx context.Context, patterns []string, worktreeRoot string) (bool, error) {
	return MatchesExcludePathFold(ctx, patterns, worktreeRoot, caseInsensitivePaths)
}

// MatchesExcludePathFold is the fold-explicit matching seam.
func MatchesExcludePathFold(_ context.Context, patterns []string, worktreeRoot string, fold bool) (bool, error) {
	roots := rootMatchForms(worktreeRoot)
	if fold {
		foldPathForms(roots)
	}
	for i, pattern := range patterns {
		expanded, err := checkExcludePathPattern(pattern)
		if err != nil {
			return false, fmt.Errorf("exclude_paths[%d]: %w", i, err)
		}
		if expanded == "" {
			continue
		}
		variants := []string{expanded}
		if alternate := resolveGlobPrefixSymlinks(expanded); alternate != "" {
			variants = append(variants, alternate)
		}
		if fold {
			foldPathForms(variants)
		}
		for _, variant := range variants {
			for _, root := range roots {
				matched, matchErr := doublestar.Match(variant, root)
				if matchErr != nil {
					return false, fmt.Errorf("exclude_paths[%d]: invalid glob: %w", i, matchErr)
				}
				if matched {
					return true, nil
				}
				if matched, _ := doublestar.Match(variant+"/**", root); matched { //nolint:errcheck // base pattern was validated
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func rootMatchForms(worktreeRoot string) []string {
	roots := []string{filepath.ToSlash(worktreeRoot)}
	if resolved, err := filepath.EvalSymlinks(worktreeRoot); err == nil {
		if slashed := filepath.ToSlash(resolved); slashed != roots[0] {
			roots = append(roots, slashed)
		}
	}
	return roots
}

func foldPathForms(forms []string) {
	for i := range forms {
		forms[i] = strings.ToLower(forms[i])
	}
}

// MatchesExcludePathExact reports whether a root exactly matches an entry.
func MatchesExcludePathExact(ctx context.Context, entries []string, worktreeRoot string) (bool, error) {
	return MatchesExcludePathExactFold(ctx, entries, worktreeRoot, caseInsensitivePaths)
}

// MatchesExcludePathExactFold is the fold-explicit exact matching seam.
func MatchesExcludePathExactFold(_ context.Context, entries []string, worktreeRoot string, fold bool) (bool, error) {
	roots := rootMatchForms(worktreeRoot)
	if fold {
		foldPathForms(roots)
	}
	for i, entry := range entries {
		expanded, err := ExpandTilde(entry)
		if err != nil {
			return false, fmt.Errorf("exclude_paths_exact[%d]: %w", i, err)
		}
		if expanded == "" {
			continue
		}
		variants := []string{expanded}
		if resolved, err := filepath.EvalSymlinks(filepath.FromSlash(expanded)); err == nil {
			if slashed := filepath.ToSlash(resolved); slashed != expanded {
				variants = append(variants, slashed)
			}
		}
		if fold {
			foldPathForms(variants)
		}
		for _, variant := range variants {
			for _, root := range roots {
				if variant == root {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// MatchesExcludeOrigin reports whether a normalized origin matches an entry.
// Entries are doublestar globs over the host/owner/repo form; an entry with no
// glob characters is also accepted as a pasted remote URL
// (`https://github.com/acme/widgets.git`, `git@github.com:acme/widgets`) and
// compared after the same normalization the origin went through — an
// exclusion is a veto, and one that silently never matched because the user
// pasted a URL instead of a key would fail open.
func MatchesExcludeOrigin(_ context.Context, patterns []string, normalizedOrigin string) (bool, error) {
	for i, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		lowered := strings.ToLower(pattern)
		if !doublestar.ValidatePattern(lowered) {
			return false, fmt.Errorf("exclude_origins[%d]: invalid glob: %w", i, doublestar.ErrBadPattern)
		}
		if normalizedOrigin == "" {
			continue
		}
		matched, err := doublestar.Match(lowered, normalizedOrigin)
		if err != nil {
			return false, fmt.Errorf("exclude_origins[%d]: invalid glob: %w", i, err)
		}
		if matched {
			return true, nil
		}
		if !strings.ContainsAny(pattern, "*?[{") && NormalizeOrigin(pattern) == normalizedOrigin {
			return true, nil
		}
	}
	return false, nil
}

// ValidateGlobalPatterns validates exclusion and trusted path entries without
// consulting repository state.
func ValidateGlobalPatterns(config *GlobalConfig) []string {
	if config == nil {
		return nil
	}
	problems := validateGlobalExclusions(config)
	for i, entry := range config.TrustedPaths {
		if _, err := ExpandTilde(entry); err != nil {
			problems = append(problems, fmt.Sprintf("trusted_paths[%d]: %v", i, err))
		}
	}
	return problems
}

// validateGlobalExclusions is the fail-closed subset of validation. Invalid
// trust grants cannot grant egress, but they do not disable otherwise valid
// activation; exclusion mistakes must deactivate before repository discovery.
func validateGlobalExclusions(config *GlobalConfig) []string {
	var problems []string
	for i, pattern := range config.ExcludePaths {
		if _, err := checkExcludePathPattern(pattern); err != nil {
			problems = append(problems, fmt.Sprintf("exclude_paths[%d]: %v", i, err))
		}
	}
	for i, entry := range config.ExcludePathsExact {
		if _, err := ExpandTilde(entry); err != nil {
			problems = append(problems, fmt.Sprintf("exclude_paths_exact[%d]: %v", i, err))
		}
	}
	for i, pattern := range config.ExcludeOrigins {
		pattern = strings.TrimSpace(pattern)
		if pattern != "" && !doublestar.ValidatePattern(strings.ToLower(pattern)) {
			problems = append(problems, fmt.Sprintf("exclude_origins[%d]: invalid glob", i))
		}
	}
	return problems
}
