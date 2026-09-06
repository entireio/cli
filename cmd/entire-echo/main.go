// entire-echo is an external Entire command that builds an evidence-linked,
// deterministic review bundle for one checkpoint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const schemaVersion = "entire-echo.review-bundle/v1"

// CommandRunner deliberately takes argv, rather than a command string. Echo
// never invokes a shell; the interface also makes command contracts testable.
type CommandRunner interface {
	Run(context.Context, string, []string, string) (stdout, stderr []byte, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, dir string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

type checkpointEnvelope struct {
	CheckpointID string   `json:"checkpoint_id"`
	FilesTouched []string `json:"files_touched"`
	Sessions     []struct {
		Index     int      `json:"index"`
		SessionID string   `json:"session_id"`
		Files     []string `json:"files_touched"`
	} `json:"sessions"`
}

type Evidence struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Locator    string   `json:"locator"`
	Command    []string `json:"command"`
	Excerpt    string   `json:"excerpt"`
	Confidence string   `json:"confidence"`
}

type Claim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
	Confidence  string   `json:"confidence"`
}

type Warning struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type Target struct {
	CheckpointID string `json:"checkpoint_id"`
	SessionID    string `json:"session_id"`
	Commit       string `json:"commit,omitempty"`
}

type ReviewBundle struct {
	SchemaVersion       string     `json:"schema_version"`
	Target              Target     `json:"target"`
	Overview            Claim      `json:"overview"`
	Requested           []Claim    `json:"requested"`
	Implemented         []Claim    `json:"implemented"`
	MissingOrUncertain  []Claim    `json:"missing_or_uncertain"`
	PotentiallyAffected []Claim    `json:"potentially_affected"`
	Continuation        []Claim    `json:"continuation"`
	Evidence            []Evidence `json:"evidence"`
	Warnings            []Warning  `json:"warnings"`
}

type builder struct{ bundle ReviewBundle }

func (b *builder) evidence(kind, locator string, command []string, excerpt, confidence string) string {
	id := fmt.Sprintf("E%03d", len(b.bundle.Evidence)+1)
	b.bundle.Evidence = append(b.bundle.Evidence, Evidence{ID: id, Kind: kind, Locator: locator, Command: command, Excerpt: bound(excerpt, 500), Confidence: confidence})
	return id
}

func bound(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func parseCheckpointMetadata(raw []byte) (checkpointEnvelope, error) {
	var meta checkpointEnvelope
	if err := json.Unmarshal(raw, &meta); err != nil {
		return meta, fmt.Errorf("parse checkpoint metadata JSON: %w", err)
	}
	if meta.CheckpointID == "" {
		return meta, errors.New("checkpoint metadata has no checkpoint_id")
	}
	if len(meta.Sessions) == 0 {
		return meta, errors.New("checkpoint metadata has no sessions")
	}
	seen := map[int]bool{}
	for _, s := range meta.Sessions {
		if s.SessionID == "" || seen[s.Index] {
			return meta, errors.New("checkpoint metadata has malformed or ambiguous sessions")
		}
		seen[s.Index] = true
	}
	return meta, nil
}

func latestSession(meta checkpointEnvelope) (int, string) {
	last := meta.Sessions[0]
	for _, s := range meta.Sessions[1:] {
		if s.Index > last.Index {
			last = s
		}
	}
	return last.Index, last.SessionID
}

type transcriptEntry struct {
	kind string
	text string
}

func transcriptEntries(raw []byte) []transcriptEntry {
	var entries []transcriptEntry
	for _, line := range strings.Split(string(raw), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		var texts []string
		collectText(record, &texts)
		for _, text := range uniqueStrings(texts) {
			entries = append(entries, transcriptEntry{kind: fmt.Sprint(record["type"]), text: text})
		}
	}
	return entries
}

func collectText(v any, texts *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "text" || k == "content" || k == "message" {
				if s, ok := child.(string); ok && strings.TrimSpace(s) != "" {
					*texts = append(*texts, s)
					continue
				}
			}
			collectText(child, texts)
		}
	case []any:
		for _, child := range x {
			collectText(child, texts)
		}
	}
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

var diffFile = regexp.MustCompile(`^\+\+\+ b/(.+)$`)
var diffSymbol = regexp.MustCompile(`^\+.*\b(?:func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)|type\s+([A-Za-z_][A-Za-z0-9_]*))`)

type symbol struct{ file, name string }

func diffSymbols(diff string) []symbol {
	file := ""
	out := []symbol{}
	seen := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		if m := diffFile.FindStringSubmatch(line); len(m) == 2 {
			file = m[1]
			continue
		}
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		if m := diffSymbol.FindStringSubmatch(line); len(m) == 3 && file != "" {
			name := strings.TrimSpace(m[1] + m[2])
			key := file + ":" + name
			if !seen[key] {
				seen[key] = true
				out = append(out, symbol{file, name})
			}
		}
	}
	return out
}

func associatedCommit(ctx context.Context, r CommandRunner, root, checkpointID string) (string, string) {
	out, stderr, err := r.Run(ctx, "git", []string{"log", "--all", "--format=%H", "--fixed-strings", "--grep=Entire-Checkpoint: " + checkpointID}, root)
	if err != nil {
		return "", "could not resolve checkpoint commit: " + strings.TrimSpace(string(stderr))
	}
	commits := uniqueStrings(strings.Split(string(out), "\n"))
	if len(commits) != 1 {
		return "", fmt.Sprintf("associated commit is unavailable or ambiguous (%d exact trailer matches)", len(commits))
	}
	return commits[0], ""
}

func build(ctx context.Context, r CommandRunner, cwd, target string) (ReviewBundle, error) {
	if strings.TrimSpace(target) == "" {
		return ReviewBundle{}, errors.New("provide exactly one checkpoint ID or commit")
	}
	metaOut, metaErr, err := r.Run(ctx, "entire", []string{"checkpoint", "explain", target, "--json"}, cwd)
	if err != nil {
		return ReviewBundle{}, fmt.Errorf("checkpoint explain metadata: %w: %s", err, strings.TrimSpace(string(metaErr)))
	}
	meta, err := parseCheckpointMetadata(metaOut)
	if err != nil {
		return ReviewBundle{}, err
	}
	index, sessionID := latestSession(meta)
	b := builder{bundle: ReviewBundle{SchemaVersion: schemaVersion, Target: Target{CheckpointID: meta.CheckpointID, SessionID: sessionID}}}
	metaID := b.evidence("checkpoint_metadata", "checkpoint:"+meta.CheckpointID, []string{"entire", "checkpoint", "explain", target, "--json"}, string(metaOut), "confirmed")

	rootOut, rootErr, rootRunErr := r.Run(ctx, "git", []string{"rev-parse", "--show-toplevel"}, cwd)
	root := strings.TrimSpace(string(rootOut))
	if rootRunErr != nil || root == "" {
		warnID := b.evidence("warning", "git:worktree-root", []string{"git", "rev-parse", "--show-toplevel"}, string(rootErr), "question")
		b.bundle.Warnings = append(b.bundle.Warnings, Warning{Text: "Git diff unavailable: repository root could not be resolved.", EvidenceIDs: []string{warnID}})
	}

	transcript, transcriptErr, transcriptRunErr := r.Run(ctx, "entire", []string{"checkpoint", "explain", target, "--transcript", "--session-index", fmt.Sprint(index)}, cwd)
	if transcriptRunErr != nil || len(strings.TrimSpace(string(transcript))) == 0 {
		warnID := b.evidence("warning", "checkpoint:"+meta.CheckpointID+"/transcript", []string{"entire", "checkpoint", "explain", target, "--transcript", "--session-index", fmt.Sprint(index)}, string(transcriptErr), "question")
		b.bundle.Warnings = append(b.bundle.Warnings, Warning{Text: "Transcript reconstruction unavailable.", EvidenceIDs: []string{warnID}})
		b.bundle.MissingOrUncertain = append(b.bundle.MissingOrUncertain, Claim{Text: "Requested and implemented details may be incomplete because the stored transcript was unavailable.", EvidenceIDs: []string{warnID}, Confidence: "question"})
	} else {
		transcriptID := b.evidence("checkpoint_transcript", "checkpoint:"+meta.CheckpointID+"/session:"+sessionID, []string{"entire", "checkpoint", "explain", target, "--transcript", "--session-index", fmt.Sprint(index)}, string(transcript), "confirmed")
		b.bundle.Warnings = append(b.bundle.Warnings, Warning{Text: "Transcript evidence is stored-session output; its checkpoint scope is not exposed by the public export contract.", EvidenceIDs: []string{transcriptID}})
		requested, implemented := 0, 0
		for _, entry := range transcriptEntries(transcript) {
			claim := Claim{Text: "Stored transcript text: " + bound(entry.text, 240), EvidenceIDs: []string{transcriptID}, Confidence: "confirmed"}
			if entry.kind == "user" && requested < 6 {
				b.bundle.Requested = append(b.bundle.Requested, claim)
				requested++
			} else if entry.kind == "assistant" && implemented < 6 {
				b.bundle.Implemented = append(b.bundle.Implemented, claim)
				implemented++
			}
		}
		if requested == 0 {
			b.bundle.MissingOrUncertain = append(b.bundle.MissingOrUncertain, Claim{Text: "No user request could be extracted from the stored transcript.", EvidenceIDs: []string{transcriptID}, Confidence: "question"})
		}
	}

	if root != "" {
		commit, warning := associatedCommit(ctx, r, root, meta.CheckpointID)
		if warning != "" {
			id := b.evidence("warning", "checkpoint:"+meta.CheckpointID+"/commit", []string{"git", "log", "--all", "--format=%H", "--fixed-strings", "--grep=Entire-Checkpoint: " + meta.CheckpointID}, warning, "question")
			b.bundle.Warnings = append(b.bundle.Warnings, Warning{Text: "Git diff unavailable: " + warning + ".", EvidenceIDs: []string{id}})
		} else {
			b.bundle.Target.Commit = commit
			diff, diffErr, diffRunErr := r.Run(ctx, "git", []string{"diff", "--find-renames", commit + "^", commit, "--"}, root)
			if diffRunErr != nil {
				id := b.evidence("warning", "commit:"+commit, []string{"git", "diff", "--find-renames", commit + "^", commit, "--"}, string(diffErr), "question")
				b.bundle.Warnings = append(b.bundle.Warnings, Warning{Text: "Git diff unavailable for associated commit.", EvidenceIDs: []string{id}})
			} else {
				diffID := b.evidence("git_diff", "commit:"+commit, []string{"git", "diff", "--find-renames", commit + "^", commit, "--"}, string(diff), "confirmed")
				files := uniqueStrings(append(meta.FilesTouched, changedFiles(string(diff))...))
				sort.Strings(files)
				if len(files) > 0 {
					b.bundle.Implemented = append(b.bundle.Implemented, Claim{Text: "Associated commit changes: " + strings.Join(files, ", ") + ".", EvidenceIDs: []string{diffID, metaID}, Confidence: "confirmed"})
				}
				b.graph(ctx, r, root, diffSymbols(string(diff)), diffID)
			}
		}
	}

	b.bundle.Overview = Claim{Text: fmt.Sprintf("Review of checkpoint %s, session %s.", meta.CheckpointID, sessionID), EvidenceIDs: []string{metaID}, Confidence: "confirmed"}
	b.bundle.Continuation = append(b.bundle.Continuation, Claim{Text: "Continue from the identified checkpoint and session; rerun the evidence commands recorded below before changing code.", EvidenceIDs: []string{metaID}, Confidence: "confirmed"})
	if err := validateBundle(b.bundle); err != nil {
		return ReviewBundle{}, err
	}
	return b.bundle, nil
}

func changedFiles(diff string) []string {
	var files []string
	for _, line := range strings.Split(diff, "\n") {
		if m := diffFile.FindStringSubmatch(line); len(m) == 2 {
			files = append(files, m[1])
		}
	}
	return uniqueStrings(files)
}

func (b *builder) graph(ctx context.Context, r CommandRunner, root string, symbols []symbol, diffID string) {
	if len(symbols) == 0 {
		return
	}
	if len(symbols) > 5 {
		symbols = symbols[:5]
	}
	for _, s := range symbols {
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		args := []string{"graph", "impact", "--repo", root, "--symbol", s.name, "--file", s.file, "--depth", "1", "--limit", "10", "--format", "json"}
		out, stderr, err := r.Run(queryCtx, "entire", args, root)
		cancel()
		if err != nil || len(strings.TrimSpace(string(out))) == 0 {
			id := b.evidence("graph_warning", "symbol:"+s.file+":"+s.name, append([]string{"entire"}, args...), string(stderr), "question")
			b.bundle.Warnings = append(b.bundle.Warnings, Warning{Text: "Graph evidence unavailable for " + s.name + ".", EvidenceIDs: []string{id}})
			continue
		}
		confidence := "potential"
		if strings.Contains(strings.ToLower(string(out)), "partial") || strings.Contains(strings.ToLower(string(out)), "warning") {
			confidence = "question"
		}
		id := b.evidence("graph_impact", "symbol:"+s.file+":"+s.name, append([]string{"entire"}, args...), string(out), confidence)
		b.bundle.PotentiallyAffected = append(b.bundle.PotentiallyAffected, Claim{Text: "Potential static impact for " + s.name + " in " + s.file + ".", EvidenceIDs: []string{diffID, id}, Confidence: confidence})
	}
}

func validateBundle(bundle ReviewBundle) error {
	evidence := map[string]bool{}
	for _, e := range bundle.Evidence {
		if e.ID == "" || evidence[e.ID] {
			return errors.New("invalid evidence IDs")
		}
		evidence[e.ID] = true
	}
	claims := append([]Claim{bundle.Overview}, bundle.Requested...)
	claims = append(claims, bundle.Implemented...)
	claims = append(claims, bundle.MissingOrUncertain...)
	claims = append(claims, bundle.PotentiallyAffected...)
	claims = append(claims, bundle.Continuation...)
	for _, c := range claims {
		if strings.TrimSpace(c.Text) == "" || len(c.EvidenceIDs) == 0 {
			return errors.New("every factual claim must reference evidence")
		}
		for _, id := range c.EvidenceIDs {
			if !evidence[id] {
				return fmt.Errorf("claim references unknown evidence %s", id)
			}
		}
	}
	for _, warning := range bundle.Warnings {
		if strings.TrimSpace(warning.Text) == "" || len(warning.EvidenceIDs) == 0 {
			return errors.New("every warning must reference evidence")
		}
		for _, id := range warning.EvidenceIDs {
			if !evidence[id] {
				return fmt.Errorf("warning references unknown evidence %s", id)
			}
		}
	}
	return nil
}

func renderText(b ReviewBundle) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Entire Echo review\n\n%s\n", b.Overview.Text)
	for _, section := range []struct {
		name   string
		claims []Claim
	}{{"Requested", b.Requested}, {"Implemented", b.Implemented}, {"Missing or uncertain", b.MissingOrUncertain}, {"Potentially affected", b.PotentiallyAffected}, {"Continuation", b.Continuation}} {
		fmt.Fprintf(&out, "\n%s:\n", section.name)
		if len(section.claims) == 0 {
			out.WriteString("- None established by available evidence.\n")
		}
		for _, c := range section.claims {
			fmt.Fprintf(&out, "- %s [evidence: %s]\n", c.Text, strings.Join(c.EvidenceIDs, ", "))
		}
	}
	if len(b.Warnings) > 0 {
		out.WriteString("\nWarnings:\n")
		for _, w := range b.Warnings {
			fmt.Fprintf(&out, "- %s [evidence: %s]\n", w.Text, strings.Join(w.EvidenceIDs, ", "))
		}
	}
	out.WriteString("\nEvidence:\n")
	for _, e := range b.Evidence {
		fmt.Fprintf(&out, "- %s (%s): %s\n", e.ID, e.Kind, e.Locator)
	}
	return out.String()
}

func main() {
	jsonOutput := flag.Bool("json", false, "write the versioned ReviewBundle JSON")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: entire-echo [--json] <checkpoint-id-or-commit>")
		os.Exit(2)
	}
	bundle, err := build(context.Background(), execRunner{}, mustGetwd(), flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "entire echo:", err)
		os.Exit(1)
	}
	if *jsonOutput {
		data, _ := json.MarshalIndent(bundle, "", "  ")
		fmt.Println(string(data))
		return
	}
	fmt.Print(renderText(bundle))
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
