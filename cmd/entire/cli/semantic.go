package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

const (
	semanticPluginBinary = "entire-sem"
	semanticDisableEnv   = "ENTIRE_SEM_DISABLE"
	semanticTimeout      = 8 * time.Second
)

var runSemanticPluginDiff = defaultRunSemanticPluginDiff

type semanticDiffResult struct {
	Checkpoint string               `json:"checkpoint,omitempty"`
	Base       string               `json:"base"`
	Head       string               `json:"head"`
	Files      []semanticFileChange `json:"files"`
}

type semanticFileChange struct {
	Path     string                 `json:"path"`
	OldPath  string                 `json:"old_path,omitempty"`
	Status   string                 `json:"status"`
	Language string                 `json:"language,omitempty"`
	Changes  []semanticEntityChange `json:"changes"`
}

type semanticEntityChange struct {
	Type            string  `json:"type"`
	Kind            string  `json:"kind"`
	Name            string  `json:"name"`
	OldName         string  `json:"old_name,omitempty"`
	NewName         string  `json:"new_name,omitempty"`
	OldSignature    string  `json:"old_signature,omitempty"`
	NewSignature    string  `json:"new_signature,omitempty"`
	BeforeStartLine int     `json:"before_start_line,omitempty"`
	AfterStartLine  int     `json:"after_start_line,omitempty"`
	DependentsCount int     `json:"dependents_count"`
	Similarity      float64 `json:"similarity,omitempty"`
}

func semanticDiffForCheckpointID(ctx context.Context, repo *git.Repository, cpID id.CheckpointID, searchAll bool) *semanticDiffResult {
	if os.Getenv(semanticDisableEnv) != "" {
		return nil
	}
	commits, err := getAssociatedCommits(ctx, repo, cpID, searchAll)
	if err != nil {
		logging.Debug(ctx, "semantic changes: associated commit lookup failed",
			slog.String("checkpoint_id", cpID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	result := semanticDiffForAssociatedCommits(ctx, repo, commits)
	if result != nil {
		result.Checkpoint = cpID.String()
	}
	return result
}

func semanticDiffForAssociatedCommits(ctx context.Context, repo *git.Repository, commits []associatedCommit) *semanticDiffResult {
	for _, commit := range commits {
		result := semanticDiffForCommit(ctx, repo, commit.SHA)
		if result != nil {
			return result
		}
	}
	return nil
}

func semanticDiffForRewindPoint(ctx context.Context, point strategy.RewindPoint) *semanticDiffResult {
	if os.Getenv(semanticDisableEnv) != "" {
		return nil
	}
	repo, err := openRepository(ctx)
	if err != nil {
		logging.Debug(ctx, "semantic changes: open repository failed", slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()

	if point.ID != "" {
		if result := semanticDiffForCommit(ctx, repo, point.ID); result != nil {
			return result
		}
	}
	if !point.CheckpointID.IsEmpty() {
		return semanticDiffForCheckpointID(ctx, repo, point.CheckpointID, false)
	}
	return nil
}

func semanticDiffForCommit(ctx context.Context, repo *git.Repository, sha string) *semanticDiffResult {
	if sha == "" {
		return nil
	}
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		logging.Debug(ctx, "semantic changes: commit lookup failed", slog.String("commit", sha), slog.String("error", err.Error()))
		return nil
	}
	parent, err := commit.Parent(0)
	if err != nil {
		logging.Debug(ctx, "semantic changes: commit has no first parent", slog.String("commit", sha), slog.String("error", err.Error()))
		return nil
	}
	result, err := runSemanticPluginDiff(ctx, parent.Hash.String(), commit.Hash.String())
	if err != nil {
		logging.Debug(ctx, "semantic changes: plugin diff failed", slog.String("commit", sha), slog.String("error", err.Error()))
		return nil
	}
	if result == nil || len(result.Files) == 0 {
		return nil
	}
	return result
}

func defaultRunSemanticPluginDiff(ctx context.Context, base, head string) (*semanticDiffResult, error) {
	pluginPath, ok := findSemanticPlugin()
	if !ok {
		return nil, nil
	}
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, err
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, semanticTimeout)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, pluginPath, "diff", "--base", base, "--head", head, "--json", "--repo", repoRoot)
	cmd.Dir = repoRoot
	cmd.Env = semanticPluginEnv(repoRoot)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("%s: %s", filepath.Base(pluginPath), detail)
	}

	var result semanticDiffResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse semantic plugin json: %w", err)
	}
	return &result, nil
}

func findSemanticPlugin() (string, bool) {
	if path, err := exec.LookPath(semanticPluginBinary); err == nil {
		return path, true
	}
	if dir, err := PluginBinDir(); err == nil {
		candidate := filepath.Join(dir, executableName(semanticPluginBinary))
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func executableName(name string) string {
	if runtime.GOOS == windowsGOOS && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return name + ".exe"
	}
	return name
}

func semanticPluginEnv(repoRoot string) []string {
	extras := []string{
		"ENTIRE_REPO_ROOT=" + repoRoot,
		"ENTIRE_CLI_VERSION=" + versioninfo.Version,
		semanticDisableEnv + "=1",
	}
	if dataDir, err := PluginDataDir("sem"); err == nil {
		extras = append(extras, pluginEnvPluginData+"="+dataDir)
	}
	return pluginEnv(os.Environ(), extras...)
}

func buildSemanticChangesMarkdown(result *semanticDiffResult) string {
	if result == nil || len(result.Files) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n## Semantic Changes\n\n")
	for _, file := range result.Files {
		if file.OldPath != "" && file.OldPath != file.Path {
			fmt.Fprintf(&sb, "`%s -> %s`", escapeInlineCodeText(file.OldPath), escapeInlineCodeText(file.Path))
		} else {
			fmt.Fprintf(&sb, "`%s`", escapeInlineCodeText(file.Path))
		}
		if file.Language != "" {
			fmt.Fprintf(&sb, " _%s_", escapeSummaryText(file.Language))
		}
		sb.WriteString("\n\n")
		for _, change := range file.Changes {
			fmt.Fprintf(&sb, "- %s\n", escapeSummaryText(semanticChangeText(change)))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func semanticChangeText(change semanticEntityChange) string {
	dependents := semanticDependentsSuffix(change)
	switch change.Type {
	case "added":
		return fmt.Sprintf("+ %s %s added", change.Kind, change.Name)
	case "removed":
		return fmt.Sprintf("- %s %s removed%s", change.Kind, change.Name, dependents)
	case "renamed":
		return fmt.Sprintf("~ %s %s renamed from %s%s", change.Kind, firstNonEmpty(change.NewName, change.Name), change.OldName, dependents)
	case "signature_changed":
		return fmt.Sprintf("~ %s %s signature changed%s", change.Kind, change.Name, dependents)
	case "body_changed":
		return fmt.Sprintf("~ %s %s body changed%s", change.Kind, change.Name, dependents)
	default:
		return fmt.Sprintf("~ %s %s changed%s", change.Kind, change.Name, dependents)
	}
}

func semanticDependentsSuffix(change semanticEntityChange) string {
	if change.Type == "added" {
		return ""
	}
	if change.DependentsCount == 1 {
		return " (1 dependent)"
	}
	return fmt.Sprintf(" (%d dependents)", change.DependentsCount)
}

func semanticPreviewLines(result *semanticDiffResult, limit int) []string {
	if result == nil || limit <= 0 {
		return nil
	}
	var lines []string
	for _, file := range result.Files {
		for _, change := range file.Changes {
			lines = append(lines, fmt.Sprintf("%s: %s", file.Path, semanticChangeText(change)))
			if len(lines) >= limit {
				return lines
			}
		}
	}
	return lines
}

func printSemanticRewindPreview(ctx context.Context, w io.Writer, point strategy.RewindPoint) {
	lines := semanticPreviewLines(semanticDiffForRewindPoint(ctx, point), 5)
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "\nSemantic changes:")
	for _, line := range lines {
		fmt.Fprintf(w, "  - %s\n", line)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
