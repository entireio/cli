package checkpoint

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/stretchr/testify/require"
)

func TestReadTranscriptFromTreeFormats(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files map[string][]byte
		want  []byte
	}{
		{name: "base", files: map[string][]byte{"full.jsonl": []byte("first\nsecond\n")}, want: []byte("first\nsecond\n")},
		{name: "legacy", files: map[string][]byte{paths.TranscriptFileNameLegacy: []byte("legacy\n")}, want: []byte("legacy\n")},
		{name: "empty", files: map[string][]byte{"full.jsonl": {}}, want: []byte{}},
		{name: "absent", files: map[string][]byte{}, want: nil},
		{
			name: "chunk order",
			files: map[string][]byte{
				"full.jsonl":     []byte("zero"),
				"full.jsonl.010": []byte("ten"),
				"full.jsonl.002": []byte("two"),
				"full.jsonl.001": []byte("one"),
			},
			want: []byte("zero\none\ntwo\nten"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := git.Init(memory.NewStorage(), nil)
			require.NoError(t, err)
			tree := transcriptReadFixture(t, repo, tc.files)
			got, err := readTranscriptFromTree(t.Context(), NewFetchingTree(t.Context(), tree, repo.Storer, nil), "")
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func BenchmarkReadTranscriptFromTree(b *testing.B) {
	for _, size := range []int{1 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			// In-memory objects isolate transcript decoding and allocation costs
			// from filesystem cache state; tree and blob lookup are still real.
			repo, err := git.Init(memory.NewStorage(), nil)
			require.NoError(b, err)
			payload := bytes.Repeat([]byte("x"), size)
			tree := transcriptReadFixture(b, repo, map[string][]byte{paths.TranscriptFileName: payload})
			ft := NewFetchingTree(b.Context(), tree, repo.Storer, nil)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				got, readErr := readTranscriptFromTree(b.Context(), ft, "")
				if readErr != nil || !bytes.Equal(got, payload) {
					b.Fatalf("transcript differs: size=%d, error=%v", len(got), readErr)
				}
			}
		})
	}
}

func transcriptReadFixture(t testing.TB, repo *git.Repository, files map[string][]byte) *object.Tree {
	t.Helper()
	entries := make([]object.TreeEntry, 0, len(files))
	for name, content := range files {
		hash, err := CreateBlobFromContent(repo, content)
		require.NoError(t, err)
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: hash})
	}
	sortTreeEntries(entries)
	hash, err := storeTree(repo, entries)
	require.NoError(t, err)
	tree, err := repo.TreeObject(hash)
	require.NoError(t, err)
	return tree
}
