package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// managedScaffoldStatus is the outcome of writing an Entire-managed scaffold file
// (a search skill, an agent-help skill, ...). Statuses are shared so each
// feature's reporter can switch on them.
type managedScaffoldStatus string

const (
	managedScaffoldUnsupported     managedScaffoldStatus = "unsupported"
	managedScaffoldCreated         managedScaffoldStatus = "created"
	managedScaffoldUpdated         managedScaffoldStatus = "updated"
	managedScaffoldUnchanged       managedScaffoldStatus = "unchanged"
	managedScaffoldSkippedConflict managedScaffoldStatus = "skipped_conflict"
)

type managedScaffoldResult struct {
	Status  managedScaffoldStatus
	RelPath string
}

// writeManagedScaffold writes content to targetPath idempotently: it creates the
// file when absent, leaves it untouched (Unchanged) when identical, rewrites it
// (Updated) only when Entire already manages it, and refuses to clobber an
// unmanaged file (SkippedConflict). isManaged reports whether existing bytes
// carry this feature's management marker.
func writeManagedScaffold(targetPath, relPath string, content []byte, isManaged func([]byte) bool) (managedScaffoldResult, error) {
	existingData, err := os.ReadFile(targetPath) //nolint:gosec // target path is derived from repo root + fixed relative path
	if err == nil {
		if !isManaged(existingData) {
			return managedScaffoldResult{Status: managedScaffoldSkippedConflict, RelPath: relPath}, nil
		}
		if bytes.Equal(existingData, content) {
			return managedScaffoldResult{Status: managedScaffoldUnchanged, RelPath: relPath}, nil
		}
		if err := os.WriteFile(targetPath, content, 0o600); err != nil {
			return managedScaffoldResult{}, fmt.Errorf("update managed scaffold: %w", err)
		}
		return managedScaffoldResult{Status: managedScaffoldUpdated, RelPath: relPath}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return managedScaffoldResult{}, fmt.Errorf("read managed scaffold: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return managedScaffoldResult{}, fmt.Errorf("create managed scaffold directory: %w", err)
	}
	if err := os.WriteFile(targetPath, content, 0o600); err != nil {
		return managedScaffoldResult{}, fmt.Errorf("write managed scaffold: %w", err)
	}
	return managedScaffoldResult{Status: managedScaffoldCreated, RelPath: relPath}, nil
}

// setupOptionalSkillForNames installs an optional skill (search, agent-help, ...)
// for each named agent when enabled: it dedups names, resolves each agent, and
// runs install, joining any errors. Both the search and agent-help skills share
// this dedup/iterate/error-join plumbing; only the guard bool and the per-agent
// installer differ.
func setupOptionalSkillForNames(
	ctx context.Context,
	w io.Writer,
	names []string,
	enabled bool,
	install func(context.Context, io.Writer, agent.Agent, EnableOptions) error,
	opts EnableOptions,
) error {
	if !enabled {
		return nil
	}

	var errs []error
	seen := make(map[types.AgentName]struct{}, len(names))
	for _, name := range names {
		agentName := types.AgentName(name)
		if _, ok := seen[agentName]; ok {
			continue
		}
		seen[agentName] = struct{}{}

		ag, err := agent.Get(agentName)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get agent %s: %w", name, err))
			continue
		}
		if err := install(ctx, w, ag, opts); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// printSkillNonInteractiveNoAgentsGuidance prints the actionable message shown
// when an optional skill is requested in non-interactive mode but no agent is
// available to install it for. label is the human name ("search skill"), flag is
// the install flag name ("search-skill").
func printSkillNonInteractiveNoAgentsGuidance(w io.Writer, label, flag string) {
	fmt.Fprintf(w, "Cannot install the %s in non-interactive mode because no agents are enabled.\n", label)
	fmt.Fprintln(w, "Install it for a specific agent with:")
	fmt.Fprintf(w, "  entire enable --agent <name> --%s\n", flag)
	fmt.Fprintln(w, "or:")
	fmt.Fprintf(w, "  entire agent add <name> --%s\n", flag)
}
