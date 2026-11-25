package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
	// RemoveDir also removes the file's directory, when empty, on removal.
	RemoveDir bool
}

// Ensure writes the file when it is missing or differs, under the shared
// user-level lock while wanted holds. A file there that is not Entire's is
// refused, not overwritten.
func (g GeneratedUserFile) Ensure(ctx context.Context, wanted func(context.Context) bool) (int, error) {
	if configDirMissing(g.Home) {
		return 0, ErrUserHooksNotInUse
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
	// The user's own config directory: plain MkdirAll, which follows a
	// symlinked dotfiles directory as the file write does.
	if err := os.MkdirAll(filepath.Dir(g.Path), 0o755); err != nil { //nolint:gosec // the agent reads the file itself
		return 0, fmt.Errorf("create %s: %w", filepath.Dir(g.Path), err)
	}
	if err := file.Write([]byte(g.Content), 0o644); err != nil {
		return 0, err
	}
	return 1, nil
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
	if g.RemoveDir {
		// Fails, as intended, while anything else is in the directory.
		_ = os.Remove(filepath.Dir(g.Path))
	}
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

// CurrentProjectCopyCheck is the TypeScript function isCurrentProjectCopy(path)
// for a user-level render: true when path holds exactly the project render
// whose ProjectRenderDigest is digest, line endings aside. The user-level copy
// steps aside only for that one: an older or partial project copy may not run
// on the agent version in use, and stepping aside for it would record nothing.
// The file using it imports readFileSync (node:fs) and createHash (node:crypto).
func CurrentProjectCopyCheck(digest string) string {
	return `function isCurrentProjectCopy(path: string): boolean {
  try {
    const text = readFileSync(path, "utf8").replace(/\r\n/g, "\n")
    return createHash("sha256").update(text).digest("hex") === "` + digest + `"
  } catch {
    return false
  }
}
`
}

// ProjectRenderDigest is the hex SHA-256 of a project render, the value
// CurrentProjectCopyCheck compares against.
func ProjectRenderDigest(projectRender string) string {
	sum := sha256.Sum256([]byte(projectRender))
	return hex.EncodeToString(sum[:])
}
