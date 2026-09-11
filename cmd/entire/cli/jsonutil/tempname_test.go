package jsonutil_test

import (
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/stretchr/testify/require"
)

func TestIsTempName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{"full.jsonl.0123456789abcdef.tmp", true},
		{"metadata/sess/full.jsonl.0123456789ABCDEF.tmp", true},
		{"settings.json.deadbeefcafef00d.tmp", true},
		{"full.jsonl", false},
		{"full.jsonl.001", false},
		{"notes.tmp", false},
		{"full.jsonl.short.tmp", false},
		{"full.jsonl.0123456789abcdeg.tmp", false},
		{"full.jsonl.0123456789abcdef0.tmp", false},
		{".0123456789abcdef.tmp", false},
	}
	for _, tt := range tests {
		require.Equalf(t, tt.want, jsonutil.IsTempName(tt.name), "IsTempName(%q)", tt.name)
	}
}

// TestIsTempName_MatchesWhatCreateTempInProduces keeps the predicate honest if
// the temp naming ever changes: the walk filters that depend on it would go
// silently ineffective otherwise.
func TestIsTempName_MatchesWhatCreateTempInProduces(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer root.Close()

	require.NoError(t, root.MkdirAll("metadata/sess", 0o750))
	for _, target := range []string{"full.jsonl", "metadata/sess/full.jsonl", "settings.json"} {
		f, name, err := jsonutil.CreateTempIn(root, target)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		require.Truef(t, jsonutil.IsTempName(name), "CreateTempIn produced %q, which IsTempName rejects", name)
	}
}
