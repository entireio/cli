package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/stretchr/testify/require"
)

func TestAgentCheckPersistVerificationEvidenceSuccess(t *testing.T) {
	repoRoot := t.TempDir()
	cpID := id.MustCheckpointID("abc123abc123")
	evidence := sampleAgentCheckVerificationEvidence(agentCheckVerificationSuccess)

	path, err := persistAgentCheckVerificationEvidence(repoRoot, cpID, evidence)

	require.NoError(t, err)
	require.Equal(t, filepath.Join(repoRoot, agentCheckDirName, agentCheckReportsName, cpID.String(), agentCheckVerificationEvidenceName), path)
	require.FileExists(t, path)
}

func TestAgentCheckPersistVerificationEvidenceCanBeReadBack(t *testing.T) {
	repoRoot := t.TempDir()
	cpID := id.MustCheckpointID("abc123abc123")
	want := sampleAgentCheckVerificationEvidence(agentCheckVerificationSuccess)

	path, err := persistAgentCheckVerificationEvidence(repoRoot, cpID, want)
	require.NoError(t, err)

	got := readPersistedAgentCheckVerificationEvidence(t, path)
	require.Equal(t, want, got)
}

func TestAgentCheckPersistVerificationEvidenceStatuses(t *testing.T) {
	for _, status := range []string{
		agentCheckVerificationSuccess,
		agentCheckVerificationFailed,
		agentCheckVerificationUnableToRun,
	} {
		t.Run(status, func(t *testing.T) {
			repoRoot := t.TempDir()
			cpID := id.MustCheckpointID("abc123abc123")
			want := sampleAgentCheckVerificationEvidence(status)

			path, err := persistAgentCheckVerificationEvidence(repoRoot, cpID, want)
			require.NoError(t, err)

			got := readPersistedAgentCheckVerificationEvidence(t, path)
			require.Equal(t, status, got.Status)
			require.Equal(t, want.ExitCode, got.ExitCode)
			require.Equal(t, want.Command, got.Command)
		})
	}
}

func TestAgentCheckPersistVerificationEvidencePreservesOutputAndTruncation(t *testing.T) {
	repoRoot := t.TempDir()
	cpID := id.MustCheckpointID("abc123abc123")
	want := sampleAgentCheckVerificationEvidence(agentCheckVerificationFailed)
	want.Stdout = "bounded stdout"
	want.StdoutTruncated = true
	want.Stderr = "bounded stderr"
	want.StderrTruncated = true

	path, err := persistAgentCheckVerificationEvidence(repoRoot, cpID, want)
	require.NoError(t, err)

	got := readPersistedAgentCheckVerificationEvidence(t, path)
	require.Equal(t, "bounded stdout", got.Stdout)
	require.True(t, got.StdoutTruncated)
	require.Equal(t, "bounded stderr", got.Stderr)
	require.True(t, got.StderrTruncated)
}

func TestAgentCheckPersistVerificationEvidenceRepeatedWriteReplacesFile(t *testing.T) {
	repoRoot := t.TempDir()
	cpID := id.MustCheckpointID("abc123abc123")
	first := sampleAgentCheckVerificationEvidence(agentCheckVerificationFailed)
	first.Stdout = "first"
	second := sampleAgentCheckVerificationEvidence(agentCheckVerificationSuccess)
	second.Stdout = "second"

	path, err := persistAgentCheckVerificationEvidence(repoRoot, cpID, first)
	require.NoError(t, err)
	pathAgain, err := persistAgentCheckVerificationEvidence(repoRoot, cpID, second)
	require.NoError(t, err)

	require.Equal(t, path, pathAgain)
	got := readPersistedAgentCheckVerificationEvidence(t, path)
	require.Equal(t, second, got)

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, agentCheckVerificationEvidenceName, entries[0].Name())
}

func TestAgentCheckPersistVerificationEvidenceRejectsInvalidCheckpointID(t *testing.T) {
	repoRoot := t.TempDir()
	invalidID := id.CheckpointID("../escaped")

	path, err := persistAgentCheckVerificationEvidence(repoRoot, invalidID, sampleAgentCheckVerificationEvidence(agentCheckVerificationSuccess))

	require.Error(t, err)
	require.Empty(t, path)
	require.NoFileExists(t, filepath.Join(repoRoot, "escaped", agentCheckVerificationEvidenceName))
	require.NoDirExists(t, filepath.Join(repoRoot, agentCheckDirName))
}

func sampleAgentCheckVerificationEvidence(status string) agentCheckVerificationEvidence {
	exitCode := 0
	if status == agentCheckVerificationFailed {
		exitCode = 1
	}
	if status == agentCheckVerificationUnableToRun {
		exitCode = -1
	}
	return agentCheckVerificationEvidence{
		Command:         "go test ./...",
		Ecosystem:       agentCheckEcosystemGo,
		Status:          status,
		ExitCode:        exitCode,
		Duration:        1500 * time.Millisecond,
		Stdout:          "stdout",
		StdoutTruncated: false,
		Stderr:          "stderr",
		StderrTruncated: false,
	}
}

func readPersistedAgentCheckVerificationEvidence(t *testing.T, path string) agentCheckVerificationEvidence {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var got agentCheckVerificationEvidence
	require.NoError(t, json.Unmarshal(data, &got))
	return got
}
