package agentimport

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// initRepoWithFormat creates an on-disk repo in the given object format and
// returns it with its single commit's ID.
//
// On disk deliberately, not in memory: git.WithObjectFormat has no effect on a
// pre-made memory.NewStorage, so an in-memory "sha256" repo silently stays
// SHA-1 — both formats produce the same 40-char hash and the same empty
// extensions.objectformat. The earlier version of this test did exactly that
// and so ran SHA-1 twice, leaving the HexSize() branch (the only reason
// ValidateAnchorCommit does not hard-code 40) with no coverage at all.
// extensions.objectformat is what carries the format, and only a real
// repository has one.
func initRepoWithFormat(t *testing.T, format formatcfg.ObjectFormat) (*git.Repository, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(skippable bool, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, err := cmd.CombinedOutput()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
		// Only the format-dependent steps may skip. Letting every git call skip
		// means an unrelated breakage silently deletes a subtest — including the
		// sha1 one, which no git can legitimately skip — and `go test` reports
		// that as success. This test exists because its predecessor passed while
		// testing nothing; it must not be able to do so again.
		if skippable {
			t.Skipf("git %v failed (no support for %s?): %v\n%s", args, format, err, out)
		}
		t.Fatalf("git %v: %v\n%s", args, err, out)
		return ""
	}
	run(true, "init", "--object-format="+string(format), ".")
	if got := run(true, "rev-parse", "--show-object-format=storage"); got != string(format) {
		t.Skipf("git initialized object format %q, not %s", got, format)
	}
	runGit := func(args ...string) string { t.Helper(); return run(false, args...) }
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@test.com")
	runGit("config", "commit.gpgsign", "false")
	runGit("commit", "--allow-empty", "-m", "anchor")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo, runGit("rev-parse", "HEAD")
}

func TestValidateAnchorCommit_ObjectFormats(t *testing.T) {
	t.Parallel()
	for _, format := range []formatcfg.ObjectFormat{formatcfg.SHA1, formatcfg.SHA256} {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			repo, commit := initRepoWithFormat(t, format)
			// Pin that the fixture is the format it claims, so a future
			// regression degrades to a skip rather than to a silent duplicate
			// of the SHA-1 case.
			if want := format.HexSize(); len(commit) != want {
				t.Fatalf("fixture commit %q is %d chars, want %d for %s", commit, len(commit), want, format)
			}
			got, err := ValidateAnchorCommit(repo, strings.ToUpper(commit))
			if err != nil || got != commit {
				t.Fatalf("ValidateAnchorCommit = %q, %v; want %q", got, err, commit)
			}
			// A length valid under the OTHER object format is not a full ID
			// here. All-'a' is hex, so this reaches the length check and only
			// the length check.
			wrongSize := formatcfg.SHA256.HexSize()
			if format == formatcfg.SHA256 {
				wrongSize = formatcfg.SHA1.HexSize()
			}
			_, err = ValidateAnchorCommit(repo, strings.Repeat("a", wrongSize))
			if err == nil {
				t.Fatal("accepted another object format's length")
			}
			if !strings.Contains(err.Error(), "hexadecimal commit ID") {
				t.Errorf("wrong-length error = %q, want the length complaint", err)
			}
		})
	}
}

func repoHeadSHA(t *testing.T, repo *git.Repository) string {
	t.Helper()
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Hash().String()
}

// Snapshot files as well as refs: opening a writable store must not initialize
// checkpoint data or session state before the invalid anchor is rejected.
func importGitFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(filepath.Join(dir, ".git"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRun_RejectsInvalidAnchorBeforeWrites(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"import", "dry-run", "already-imported"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			for _, name := range []string{"empty", "short", "overlong", "nonhex", "revision", "expression", "missing", "tree", "blob", "tag", "hex-ref"} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					repo, dir := initRepoWithCommit(t)
					sha := repoHeadSHA(t, repo)
					commit, err := repo.CommitObject(plumbing.NewHash(sha))
					if err != nil {
						t.Fatal(err)
					}
					file, err := commit.File("f.txt")
					if err != nil {
						t.Fatal(err)
					}
					tag, err := repo.CreateTag("anchor-tag", commit.Hash, &git.CreateTagOptions{
						Tagger: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()}, Message: "anchor tag",
					})
					if err != nil {
						t.Fatal(err)
					}
					missing := strings.Repeat("a", len(sha))
					if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(missing), commit.Hash)); err != nil {
						t.Fatal(err)
					}
					inputs := map[string]string{
						"empty": "", "short": sha[:8], "overlong": sha + "00", "nonhex": strings.Repeat("z", len(sha)),
						"revision": "HEAD", "expression": "HEAD~1", "missing": strings.Repeat("0", len(sha)),
						"tree": commit.TreeHash.String(), "blob": file.Hash.String(), "tag": tag.Hash().String(), "hex-ref": missing,
					}
					// Assert WHICH complaint each input draws, not merely that
					// it failed. Rejection alone is nearly free: an input of the
					// wrong length or the wrong alphabet becomes the zero hash
					// and fails the object lookup anyway, so "short" and
					// "nonhex" pass with the length and hex checks deleted
					// outright. Pinning the message is what keeps those two
					// checks — and the diagnostics they exist to produce — alive.
					wantErr := map[string]string{
						"empty": "import anchor is required", "short": "must be a full", "overlong": "must be a full",
						"nonhex": "must be hexadecimal", "revision": "must be a full", "expression": "must be a full",
						"missing": "does not resolve to a commit object", "tree": "does not resolve to a commit object",
						"blob": "does not resolve to a commit object", "tag": "does not resolve to a commit object",
						"hex-ref": "does not resolve to a commit object",
					}
					transcripts := t.TempDir()
					writeFixtureSession(t, transcripts, "anchor.jsonl")
					opts := Options{RepoRoot: dir, OverridePath: transcripts, Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC), LinkCommitSHA: sha}
					if mode == "already-imported" {
						// Assert the seeding run actually imported: without
						// this, a fixture that stopped yielding turns would
						// silently turn this mode into a duplicate of "import"
						// while the test stayed green.
						seeded, err := Run(t.Context(), repo, claudeImporter{}, opts)
						if err != nil {
							t.Fatal(err)
						}
						if seeded.TurnsImported == 0 {
							t.Fatal("seeding run imported nothing; this mode would not test already-imported turns")
						}
					}
					before := importGitFiles(t, dir)
					opts.LinkCommitSHA = inputs[name]
					opts.DryRun = mode == "dry-run"
					_, runErr := Run(context.Background(), repo, claudeImporter{}, opts)
					if runErr == nil {
						t.Errorf("expected invalid anchor %q to fail", opts.LinkCommitSHA)
					} else if !strings.Contains(runErr.Error(), wantErr[name]) {
						t.Errorf("anchor %q error = %q, want it to contain %q", opts.LinkCommitSHA, runErr, wantErr[name])
					}
					if after := importGitFiles(t, dir); !reflect.DeepEqual(before, after) {
						t.Error("invalid anchor changed git/checkpoint/session files")
					}
				})
			}
		})
	}
}

// discoverFatalImporter fails the test if Run reaches transcript discovery.
type discoverFatalImporter struct{ t *testing.T }

func (discoverFatalImporter) Name() string               { return string(agent.AgentNameClaudeCode) }
func (discoverFatalImporter) AgentType() types.AgentType { return agent.AgentTypeClaudeCode }
func (i discoverFatalImporter) Discover(_, _ string, _ time.Time, _ []string) ([]SessionFile, error) {
	i.t.Fatal("an invalid anchor must be rejected before transcript discovery")
	return nil, nil
}
func (discoverFatalImporter) SplitTurns(_ SessionFile, _ []byte) ([]Turn, error) { return nil, nil }

// The no-writes assertion above cannot prove the ordering the contract states:
// the checkpoint store is opened after discovery, and in a remoteless temp repo
// opening it reads without leaving a trace, so moving the check below Open
// would still pass there. Stopping before Discover is what pins "before opening
// a writable checkpoint store".
func TestRun_RejectsInvalidAnchorBeforeDiscovery(t *testing.T) {
	t.Parallel()
	// The messages are asserted here because an unset field and a mistyped one
	// are different mistakes: a caller that never set LinkCommitSHA is not
	// helped by advice about hexadecimal length.
	for _, tc := range []struct{ name, sha, wantErr string }{
		{"unset", "", "import anchor is required"},
		{"nonexistent", strings.Repeat("0", 40), "does not resolve to a commit object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, dir := initRepoWithCommit(t)
			_, err := Run(t.Context(), repo, discoverFatalImporter{t: t}, Options{
				RepoRoot: dir, Now: time.Now(), LinkCommitSHA: tc.sha,
			})
			if err == nil {
				t.Fatalf("expected anchor %q to fail", tc.sha)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
