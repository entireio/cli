package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

// Verdict enumerates the outcome for a single audited claim. Claim evaluation
// is deliberately not part of evidence collection; these values are reserved
// for the audit/evaluation layer that consumes AuditReport.
type Verdict string

const (
	VerdictSupported    Verdict = "supported"
	VerdictMissing      Verdict = "missing"
	VerdictOutOfScope   Verdict = "out_of_scope"
	VerdictUnverifiable Verdict = "unverifiable"
)

// IntentPacket is the checkpoint-native input to a later audit evaluator.
// Prompts holds the stored prompt document for each selected session. The
// checkpoint format does not promise an individual-prompt delimiter, so this
// command preserves those bytes as text instead of guessing a split.
type IntentPacket struct {
	CheckpointID         string   `json:"checkpoint_id"`
	Prompts              []string `json:"prompts"`
	TranscriptStart      int      `json:"checkpoint_transcript_start"`
	DeclaredFilesTouched []string `json:"declared_files_touched"`
	Model                string   `json:"model"`
	Agent                string   `json:"agent"`
	SkillEvents          []string `json:"skill_events,omitempty"`
}

// ImplementationEvidence captures implementation facts without judging them.
type ImplementationEvidence struct {
	LinkedCommits      []string          `json:"linked_commits"`
	ActualFilesTouched []string          `json:"actual_files_touched"`
	Diffs              map[string]string `json:"diffs,omitempty"`
	FocusedTests       []string          `json:"focused_tests,omitempty"`
	GraphEvidence      json.RawMessage   `json:"graph_evidence,omitempty"`
	Warnings           []string          `json:"warnings,omitempty"`
}

// AuditFinding is one comparison emitted by a future audit/evaluation layer.
type AuditFinding struct {
	Claim   string  `json:"claim"`
	Verdict Verdict `json:"verdict"`
	Detail  string  `json:"detail"`
}

// AuditReport is the native command's stable evidence envelope. Findings stay
// empty until an evaluator is wired in; evidence collection must not invent
// audit outcomes.
type AuditReport struct {
	Intent         IntentPacket            `json:"intent"`
	Implementation ImplementationEvidence `json:"implementation"`
	Findings       []AuditFinding          `json:"findings"`
}

func newCheckpointAuditCmd() *cobra.Command {
	var jsonOut bool
	var sessionIndex int
	var testFilter string

	cmd := &cobra.Command{
		Use:   "audit <checkpoint-id|commit-sha>",
		Short: "Collect checkpoint intent and implementation evidence",
		Long: `Collect the developer intent stored in an Entire checkpoint and the
Git, test-file, and Entire Graph evidence needed to audit its implementation.

This command only collects normalized evidence. It does not yet evaluate
claims or call a model.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheckpointAudit(cmd, args[0], jsonOut, sessionIndex, testFilter)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON evidence")
	cmd.Flags().IntVar(&sessionIndex, "session-index", -1, "Collect a specific session within the checkpoint (0-based; default latest)")
	cmd.Flags().StringVar(&testFilter, "test", "", "Restrict changed test-file evidence to paths containing this text")
	return cmd
}

func runCheckpointAudit(cmd *cobra.Command, target string, jsonOut bool, sessionIndex int, testFilter string) error {
	ctx := cmd.Context()
	cpID, lookup, err := resolveExplainCheckpointID(ctx, cmd.ErrOrStderr(), explainExportOptions{target: target})
	if err != nil {
		return fmt.Errorf("resolve checkpoint: %w", err)
	}
	defer lookup.Close()

	summary, err := checkpoint.ReadCheckpoint(ctx, lookup.store, cpID)
	if err != nil {
		return fmt.Errorf("read checkpoint summary: %w", err)
	}
	index, err := resolveSessionIndex(summary, sessionIndex)
	if err != nil {
		return err
	}
	content, err := lookup.store.ReadSessionContent(ctx, cpID, index)
	if err != nil {
		return fmt.Errorf("read checkpoint session %d: %w", index, err)
	}

	intent := buildAuditIntent(cpID, summary, content)
	implementation, err := gatherAuditImplementationEvidence(ctx, lookup.repo, cpID, testFilter)
	if err != nil {
		return fmt.Errorf("gather implementation evidence: %w", err)
	}
	if graphEvidence, graphErr := runEntireGraphSearch(ctx, intent.Prompts); graphErr != nil {
		implementation.Warnings = append(implementation.Warnings, "Entire Graph evidence unavailable: "+graphErr.Error())
	} else {
		implementation.GraphEvidence = graphEvidence
	}

	report := AuditReport{
		Intent:         intent,
		Implementation: implementation,
		Findings:       []AuditFinding{},
	}
	if jsonOut {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
	}
	renderCheckpointAuditReport(cmd.OutOrStdout(), report)
	return nil
}

func buildAuditIntent(cpID id.CheckpointID, summary *checkpoint.CheckpointSummary, content *checkpoint.SessionContent) IntentPacket {
	prompts := make([]string, 0, 1)
	if prompt := strings.TrimSpace(content.Prompts); prompt != "" {
		prompts = append(prompts, prompt)
	}
	skillEvents := make([]string, 0, len(content.Metadata.SkillEvents))
	for _, event := range content.Metadata.SkillEvents {
		if event.Skill.Name != "" {
			skillEvents = append(skillEvents, event.Skill.Name)
		}
	}
	files := append([]string(nil), content.Metadata.FilesTouched...)
	if len(files) == 0 {
		files = append(files, summary.FilesTouched...)
	}
	return IntentPacket{
		CheckpointID:         cpID.String(),
		Prompts:              prompts,
		TranscriptStart:      content.Metadata.GetTranscriptStart(),
		DeclaredFilesTouched: sortedUniqueStrings(files),
		Model:                content.Metadata.Model,
		Agent:                string(content.Metadata.Agent),
		SkillEvents:          skillEvents,
	}
}

func gatherAuditImplementationEvidence(ctx context.Context, repo *git.Repository, cpID id.CheckpointID, testFilter string) (ImplementationEvidence, error) {
	commits, err := auditLinkedCommits(ctx, repo, cpID)
	if err != nil {
		return ImplementationEvidence{}, err
	}
	evidence := ImplementationEvidence{LinkedCommits: make([]string, 0, len(commits)), Diffs: make(map[string]string)}
	if len(commits) == 0 {
		evidence.Warnings = append(evidence.Warnings, "no reachable Git commit carries this checkpoint's Entire-Checkpoint trailer")
	}
	files := make([]string, 0)
	for _, commit := range commits {
		evidence.LinkedCommits = append(evidence.LinkedCommits, commit.Hash.String())
		if commit.NumParents() == 0 {
			evidence.Warnings = append(evidence.Warnings, "root commit "+commit.Hash.String()+" has no parent diff")
			continue
		}
		parent, parentErr := commit.Parent(0)
		if parentErr != nil {
			return ImplementationEvidence{}, fmt.Errorf("read parent of linked commit %s: %w", commit.Hash, parentErr)
		}
		patch, patchErr := parent.Patch(commit)
		if patchErr != nil {
			return ImplementationEvidence{}, fmt.Errorf("diff linked commit %s: %w", commit.Hash, patchErr)
		}
		evidence.Diffs[commit.Hash.String()] = patch.String()
		for _, filePatch := range patch.FilePatches() {
			from, to := filePatch.Files()
			if from != nil {
				files = append(files, from.Path())
			}
			if to != nil {
				files = append(files, to.Path())
			}
		}
	}
	evidence.ActualFilesTouched = sortedUniqueStrings(files)
	for _, file := range evidence.ActualFilesTouched {
		if strings.HasSuffix(file, "_test.go") && (testFilter == "" || strings.Contains(file, testFilter)) {
			evidence.FocusedTests = append(evidence.FocusedTests, file)
		}
	}
	if len(evidence.Diffs) == 0 {
		evidence.Diffs = nil
	}
	return evidence, nil
}

func auditLinkedCommits(ctx context.Context, repo *git.Repository, cpID id.CheckpointID) ([]*object.Commit, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("read repository HEAD: %w", err)
	}
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return nil, fmt.Errorf("walk repository history: %w", err)
	}
	defer iter.Close()

	var commits []*object.Commit
	err = iter.ForEach(func(commit *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, linkedID := range trailers.ParseAllCheckpoints(commit.Message) {
			if linkedID == cpID {
				commits = append(commits, commit)
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate repository history: %w", err)
	}
	return commits, nil
}

// runEntireGraphSearch obtains optional local graph evidence. The graph command
// is intentionally outside the checkpoint package: graph installation is not a
// checkpoint-storage requirement, and a missing graph tool must not block audit
// evidence from Git and the checkpoint itself.
func runEntireGraphSearch(ctx context.Context, prompts []string) (json.RawMessage, error) {
	if len(prompts) == 0 {
		return nil, fmt.Errorf("checkpoint contains no stored prompts to query")
	}
	path, err := exec.LookPath("entire")
	if err != nil {
		return nil, fmt.Errorf("find entire graph command: %w", err)
	}
	query := strings.Join(prompts, "\n")
	output, err := exec.CommandContext(ctx, path, "graph", "search", "--repo", ".", "--profile", "fast", "--query", query).Output()
	if err != nil {
		return nil, fmt.Errorf("run entire graph search: %w", err)
	}
	// Graph search is intentionally human-readable today. Preserve its exact
	// output in a JSON envelope so the audit report remains valid JSON without
	// pretending the graph tool exposes a stable JSON search schema.
	encoded, err := json.Marshal(struct {
		Command string `json:"command"`
		Output  string `json:"output"`
	}{
		Command: "entire graph search --profile fast",
		Output:  string(output),
	})
	if err != nil {
		return nil, fmt.Errorf("encode Entire Graph evidence: %w", err)
	}
	return json.RawMessage(encoded), nil
}

func renderCheckpointAuditReport(w io.Writer, report AuditReport) {
	fmt.Fprintf(w, "Checkpoint audit evidence: %s\n", report.Intent.CheckpointID)
	fmt.Fprintf(w, "Session: agent=%s model=%s transcript-start=%d\n", report.Intent.Agent, report.Intent.Model, report.Intent.TranscriptStart)
	fmt.Fprintf(w, "Prompts: %d  Declared files: %d\n", len(report.Intent.Prompts), len(report.Intent.DeclaredFilesTouched))
	fmt.Fprintf(w, "Linked commits: %d  Changed files: %d  Changed tests: %d\n", len(report.Implementation.LinkedCommits), len(report.Implementation.ActualFilesTouched), len(report.Implementation.FocusedTests))
	if len(report.Implementation.Warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, warning := range report.Implementation.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}
	fmt.Fprintln(w, "No audit findings were generated; an evaluation layer has not been configured.")
}

func sortedUniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
