package bbfetch

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// gitEnv is the environment of every git process in these tests: no
// developer config, no inherited repository, and fixed identities.
func gitEnv() []string {
	env := slices.DeleteFunc(gitenv.Isolated(), func(e string) bool {
		for _, k := range []string{"GIT_DIR=", "GIT_COMMON_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=",
			"GIT_OBJECT_DIRECTORY=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=", "GIT_NAMESPACE="} {
			if strings.HasPrefix(e, k) {
				return true
			}
		}
		return false
	})
	return append(env, "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_DATE=1700000000 +0000")
}

// testRepo is a bare repository in a temporary directory.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T, objectFormat string) *testRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	r := &testRepo{t: t, dir: dir}
	r.gitIn(nil, "", "init", "-q", "--bare", "--object-format="+objectFormat, dir)
	return r
}

// gitIn runs git in the repository, or with no repository when dir is "".
func (r *testRepo) gitIn(stdin []byte, dir string, args ...string) []byte {
	r.t.Helper()
	if dir != "" {
		args = append([]string{"--git-dir=" + dir}, args...)
	}
	cmd := execx.NonInteractive(r.t.Context(), "git", args...)
	cmd.Env = gitEnv()
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(r.t, err, "git %v: %s", args, stderr.String())
	return out
}

func (r *testRepo) git(stdin []byte, args ...string) string {
	r.t.Helper()
	return strings.TrimSpace(string(r.gitIn(stdin, r.dir, args...)))
}

func (r *testRepo) blob(content []byte) string {
	r.t.Helper()
	return r.git(content, "hash-object", "-w", "--stdin")
}

// history writes commits whose files change a little in each commit, so
// pack-objects makes deltas, and the extra blobs. It returns every object
// ID of the history and of extra.
func (r *testRepo) history(commits int, extra map[string]string) []string {
	r.t.Helper()
	var objects []string
	parent := ""
	for i := range commits {
		var tree strings.Builder
		for f := range 3 {
			var content strings.Builder
			for line := range 400 {
				fmt.Fprintf(&content, "file %d line %d %s\n", f, line, strings.Repeat("x", (line*7+f)%13))
			}
			fmt.Fprintf(&content, "commit %d\n", i)
			oid := r.blob([]byte(content.String()))
			objects = append(objects, oid)
			fmt.Fprintf(&tree, "100644 blob %s\tfile%d\n", oid, f)
		}
		if i == commits-1 {
			for name, oid := range extra {
				fmt.Fprintf(&tree, "100644 blob %s\t%s\n", oid, name)
				objects = append(objects, oid)
			}
		}
		treeID := r.git([]byte(tree.String()), "mktree")
		args := []string{"commit-tree", treeID, "-m", fmt.Sprintf("commit %d", i)}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		parent = r.git(nil, args...)
		objects = append(objects, treeID, parent)
	}
	return objects
}

// pack returns a pack of objects from pack-objects.
func (r *testRepo) pack(objects []string, ofsDelta bool) []byte {
	r.t.Helper()
	args := []string{"pack-objects", "--stdout", "-q"}
	if ofsDelta {
		args = append(args, "--delta-base-offset")
	}
	return r.gitIn([]byte(strings.Join(objects, "\n")+"\n"), r.dir, args...)
}

// indexPack stores pack in a new repository with git index-pack, checks it
// with git verify-pack, and returns the repository and the verify-pack
// output.
func indexPack(t *testing.T, objectFormat string, pack []byte) (*testRepo, string) {
	t.Helper()
	dst := newTestRepo(t, objectFormat)
	out := dst.git(pack, "index-pack", "--stdin", "--strict")
	name, ok := strings.CutPrefix(out, "pack\t")
	require.True(t, ok, "index-pack said %q", out)
	verify := dst.git(nil, "verify-pack", "-v", filepath.Join(dst.dir, "objects", "pack", "pack-"+name+".idx"))
	return dst, verify
}

// objectList returns `cat-file --batch-check` for every object.
func (r *testRepo) objectList() string {
	r.t.Helper()
	return r.git(nil, "cat-file", "--batch-all-objects", "--batch-check")
}
