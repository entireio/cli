package gitrepo

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
)

// configFilePath is the repository-relative path of the git config file as
// go-git opens it (the filesystem is rooted at the git directory).
const configFilePath = "config"

// maxConfigReadBytes bounds how much of the config file sanitization will
// read. Real .git/config files are a few KB even with hundreds of
// remotes/submodules; the cap exists purely to bound memory against a
// pathological or corrupted file and is set far above any realistic config
// size. Unlike a defensive prefix cap, a config that exceeds this limit is
// never silently served truncated — doing so could drop remotes, branches,
// or submodule config past the cutoff without any indication, which is worse
// than the #778 bug this sanitizer exists to fix. sanitizedConfig instead
// returns an error so the caller surfaces a loud failure.
const maxConfigReadBytes = 64 << 20 // 64 MiB

// negativeFetchRefspecRE matches a fetch line whose refspec is a git 2.29+
// negative (exclusion) refspec, e.g. "\tfetch = ^refs/heads/excluded". go-git's
// refspec parser rejects the '^' form with "malformed refspec, separators are
// wrong", which fails every repository open (issue #778).
var negativeFetchRefspecRE = regexp.MustCompile(`^\s*fetch\s*=\s*\^`)

// configSanitizeFS wraps a git-directory filesystem and, on read of the config
// file, drops negative fetch refspec lines that go-git cannot parse. The
// on-disk config is never modified — go-git is served an in-memory copy with
// only the unsupported lines removed, so positive refspecs and every other
// setting are preserved, and native git still honors the negative refspecs from
// the real file.
type configSanitizeFS struct {
	billy.Filesystem // wrapped git-dir FS; promotes the full billy interface
}

// wrapConfigSanitize wraps fs so reads of the config file omit negative fetch
// refspecs. Compose it outside wrapAlternatesRewrite; the two intercept
// different files and do not interfere.
func wrapConfigSanitize(fs billy.Filesystem) billy.Filesystem {
	return &configSanitizeFS{Filesystem: fs}
}

func (fs *configSanitizeFS) Open(filename string) (billy.File, error) {
	if isConfigFile(filename) {
		content, ok, err := fs.sanitizedConfig()
		if err != nil {
			return nil, err
		}
		if ok {
			return inMemoryConfigFile(content)
		}
	}
	return fs.Filesystem.Open(filename) //nolint:wrapcheck // preserve underlying FS errors
}

// OpenFile intercepts read-only opens of the config file too, since go-git may
// reach config through OpenFile rather than Open. Any write intent (a flag
// other than read-only) is passed straight through so config writes always hit
// the real file untouched.
func (fs *configSanitizeFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	if flag == os.O_RDONLY && isConfigFile(filename) {
		content, ok, err := fs.sanitizedConfig()
		if err != nil {
			return nil, err
		}
		if ok {
			return inMemoryConfigFile(content)
		}
	}
	return fs.Filesystem.OpenFile(filename, flag, perm) //nolint:wrapcheck // preserve underlying FS errors
}

func isConfigFile(filename string) bool {
	return filepath.ToSlash(filepath.Clean(filename)) == configFilePath
}

// sanitizedConfig returns a copy of the config with negative fetch refspec
// lines removed. ok is false when the file is missing/unreadable or it
// contained no negative refspecs; in that case the caller serves the
// original file unchanged. err is non-nil when the file exceeds
// maxConfigReadBytes: rather than sanitizing a truncated prefix and silently
// dropping any remotes/branches/submodules configured past the cutoff (which
// could also hide an unstripped negative refspec from go-git), the caller
// surfaces a loud failure instead of serving a partial config.
func (fs *configSanitizeFS) sanitizedConfig() (string, bool, error) {
	f, err := fs.Filesystem.Open(filepath.FromSlash(configFilePath))
	if err != nil {
		return "", false, nil
	}
	defer func() { _ = f.Close() }()

	// Read one byte past the cap so we can tell whether the underlying file
	// has more data than fits in the budget.
	data, err := io.ReadAll(io.LimitReader(f, maxConfigReadBytes+1))
	if err != nil {
		return "", false, nil
	}
	if len(data) > maxConfigReadBytes {
		return "", false, fmt.Errorf("gitrepo: .git/config exceeds %d bytes; refusing to sanitize a partial config (this could silently drop config past the limit)", maxConfigReadBytes)
	}

	text := string(data)

	// Fast path: '^' can only appear in a negative refspec's value, so a
	// config without it never needs sanitizing.
	if !strings.Contains(text, "^") {
		return "", false, nil
	}

	lines := strings.Split(text, "\n")
	kept := lines[:0]
	changed := false
	for _, line := range lines {
		if negativeFetchRefspecRE.MatchString(line) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		return "", false, nil
	}
	return strings.Join(kept, "\n"), true, nil
}

func inMemoryConfigFile(content string) (billy.File, error) {
	mem := memfs.New()
	f, err := mem.Create(configFilePath)
	if err != nil {
		return nil, err //nolint:wrapcheck // memfs errors are descriptive enough
	}
	if _, err := f.Write([]byte(content)); err != nil {
		_ = f.Close()
		return nil, err //nolint:wrapcheck // memfs errors are descriptive enough
	}
	if err := f.Close(); err != nil {
		return nil, err //nolint:wrapcheck // memfs errors are descriptive enough
	}
	return mem.Open(configFilePath) //nolint:wrapcheck // memfs errors are descriptive enough
}
