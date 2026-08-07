package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
)

// The plain-text report abbreviates commit SHAs with the package's shared
// shortSHA helper. Checkpoint IDs are never abbreviated: a ULID or a 12-hex
// import ID is the whole identifier, so both the text and JSON renderings
// print them in full.

// unmatchedSubjectLength caps how much of a commit subject the text report
// shows, so a long message can't wrap the one-line-per-commit layout.
const unmatchedSubjectLength = 40

// importJSON is the machine-readable result of `entire import <agent> --json`.
// Reconcile fields are present only when --reconcile ran; summary is always
// present, so a caller can parse one shape either way.
type importJSON struct {
	CommitsScanned   int                   `json:"commits_scanned"`
	Links            []importLinkJSON      `json:"links"`
	Candidates       []importLinkJSON      `json:"candidates"`
	UnmatchedCommits []importUnmatchedJSON `json:"unmatched_commits"`
	Summary          importJSONSummary     `json:"summary"`
}

// importLinkJSON is one commit↔checkpoint link. SHAs and checkpoint IDs are
// full-length: consumers match on them.
type importLinkJSON struct {
	CommitSHA    string `json:"commit_sha"`
	CheckpointID string `json:"checkpoint_id"`
	SessionID    string `json:"session_id"`
	TurnUUID     string `json:"turn_uuid"`
	Method       string `json:"method"`
	Action       string `json:"action"`
}

type importUnmatchedJSON struct {
	CommitSHA   string    `json:"commit_sha"`
	Subject     string    `json:"subject"`
	CommittedAt time.Time `json:"committed_at"`
}

type importJSONSummary struct {
	Agent           string `json:"agent"`
	DryRun          bool   `json:"dry_run"`
	Reconciled      bool   `json:"reconciled"`
	SessionsScanned int    `json:"sessions_scanned"`
	TurnsImported   int    `json:"turns_imported"`
	TurnsSkipped    int    `json:"turns_skipped"`
	LinksRecorded   int    `json:"links_recorded"`
	LinksHeuristic  int    `json:"links_heuristic"`
	Backfilled      int    `json:"backfilled"`
}

// writeImportJSON emits the machine-readable import result. It never depends on
// a terminal, so an agent gets the same bytes as a pipe.
func writeImportJSON(w io.Writer, agentName string, res agentimport.Result, dryRun bool) error {
	out := importJSON{
		Links:            []importLinkJSON{},
		Candidates:       []importLinkJSON{},
		UnmatchedCommits: []importUnmatchedJSON{},
		Summary: importJSONSummary{
			Agent:           agentName,
			DryRun:          dryRun,
			Reconciled:      res.Report != nil,
			SessionsScanned: res.SessionsScanned,
			TurnsImported:   res.TurnsImported,
			TurnsSkipped:    res.TurnsSkipped,
			LinksRecorded:   res.LinksRecorded,
			LinksHeuristic:  res.LinksHeuristic,
			Backfilled:      res.Backfilled,
		},
	}
	if rep := res.Report; rep != nil {
		out.CommitsScanned = rep.CommitsScanned
		out.Links = importLinksJSON(rep.Links)
		out.Candidates = importLinksJSON(rep.Candidates)
		for _, c := range rep.UnmatchedCommits {
			out.UnmatchedCommits = append(out.UnmatchedCommits, importUnmatchedJSON{
				CommitSHA: c.SHA, Subject: c.Subject, CommittedAt: c.When,
			})
		}
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		return fmt.Errorf("encode import result: %w", err)
	}
	return nil
}

func importLinksJSON(links []agentimport.LinkResult) []importLinkJSON {
	out := make([]importLinkJSON, 0, len(links))
	for _, l := range links {
		out = append(out, importLinkJSON{
			CommitSHA:    l.CommitSHA,
			CheckpointID: l.CheckpointID.String(),
			SessionID:    l.SessionID,
			TurnUUID:     l.TurnUUID,
			Method:       l.Method,
			Action:       l.Action,
		})
	}
	return out
}

// writeReconcileReport renders the human-readable reconcile report: one line
// per link, candidate, and still-unlinked commit, then a count line. Plain
// text with no terminal dependency, so the same information reaches an agent
// reading a pipe.
func writeReconcileReport(w io.Writer, rep *agentimport.ReconcileReport) {
	if rep == nil {
		return
	}
	for _, l := range rep.Links {
		// The action is printed alongside the method so a reader can tell a
		// link this run wrote from one it merely re-confirmed.
		fmt.Fprintf(w, "linked %s <- checkpoint %s (session %s, %s, %s)\n",
			shortSHA(l.CommitSHA), l.CheckpointID, l.SessionID, l.Method, l.Action)
	}
	for _, l := range rep.Candidates {
		fmt.Fprintf(w, "candidate %s <- checkpoint %s (session %s, heuristic; rerun with --accept-heuristics)\n",
			shortSHA(l.CommitSHA), l.CheckpointID, l.SessionID)
	}
	for _, c := range rep.UnmatchedCommits {
		fmt.Fprintf(w, "unmatched %s %s\n", shortSHA(c.SHA), truncateSubject(c.Subject))
	}
	fmt.Fprintf(w, "Scanned %d commit(s) without session data: %d linked, %d candidate(s), %d unmatched.\n",
		rep.CommitsScanned, len(rep.Links), len(rep.Candidates), len(rep.UnmatchedCommits))
}

func truncateSubject(subject string) string {
	if len(subject) <= unmatchedSubjectLength {
		return subject
	}
	return subject[:unmatchedSubjectLength] + "…"
}
