package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"golang.org/x/mod/semver"
)

// Host-agnostic remote inspection over the git protocol. Version listing
// uses `git ls-remote --tags`, which behaves identically on GitHub, GitLab,
// Gitea/Forgejo, and any self-hosted server, inherits the user's existing
// git auth (SSH agent, credential helpers), and needs no forge REST client.
// The shell-out (vs go-git) is deliberate: this path needs the user's
// credential helpers and proxy config, the same reason hooks.go shells out
// for network-touching operations.

// pluginGitTimeout bounds any single git subprocess this package spawns.
// Without it these calls are unbounded: the root context is WithCancel, not
// WithTimeout, so a black-holed host hangs the command with no output. That
// matters most during dependency planning, which issues one ls-remote plus up
// to two shallow clones per dependency against author-controlled URLs, before
// the user has confirmed anything. The asset download already caps at 5
// minutes; this is the git half of the same bound.
const pluginGitTimeout = 2 * time.Minute

// Output caps for git subprocesses. cmd.Output() buffers the whole of stdout in
// memory, and both stdout-producing calls read remote-controlled data: ls-remote
// returns whatever refs a repository advertises, and `show <tag>:file` returns
// whatever an author committed. A hostile repository could otherwise hand us an
// arbitrarily large response and take the process down.
const (
	// maxGitRefsOutput bounds `ls-remote`. Each ref line is ~60 bytes, so this
	// still admits a repository with hundreds of thousands of tags.
	maxGitRefsOutput = 16 << 20
	// maxPluginMetadataSize bounds entire-plugin.yml. It holds a name, a
	// description, a URL template and a short requires list; 64 KiB is orders
	// of magnitude more than any real one needs.
	maxPluginMetadataSize = 64 << 10
	// maxGitNoOutput is for commands run with --quiet that should print
	// nothing. A non-empty stdout there means something unexpected happened.
	maxGitNoOutput = 4 << 10
	// maxGitStderr bounds captured stderr. Capping stdout alone left the same
	// hole open on the other channel: the remote controls what git echoes, and
	// a bytes.Buffer grows without limit. git's own diagnostics are short, so
	// this only ever truncates a hostile one.
	maxGitStderr = 64 << 10
)

// errOutputTooLarge fails a write once a subprocess exceeds its cap.
var errOutputTooLarge = errors.New("output exceeds the limit")

// limitedWriter buffers at most limit bytes of a subprocess stream.
//
// Used as cmd.Stdout/cmd.Stderr rather than reading a pipe ourselves: os/exec
// closes the pipe as soon as a write fails, so an oversize response kills the
// child immediately, where reading a pipe and returning early left Wait
// blocked on a full pipe until pluginGitTimeout — a two-minute hang in exactly
// the case the cap exists to handle.
//
// Overflow policy differs per stream. stdout fails, because a truncated
// payload is not the payload. stderr absorbs and drops the excess: diagnostics
// are best-effort and must never fail a command git itself completed. Either
// way the buffered prefix is kept, so fetchPluginMetadataAtTag can still match
// git's message — git emits that before any remote-supplied bulk.
type limitedWriter struct {
	buf            bytes.Buffer
	limit          int64
	exceeded       bool
	failOnOverflow bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	room := w.limit - int64(w.buf.Len())
	if int64(len(p)) <= room {
		n, err := w.buf.Write(p)
		return n, err //nolint:wrapcheck // bytes.Buffer.Write never errors
	}
	if room > 0 {
		_, _ = w.buf.Write(p[:room])
	}
	w.exceeded = true
	if w.failOnOverflow {
		return 0, errOutputTooLarge
	}
	return len(p), nil
}

// runGitDiscard runs a git command that is expected to print nothing, and
// reports only whether it succeeded. Every --quiet call site wants this shape.
func runGitDiscard(ctx context.Context, args ...string) error {
	_, err := runGitQuiet(ctx, maxGitNoOutput, args...)
	return err
}

// runGitQuiet runs git and returns stdout, with a bounded deadline and an
// environment that cannot block on a prompt.
//
// LC_ALL=C/LANG=C keep git's messages in English: fetchPluginMetadataAtTag decides
// the benign "no metadata file" case by substring-matching git's stderr, and
// git builds with NLS translate those strings — on a localized host every
// plugin repo without an entire-plugin.yml (the documented default) would
// otherwise fail to install. BatchMode stops ssh prompting for a key
// passphrase on /dev/tty, which GIT_TERMINAL_PROMPT does not cover — but only
// when the user has not set GIT_SSH_COMMAND themselves.
func runGitQuiet(ctx context.Context, maxOutput int64, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, pluginGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "LANG=C")
	// Only supply a batch-mode ssh when the user has not configured one. A
	// blanket GIT_SSH_COMMAND would override both the env var and
	// core.sshCommand, discarding a custom ssh invocation (jump hosts, an
	// explicit identity) and breaking clones that only work through it.
	// Respecting an existing setting costs the passphrase-prompt guard in that
	// one case, which is the right trade — the user chose that command.
	if _, set := os.LookupEnv("GIT_SSH_COMMAND"); !set {
		env = append(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	}
	cmd.Env = env

	// Not cmd.Output(): that buffers stdout to EOF with no bound, and leaves
	// stderr unbounded too. Both streams get a cap instead.
	stdout := &limitedWriter{limit: maxOutput, failOnOverflow: true}
	stderr := &limitedWriter{limit: maxGitStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	switch {
	case stdout.exceeded:
		// Checked before runErr: killing the child is how the cap is enforced,
		// so the exit status here is a symptom, not the diagnosis.
		return nil, fmt.Errorf("git %s: stdout exceeds the %d-byte limit", args[0], maxOutput)
	case runErr != nil:
		// stderr is folded in here rather than left for callers to dig out of
		// ExitError: cmd.Stderr is set, so ExitError.Stderr is empty now.
		return stdout.buf.Bytes(), fmt.Errorf("git %s: %w%s", args[0], runErr, stderrDetail(stderr.buf.Bytes()))
	}
	return stdout.buf.Bytes(), nil
}

// stderrDetail formats captured git stderr for an error message. git's stderr
// is where the actionable detail lives, and it can echo a remote URL, so it is
// scrubbed for credentials on the way out.
func stderrDetail(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return ""
	}
	return ": " + redactCredentials(text)
}

// redactURL strips credentials from a URL so they never reach stderr,
// .entire/logs/ — which lives in the user's working tree and is collected
// wholesale by `entire doctor bundle` — or a persisted manifest.yml.
//
// Delegates to the repo-wide helper rather than reimplementing it: that one also
// drops the query string, which matters here because release hosts redirect to
// signed CDN URLs carrying ?token=/X-Amz-Signature=. A local implementation that
// only stripped userinfo would have leaked those.
func redactURL(raw string) string {
	return gitremote.RedactURL(raw)
}

// redactCredentials removes userinfo from any URL appearing in free text, for
// git's stderr where we do not know which URL was echoed.
var credentialInURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]*@`)

func redactCredentials(text string) string {
	return credentialInURL.ReplaceAllString(text, "${1}")
}

// gitURLSchemes are the transports a plugin or index repository URL may use in
// production.
//
// http:// and git:// are deliberately absent. Both are unauthenticated and
// unencrypted, and the catalog fetched over them decides which repositories
// install without a confirmation prompt — so an attacker who can rewrite the
// transport chooses the binary. That is strictly worse than the plaintext-asset
// case the download path already refuses.
//
// file:// is absent too, and only accepted under test (see allowLocalGitURL).
// Nothing in production needs it: a file:// plugin repository cannot even
// complete an install, because releaseAssetBaseURL requires an http(s) repo URL
// to derive the asset location. Its only real consumer was the test suite, and
// a security allowlist should not be widened to pay for test convenience —
// especially when the alternative transports a test could use are worse
// (git daemon serves git://, which is exactly what was removed above).
var gitURLSchemes = []string{"https://", "ssh://"}

// envAllowFileRemotes lets a spawned test binary accept file:// remotes.
// testing.Testing() is false in a subprocess, so the integration harness — which
// runs the real `entire` against file:// fixture repos — has to say so through
// the environment, the same way it passes the rest of its isolation down.
const envAllowFileRemotes = "ENTIRE_TEST_ALLOW_FILE_REMOTES"

// fileRemotesAllowed is the policy itself, taking its two inputs as arguments
// so it can be tested directly. In-process, testing.Testing() is always true
// where a test runs, so a test that reads the ambient value can only ever
// observe "allowed" — the spawned-binary case, which is the one the env var
// exists for, is unreachable without parameterizing it.
func fileRemotesAllowed(underTest bool, envValue string) bool {
	return underTest || envValue != ""
}

// allowLocalGitURL reports whether file:// remotes are acceptable. True only
// under `go test`, or in a binary a test harness spawned.
//
// Setting the env var is not a meaningful weakening: anyone able to set it on
// the victim's machine already has code execution. What it buys is that the
// shipped allowlist has no local-filesystem transport, so a hostile catalog
// entry cannot use one to probe the filesystem.
//
// A var, not a func, so a test can flip it and exercise the shipped rejection.
// Same seam pattern as postPluginVersionCheck in this package.
//
//nolint:gochecknoglobals // test seam; see above
var allowLocalGitURL = func() bool {
	return fileRemotesAllowed(testing.Testing(), os.Getenv(envAllowFileRemotes))
}

// scpLikeGitURL matches git's scp-like syntax (user@host:path) — the form
// `git@github.com:owner/repo.git` uses. Requires a user@, a host without a
// slash, and a colon before the path, so it cannot match a bare option or a
// local path.
var scpLikeGitURL = regexp.MustCompile(`^[A-Za-z0-9._~-]+@[A-Za-z0-9._-]+:[^\s]`)

// validatePluginRepoURL rejects anything that is not recognizably a git
// repository URL before it reaches the git CLI as a positional argument.
//
// This is a security boundary, not a nicety. These URLs arrive from
// attacker-influenced places — index.json entries, another plugin's
// entire-plugin.yml requires[], a stored manifest, --index — and git parses an
// option-shaped positional as an option. `--upload-pack=<cmd>` is
// shell-interpreted and runs against the *ambient* repo when no positional
// remains, so an entry like "--upload-pack=curl … | sh; git-upload-pack" would
// execute arbitrary commands during an index-resolved install, which never
// prompts. validatePluginName already refuses a leading '-' on names for the
// same reason.
//
// Local remotes — both bare absolute paths and file:// URLs — are rejected
// outside tests. A local plugin repository could not complete an install anyway
// (releaseAssetBaseURL needs an http(s) repo URL to locate the asset), and
// `install ./path` is the supported way to use a local build. Callers
// additionally pass "--" before positionals, so an option-shaped value would be
// refused by git even if it got this far.
func validatePluginRepoURL(rawURL string) error {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return errors.New("repository URL is empty")
	}
	// Before the scheme checks, because the scp-like form bypasses them and
	// RedactURL passes it through untouched — no url.Parse, which is what
	// rejects control characters in the https:// case. So an scp-like URL was
	// the one shape that could carry an escape sequence all the way into the
	// confirmation prompt that names the repository.
	if hasTerminalControlChars(u) {
		return fmt.Errorf("repository URL %q must not contain control characters", rawURL)
	}
	if strings.HasPrefix(u, "-") {
		return fmt.Errorf("repository URL %q must not start with '-'", redactURL(rawURL))
	}
	for _, scheme := range gitURLSchemes {
		if strings.HasPrefix(u, scheme) && len(u) > len(scheme) {
			return nil
		}
	}
	if strings.HasPrefix(u, "file://") && len(u) > len("file://") && allowLocalGitURL() {
		return nil
	}
	if scpLikeGitURL.MatchString(u) {
		return nil
	}
	return fmt.Errorf("repository URL %q must use one of %s or git's user@host:path form",
		redactURL(rawURL), strings.Join(gitURLSchemes, ", "))
}

// listRemoteSemverTags returns the repo's semver tags ("v"-prefixed or
// bare), sorted descending by semantic version. Non-semver tags are
// ignored.
func listRemoteSemverTags(ctx context.Context, repoURL string) ([]string, error) {
	if err := validatePluginRepoURL(repoURL); err != nil {
		return nil, err
	}
	// --refs suppresses peeled ^{} entries for annotated tags. "--" keeps an
	// option-shaped URL from being parsed as a git option.
	out, err := runGitQuiet(ctx, maxGitRefsOutput, "ls-remote", "--tags", "--refs", "--", repoURL)
	if err != nil {
		return nil, fmt.Errorf("list tags of %s: %w", redactURL(repoURL), err)
	}
	var tags []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		tag := strings.TrimPrefix(fields[1], "refs/tags/")
		canon := canonicalSemver(tag)
		if !semver.IsValid(canon) {
			continue
		}
		// Prereleases are excluded: semver ranks v2.0.0-rc1 above stable
		// v1.9.0, so an author pushing a release candidate would migrate every
		// user onto it on the next `plugin upgrade --all`. --pin installs an
		// exact tag and bypasses this listing, which is the opt-in.
		if semver.Prerelease(canon) != "" {
			continue
		}
		tags = append(tags, tag)
	}
	// Stable, so tags that compare equal (v1.0.0 vs 1.0.0, or +build metadata,
	// which semver ignores) resolve in a deterministic order.
	sort.SliceStable(tags, func(i, j int) bool {
		return semver.Compare(canonicalSemver(tags[i]), canonicalSemver(tags[j])) > 0
	})
	return tags, nil
}

// canonicalSemver maps a tag to the "v"-prefixed form x/mod/semver expects.
func canonicalSemver(tag string) string {
	if strings.HasPrefix(tag, "v") {
		return tag
	}
	return "v" + tag
}

// fetchPluginMetadataAtTag reads entire-plugin.yml at the given tag without
// a user-visible clone: a blobless shallow clone into a discarded temp dir,
// falling back to a plain shallow clone for servers that don't allow
// partial-clone filters (uploadpack.allowFilter defaults to off outside the
// big forges). Returns (nil, nil) when the repo has no metadata file —
// metadata is optional by design.
func fetchPluginMetadataAtTag(ctx context.Context, repoURL, tag string) (*PluginMetadata, error) {
	if err := validatePluginRepoURL(repoURL); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "entire-plugin-meta-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	cloneArgs := func(filter bool) []string {
		args := []string{"clone", "--depth", "1", "--no-checkout", "--branch", tag, "--quiet"}
		if filter {
			args = append(args, "--filter=blob:none")
		}
		// "--" so an option-shaped URL can't be parsed as a git option.
		return append(args, "--", repoURL, tmp)
	}
	if err := runGitDiscard(ctx, cloneArgs(true)...); err != nil {
		// Retry without the filter; the clone dir may be half-created.
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			return nil, fmt.Errorf("clean temp clone dir: %w", rmErr)
		}
		if err := runGitDiscard(ctx, cloneArgs(false)...); err != nil {
			return nil, fmt.Errorf("clone %s at %s: %w", redactURL(repoURL), tag, err)
		}
	}
	// `git show <rev>:<path>` peels annotated tags and, in a blobless
	// clone, lazily fetches just this one blob from the promisor remote.
	out, err := runGitQuiet(ctx, maxPluginMetadataSize, "-C", tmp, "show", tag+":"+pluginMetadataFileName)
	if err != nil {
		// Missing file and missing tag both surface as exit 128; only the
		// missing-file case is benign. Disambiguate via stderr.
		if strings.Contains(err.Error(), "does not exist") ||
			strings.Contains(err.Error(), "exists on disk, but not in") {
			return nil, nil //nolint:nilnil // metadata is optional
		}
		return nil, fmt.Errorf("read %s at %s: %w", pluginMetadataFileName, tag, err)
	}
	return ParsePluginMetadata(out)
}

// pluginNameFromRepoURL derives the bare plugin name from a repository URL
// basename: .../entire-run(.git) → "run". Used when entire-plugin.yml is
// absent or doesn't declare a name.
func pluginNameFromRepoURL(repoURL string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(repoURL, "/"), ".git")
	base := trimmed
	if i := strings.LastIndexAny(trimmed, "/:"); i >= 0 {
		base = trimmed[i+1:]
	}
	if !strings.HasPrefix(base, pluginBinaryPrefix) {
		return "", fmt.Errorf("repository basename %q does not start with %q; declare a name in %s", base, pluginBinaryPrefix, pluginMetadataFileName)
	}
	name := strings.TrimPrefix(base, pluginBinaryPrefix)
	if err := validatePluginName(name); err != nil {
		return "", err
	}
	return name, nil
}
