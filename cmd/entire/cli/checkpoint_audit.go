package cli

import (
	"context"
	"encoding/json"
	"fmt"
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

const (
	maxCheckpointAuditRequirements     = 25
	maxCheckpointAuditRequirementRunes = 240
	redactedCheckpointAuditRequirement = "[REDACTED INTENT]"
)

var (
	auditIntentListMarkerPattern = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)])\s+`)
	auditIntentSecretPattern     = regexp.MustCompile(`(?i)\b(?:secret|token|api[_ -]?key|credential|authorization|bearer|password)\b\s*[:=]`)
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
	Intent         IntentPacket           `json:"intent"`
	Implementation ImplementationEvidence `json:"implementation"`
	Findings       []AuditFinding         `json:"findings"`
}

type checkpointAuditCollector func(cmd *cobra.Command, target string, sessionIndex int, testFilter string) (AuditReport, error)

type checkpointAuditDeps struct {
	collect   checkpointAuditCollector
	evaluator intentlens.Evaluator
}

func (deps checkpointAuditDeps) withDefaults() checkpointAuditDeps {
	if deps.collect == nil {
		deps.collect = collectCheckpointAuditEvidence
	}
	if deps.evaluator == nil {
		deps.evaluator = intentlens.NewGeminiEvaluator(nil)
	}
	return deps
}

func newCheckpointAuditCmd() *cobra.Command {
	return newCheckpointAuditCmdWithDeps(checkpointAuditDeps{})
}

func newCheckpointAuditCmdWithDeps(deps checkpointAuditDeps) *cobra.Command {
	var jsonOut bool
	var requirementID string
	var sessionIndex int
	var testFilter string

	cmd := &cobra.Command{
		Use:   "audit <checkpoint-id>",
		Short: "Audit checkpoint intent against implementation evidence",
		Long: `Collect checkpoint intent and local Git/test-file evidence with IntentLens.
Results remain UNCERTAIN when verified per-requirement evidence is unavailable.
Raw checkpoint content stays local; evaluators receive only typed signals.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheckpointAudit(cmd, args[0], jsonOut, requirementID, sessionIndex, testFilter, deps.withDefaults())
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit validated audit JSON")
	cmd.Flags().StringVar(&requirementID, "requirement", "", "Show full evidence and recommendation for one requirement ID, such as R2")
	cmd.Flags().IntVar(&sessionIndex, "session-index", -1, "Collect a specific session within the checkpoint (0-based; default latest)")
	cmd.Flags().StringVar(&testFilter, "test", "", "Restrict changed test-file evidence to paths containing this text")
	return cmd
}

func runCheckpointAudit(cmd *cobra.Command, target string, jsonOut bool, requirementID string, sessionIndex int, testFilter string, deps checkpointAuditDeps) error {
	report, err := deps.collect(cmd, target, sessionIndex, testFilter)
	if err != nil {
		return err
	}
	evidence := buildIntentLensEvidencePackage(report)
	// File changes and graph output do not prove per-requirement behavior.
	// Until a local verifier supplies those facts, every signal remains unknown.
	input, err := intentlens.NewEvaluatorInput(make([]intentlens.RequirementSignals, len(evidence.Requirements)), evidence.Context.Status == intentlens.ContextComplete)
	if err != nil {
		return err
	}
	auditJSON, err := deps.evaluator.Evaluate(cmd.Context(), input)
	if err != nil {
		return err
	}
	audit, err := intentlens.ParseAuditJSON(auditJSON)
	if err != nil {
		return fmt.Errorf("validate IntentLens audit: %w", err)
	}
	if err := input.ValidateResult(audit); err != nil {
		return err
	}
	for i := range audit.Requirements {
		audit.Requirements[i].Requirement = string(evidence.Requirements[i].Requirement)
	}
	if jsonOut {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(audit)
	}
	return intentlens.RenderDashboard(cmd.OutOrStdout(), intentlens.DashboardState{
		Audit:         &audit,
		CheckpointID:  report.Intent.CheckpointID,
		ContextStatus: evidence.Context.Status,
		ContextNote:   string(evidence.Context.Note),
		Agent:         report.Intent.Agent,
		RequirementID: requirementID,
	})
}

func collectCheckpointAuditEvidence(cmd *cobra.Command, target string, sessionIndex int, testFilter string) (AuditReport, error) {
	ctx := cmd.Context()
	cpID, lookup, err := resolveExplainCheckpointID(ctx, cmd.ErrOrStderr(), explainExportOptions{target: target})
	if err != nil {
		return AuditReport{}, fmt.Errorf("resolve checkpoint: %w", err)
	}
	defer lookup.Close()

	summary, err := checkpoint.ReadCheckpoint(ctx, lookup.store, cpID)
	if err != nil {
		return AuditReport{}, fmt.Errorf("read checkpoint summary: %w", err)
	}
	index, err := resolveSessionIndex(summary, sessionIndex)
	if err != nil {
		return AuditReport{}, err
	}
	metadata, prompts, err := lookup.store.ReadSessionMetadataAndPrompts(ctx, cpID, index)
	if err != nil {
		return AuditReport{}, fmt.Errorf("read checkpoint session %d: %w", index, err)
	}

	if metadata == nil {
		return AuditReport{}, fmt.Errorf("checkpoint session metadata unavailable")
	}
	intent := buildAuditIntent(cpID, summary, &checkpoint.SessionContent{Metadata: *metadata, Prompts: prompts})
	implementation, err := gatherAuditImplementationEvidence(ctx, lookup.repo, cpID, testFilter)
	if err != nil {
		return AuditReport{}, fmt.Errorf("gather implementation evidence: %w", err)
	}
	// No safe local Graph adapter is available. Never send checkpoint prose as
	// a Graph query or invoke an unknown external command with it.
	implementation.Warnings = append(implementation.Warnings, "Entire Graph evidence unavailable: local typed adapter is not integrated")

	report := AuditReport{
		Intent:         intent,
		Implementation: implementation,
		Findings:       []AuditFinding{},
	}
	return report, nil
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

func buildIntentLensEvidencePackage(report AuditReport) intentlens.EvidencePackage {
	evidence := intentlens.EvidencePackage{
		Context: intentlens.ContextEvidence{
			Status: intentlens.ContextComplete,
		},
	}
	var intentIncomplete bool
	evidence.Requirements, intentIncomplete = sanitizedCheckpointAuditRequirements(report.Intent.Prompts)
	if intentIncomplete {
		evidence.Context.Status = intentlens.ContextIncomplete
		evidence.Context.Note = "checkpoint intent was missing or redacted before evaluation"
	}

	for _, file := range sortedUniqueStrings(append(append([]string(nil), report.Intent.DeclaredFilesTouched...), report.Implementation.ActualFilesTouched...)) {
		evidence.ChangedFiles = append(evidence.ChangedFiles, intentlens.ChangedFileEvidence{Path: intentlens.SanitizedPath(file)})
	}
	for _, commit := range report.Implementation.LinkedCommits {
		evidence.StructuralEvidence = append(evidence.StructuralEvidence, intentlens.StructuralEvidence{
			Kind:        "linked_commit",
			Observation: intentlens.SanitizedText("Commit " + commit + " carries the checkpoint trailer."),
		})
	}
	if len(report.Implementation.GraphEvidence) > 0 {
		evidence.GraphEvidence = append(evidence.GraphEvidence, intentlens.GraphEvidence{
			Source:   "Entire Graph",
			Relation: "returned",
			Target:   "bounded graph evidence available; raw graph output withheld",
		})
	}
	for _, warning := range report.Implementation.Warnings {
		if strings.Contains(warning, "Entire Graph evidence unavailable") {
			evidence.StructuralEvidence = append(evidence.StructuralEvidence, intentlens.StructuralEvidence{
				Kind:        "missing_graph_evidence",
				Observation: "Entire Graph evidence was unavailable; no graph conclusion was collected.",
			})
		}
	}
	for _, testFile := range report.Implementation.FocusedTests {
		evidence.TestEvidence = append(evidence.TestEvidence, intentlens.TestEvidence{
			Name:       intentlens.SanitizedText(testFile),
			Result:     "not_run",
			Summary:    "Changed test file path was collected; no test execution result was supplied.",
			Provenance: "checkpoint audit collector changed test-file evidence",
		})
	}
	if len(report.Implementation.Warnings) > 0 {
		evidence.Context.Note += " Local evidence is incomplete; graph or behavior verification may be unavailable."
	}
	return evidence
}

type checkpointAuditSanitizedRequirement struct {
	Text string
}

func sanitizedCheckpointAuditRequirements(prompts []string) ([]intentlens.AtomicRequirement, bool) {
	candidates := make([]checkpointAuditSanitizedRequirement, 0, len(prompts))
	incomplete := false
	for _, prompt := range prompts {
		for _, candidate := range checkpointAuditIntentCandidates(prompt) {
			if len(candidates) == maxCheckpointAuditRequirements {
				incomplete = true
				break
			}
			text, ok := sanitizeCheckpointAuditRequirement(candidate)
			if !ok {
				incomplete = true
				continue
			}
			candidates = append(candidates, checkpointAuditSanitizedRequirement{Text: text})
		}
		if len(candidates) == maxCheckpointAuditRequirements {
			break
		}
	}
	if len(candidates) == 0 {
		return []intentlens.AtomicRequirement{{
			ID:             "R1",
			Requirement:    redactedCheckpointAuditRequirement,
			IntentRedacted: true,
		}}, true
	}
	requirements := make([]intentlens.AtomicRequirement, 0, len(candidates))
	for i, candidate := range candidates {
		requirements = append(requirements, intentlens.AtomicRequirement{
			ID:          fmt.Sprintf("R%d", i+1),
			Requirement: intentlens.SanitizedText(candidate.Text),
		})
	}
	return requirements, incomplete
}

func checkpointAuditIntentCandidates(intent string) []string {
	intent = strings.ReplaceAll(intent, "\r\n", "\n")
	intent = strings.ReplaceAll(intent, "\r", "\n")
	var candidates []string
	var current string
	flush := func() {
		for _, sentence := range splitCheckpointAuditSentences(current) {
			if sentence != "" {
				candidates = append(candidates, sentence)
			}
		}
		current = ""
	}

	for _, rawLine := range strings.Split(intent, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			flush()
			continue
		}
		stripped := auditIntentListMarkerPattern.ReplaceAllString(line, "")
		if stripped != line {
			flush()
			current = stripped
			continue
		}
		if current != "" && (strings.HasSuffix(current, ":") || rawLine != strings.TrimLeft(rawLine, " \t")) {
			current += " " + strings.Trim(line, "`")
			continue
		}
		flush()
		current = line
	}
	flush()
	return candidates
}

func splitCheckpointAuditSentences(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var parts []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ';' || r == '!' || r == '?'
	}) {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func sanitizeCheckpointAuditRequirement(candidate string) (string, bool) {
	candidate = strings.TrimSpace(strings.Trim(candidate, "`"))
	if candidate == "" || unsafeCheckpointAuditIntent(candidate) {
		return "", false
	}
	candidate = strings.Join(strings.Fields(candidate), " ")
	runes := []rune(candidate)
	if len(runes) > maxCheckpointAuditRequirementRunes {
		return "", false
	}
	return candidate, candidate != ""
}

func unsafeCheckpointAuditIntent(candidate string) bool {
	lower := strings.ToLower(candidate)
	switch {
	case strings.Contains(lower, "begin transcript"),
		strings.Contains(lower, "end transcript"),
		strings.Contains(lower, "session log"),
		strings.Contains(lower, "raw checkpoint"),
		strings.Contains(lower, "checkpoint prompt"):
		return true
	case strings.HasPrefix(strings.TrimSpace(lower), "user:"),
		strings.HasPrefix(strings.TrimSpace(lower), "assistant:"),
		strings.HasPrefix(strings.TrimSpace(lower), "system:"),
		strings.HasPrefix(strings.TrimSpace(lower), "tool:"):
		return true
	case strings.Contains(lower, "secret_transcript_"):
		return true
	case auditIntentSecretPattern.MatchString(candidate):
		return true
	default:
		return false
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
