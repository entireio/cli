package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// GeneratedUserFile is an Entire-generated file (an extension or plugin) in
// an agent's user-level config, recognized as Entire's by Marker.
type GeneratedUserFile struct {
	// Home is the agent's user-level directory; when it does not exist the
	// agent is not in use.
	Home string
	// Path is the file.
	Path string
	// Marker is a string only Entire's file contains.
	Marker string
	// Content is what the file should hold.
	Content string
	// Version finds the stamped hook version in the file (one digit group).
	Version *regexp.Regexp
	// RemoveDir also removes the file's directory, when empty, on removal,
	// even one Entire did not create (directories it created always go).
	RemoveDir bool
}

// Ensure writes the file when it is missing or differs, under the shared
// user-level lock while wanted holds. A file there that is not Entire's is
// refused, not overwritten.
func (g GeneratedUserFile) Ensure(ctx context.Context, wanted func(context.Context) bool) (int, error) {
	if configDirMissing(g.Home) {
		return 0, userHooksNotInUseError{dir: g.Home}
	}
	return EnsureUserHookFile(ctx, g.lockPath(), wanted, g.apply)
}

func (g GeneratedUserFile) apply(write bool) (int, error) {
	file := UserConfigFile{Path: g.Path}
	existing, err := file.Read()
	switch {
	case err == nil && generatedStateFromContent(string(existing), g.Marker, g.Content) == HooksCurrent:
		// Equal but for line endings (a CRLF checkout of a dotfiles repo):
		// rewriting would only be converted back.
		return 0, nil
	case err == nil && !strings.Contains(string(existing), g.Marker):
		return 0, fmt.Errorf("refusing to overwrite foreign file at %s", g.Path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return 0, fmt.Errorf("read %s: %w", g.Path, err)
	}
	if !write {
		return 1, nil
	}
	created := g.missingDirs()
	// The user's own config directory: plain MkdirAll, which follows a
	// symlinked dotfiles directory as the file write does.
	if err := os.MkdirAll(filepath.Dir(g.Path), 0o755); err != nil { //nolint:gosec // the agent reads the file itself
		return 0, fmt.Errorf("create %s: %w", filepath.Dir(g.Path), err)
	}
	if err := g.recordCreatedDirs(created); err != nil {
		return 0, err
	}
	if err := file.Write([]byte(g.Content), 0o644); err != nil {
		return 0, err
	}
	return 1, nil
}

// createdDirsFile records, in Home, the directories Entire created for the
// file, one Home-relative path per line, so removal takes away only those
// (when empty) and never a directory the user had before.
func (g GeneratedUserFile) createdDirsFile() UserConfigFile {
	return UserConfigFile{Path: filepath.Join(g.Home, ".entire-user-hooks-dirs")}
}

// missingDirs returns the file's directory and its ancestors below Home that
// do not exist yet.
func (g GeneratedUserFile) missingDirs() []string {
	var missing []string
	for dir := filepath.Dir(g.Path); dir != g.Home && strings.HasPrefix(dir, g.Home+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		missing = append(missing, dir)
	}
	return missing
}

func (g GeneratedUserFile) recordCreatedDirs(created []string) error {
	if len(created) == 0 {
		return nil
	}
	recorded := g.recordedDirs()
	for _, dir := range created {
		rel, err := filepath.Rel(g.Home, dir)
		if err != nil {
			continue
		}
		recorded = append(recorded, filepath.ToSlash(rel))
	}
	return g.createdDirsFile().Write([]byte(strings.Join(recorded, "\n")+"\n"), 0o600)
}

// recordedDirs returns the Home-relative directories Entire recorded
// creating, ignoring any entry that would leave Home.
func (g GeneratedUserFile) recordedDirs() []string {
	data, err := g.createdDirsFile().Read()
	if err != nil {
		return nil
	}
	var dirs []string
	for _, line := range strings.Split(string(data), "\n") {
		rel := filepath.Clean(filepath.FromSlash(strings.TrimSpace(line)))
		if line == "" || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		dirs = append(dirs, filepath.ToSlash(rel))
	}
	return dirs
}

// removeCreatedDirs removes, deepest first, the directories Entire created
// for the file (and, with RemoveDir, the file's own), each only when empty.
func (g GeneratedUserFile) removeCreatedDirs() {
	var dirs []string
	if g.RemoveDir {
		dirs = append(dirs, filepath.Dir(g.Path))
	}
	for _, rel := range g.recordedDirs() {
		dirs = append(dirs, filepath.Join(g.Home, filepath.FromSlash(rel)))
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		// Fails, as intended, while anything else is in the directory.
		_ = os.Remove(dir)
	}
	_ = g.createdDirsFile().Remove() //nolint:errcheck // a stale record only names directories to try again
}

// Remove deletes Entire's file (and, with RemoveDir, its directory when
// empty), leaving a file that is not Entire's alone. Reports whether it
// removed one.
func (g GeneratedUserFile) Remove(ctx context.Context) (bool, error) {
	if configDirMissing(g.Home) {
		return false, nil
	}
	release, err := LockUserHookFile(ctx, g.lockPath())
	if err != nil {
		return false, err
	}
	defer release()
	file := UserConfigFile{Path: g.Path}
	data, err := file.Read()
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", g.Path, err)
	}
	if !strings.Contains(string(data), g.Marker) {
		return false, nil
	}
	if err := file.Remove(); err != nil {
		return false, err
	}
	g.removeCreatedDirs()
	return true, nil
}

// lockPath is in Home, which exists whenever anything is written: the
// file's own directory may not exist yet on a first install.
func (g GeneratedUserFile) lockPath() string {
	return filepath.Join(g.Home, ".entire-user-hooks.lock")
}

// Inspect reports the file's state without writing.
func (g GeneratedUserFile) Inspect() UserHookReport {
	report := UserHookReport{Path: g.Path}
	if configDirMissing(g.Home) {
		report.State = UserHooksNotInUse
		return report
	}
	data, err := UserConfigFile{Path: g.Path}.Read()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		report.State = UserHooksAbsent
		return report
	case err != nil:
		report.State, report.Err = UserHooksUnavailable, err
		return report
	}
	switch generatedStateFromContent(string(data), g.Marker, g.Content) {
	case HooksAbsent:
		report.State = UserHooksAbsent
		report.Notes = append(report.Notes, "A file that is not Entire's is in the way: "+g.Path)
		return report
	case HooksCurrent:
		report.State = UserHooksCurrent
	case HooksOutdated, HooksForOtherHost:
		report.State = UserHooksOutdated
	}
	if g.Version != nil {
		if m := g.Version.FindSubmatch(data); m != nil {
			report.Version, _ = strconv.Atoi(string(m[1])) //nolint:errcheck // the pattern matched digits
		}
	}
	return report
}

// UserFileSource names an agent's generated user-level file. Implementations
// are zero-size types, so GeneratedUserHooks works in a zero-value agent.
type UserFileSource interface {
	UserFile() (GeneratedUserFile, error)
}

// GeneratedUserHooks implements UserHookInstaller's methods for an agent
// whose user-level hooks are one generated file (an extension or plugin)
// that steps aside itself where a project install exists. Embed it.
type GeneratedUserHooks[S UserFileSource] struct{}

// EnsureUserHooks implements UserHookInstaller.
func (GeneratedUserHooks[S]) EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (int, string, error) {
	var source S
	file, err := source.UserFile()
	if err != nil {
		return 0, "", err //nolint:wrapcheck // the source names the path it could not resolve
	}
	n, err := file.Ensure(ctx, wanted)
	return n, file.Path, err
}

// RemoveUserHooks implements UserHookInstaller.
func (GeneratedUserHooks[S]) RemoveUserHooks(ctx context.Context) (bool, string, error) {
	var source S
	file, err := source.UserFile()
	if err != nil {
		return false, "", err //nolint:wrapcheck // the source names the path it could not resolve
	}
	removed, err := file.Remove(ctx)
	return removed, file.Path, err
}

// InspectUserHooks implements UserHookInstaller.
func (GeneratedUserHooks[S]) InspectUserHooks(context.Context) UserHookReport {
	var source S
	file, err := source.UserFile()
	if err != nil {
		return UserHookReport{State: UserHooksUnavailable, Err: err}
	}
	return file.Inspect()
}

// UserHookFileExists reports whether the generated file is already there, so
// a write that follows replaces it (an older render, or one the user edited)
// rather than adding it.
func (GeneratedUserHooks[S]) UserHookFileExists() bool {
	var source S
	file, err := source.UserFile()
	if err != nil {
		return false
	}
	_, err = os.Lstat(file.Path)
	return err == nil
}

// UserHookFileReplacer is implemented by agents whose user-level hooks are one
// generated file, which an install replaces whole.
type UserHookFileReplacer interface {
	UserHookFileExists() bool
}

// AsUserHookFileReplacer returns the agent as UserHookFileReplacer if it
// implements it. Built-in only, like UserHookInstaller.
func AsUserHookFileReplacer(ag Agent) (UserHookFileReplacer, bool) {
	return builtinCapability[UserHookFileReplacer](ag)
}

// ProjectHooksSupersede implements UserHookInstaller: the generated file
// steps aside itself when the agent loads it, so no hook command needs to ask.
func (GeneratedUserHooks[S]) ProjectHooksSupersede(context.Context) bool { return false }

// RenderUserLevel renders a generated file's template. Each key of lines is a
// whole template line: the project render removes it (staying byte-identical
// to what earlier CLIs wrote), the user-level render replaces it with its
// value.
func RenderUserLevel(template string, userLevel bool, lines map[string]string) string {
	for placeholder, replacement := range lines {
		if !userLevel {
			replacement = ""
		}
		template = strings.Replace(template, placeholder, replacement, 1)
	}
	return template
}

// UserLevelHeader is the comment that opens a generated file's user-level
// code: what it is, its hook version, and why it steps aside. kind names the
// file ("extension", "plugin"); every line starts with indent.
func UserLevelHeader(indent, kind string, version int) string {
	return indent + "// Entire user-level " + kind + ", hook version " + strconv.Itoa(version) + ", installed by\n" +
		indent + "// `entire enable --global`. A project with its own Entire " + kind + " decides\n" +
		indent + "// for itself (its settings, its version), so this copy steps aside there.\n"
}

// ProjectCopyCheck is the TypeScript function isEntireProjectCopy(path,
// requireSetup) for a user-level render: true when path holds a project copy
// of Entire's file (one containing marker), whatever CLI version wrote it, so
// the user-level copy steps aside for it rather than record every event a
// second time. With requireSetup, the copy must also export OpenCode 2's
// setup entry point: an older copy without it does not run there. The file
// using it imports readFileSync (node:fs).
func ProjectCopyCheck(marker string) string {
	return `function isEntireProjectCopy(path: string, requireSetup: boolean): boolean {
  try {
    const text = readFileSync(path, "utf8")
    if (!text.includes(` + strconv.Quote(marker) + `)) return false
    return !requireSetup || /^export default[^\n]*\bsetup\b/m.test(text)
  } catch {
    return false
  }
}
`
}
