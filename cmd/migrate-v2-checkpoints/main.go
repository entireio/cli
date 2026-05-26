package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/paths"

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

	_, repo, err := openRepository(ctx, opts.repoPath)
	if err != nil {
		return err
	}

	checkpoints, err := discoverCheckpointHistory(ctx, repo, discoveryOptions{
		since: opts.since,
		head:  opts.head,
	})
	if err != nil {
		return err
	}

	switch opts.mode {
	case modeList:
		writeCheckpointList(stdout, checkpoints)
		return nil
	case modePlan, modeDryRun:
		fmt.Fprintf(stdout, "Discovered %d checkpoint(s) with Entire-Checkpoint trailers.\n", len(checkpoints))
		fmt.Fprintln(stdout, "V2-to-v1 migration planning will be added in the next implementation step.")
		return nil
	case modeApply:
		return errors.New("--apply migration is not implemented yet")
	default:
		return fmt.Errorf("unknown mode %q", opts.mode)
	}
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

	repo, err := git.PlainOpenWithOptions(repoPath, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", nil, fmt.Errorf("open repository %q: %w", repoPath, err)
	}
	return repoPath, repo, nil
}
