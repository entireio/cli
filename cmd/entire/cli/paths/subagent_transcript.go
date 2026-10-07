package paths

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// SubagentWorkflowsDirName is the directory under SubagentsDir where Claude
// Code keeps the transcripts of agents a Workflow launched, one subdirectory
// per run: <subagentsDir>/workflows/<runId>/agent-<agentID>.jsonl. Each run
// directory also holds an agent-<agentID>.meta.json per agent and a
// journal.jsonl, neither of which is a transcript.
const SubagentWorkflowsDirName = "workflows"

// ResolveSubagentTranscriptPath returns the path to an existing transcript of
// subagent agentID, or "" when none exists. The candidates, in order: the
// current layout (SubagentsDir), the legacy layout (agent-<id>.jsonl beside
// the main transcript), and a Workflow run directory under SubagentsDir.
//
// sessionID is the agent's own session ID (the transcript's basename). An
// agentID that is empty or not path-safe never resolves.
func ResolveSubagentTranscriptPath(transcriptDir, sessionID, agentID string) string {
	if agentID == "" || validation.ValidateAgentID(agentID) != nil {
		return ""
	}
	name := AgentTranscriptFileName(agentID)
	subagentsDir := SubagentsDir(transcriptDir, sessionID)
	if nested := filepath.Join(subagentsDir, name); pathExists(nested) {
		return nested
	}
	if legacy := filepath.Join(transcriptDir, name); pathExists(legacy) {
		return legacy
	}
	return WorkflowAgentTranscripts(subagentsDir)[agentID]
}

// WorkflowAgentTranscripts maps the ID of every Workflow agent with a
// transcript in any run under subagentsDir to that transcript's path. It
// serves lookups by agent ID, where the run is not known; see
// WorkflowRunAgentTranscripts for the rules a transcript must meet. An agent ID
// found in more than one run resolves as MergeWorkflowAgentTranscripts does.
// Symlinked run directories are skipped. A missing or unreadable directory
// yields an empty map: no Workflow ran, or none can be read.
//
// The reads here go straight to the agent's session store without a root, the
// read-side gap docs/development/filesystem-safety.md records under
// "Deliberately not rooted"; containment comes from the validated names.
func WorkflowAgentTranscripts(subagentsDir string) map[string]string {
	found := make(map[string]string)
	if subagentsDir == "" {
		return found
	}
	runs, err := os.ReadDir(filepath.Join(subagentsDir, SubagentWorkflowsDirName))
	if err != nil {
		return found
	}
	for _, run := range runs {
		// DirEntry types come from Lstat, so a symlinked run is not a dir here.
		if !run.IsDir() {
			continue
		}
		MergeWorkflowAgentTranscripts(found, WorkflowRunAgentTranscripts(subagentsDir, run.Name()))
	}
	return found
}

// MergeWorkflowAgentTranscripts adds src's agent transcripts to dst. An agent
// ID already in dst (the same agent in another run, as a resumed run can
// carry it) keeps whichever transcript was modified last, so the choice does
// not depend on the order runs are read in and the agent is counted once.
func MergeWorkflowAgentTranscripts(dst, src map[string]string) {
	for agentID, path := range src {
		if existing, ok := dst[agentID]; ok && !modifiedAfter(path, existing) {
			continue
		}
		dst[agentID] = path
	}
}

// modifiedAfter reports whether a was modified after b; a file that cannot be
// stat'ed never wins, and a tie keeps b.
func modifiedAfter(a, b string) bool {
	aInfo, err := os.Stat(a)
	if err != nil {
		return false
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		return true
	}
	return aInfo.ModTime().After(bInfo.ModTime())
}

// WorkflowRunAgentTranscripts maps the ID of every agent with a transcript in
// Workflow run runID under subagentsDir to that transcript's path. Only
// regular files named agent-<id>.jsonl, with a path-safe id, directly inside
// the run directory count, so neither a link nor the run's meta and journal
// files are taken for a transcript. A run ID that is not path-safe, or a run
// directory that is a link, missing, or unreadable, yields an empty map.
func WorkflowRunAgentTranscripts(subagentsDir, runID string) map[string]string {
	found := make(map[string]string)
	if subagentsDir == "" || validation.ValidateWorkflowRunID(runID) != nil {
		return found
	}
	runDir := filepath.Join(subagentsDir, SubagentWorkflowsDirName, runID)
	if info, err := os.Lstat(runDir); err != nil || !info.IsDir() {
		return found
	}
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return found
	}
	for _, entry := range entries {
		agentID, ok := workflowAgentIDFromFileName(entry.Name())
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		found[agentID] = filepath.Join(runDir, entry.Name())
	}
	return found
}

// workflowAgentIDFromFileName returns the agent ID in an agent-<id>.jsonl file
// name, or false when name is not one or the id is not path-safe.
func workflowAgentIDFromFileName(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "agent-")
	if !ok {
		return "", false
	}
	agentID, ok := strings.CutSuffix(rest, ".jsonl")
	if !ok || agentID == "" || validation.ValidateAgentID(agentID) != nil {
		return "", false
	}
	return agentID, true
}

// pathExists reports whether path names an existing file, following links the
// way the resolvers' earlier os.Stat checks did.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
