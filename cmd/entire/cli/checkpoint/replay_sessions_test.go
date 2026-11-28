package checkpoint

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestReplayTrees_PreservesUnknownFieldsAndRebasesPaths(t *testing.T) {
	t.Parallel()
	repo, write := replaySessionTestWriter(t)
	base := write(plumbing.ZeroHash, "shared")
	local := write(base, "local")
	remote := write(base, "remote")
	custom, err := CreateBlobFromContent(repo, []byte("local sidecar"))
	require.NoError(t, err)
	local, err = ApplyTreeChanges(t.Context(), repo, local, []TreeChange{{Path: "1/custom.bin", Entry: &object.TreeEntry{Mode: filemode.Regular, Hash: custom}}})
	require.NoError(t, err)
	local = rewriteReplayJSON(t, repo, local, paths.MetadataFileName, func(fields map[string]json.RawMessage) {
		var sessions []map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(fields["sessions"], &sessions))
		sessions[1]["future_path"] = json.RawMessage(`"/1/custom.bin"`)
		sessions[1]["future_number"] = json.RawMessage(`9007199254740993`)
		raw, err := json.Marshal(sessions)
		require.NoError(t, err)
		fields["sessions"] = raw
		fields["future_number"] = json.RawMessage(`9007199254740993`)
		fields["local_extension"] = json.RawMessage(`{"value":1}`)
	})
	remote = rewriteReplayJSON(t, repo, remote, paths.MetadataFileName, func(fields map[string]json.RawMessage) {
		fields["future_number"] = json.RawMessage(`9007199254740993`)
		fields["remote_extension"] = json.RawMessage(`{"value":2}`)
	})
	merged, err := ReplayTrees(t.Context(), repo, base, local, remote)
	require.NoError(t, err)
	fields := replayTestJSON(t, repo, merged, paths.MetadataFileName)
	require.Equal(t, `9007199254740993`, string(fields["future_number"]))
	require.JSONEq(t, `{"value":1}`, string(fields["local_extension"]))
	require.JSONEq(t, `{"value":2}`, string(fields["remote_extension"]))
	var sessions []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fields["sessions"], &sessions))
	require.Len(t, sessions, 3)
	require.JSONEq(t, `"/2/custom.bin"`, string(sessions[2]["future_path"]))
	require.Equal(t, `9007199254740993`, string(sessions[2]["future_number"]))
	tree := mustTreeObject(t, repo, merged)
	entry, err := tree.FindEntry("2/custom.bin")
	require.NoError(t, err)
	require.Equal(t, custom, entry.Hash)
}

func TestReplayTrees_MergesSessionMetadataAndReaggregates(t *testing.T) {
	t.Parallel()
	repo, write := replaySessionTestWriter(t)
	base := write(plumbing.ZeroHash, "shared")
	local := rewriteReplayJSON(t, repo, base, "0/metadata.json", func(fields map[string]json.RawMessage) {
		fields["summary"] = json.RawMessage(`{"intent":"local summary"}`)
	})
	remote := rewriteReplayJSON(t, repo, base, "0/metadata.json", func(fields map[string]json.RawMessage) {
		fields["token_usage"] = json.RawMessage(`{"input_tokens":42}`)
		fields["future_value"] = json.RawMessage(`9007199254740993`)
	})
	merged, err := ReplayTrees(t.Context(), repo, base, local, remote)
	require.NoError(t, err)
	metadata := replayTestJSON(t, repo, merged, "0/metadata.json")
	require.JSONEq(t, `{"intent":"local summary"}`, string(metadata["summary"]))
	require.Equal(t, `9007199254740993`, string(metadata["future_value"]))
	root := replayTestJSON(t, repo, merged, paths.MetadataFileName)
	var tokens types.TokenUsage
	require.NoError(t, json.Unmarshal(root["token_usage"], &tokens))
	require.Equal(t, 42, tokens.InputTokens)
}

func TestReplayTrees_PreservesFlagsAndCommitLinks(t *testing.T) {
	t.Parallel()
	repo, write := replaySessionTestWriter(t)
	base := write(plumbing.ZeroHash, "shared")
	local := write(base, "local")
	remote := write(base, "remote")
	local = rewriteReplayJSON(t, repo, local, paths.MetadataFileName, func(fields map[string]json.RawMessage) {
		fields["has_review"] = json.RawMessage(`true`)
		fields["linked_commits"] = json.RawMessage(`[{"sha":"local-commit","future":9007199254740993}]`)
	})
	remote = rewriteReplayJSON(t, repo, remote, paths.MetadataFileName, func(fields map[string]json.RawMessage) {
		fields["has_investigation"] = json.RawMessage(`true`)
		fields["imported"] = json.RawMessage(`true`)
		fields["linked_commits"] = json.RawMessage(`[{"sha":"remote-commit"}]`)
	})
	merged, err := ReplayTrees(t.Context(), repo, base, local, remote)
	require.NoError(t, err)
	root := replayTestJSON(t, repo, merged, paths.MetadataFileName)
	for _, key := range []string{"has_review", "has_investigation", "imported"} {
		require.Equal(t, `true`, string(root[key]))
	}
	require.JSONEq(t, `[{"sha":"remote-commit"},{"sha":"local-commit","future":9007199254740993}]`, string(root["linked_commits"]))
}

func TestReplayTrees_RefusesInconsistentSessions(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"duplicate identity", "outside path", "missing path", "unlisted slot"} {
		t.Run(invalid, func(t *testing.T) {
			t.Parallel()
			repo, write := replaySessionTestWriter(t)
			base := write(plumbing.ZeroHash, "shared")
			local := write(base, "local")
			remote := write(base, "remote")
			switch invalid {
			case "duplicate identity":
				local = rewriteReplayJSON(t, repo, local, "1/metadata.json", func(fields map[string]json.RawMessage) {
					fields["session_id"] = json.RawMessage(`"shared"`)
				})
			case "outside path", "missing path":
				local = rewriteReplayJSON(t, repo, local, paths.MetadataFileName, func(fields map[string]json.RawMessage) {
					var sessions []map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(fields["sessions"], &sessions))
					value := `"/0/stolen"`
					if invalid == "missing path" {
						value = `"/1/missing"`
					}
					sessions[1]["future_path"] = json.RawMessage(value)
					raw, err := json.Marshal(sessions)
					require.NoError(t, err)
					fields["sessions"] = raw
				})
			case "unlisted slot":
				local = rewriteReplayJSON(t, repo, local, paths.MetadataFileName, func(fields map[string]json.RawMessage) {
					var sessions []map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(fields["sessions"], &sessions))
					raw, err := json.Marshal(sessions[:1])
					require.NoError(t, err)
					fields["sessions"] = raw
				})
			}
			_, err := ReplayTrees(t.Context(), repo, base, local, remote)
			require.Error(t, err, "inconsistent input must never be published")
		})
	}
}

func TestReplayTrees_MetadataFileModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		conflict bool
	}{
		{name: "matching modes"},
		{name: "conflicting modes", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, write := replaySessionTestWriter(t)
			base := write(plumbing.ZeroHash, "shared")
			local, remote := write(base, "local"), write(base, "remote")
			setMode := func(hash plumbing.Hash) plumbing.Hash {
				t.Helper()
				tree := mustTreeObject(t, repo, hash)
				entry, err := tree.FindEntry(paths.MetadataFileName)
				require.NoError(t, err)
				entry.Mode = filemode.Executable
				changed, err := ApplyTreeChanges(t.Context(), repo, hash, []TreeChange{{Path: paths.MetadataFileName, Entry: entry}})
				require.NoError(t, err)
				return changed
			}
			remote = setMode(remote)
			if !tc.conflict {
				base, local = setMode(base), setMode(local)
			}
			merged, err := ReplayTrees(t.Context(), repo, base, local, remote)
			if tc.conflict {
				require.ErrorIs(t, err, ErrReplayConflict)
				return
			}
			require.NoError(t, err)
			tree := mustTreeObject(t, repo, merged)
			entry, err := tree.FindEntry(paths.MetadataFileName)
			require.NoError(t, err)
			require.Equal(t, filemode.Executable, entry.Mode)
		})
	}
}

func replaySessionTestWriter(t *testing.T) (*git.Repository, func(plumbing.Hash, string) plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Close()) })
	writer := &treeWriter{repo: repo}
	write := func(base plumbing.Hash, sid string) plumbing.Hash {
		t.Helper()
		var existing *object.Tree
		if !base.IsZero() {
			existing = mustTreeObject(t, repo, base)
		}
		hash, err := writer.applySessionWrite(t.Context(), WriteOptions{
			CheckpointID: id.MustCheckpointID("a1b2c3d4e5f6"), SessionID: sid,
			Strategy: "manual-commit", CreatedAt: time.Unix(0, 0).UTC(),
			Transcript: redact.AlreadyRedacted([]byte("{\"text\":\"" + sid + "\"}\n")),
			TokenUsage: &types.TokenUsage{InputTokens: 1},
		}, existing, "")
		require.NoError(t, err)
		return hash
	}
	return repo, write
}

func replayTestJSON(t *testing.T, repo *git.Repository, hash plumbing.Hash, name string) map[string]json.RawMessage {
	t.Helper()
	tree := mustTreeObject(t, repo, hash)
	entry, err := tree.FindEntry(name)
	require.NoError(t, err)
	fields, err := readJSONFromBlob[map[string]json.RawMessage](repo, entry.Hash)
	require.NoError(t, err)
	return *fields
}

func rewriteReplayJSON(t *testing.T, repo *git.Repository, hash plumbing.Hash, name string, edit func(map[string]json.RawMessage)) plumbing.Hash {
	t.Helper()
	fields := replayTestJSON(t, repo, hash, name)
	edit(fields)
	data, err := json.Marshal(fields)
	require.NoError(t, err)
	blob, err := CreateBlobFromContent(repo, data)
	require.NoError(t, err)
	updated, err := ApplyTreeChanges(t.Context(), repo, hash, []TreeChange{{Path: name, Entry: &object.TreeEntry{Mode: filemode.Regular, Hash: blob}}})
	require.NoError(t, err)
	return updated
}
