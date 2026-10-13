//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// tokenCounts is the four billable counters the invariants compare. API call
// counts are left out: agents report them inconsistently and they carry no
// cost.
type tokenCounts struct {
	In, Out, CacheRead, CacheWrite int
}

func (c tokenCounts) plus(o tokenCounts) tokenCounts {
	return tokenCounts{c.In + o.In, c.Out + o.Out, c.CacheRead + o.CacheRead, c.CacheWrite + o.CacheWrite}
}

func (c tokenCounts) total() int { return c.In + c.Out + c.CacheRead + c.CacheWrite }

// atMost reports whether every field of c is <= the same field of o.
func (c tokenCounts) atMost(o tokenCounts) bool {
	return c.In <= o.In && c.Out <= o.Out && c.CacheRead <= o.CacheRead && c.CacheWrite <= o.CacheWrite
}

func (c tokenCounts) String() string {
	return fmt.Sprintf("[in=%d out=%d cacheRead=%d cacheWrite=%d]", c.In, c.Out, c.CacheRead, c.CacheWrite)
}

// foldedTokenCounts reads a stored TokenUsage as the session's inclusive
// spend: main agent plus subagents. Subagents are folded into the parent
// session's total (token accounting decision, 2026-10).
//
// Today slot token_usage holds the main agent at the top level and the
// subagents only in the nested subagent_tokens, so the two are added here.
// TODO(token-accounting PR4): once the CLI writes inclusive top-level fields
// and documents subagent_tokens as already included, stop adding the nested
// value or every subagent token is counted twice.
func foldedTokenCounts(u *agent.TokenUsage) tokenCounts {
	if u == nil {
		return tokenCounts{}
	}
	c := tokenCounts{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens}
	if s := u.SubagentTokens; s != nil {
		c = c.plus(tokenCounts{s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheCreationTokens})
	}
	return c
}

// tokenTruth is the ledger of tokens the fake agents actually spent, per
// session ID, with subagents folded into their parent session.
type tokenTruth struct {
	spent map[string]tokenCounts
}

func newTokenTruth() *tokenTruth { return &tokenTruth{spent: map[string]tokenCounts{}} }

func (l *tokenTruth) add(sessionID string, c tokenCounts) {
	l.spent[sessionID] = l.spent[sessionID].plus(c)
}

func (l *tokenTruth) of(sessionID string) tokenCounts { return l.spent[sessionID] }

// tokenSlot is one session's entry in one committed checkpoint.
type tokenSlot struct {
	CheckpointID string
	Index        string
	SessionID    string
	Usage        tokenCounts
}

// tokenCheckpoint is a committed checkpoint's root summary and its slots.
type tokenCheckpoint struct {
	ID    string
	Root  tokenCounts
	Slots []tokenSlot
}

var slotMetadataPath = regexp.MustCompile(`^([^/]{2})/([^/]+)/(\d+)/` + regexp.QuoteMeta(paths.MetadataFileName) + `$`)
var rootMetadataPath = regexp.MustCompile(`^([^/]{2})/([^/]+)/` + regexp.QuoteMeta(paths.MetadataFileName) + `$`)

// readTokenCheckpoints reads every checkpoint on the metadata branch tip: the
// view a server ingesting the pushed branch would see, where a slot rewritten
// in place (amend) replaces the earlier version.
func readTokenCheckpoints(env *TestEnv) ([]tokenCheckpoint, error) {
	repo, err := gitrepo.OpenPath(env.RepoDir)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}
	defer repo.Close()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		// No metadata branch yet: no checkpoints.
		return nil, nil
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("read metadata tip: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read metadata tree: %w", err)
	}
	byID := map[string]*tokenCheckpoint{}
	get := func(id string) *tokenCheckpoint {
		if byID[id] == nil {
			byID[id] = &tokenCheckpoint{ID: id}
		}
		return byID[id]
	}
	err = tree.Files().ForEach(func(f *object.File) error {
		if m := slotMetadataPath.FindStringSubmatch(f.Name); m != nil {
			content, err := f.Contents()
			if err != nil {
				return fmt.Errorf("read %s: %w", f.Name, err)
			}
			var meta checkpoint.Metadata
			if err := json.Unmarshal([]byte(content), &meta); err != nil {
				return fmt.Errorf("parse %s: %w", f.Name, err)
			}
			cp := get(m[1] + m[2])
			cp.Slots = append(cp.Slots, tokenSlot{
				CheckpointID: cp.ID, Index: m[3], SessionID: meta.SessionID, Usage: foldedTokenCounts(meta.TokenUsage),
			})
			return nil
		}
		if m := rootMetadataPath.FindStringSubmatch(f.Name); m != nil {
			content, err := f.Contents()
			if err != nil {
				return fmt.Errorf("read %s: %w", f.Name, err)
			}
			var summary checkpoint.CheckpointSummary
			if err := json.Unmarshal([]byte(content), &summary); err != nil {
				return fmt.Errorf("parse %s: %w", f.Name, err)
			}
			get(m[1] + m[2]).Root = foldedTokenCounts(summary.TokenUsage)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk metadata tree: %w", err)
	}
	out := make([]tokenCheckpoint, 0, len(byID))
	for _, cp := range byID {
		sort.Slice(cp.Slots, func(i, j int) bool { return cp.Slots[i].Index < cp.Slots[j].Index })
		out = append(out, *cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// checkpointSumFor sums a session's slot usage across all checkpoints:
// invariant (b)'s left-hand side, and what the server totals today.
func checkpointSumFor(cps []tokenCheckpoint, sessionID string) tokenCounts {
	var sum tokenCounts
	for _, cp := range cps {
		for _, s := range cp.Slots {
			if s.SessionID == sessionID {
				sum = sum.plus(s.Usage)
			}
		}
	}
	return sum
}

// describeSlots renders a session's per-checkpoint usage for failure output.
func describeSlots(cps []tokenCheckpoint, sessionID string) string {
	var b strings.Builder
	for _, cp := range cps {
		for _, s := range cp.Slots {
			if s.SessionID == sessionID {
				fmt.Fprintf(&b, "\n    %s/%s %v", s.CheckpointID, s.Index, s.Usage)
			}
		}
	}
	return b.String()
}

// TODO(token-accounting PR4): add latestCumulativeFor, invariant (a): the
// highest session_tokens.cumulative across a session's slots equals truth.
// The slot field does not exist yet.

// stateTokenCounts reads the session total `entire status` and `session
// tokens` report, straight from the state file. ok is false when the session
// has no state (cleared, or cleaned up after ending).
func stateTokenCounts(env *TestEnv, sessionID string) (tokenCounts, bool, error) {
	st, err := env.GetSessionState(sessionID)
	if err != nil || st == nil {
		return tokenCounts{}, false, err
	}
	return foldedTokenCounts(st.TokenUsage), true, nil
}

// sessionTokensTotal runs `entire session tokens <id> --json` in dir and
// returns its inclusive total (main + subagents). ok is false only when the
// command reports the session is not found; any other failure is an error.
func sessionTokensTotal(env *TestEnv, dir, sessionID string) (int, bool, error) {
	cmd := execx.NonInteractive(env.T.Context(), getTestBinary(), "session", "tokens", sessionID, "--json")
	cmd.Dir = dir
	cmd.Env = env.cliEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "Session not found.") {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("session tokens %s: %w\n%s%s", sessionID, err, stdout.String(), stderr.String())
	}
	var report struct {
		Tokens *struct {
			Total int `json:"total"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return 0, false, fmt.Errorf("parse session tokens --json %q: %w", stdout.String(), err)
	}
	if report.Tokens == nil {
		return 0, true, nil
	}
	return report.Tokens.Total, true, nil
}
