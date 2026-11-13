package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCrossRepoCommitsAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content *string
		want    bool
	}{
		{name: "missing file is off"},
		{name: "on", content: new("on\n"), want: true},
		{name: "on without newline", content: new("on"), want: true},
		{name: "off", content: new("off\n")},
		{name: "garbage is off", content: new("yes\n")},
		{name: "wrong case is off", content: new("On\n")},
		{name: "empty is off", content: new("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			commonDir := checkpointPushPolicyTestCommonDir(t)
			if tt.content != nil {
				path := filepath.Join(commonDir, CrossRepoCommitsFile)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
				require.NoError(t, os.WriteFile(path, []byte(*tt.content), 0o600))
			}
			assert.Equal(t, tt.want, LoadCrossRepoCommitsAt(commonDir))
		})
	}
}

func TestLoadCrossRepoCommitsAt_SymlinkIsOff(t *testing.T) {
	t.Parallel()
	commonDir := checkpointPushPolicyTestCommonDir(t)
	target := filepath.Join(t.TempDir(), "on")
	require.NoError(t, os.WriteFile(target, []byte("on\n"), 0o600))
	path := filepath.Join(commonDir, CrossRepoCommitsFile)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.Symlink(target, path))

	assert.False(t, LoadCrossRepoCommitsAt(commonDir))
}

func TestSaveCrossRepoCommitsAt(t *testing.T) {
	t.Parallel()
	commonDir := checkpointPushPolicyTestCommonDir(t)

	require.NoError(t, saveCrossRepoCommitsAt(commonDir, true))
	assert.True(t, LoadCrossRepoCommitsAt(commonDir))
	data, err := os.ReadFile(filepath.Join(commonDir, CrossRepoCommitsFile))
	require.NoError(t, err)
	assert.Equal(t, "on\n", string(data))

	require.NoError(t, saveCrossRepoCommitsAt(commonDir, false))
	assert.False(t, LoadCrossRepoCommitsAt(commonDir))
}

func TestParseCrossRepoCommits(t *testing.T) {
	t.Parallel()
	on, err := ParseCrossRepoCommits("on")
	require.NoError(t, err)
	assert.True(t, on)
	on, err = ParseCrossRepoCommits("off")
	require.NoError(t, err)
	assert.False(t, on)
	_, err = ParseCrossRepoCommits("true")
	require.Error(t, err)
}
