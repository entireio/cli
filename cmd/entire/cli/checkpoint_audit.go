package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/review/intentlens"
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

type AuditContext string

const (
	AuditContextComplete   AuditContext = "COMPLETE"
	AuditContextIncomplete AuditContext = "INCOMPLETE"
)

// SanitizedRequirement is a locally-derived, typed representation of a stored
// prompt. It deliberately contains no prompt text: checkpoint prompts never
// cross the local privacy boundary.
type SanitizedRequirement struct {
	ID       string `json:"id"`
	Statement string `json:"statement"`
}

// IntentPacket contains only local-safe checkpoint context. Raw prompts and
// transcripts are intentionally absent.
type IntentPacket struct {
	CheckpointID         string   `json:"checkpoint_id"`
	Requirements         []SanitizedRequirement `json:"requirements"`
	Context              AuditContext `json:"context"`
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
		Short: "Audit a checkpoint using sanitized evidence",
		Long: `Audit a checkpoint using locally sanitized requirements plus Git,
test-file, and Entire Graph evidence. Raw checkpoint prompts and transcripts
remain local and are never sent to the evaluator.`,
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
	metadata, prompts, err := lookup.store.ReadSessionMetadataAndPrompts(ctx, cpID, index)
	if err != nil {
		return fmt.Errorf("read checkpoint session %d metadata and prompts: %w", index, err)
	}

	intent := buildAuditIntent(cpID, summary, metadata, prompts)
	implementation, err := gatherAuditImplementationEvidence(ctx, lookup.repo, cpID, testFilter)
	if err != nil {
		return fmt.Errorf("gather implementation evidence: %w", err)
	}
	if graphEvidence, graphErr := runEntireGraphSearch(ctx, intent.Requirements); graphErr != nil {
		implementation.Warnings = append(implementation.Warnings, "Entire Graph evidence unavailable: "+graphErr.Error())
	} else {
		implementation.GraphEvidence = graphEvidence
	}

	context := auditContext(intent, implementation)
	intent.Context = context
	findings, err := evaluateCheckpointAudit(ctx, intent, implementation)
	if err != nil {
		return fmt.Errorf("evaluate sanitized audit evidence: %w", err)
	}
	if context == AuditContextIncomplete {
		implementation.Warnings = append(implementation.Warnings, "audit context is INCOMPLETE; findings are non-authoritative")
		findings = makeNonAuthoritative(findings)
	}
	report := AuditReport{
		Intent:         intent,
		Implementation: implementation,
		Findings:       findings,
	}
	if jsonOut {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
	}
	renderCheckpointAuditReport(cmd.OutOrStdout(), report)
	return nil
}

func buildAuditIntent(cpID id.CheckpointID, summary *checkpoint.CheckpointSummary, metadata *checkpoint.Metadata, prompts string) IntentPacket {
	skillEvents := make([]string, 0, len(metadata.SkillEvents))
	for _, event := range metadata.SkillEvents {
		if event.Skill.Name != "" {
			skillEvents = append(skillEvents, event.Skill.Name)
		}
	}
	files := append([]string(nil), metadata.FilesTouched...)
	if len(files) == 0 {
		files = append(files, summary.FilesTouched...)
	}
	return IntentPacket{
		CheckpointID:         cpID.String(),
		Requirements:         sanitizeCheckpointRequirements(prompts, files),
		DeclaredFilesTouched: sortedUniqueStrings(files),
		Model:                metadata.Model,
		Agent:                string(metadata.Agent),
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
func runEntireGraphSearch(ctx context.Context, requirements []SanitizedRequirement) (json.RawMessage, error) {
	if len(requirements) == 0 {
		return nil, fmt.Errorf("checkpoint contains no sanitized requirements to query")
	}
	path, err := exec.LookPath("entire")
	if err != nil {
		return nil, fmt.Errorf("find entire graph command: %w", err)
	}
	queries := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		queries = append(queries, requirement.Statement)
	}
	query := strings.Join(queries, "\n")
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

var requirementBreak = regexp.MustCompile(`[\r\n]+|[.!?]+`)

// sanitizeCheckpointRequirements performs the only raw-prompt processing in
// this command. Its output is a closed vocabulary of behavior categories, not
// a redacted copy of the prompt, so neither a prompt nor its free-form content
// can reach Graph, Gemini, or command output.
func sanitizeCheckpointRequirements(prompts string, declaredFiles []string) []SanitizedRequirement {
	units := requirementBreak.Split(prompts, -1)
	requirements := make([]SanitizedRequirement, 0, len(units))
	for _, unit := range units {
		if strings.TrimSpace(unit) == "" {
			continue
		}
		category := sanitizedRequirementCategory(strings.ToLower(unit))
		requirements = append(requirements, SanitizedRequirement{
			ID:       fmt.Sprintf("R%d", len(requirements)+1),
			Statement: "Implement the requested " + category + " behavior.",
		})
	}
	if len(requirements) == 0 && len(declaredFiles) > 0 {
		requirements = append(requirements, SanitizedRequirement{ID: "R1", Statement: "Implement the requested repository behavior."})
	}
	return requirements
}

func sanitizedRequirementCategory(unit string) string {
	switch {
	case strings.Contains(unit, "test") || strings.Contains(unit, "spec"):
		return "verification"
	case strings.Contains(unit, "security") || strings.Contains(unit, "privacy") || strings.Contains(unit, "auth"):
		return "security"
	case strings.Contains(unit, "error") || strings.Contains(unit, "fail") || strings.Contains(unit, "bug") || strings.Contains(unit, "fix"):
		return "error-handling"
	case strings.Contains(unit, "performance") || strings.Contains(unit, "fast") || strings.Contains(unit, "cache"):
		return "performance"
	default:
		return "repository"
	}
}

func auditContext(intent IntentPacket, implementation ImplementationEvidence) AuditContext {
	// This collector has changed-test paths, but no historical test-result
	// artifact. Until a verified result is collected, conclusions cannot be
	// authoritative even when Git and Graph evidence are available.
	if len(intent.Requirements) == 0 || len(implementation.LinkedCommits) == 0 || len(implementation.FocusedTests) == 0 {
		return AuditContextIncomplete
	}
	return AuditContextIncomplete
}

func evaluateCheckpointAudit(ctx context.Context, intent IntentPacket, implementation ImplementationEvidence) ([]AuditFinding, error) {
	// Keep this value deliberately narrow. In particular, it excludes the raw
	// prompts, transcript, patch text, and raw Graph output.
	evidence, err := json.Marshal(struct {
		CheckpointID string                 `json:"checkpoint_id"`
		Context      AuditContext           `json:"context"`
		Requirements []SanitizedRequirement `json:"requirements"`
		Commits      []string               `json:"linked_commits"`
		Files        []string               `json:"files_touched"`
		Tests        []string               `json:"changed_test_files"`
		Graph        bool                   `json:"graph_evidence_available"`
		TestResults  string                 `json:"historical_test_results"`
	}{
		CheckpointID: intent.CheckpointID,
		Context:      intent.Context,
		Requirements: intent.Requirements,
		Commits:      implementation.LinkedCommits,
		Files:        implementation.ActualFilesTouched,
		Tests:        implementation.FocusedTests,
		Graph:        len(implementation.GraphEvidence) > 0,
		TestResults:  "unavailable",
	})
	if err != nil {
		return nil, fmt.Errorf("encode sanitized evidence package: %w", err)
	}

	output, err := intentlens.NewGeminiEvaluator(nil).Evaluate(ctx, intentlens.EvidencePackage(evidence))
	if err != nil {
		return nil, err
	}
	audit, err := intentlens.ParseAuditJSON(output)
	if err != nil {
		return nil, fmt.Errorf("parse evaluator findings: %w", err)
	}
	findings := make([]AuditFinding, 0, len(audit.Requirements))
	for _, requirement := range audit.Requirements {
		findings = append(findings, AuditFinding{
			Claim:   requirement.Requirement,
			Verdict: auditVerdict(requirement.Status),
			Detail:  requirement.Recommendation,
		})
	}
	return findings, nil
}

func auditVerdict(status intentlens.Status) Verdict {
	switch status {
	case intentlens.StatusImplemented:
		return VerdictSupported
	case intentlens.StatusIncomplete:
		return VerdictMissing
	default:
		return VerdictUnverifiable
	}
}

func makeNonAuthoritative(findings []AuditFinding) []AuditFinding {
	for i := range findings {
		findings[i].Verdict = VerdictUnverifiable
		findings[i].Detail = "INCOMPLETE context: this finding is non-authoritative. " + findings[i].Detail
	}
	return findings
}

func renderCheckpointAuditReport(w io.Writer, report AuditReport) {
	fmt.Fprintf(w, "Checkpoint audit evidence: %s\n", report.Intent.CheckpointID)
	fmt.Fprintf(w, "Session: agent=%s model=%s context=%s\n", report.Intent.Agent, report.Intent.Model, report.Intent.Context)
	fmt.Fprintf(w, "Sanitized requirements: %d  Declared files: %d\n", len(report.Intent.Requirements), len(report.Intent.DeclaredFilesTouched))
	fmt.Fprintf(w, "Linked commits: %d  Changed files: %d  Changed tests: %d\n", len(report.Implementation.LinkedCommits), len(report.Implementation.ActualFilesTouched), len(report.Implementation.FocusedTests))
	if len(report.Implementation.Warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, warning := range report.Implementation.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}
	if len(report.Findings) > 0 {
		fmt.Fprintln(w, "Findings:")
		for _, finding := range report.Findings {
			fmt.Fprintf(w, "  - [%s] %s: %s\n", finding.Verdict, finding.Claim, finding.Detail)
		}
	}
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
