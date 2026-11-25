package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

	state userHookState
}

// Ensure writes the file when it is missing or differs, under the shared
// user-level lock while wanted holds. A file there that is not Entire's is
// refused, not overwritten.
func (g GeneratedUserFile) Ensure(ctx context.Context, wanted func(context.Context) bool) (UserHookInstall, error) {
	if configDirMissing(g.Home) {
		return UserHookInstall{Path: g.Path}, userHooksNotInUseError{dir: g.Home}
	}
	return g.state.ensure(ctx, g.Path, wanted, g.apply)
}

func (g GeneratedUserFile) apply(rec *userHookRecord) (UserHookInstall, error) {
	result := UserHookInstall{Path: g.Path}
	file := UserConfigFile{Path: g.Path}
	existing, err := file.Read()
	switch {
	case err == nil && generatedStateFromContent(string(existing), g.Marker, g.Content) == HooksCurrent:
		// Equal but for line endings (a CRLF checkout of a dotfiles repo):
		// rewriting would only be converted back.
		return result, nil
	case err == nil && !strings.Contains(string(existing), g.Marker):
		return result, fmt.Errorf("refusing to overwrite foreign file at %s", g.Path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return result, fmt.Errorf("read %s: %w", g.Path, err)
	}
	result.Changed, result.Updated = 1, err == nil
	if rec == nil {
		return result, nil
	}
	created := g.missingDirs()
	// The user's own config directory: plain MkdirAll, which follows a
	// symlinked dotfiles directory as the file write does.
	if err := os.MkdirAll(filepath.Dir(g.Path), 0o755); err != nil { //nolint:gosec // the agent reads the file itself
		return result, fmt.Errorf("create %s: %w", filepath.Dir(g.Path), err)
	}
	for _, dir := range created {
		if !slices.Contains(rec.CreatedDirs, dir) {
			rec.CreatedDirs = append(rec.CreatedDirs, dir)
		}
	}
	return result, file.Write([]byte(g.Content), 0o644)
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

// Remove deletes Entire's file, and then, when empty, the directories Entire
// created for it (with RemoveDir, the file's own too), leaving a file that is
// not Entire's alone. Reports whether it removed one.
func (g GeneratedUserFile) Remove(ctx context.Context) (bool, error) {
	if configDirMissing(g.Home) {
		return false, nil
	}
	removed := false
	err := g.state.locked(ctx, g.Path, func(rec *userHookRecord) error {
		file := UserConfigFile{Path: g.Path}
		data, err := file.Read()
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return fmt.Errorf("read %s: %w", g.Path, err)
		case !strings.Contains(string(data), g.Marker):
			return nil
		default:
			if err := file.Remove(); err != nil {
				return err
			}
			removed = true
		}
		dirs := slices.Clone(rec.CreatedDirs)
		if g.RemoveDir {
			dirs = append(dirs, filepath.Dir(g.Path))
		}
		removeDirsIfEmpty(dirs)
		*rec = userHookRecord{}
		return nil
	})
	return removed, err
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
		// Not "missing": no command will add the hooks while it is there.
		report.State = UserHooksUnavailable
		report.Err = fmt.Errorf("a file that is not Entire's is in the way at %s; move it for Entire to add its hooks", g.Path)
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
func (GeneratedUserHooks[S]) EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (UserHookInstall, error) {
	var source S
	file, err := source.UserFile()
	if err != nil {
		return UserHookInstall{}, err //nolint:wrapcheck // the source names the path it could not resolve
	}
	return file.Ensure(ctx, wanted)
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

// UserLevelMarker opens a user-level generated file in place of the project
// copy's "Auto-generated by `entire enable --agent <name>`" line, and marks
// the file as Entire's.
const UserLevelMarker = "Auto-generated by `entire enable --global`"

// MarkUserLevel swaps a user-level render's first projectMarker (its header
// line) for UserLevelMarker; later ones, such as the project-copy check, stay.
func MarkUserLevel(render, projectMarker string) string {
	return strings.Replace(render, projectMarker, UserLevelMarker, 1)
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
