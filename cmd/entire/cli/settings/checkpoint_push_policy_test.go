package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func checkpointPushPolicyTestCommonDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	testutil.InitRepo(t, root)
	return filepath.Join(root, ".git")
}

func writeCheckpointPushPolicyTestFile(t *testing.T, commonDir, content string) {
	t.Helper()
	path := filepath.Join(commonDir, CheckpointPushPolicyFile)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestLoadCheckpointPushPolicyAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content *string
		want    CheckpointPushPolicy
		wantErr string
	}{
		{name: "missing file is automatic", want: CheckpointPushPolicyAutomatic},
		{name: "automatic", content: new("automatic\n"), want: CheckpointPushPolicyAutomatic},
		{name: "manual", content: new("manual\n"), want: CheckpointPushPolicyManual},
		{name: "manual without newline", content: new("manual"), want: CheckpointPushPolicyManual},
		{name: "unknown value", content: new("off\n"), wantErr: `"off"`},
		{name: "wrong case", content: new("Manual\n"), wantErr: `"Manual"`},
		{name: "empty file", content: new(""), wantErr: `""`},
		{name: "two lines", content: new("manual\nautomatic\n"), wantErr: "not one of"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			commonDir := checkpointPushPolicyTestCommonDir(t)
			if tt.content != nil {
				writeCheckpointPushPolicyTestFile(t, commonDir, *tt.content)
			}

			got, err := LoadCheckpointPushPolicyAt(commonDir)
			if tt.wantErr != "" {
				require.Error(t, err, "an unrecognized policy must not read as automatic")
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoadCheckpointPushPolicyAt_SymlinkIsUnreadable(t *testing.T) {
	t.Parallel()
	commonDir := checkpointPushPolicyTestCommonDir(t)
	target := filepath.Join(t.TempDir(), "policy")
	require.NoError(t, os.WriteFile(target, []byte("automatic\n"), 0o600))
	path := filepath.Join(commonDir, CheckpointPushPolicyFile)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.Symlink(target, path))

	_, err := LoadCheckpointPushPolicyAt(commonDir)
	require.Error(t, err)
}

func TestSaveCheckpointPushPolicyAt(t *testing.T) {
	t.Parallel()
	commonDir := checkpointPushPolicyTestCommonDir(t)

	require.NoError(t, saveCheckpointPushPolicyAt(commonDir, CheckpointPushPolicyManual))
	data, err := os.ReadFile(filepath.Join(commonDir, CheckpointPushPolicyFile))
	require.NoError(t, err)
	assert.Equal(t, "manual\n", string(data))

	require.NoError(t, saveCheckpointPushPolicyAt(commonDir, CheckpointPushPolicyAutomatic))
	got, err := LoadCheckpointPushPolicyAt(commonDir)
	require.NoError(t, err)
	assert.Equal(t, CheckpointPushPolicyAutomatic, got)
}

// Released binaries rewrite preferences.json without keys they do not model,
// so the policy must not live there.
func TestCheckpointPushPolicy_SurvivesPreferencesRewrite(t *testing.T) {
	t.Parallel()
	commonDir := checkpointPushPolicyTestCommonDir(t)
	require.NoError(t, saveCheckpointPushPolicyAt(commonDir, CheckpointPushPolicyManual))

	prefsPath := filepath.Join(commonDir, ClonePreferencesFile)
	require.NoError(t, os.WriteFile(prefsPath, []byte(`{"trails_enabled":true}`), 0o600))

	got, err := LoadCheckpointPushPolicyAt(commonDir)
	require.NoError(t, err)
	assert.Equal(t, CheckpointPushPolicyManual, got)
}

func TestParseCheckpointPushPolicy(t *testing.T) {
	t.Parallel()
	for _, valid := range []CheckpointPushPolicy{CheckpointPushPolicyAutomatic, CheckpointPushPolicyManual} {
		got, err := ParseCheckpointPushPolicy(string(valid))
		require.NoError(t, err)
		assert.Equal(t, valid, got)
	}
	for _, invalid := range []string{"", "Manual", "all", "approved"} {
		_, err := ParseCheckpointPushPolicy(invalid)
		require.Error(t, err, "%q must be rejected", invalid)
	}
}

func TestCheckpointPushPolicyStoredAt(t *testing.T) {
	t.Parallel()
	commonDir := checkpointPushPolicyTestCommonDir(t)

	stored, err := checkpointPushPolicyStoredAt(commonDir)
	require.NoError(t, err)
	assert.False(t, stored, "the default is not a choice")

	require.NoError(t, saveCheckpointPushPolicyAt(commonDir, CheckpointPushPolicyAutomatic))
	stored, err = checkpointPushPolicyStoredAt(commonDir)
	require.NoError(t, err)
	assert.True(t, stored, "a chosen automatic is a choice")
}

func TestSaveCheckpointPushPolicyIfUnsetAt(t *testing.T) {
	t.Parallel()
	commonDir := checkpointPushPolicyTestCommonDir(t)

	saved, err := saveCheckpointPushPolicyIfUnsetAt(commonDir, CheckpointPushPolicyManual)
	require.NoError(t, err)
	assert.True(t, saved)

	saved, err = saveCheckpointPushPolicyIfUnsetAt(commonDir, CheckpointPushPolicyAutomatic)
	require.NoError(t, err)
	assert.False(t, saved, "an existing choice wins")
	got, err := LoadCheckpointPushPolicyAt(commonDir)
	require.NoError(t, err)
	assert.Equal(t, CheckpointPushPolicyManual, got)
}
