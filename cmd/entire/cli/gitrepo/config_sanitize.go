package gitrepo

import (
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

// maxConfigReadBytes caps how much of the config file we read for sanitizing.
// Real .git/config files are a few KB; the cap is purely defensive. If the
// file exceeds it we still sanitize the visible prefix (dropping any trailing
// line truncated by the cap) rather than falling back to the raw file, which
// could carry a negative fetch refspec past the cap and reintroduce #778 for
// oversized configs. See maxAlternatesReadBytes for the analogous rationale.
const maxConfigReadBytes = 1 << 20 // 1 MiB

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
		if content, ok := fs.sanitizedConfig(); ok {
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
		if content, ok := fs.sanitizedConfig(); ok {
			return inMemoryConfigFile(content)
		}
	}
	return fs.Filesystem.OpenFile(filename, flag, perm) //nolint:wrapcheck // preserve underlying FS errors
}

func isConfigFile(filename string) bool {
	return filepath.ToSlash(filepath.Clean(filename)) == configFilePath
}

// sanitizedConfig returns a copy of the config with negative fetch refspec
// lines removed. ok is false when the file is missing/unreadable or it fit
// within the read cap and contained no negative refspecs; in those cases the
// caller serves the original file unchanged. When the file exceeds the cap,
// ok is true even if no line needed stripping so the caller never falls back
// to the uncapped original — which could contain a negative refspec past the
// cap that go-git would still choke on.
func (fs *configSanitizeFS) sanitizedConfig() (string, bool) {
	f, err := fs.Filesystem.Open(filepath.FromSlash(configFilePath))
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	// Read one byte past the cap so we can tell whether the underlying file
	// has more data than fits in the budget.
	data, err := io.ReadAll(io.LimitReader(f, maxConfigReadBytes+1))
	if err != nil {
		return "", false
	}
	truncated := false
	if len(data) > maxConfigReadBytes {
		data = data[:maxConfigReadBytes]
		truncated = true
	}

	text := string(data)
	lines := strings.Split(text, "\n")
	if truncated && !strings.HasSuffix(text, "\n") && len(lines) > 0 {
		// Discard the trailing line cut off by the cap rather than feed a
		// partial (and possibly still-negative) refspec line through unseen.
		lines = lines[:len(lines)-1]
	}

	// Fast path: '^' can only appear in a negative refspec's value, so an
	// untruncated config without it never needs sanitizing. A truncated read
	// must still be treated as changed, since content past the cap may hold
	// a negative refspec we haven't seen.
	if !truncated && !strings.Contains(text, "^") {
		return "", false
	}

	kept := lines[:0]
	changed := false
	for _, line := range lines {
		if negativeFetchRefspecRE.MatchString(line) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed && !truncated {
		return "", false
	}
	return strings.Join(kept, "\n"), true
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
