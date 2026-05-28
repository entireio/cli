package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6"
	"github.com/spf13/pflag"
)

type runMode string

const (
	modePlan   runMode = "plan"
	modeList   runMode = "list"
	modeDryRun runMode = "dry-run"
	modeApply  runMode = "apply"
)

type options struct {
	repoPath string
	since    string
	head     string
	mode     runMode
	help     bool
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	if opts.help {
		printUsage(stdout)
		return nil
	}

	repoRoot, repo, err := openRepository(ctx, opts.repoPath)
	if err != nil {
		return err
	}
	ctx = settings.WithWorktreeRoot(ctx, repoRoot)

	if shouldEnsureCheckpointRefs(opts) {
		if err := ensureLatestV1Ref(ctx, repoRoot, repo); err != nil {
			return err
		}
		if err := ensureLatestV2Refs(ctx, repoRoot, repo); err != nil {
			return err
		}
	}

	checkpoints, v2OrphansSkipped, err := discoverCheckpointHistoryWithSkippedOrphans(ctx, repo, discoveryOptions{
		since: opts.since,
		head:  opts.head,
	})
	if err != nil {
		return err
	}
	writeDiscoveryWarnings(stdout, v2OrphansSkipped)

	switch opts.mode {
	case modeList:
		writeCheckpointList(stdout, checkpoints)
		return nil
	case modePlan, modeDryRun:
		report, err := migrateDiscoveredCheckpoints(ctx, repo, checkpoints, migrationOptions{apply: false})
		if err != nil {
			return err
		}
		writeMigrationReport(stdout, report, false)
		return nil
	case modeApply:
		report, err := migrateDiscoveredCheckpoints(ctx, repo, checkpoints, migrationOptions{apply: true})
		if err != nil {
			return err
		}
		writeMigrationReport(stdout, report, true)
		return nil
	default:
		return fmt.Errorf("unknown mode %q", opts.mode)
	}
}

func shouldEnsureCheckpointRefs(opts options) bool {
	return opts.mode == modePlan || opts.mode == modeDryRun || opts.mode == modeApply
}

func parseOptions(args []string) (options, error) {
	var opts options
	opts.mode = modePlan

	flags := pflag.NewFlagSet("migrate-v2-checkpoints", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var listMode bool
	var dryRun bool
	var apply bool
	flags.BoolVarP(&opts.help, "help", "h", false, "show help")
	flags.BoolVar(&listMode, "list", false, "print checkpoint IDs and associated commit IDs only")
	flags.BoolVar(&dryRun, "dry-run", false, "print the migration plan without writing refs")
	flags.BoolVar(&apply, "apply", false, "write migration commits")
	flags.StringVar(&opts.repoPath, "repo", "", "local repository path to inspect")
	flags.StringVar(&opts.since, "since", "", "commit before the checkpoints to inspect")
	flags.StringVar(&opts.head, "head", "", "limit scan to one history tip")

	if err := flags.Parse(args); err != nil {
		return opts, fmt.Errorf("parse options: %w", err)
	}

	positionals := flags.Args()
	if len(positionals) > 1 {
		return opts, fmt.Errorf("expected at most one since commit argument, got %d", len(positionals))
	}
	if len(positionals) == 1 {
		if opts.since != "" {
			return opts, errors.New("use either --since or positional since commit, not both")
		}
		opts.since = positionals[0]
	}

	modeCount := 0
	if listMode {
		opts.mode = modeList
		modeCount++
	}
	if dryRun {
		opts.mode = modeDryRun
		modeCount++
	}
	if apply {
		opts.mode = modeApply
		modeCount++
	}
	if modeCount > 1 {
		return opts, errors.New("use only one of --list, --dry-run, or --apply")
	}

	return opts, nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `migrate-v2-checkpoints migrates legacy v2 checkpoint data back to v1.

Usage:
  migrate-v2-checkpoints [OPTIONS] [SINCE_COMMIT]

Options:
  -h, --help       Show this help message
  --list           Print checkpoint IDs and associated commit IDs only
  --dry-run        Print the migration plan without writing refs
  --apply          Write migration commits
  --repo <path>    Local repository path to inspect
  --since <commit> Commit before the checkpoints to inspect
  --head <commit>  Limit scan to one history tip
`)
}

func openRepository(ctx context.Context, repoPath string) (string, *git.Repository, error) {
	if repoPath == "" {
		root, err := paths.WorktreeRoot(ctx)
		if err != nil {
			return "", nil, fmt.Errorf("find git worktree root: %w", err)
		}
		repoPath = root
	}

	// DetectDotGit walks up from a subdir to find the worktree root; then
	// re-open via gitrepo.OpenPath so shared clones with object alternates
	// resolve correctly.
	detector, err := git.PlainOpenWithOptions(repoPath, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", nil, fmt.Errorf("open repository %q: %w", repoPath, err)
	}
	defer detector.Close()
	repoRoot := repoPath
	if worktree, worktreeErr := detector.Worktree(); worktreeErr == nil {
		repoRoot = worktree.Filesystem().Root()
	}

	repo, err := gitrepo.OpenPath(repoRoot)
	if err != nil {
		return "", nil, fmt.Errorf("open repository %q: %w", repoRoot, err)
	}
	return repoRoot, repo, nil
}
