package ticket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

// nonSlugChars matches runs of characters that are not lowercase-alphanumeric.
var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases s and collapses non-alphanumeric runs into single dashes.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlugChars.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// defaultBranchName suggests a branch for a ticket: the platform's own
// suggestion when available (Linear's issue.branchName), else a
// "<gituser>/<ticket-id>-<title>" slug.
func defaultBranchName(ctx context.Context, task *Task, link Link) string {
	if task != nil && strings.TrimSpace(task.BranchName) != "" {
		return task.BranchName
	}
	slug := slugify(link.ID)
	if task != nil && task.Title != "" {
		if titleSlug := slugify(task.Title); titleSlug != "" {
			slug += "-" + titleSlug
		}
	}
	if user := gitUserSlug(ctx); user != "" {
		return user + "/" + slug
	}
	return slug
}

// gitUserSlug returns a slug of the git user.name's first token, or "".
func gitUserSlug(ctx context.Context) string {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return ""
	}
	out, err := gitexec.Run(ctx, root, "config", "user.name")
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(out)
	if i := strings.IndexAny(name, " \t"); i > 0 {
		name = name[:i]
	}
	return slugify(name)
}

// promptBranchName asks for a branch name, pre-filled with def.
func promptBranchName(ctx context.Context, def string) (string, error) {
	name := def
	form := uiform.New(huh.NewGroup(
		huh.NewInput().
			Title("Branch name").
			Description("New branch for this ticket. Edit or accept the default.").
			Value(&name),
	))
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", errors.New("ticket start canceled")
		}
		return "", fmt.Errorf("branch name prompt: %w", err)
	}
	return strings.TrimSpace(name), nil
}

// createOrSwitchBranch switches to name, creating it if it does not exist.
func createOrSwitchBranch(ctx context.Context, name string) error {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	if branchExists(ctx, root, name) {
		if _, err := gitexec.Run(ctx, root, "switch", name); err != nil {
			return fmt.Errorf("switch to branch %s: %w", name, err)
		}
		logging.Debug(ctx, "ticket branch switched", slog.String("branch", name))
		return nil
	}
	if _, err := gitexec.Run(ctx, root, "switch", "-c", name); err != nil {
		return fmt.Errorf("create branch %s: %w", name, err)
	}
	logging.Debug(ctx, "ticket branch created", slog.String("branch", name))
	return nil
}

// branchExists reports whether a local branch named name exists.
func branchExists(ctx context.Context, root, name string) bool {
	_, err := gitexec.Run(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}
