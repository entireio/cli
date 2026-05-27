package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type ChangedFile struct {
	Status  string `json:"status"`
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
}

func RepoRoot(ctx context.Context, cwd string) (string, error) {
	out, err := run(ctx, cwd, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func FirstParent(ctx context.Context, repo, rev string) (string, error) {
	out, err := run(ctx, repo, "git", "rev-parse", rev+"^")
	if err != nil {
		return "", fmt.Errorf("resolve first parent for %s: %w", rev, err)
	}
	return strings.TrimSpace(out), nil
}

func FindCommitWithCheckpoint(ctx context.Context, repo, checkpointID string) (string, error) {
	out, err := run(ctx, repo, "git", "log", "--all", "--format=%H", "-n", "1", "--grep=Entire-Checkpoint: "+checkpointID)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	if commit == "" {
		return "", fmt.Errorf("checkpoint %s has no associated commit in this repository", checkpointID)
	}
	return commit, nil
}

func ListFiles(ctx context.Context, repo, rev string) ([]string, error) {
	out, err := run(ctx, repo, "git", "ls-tree", "-r", "--name-only", rev)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func ChangedFiles(ctx context.Context, repo, base, head string, paths []string) ([]ChangedFile, error) {
	args := []string{"diff", "--name-status", "--find-renames", base, head, "--"}
	args = append(args, paths...)
	out, err := run(ctx, repo, "git", args...)
	if err != nil {
		return nil, err
	}

	var files []ChangedFile
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		switch {
		case strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C"):
			if len(fields) >= 3 {
				files = append(files, ChangedFile{Status: status[:1], OldPath: fields[1], Path: fields[2]})
			}
		default:
			files = append(files, ChangedFile{Status: status[:1], Path: fields[1]})
		}
	}
	return files, nil
}

func ShowFile(ctx context.Context, repo, rev, path string) (string, bool, error) {
	out, err := run(ctx, repo, "git", "show", rev+":"+path)
	if err != nil {
		if strings.Contains(err.Error(), "exists on disk, but not in") ||
			strings.Contains(err.Error(), "Path") ||
			strings.Contains(err.Error(), "does not exist") ||
			strings.Contains(err.Error(), "not found") {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
