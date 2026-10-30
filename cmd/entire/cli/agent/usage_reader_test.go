package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// lineAgent counts one call per line of each file its test names.
type lineAgent struct {
	mockAgent

	extra []UsageFile
}

func (a *lineAgent) NewUsageCounter() UsageCounter { return &lineCounter{extra: a.extra} }

type lineCounter struct {
	extra []UsageFile

	Lines map[string]int `json:"lines"`
}

func (c *lineCounter) Files(in CumulativeTokenInput) []UsageFile {
	return append([]UsageFile{{Path: in.TranscriptPath}}, c.extra...)
}

func (c *lineCounter) Add(path string, data []byte) {
	if c.Lines == nil {
		c.Lines = make(map[string]int)
	}
	c.Lines[path] += bytes.Count(data, []byte("\n"))
	if !bytes.HasSuffix(data, []byte("\n")) {
		c.Lines[path]++ // a whole file's last line
	}
}

func (c *lineCounter) Reset(path string) { delete(c.Lines, path) }

func (c *lineCounter) Totals(context.Context, CumulativeTokenInput) (*TokenUsage, map[string]*TokenUsage) {
	var calls int
	for _, n := range c.Lines {
		calls += n
	}
	return UsageOrNil(&TokenUsage{APICallCount: calls}), nil
}

func readLines(t *testing.T, ag Agent, path string, cursor []byte) CumulativeObservation {
	t.Helper()
	obs, ok, err := ReadCumulativeUsage(context.Background(), ag, CumulativeTokenInput{TranscriptPath: path}, cursor)
	require.True(t, ok)
	require.NoError(t, err)
	return obs
}

func calls(obs CumulativeObservation) int {
	if obs.Total == nil {
		return 0
	}
	return obs.Total.APICallCount
}

func TestReadCumulativeUsage(t *testing.T) {
	t.Parallel()

	t.Run("a whole file is re-read when it changes and never restarts", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path, export := filepath.Join(dir, "t.jsonl"), filepath.Join(dir, "export.json")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.WriteFile(export, []byte("1\n2\n3"), 0o600))
		ag := &lineAgent{extra: []UsageFile{{Path: export, Whole: true}}}
		obs := readLines(t, ag, path, nil)
		require.Equal(t, 3, calls(obs))
		require.NoError(t, os.WriteFile(export, []byte("1"), 0o600)) // a stale copy
		stale := readLines(t, ag, path, obs.Cursor)
		require.False(t, stale.Restarted, "only the main transcript can report a rewrite")
		require.Equal(t, 1, calls(stale))
	})

	t.Run("a file beside the transcript is never read through a symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path, target, link := filepath.Join(dir, "t.jsonl"), filepath.Join(dir, "elsewhere"), filepath.Join(dir, "side.json")
		require.NoError(t, os.WriteFile(path, []byte("a\n"), 0o600))
		require.NoError(t, os.WriteFile(target, []byte("1\n2\n"), 0o600))
		require.NoError(t, os.Symlink(target, link))
		require.Equal(t, 1, calls(readLines(t, &lineAgent{extra: []UsageFile{{Path: link}}}, path, nil)))
	})

	t.Run("a file that cannot be read keeps what was counted from it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path, side := filepath.Join(dir, "t.jsonl"), filepath.Join(dir, "side.jsonl")
		require.NoError(t, os.WriteFile(path, []byte("a\n"), 0o600))
		require.NoError(t, os.WriteFile(side, []byte("1\n2\n"), 0o600))
		ag := &lineAgent{extra: []UsageFile{{Path: side}}}
		obs := readLines(t, ag, path, nil)
		require.NoError(t, os.Rename(side, side+".moved"))
		obs = readLines(t, ag, path, obs.Cursor)
		require.NoError(t, os.Rename(side+".moved", side))
		require.Equal(t, 3, calls(readLines(t, ag, path, obs.Cursor)), "not counted twice when it returns")
	})

	t.Run("an unreadable transcript is an error; another agent's cursor is ignored", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "t.jsonl")
		_, ok, err := ReadCumulativeUsage(context.Background(), &lineAgent{}, CumulativeTokenInput{TranscriptPath: path}, nil)
		require.True(t, ok)
		require.Error(t, err)
		require.NoError(t, os.WriteFile(path, []byte("a\nb\n"), 0o600))
		foreign := []byte(`{"v":` + strconv.Itoa(usageCursorVersion) + `,"agent":"someone-else","files":{"` + path + `":{"o":4}}}`)
		obs := readLines(t, &lineAgent{}, path, foreign)
		require.Equal(t, 2, calls(obs))
		require.False(t, obs.Restarted)
	})
}

func TestKeyedUsage(t *testing.T) {
	t.Parallel()
	call := func(out int) TokenUsage { return TokenUsage{OutputTokens: out, APICallCount: 1} }

	t.Run("each policy picks its copy", func(t *testing.T) {
		t.Parallel()
		for policy, want := range map[MergePolicy]int{KeepFirst: 5, KeepLast: 3, KeepMostOutput: 9} {
			k := KeyedUsage{Policy: policy, Window: 4}
			k.Add("", "m", call(5))
			k.Add("", "m", call(9))
			k.Add("", "m", call(3))
			require.Equal(t, &TokenUsage{OutputTokens: want, APICallCount: 1}, k.Sum(""))
		}
	})

	t.Run("a closed record is matched again only when remembered", func(t *testing.T) {
		t.Parallel()
		forgetful := KeyedUsage{Window: 1}
		remembering := KeyedUsage{Window: 1, Remember: true}
		for _, k := range []*KeyedUsage{&forgetful, &remembering} {
			k.Add("", "a", call(1))
			k.Add("", "b", call(1)) // closes a
			k.Add("", "a", call(1)) // a replay
		}
		require.Equal(t, 3, forgetful.Sum("").APICallCount)
		require.Equal(t, 2, remembering.Sum("").APICallCount)
	})

	t.Run("children and a running total", func(t *testing.T) {
		t.Parallel()
		k := KeyedUsage{Window: 8}
		k.Add("", "a", call(10))
		k.Add("kid", "b", call(4))
		k.Mark()
		k.Add("", "c", call(1))
		require.Equal(t, map[string]*TokenUsage{"kid": {OutputTokens: 4, APICallCount: 1}}, k.Children())
		// The total also saw a call from before records were written.
		require.Equal(t, &TokenUsage{OutputTokens: 21, APICallCount: 4}, k.CoveredBy(&TokenUsage{OutputTokens: 20, APICallCount: 3}))
		k.ResetChild("kid")
		require.Nil(t, k.Children())
	})

	t.Run("a lower running total starts a new segment", func(t *testing.T) {
		t.Parallel()
		var r RunningTotal
		for _, out := range []int{10, 30, 30, 5, 8} {
			r.Add(&TokenUsage{OutputTokens: out})
		}
		require.Equal(t, 38, r.Value().OutputTokens)
	})

	t.Run("a child's message stays open however many records other files add", func(t *testing.T) {
		t.Parallel()
		k := KeyedUsage{Policy: KeepMostOutput, Window: 2}
		k.Add("child", "c1", call(10))
		for i := range 5 {
			k.Add("", string(rune('a'+i)), call(1))
		}
		k.Add("child", "c1", call(50))
		require.Equal(t, &TokenUsage{OutputTokens: 50, APICallCount: 1}, k.Children()["child"])
	})
}
