package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// usageCase is testdata/usage/<agent>/case.json: a session's files and the
// running totals they hold.
type usageCase struct {
	About      string                       `json:"about"`
	SessionID  string                       `json:"session_id"`
	Transcript string                       `json:"transcript"`
	Whole      bool                         `json:"whole"`
	NoRestart  bool                         `json:"no_restart"`
	Total      *agent.TokenUsage            `json:"total"`
	Children   map[string]*agent.TokenUsage `json:"children"`
}

// Every agent that reports running totals reads its fixture to the same
// totals whole, in pieces split anywhere (mid-line included), and after its
// transcript is rewritten to an older copy.
func TestUsageCounters_Conformance(t *testing.T) {
	t.Parallel()
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		require.NoError(t, err)
		if _, ok := agent.AsUsageCounterSource(ag); !ok {
			continue
		}
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			fixture := filepath.Join("testdata", "usage", string(name))
			raw, err := os.ReadFile(filepath.Join(fixture, "case.json"))
			require.NoError(t, err, "an agent that reports running totals needs a fixture in %s", fixture)
			var c usageCase
			require.NoError(t, json.Unmarshal(raw, &c))
			dir := t.TempDir()
			require.NoError(t, os.CopyFS(dir, os.DirFS(fixture)))
			in := agent.CumulativeTokenInput{SessionID: c.SessionID, TranscriptPath: filepath.Join(dir, c.Transcript)}
			read := func(cursor json.RawMessage) agent.CumulativeObservation {
				t.Helper()
				obs, ok, err := agent.ReadCumulativeUsage(context.Background(), ag, in, cursor)
				require.True(t, ok)
				require.NoError(t, err)
				return obs
			}
			assertTotals := func(label string, obs agent.CumulativeObservation) {
				t.Helper()
				require.Equal(t, c.Total, obs.Total, "%s: %s", label, c.About)
				require.Len(t, obs.Children, len(c.Children), "%s: children", label)
				for id, want := range c.Children {
					require.Equal(t, want, obs.Children[id], "%s: child %s", label, id)
				}
			}

			whole := read(nil)
			assertTotals("whole", whole)
			again := read(whole.Cursor)
			assertTotals("unchanged", again)
			require.False(t, again.Restarted)

			full, err := os.ReadFile(in.TranscriptPath)
			require.NoError(t, err)
			if c.Whole {
				return // a rewritten file has no prefix to resume
			}

			// Grow the transcript one piece at a time, cutting at every line end
			// and one byte either side of it.
			var cuts []int
			for i, b := range full {
				if b == '\n' {
					cuts = append(cuts, i, i+1, min(i+2, len(full)))
				}
			}
			var cursor json.RawMessage
			for _, cut := range append(cuts, len(full)) {
				require.NoError(t, os.WriteFile(in.TranscriptPath, full[:cut], 0o600))
				obs := read(cursor)
				require.False(t, obs.Restarted, "growing to %d bytes is not a rewrite", cut)
				cursor = obs.Cursor
			}
			assertTotals("read in pieces", read(cursor))

			// Rewrite to an older copy: the first half of its lines.
			lines := bytes.SplitAfter(full, []byte("\n"))
			older := bytes.Join(lines[:len(lines)/2], nil)
			require.NoError(t, os.WriteFile(in.TranscriptPath, older, 0o600))
			rewritten := read(whole.Cursor)
			require.Equal(t, !c.NoRestart, rewritten.Restarted, "a shorter transcript was rewritten")
			fresh := read(nil)
			require.Equal(t, fresh.Total, rewritten.Total, "after a rewrite the total is the older copy's")
		})
	}
}
