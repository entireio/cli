package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// recallBrainDir is the derived-state directory under .entire (gitignored).
const recallBrainDir = ".entire/recall/brain"

// recallHit is one ranked memory as emitted by the Rust binary. The shim
// renders it and never re-scores it: every ranking decision lives in recall/.
type recallHit struct {
	Raw        float64 `json:"raw"`
	Scored     float64 `json:"scored"`
	Confidence float64 `json:"confidence"`
	Tier       string  `json:"tier"`
	Verdict    string  `json:"verdict"`
	Why        string  `json:"why"`
	BackedBy   *string `json:"backed_by"`
	Commit     *string `json:"commit"`
	Text       string  `json:"text"`
	Context    string  `json:"context"`
	// Partial names what was missing when this hit was judged (checkpoint
	// fields the shim declared unavailable, or "claim redacted"). Empty means
	// the hit was drawn from complete context.
	Partial []string `json:"partial"`
}

// recallCoverage is what the brain was built from, as recorded at ingest and
// returned with every answer. Anything short of complete is rendered PARTIAL.
type recallCoverage struct {
	Total          int            `json:"total"`
	Complete       int            `json:"complete"`
	Partial        int            `json:"partial"`
	Skipped        int            `json:"skipped"`
	Truncated      bool           `json:"truncated"`
	GraphAvailable bool           `json:"graph_available"`
	Unavailable    map[string]int `json:"unavailable"`
}

func (c recallCoverage) isComplete() bool {
	return c.Partial == 0 && c.Skipped == 0 && !c.Truncated && c.GraphAvailable
}

// recallOutput is the activate envelope emitted by the Rust binary.
type recallOutput struct {
	Coverage recallCoverage `json:"coverage"`
	Hits     []recallHit    `json:"hits"`
}

// recallIngestReport is the ingest report emitted by the Rust binary.
type recallIngestReport struct {
	Checkpoints int            `json:"checkpoints"`
	Engrams     int            `json:"engrams"`
	GraphEdges  int            `json:"graph_edges"`
	Coverage    recallCoverage `json:"coverage"`
}

// recallFieldWords renders wire field names for a human.
var recallFieldWords = map[string]string{
	"session":        "transcript",
	"diff":           "diff",
	"files":          "file list",
	"commit_message": "commit message",
}

func recallFieldWord(f string) string {
	if w, ok := recallFieldWords[f]; ok {
		return w
	}
	return f
}

func newRecallCmd() *cobra.Command {
	var k int
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:         "recall <question>",
		Annotations: map[string]string{agentHelpAnnotation: agentHelpAnnotationEnabled},
		Hidden:      true,
		Short:       "Ask checkpoint history what was decided and why",
		Long: `Recall turns this branch's checkpoint history into associative memory and
answers a question with ranked, trust-scored hits: each carries a tier
(LEDGER = commit record, INTENT = user prompt, chat = assistant claim), a
verdict of the claim against the commit it sits on (corroborated, neutral,
CONTRADICTED, or UNVERIFIABLE when the field a check needed was redacted or
unavailable), and the backing commit.

Every answer opens with a context line. "complete" means every checkpoint
seen was indexed whole; "PARTIAL" says what was missing. Hits judged from
partial context are marked and their confidence is capped.

Run 'entire recall ingest' first to build the memory under .entire/recall.`,
		Example: "  entire recall ingest\n  entire recall why did we drop the retry wrapper\n  entire recall --json what is unfinished in dispatch",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecallActivate(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), strings.Join(args, " "), k, jsonFlag)
		},
	}
	cmd.Flags().IntVar(&k, "k", 8, "Number of memories to return")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Emit the ranked hits as JSON")

	var noGraph, noTranscripts bool
	ingest := &cobra.Command{
		Use:   "ingest",
		Short: "Build recall memory from this branch's checkpoints",
		Long: `Walks the commits on the current branch that carry an Entire-Checkpoint
trailer, reads each checkpoint's transcript and diff, asks the code graph for
each changed file's blast radius, and writes the memory to .entire/recall.
The directory is derived state and is rebuilt from scratch on every run.

Nothing leaves the machine: the memory is an embedded database under
.entire/recall and the only network traffic is git fetching the repository's
own checkpoint refs. A checkpoint whose transcript cannot be read is still
ingested as a ledger record, marked partial.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRecallIngest(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), noGraph, noTranscripts)
		},
	}
	ingest.Flags().BoolVar(&noGraph, "no-graph", false, "Skip 'entire graph impact' (faster; isolation claims become unverifiable)")
	ingest.Flags().BoolVar(&noTranscripts, "no-transcripts", false, "Never open a transcript: ingest commit records only, every checkpoint marked partial")
	cmd.AddCommand(ingest)
	return cmd
}

func runRecallIngest(ctx context.Context, w, errW io.Writer, noGraph, noTranscripts bool) error {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	input, err := collectRecallCheckpoints(ctx, errW, root, noTranscripts)
	if err != nil {
		return err
	}
	if len(input.Checkpoints) == 0 {
		fmt.Fprintln(w, "No commits with an Entire-Checkpoint trailer on this branch; nothing to ingest.")
		return nil
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode ingest input: %w", err)
	}
	args := []string{"ingest", "--brain", filepath.Join(root, recallBrainDir)}
	if noGraph {
		args = append(args, "--no-graph")
	}
	out, err := runRecallBinary(ctx, root, errW, bytes.NewReader(payload), args...)
	if err != nil {
		return err
	}
	var report recallIngestReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return fmt.Errorf("parse ingest report: %w", err)
	}
	fmt.Fprintf(w, "Ingested %d checkpoints into %s (%d engrams, %d graph edges)\n%s\n",
		report.Checkpoints, recallBrainDir, report.Engrams, report.GraphEdges, recallCoverageLine(report.Coverage))
	return nil
}

func runRecallActivate(ctx context.Context, w, errW io.Writer, question string, k int, asJSON bool) error {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	brain := filepath.Join(root, recallBrainDir)
	if _, statErr := os.Stat(brain); statErr != nil {
		return errors.New("no recall memory yet; run 'entire recall ingest' first")
	}
	out, err := runRecallBinary(ctx, root, errW, nil, "activate", "--brain", brain, "--k", strconv.Itoa(k), question)
	if err != nil {
		return err
	}
	if asJSON {
		_, err = io.WriteString(w, out)
		return err //nolint:wrapcheck // raw passthrough of the binary's JSON
	}
	var res recallOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return fmt.Errorf("parse recall output: %w", err)
	}
	renderRecallHits(w, question, res.Coverage, res.Hits)
	return nil
}

// runRecallBinary executes the Rust binary with stdin and returns its stdout.
// Its stderr is forwarded so graph warnings reach the user.
func runRecallBinary(ctx context.Context, root string, errW io.Writer, stdin io.Reader, args ...string) (string, error) {
	bin, err := recallBinary(root, os.Getenv("ENTIRE_RECALL_BIN"), exec.LookPath, func(p string) bool {
		_, statErr := os.Stat(p) //nolint:gosec // p is $ENTIRE_RECALL_BIN (the user's own choice) or a fixed name under the worktree root
		return statErr == nil
	})
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = root
	cmd.Stdin = stdin
	cmd.Stderr = errW
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("recall %s failed: %w", args[0], err)
	}
	return string(out), nil
}

// recallBinary locates the Rust binary: $ENTIRE_RECALL_BIN, then `recall` on
// $PATH, then the cargo build inside the repository.
func recallBinary(root, envBin string, lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	if envBin != "" {
		if exists(envBin) {
			return envBin, nil
		}
		return "", fmt.Errorf("ENTIRE_RECALL_BIN=%s does not exist", envBin)
	}
	if p, err := lookPath("recall"); err == nil {
		return p, nil
	}
	for _, profile := range []string{"release", "debug"} {
		p := filepath.Join(root, "recall", "target", profile, "recall")
		if exists(p) {
			return p, nil
		}
	}
	return "", errors.New("recall binary not found: run `cargo build --release` in recall/ or set ENTIRE_RECALL_BIN")
}

// recallCoverageLine is the one line that says what an answer was drawn from.
// It reads "complete" only when nothing at all was missing.
func recallCoverageLine(c recallCoverage) string {
	if c.isComplete() {
		return fmt.Sprintf("context: complete · %d checkpoints · graph ok", c.Total)
	}
	parts := []string{"context: PARTIAL", fmt.Sprintf("%d of %d checkpoints complete", c.Complete, c.Total)}
	fields := make([]string, 0, len(c.Unavailable))
	for f := range c.Unavailable {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%d without %s", c.Unavailable[f], recallFieldWord(f)))
	}
	if c.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", c.Skipped))
	}
	if c.Truncated {
		parts = append(parts, "older checkpoints not examined (walk budget)")
	}
	if c.GraphAvailable {
		parts = append(parts, "graph ok")
	} else {
		parts = append(parts, "graph unavailable (isolation claims unverifiable)")
	}
	return strings.Join(parts, " · ")
}

// recallPartialClaimRedacted is the marker the Rust side adds when the claim
// text itself carries a redaction placeholder (recall/src/rank.rs).
const recallPartialClaimRedacted = "claim redacted"

// recallPartialPhrase renders a hit's partial markers: the unavailable fields
// as one clause, the redacted-claim marker as its own.
func recallPartialPhrase(partial []string) string {
	var fields, clauses []string
	for _, p := range partial {
		if p == recallPartialClaimRedacted {
			continue
		}
		fields = append(fields, recallFieldWord(p))
	}
	if len(fields) > 0 {
		clauses = append(clauses, strings.Join(fields, ", ")+" unavailable")
	}
	if slices.Contains(partial, recallPartialClaimRedacted) {
		clauses = append(clauses, recallPartialClaimRedacted)
	}
	return strings.Join(clauses, "; ")
}

func renderRecallHits(w io.Writer, question string, cov recallCoverage, hits []recallHit) {
	fmt.Fprintf(w, "recall: %q\n%s\n\n", question, recallCoverageLine(cov))
	if len(hits) == 0 {
		fmt.Fprintln(w, "  no memories matched; try different words or run 'entire recall ingest' again")
		return
	}
	for i, h := range hits {
		mark := map[string]string{
			"corroborated": "✓ corroborated",
			"contradicted": "✗ CONTRADICTED",
			"unverifiable": "? UNVERIFIABLE",
			"neutral":      "· neutral",
		}[h.Verdict]
		if mark == "" {
			mark = "· " + h.Verdict
		}
		fmt.Fprintf(w, "%2d. [%-6s] score %.2f  conf %.2f  %s\n", i+1, h.Tier, h.Scored, h.Confidence, h.Text)
		fmt.Fprintf(w, "    %s — %s\n", mark, h.Why)
		if len(h.Partial) > 0 {
			fmt.Fprintf(w, "    ◌ partial context: %s\n", recallPartialPhrase(h.Partial))
		}
		if h.BackedBy != nil {
			fmt.Fprintf(w, "    commit %s\n", *h.BackedBy)
		}
		fmt.Fprintln(w)
	}
}
