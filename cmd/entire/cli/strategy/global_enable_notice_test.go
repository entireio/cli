package strategy

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// noticeRepo is a repository the test has chdir'd into, so gitdir resolves it.
// t.Chdir makes these tests process-global, so none of them call t.Parallel.
func noticeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	return dir
}

func noticeMarker(dir string) string {
	return filepath.Join(dir, ".git", globalEnablePushNoticeName)
}

// The first push that carries checkpoints from a repository on only through
// global enable says so; later pushes from the same clone do not.
func TestNoticeGlobalEnablePush_PrintsOncePerClone(t *testing.T) {
	dir := noticeRepo(t)
	stderr := captureHintStderr(t)
	ps := pushSettings{remote: "origin", enabledOnlyByGlobal: true}

	noticeGlobalEnablePush(t.Context(), ps)
	assert.Contains(t, stderr.String(), `pushed session checkpoints to "origin"`)
	assert.Contains(t, stderr.String(), "not `entire enable`; run 'entire disable' here to stop")
	assert.FileExists(t, noticeMarker(dir))

	stderr.Reset()
	noticeGlobalEnablePush(t.Context(), ps)
	assert.Empty(t, stderr.String())
}

// A repository the developer enabled, or any repository with global enable
// off, gets no notice and no marker.
func TestNoticeGlobalEnablePush_SilentUnlessGlobalOnly(t *testing.T) {
	dir := noticeRepo(t)
	stderr := captureHintStderr(t)

	noticeGlobalEnablePush(t.Context(), pushSettings{remote: "origin"})
	assert.Empty(t, stderr.String())
	assert.NoFileExists(t, noticeMarker(dir))
}

// The notice never prints a URL: one can carry credentials.
func TestNoticeGlobalEnablePush_NeverPrintsAURL(t *testing.T) {
	for name, tc := range map[string]struct {
		ps   pushSettings
		want string
	}{
		"https remote":     {pushSettings{remote: "https://user:secret@example.com/acme/app.git"}, "this push's remote"},
		"scp remote":       {pushSettings{remote: "git@example.com:acme/app.git"}, "this push's remote"},
		"scp without user": {pushSettings{remote: "example.com:acme/app.git"}, "this push's remote"},
		"local path":       {pushSettings{remote: "/srv/git/app.git"}, "this push's remote"},
		"checkpoint url":   {pushSettings{remote: "origin", checkpointURL: "https://user:secret@example.com/acme/cp.git"}, "your configured checkpoint remote"},
	} {
		t.Run(name, func(t *testing.T) {
			noticeRepo(t)
			stderr := captureHintStderr(t)
			tc.ps.enabledOnlyByGlobal = true

			noticeGlobalEnablePush(t.Context(), tc.ps)
			out := stderr.String()
			assert.Contains(t, out, tc.want)
			assert.NotContains(t, out, "example.com")
			assert.NotContains(t, out, "/srv/git")
			assert.NotContains(t, out, "secret")
		})
	}
}
