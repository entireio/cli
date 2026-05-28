package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/api/repov1"
	"github.com/entireio/cli/internal/entiredb/client"
	"github.com/entireio/cli/internal/entiredb/internal/cliauth"
	"github.com/entireio/cli/internal/entiredb/internal/clidial"
)

// AttachContentCmds attaches every repo content verb directly to root.
// merge/rebase/revert accept --dry-run.
func AttachContentCmds(root *cobra.Command, cfg *cliauth.Config) {
	root.AddCommand(newCloneCmd(cfg))
	root.AddCommand(newGetFileCmd(cfg))
	root.AddCommand(newLsCmd(cfg))
	root.AddCommand(newMergeBaseCmd(cfg))
	root.AddCommand(newLogCmd(cfg))
	root.AddCommand(newFilesCmd(cfg))
	root.AddCommand(newCompareCmd(cfg))
	root.AddCommand(newDiffCmd(cfg))
	root.AddCommand(newBranchesCmd(cfg))
	root.AddCommand(newTagsCmd(cfg))
	root.AddCommand(newMergeCmd(cfg))
	root.AddCommand(newRebaseCmd(cfg))
	root.AddCommand(newRevertCmd(cfg))
}

// normaliseCloneURL accepts an entire:// URL or a schemeless URL (in
// which case entire:// is prepended). URLs with any other scheme are
// rejected — there's no fallback to https/git/ssh transports here.
func normaliseCloneURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("clone url is empty")
	}
	if strings.HasPrefix(s, "entire://") {
		return s, nil
	}
	if strings.Contains(s, "://") {
		return "", fmt.Errorf("invalid clone url %q: only entire:// is supported", s)
	}
	return "entire://" + s, nil
}

func newCloneCmd(_ *cliauth.Config) *cobra.Command {
	return &cobra.Command{
		Use:     "clone <url>",
		Short:   "Clone a repository",
		Long:    "Clones a repository by execing `git clone <url>`.",
		Example: "  entire-repo clone entire://aws-us-east-2.entire.io/et/widgets/web",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote, err := normaliseCloneURL(args[0])
			if err != nil {
				return err
			}
			// #nosec G204 -- remote is a normalised entire:// URL, passed as an arg to git (not a shell)
			gitCmd := exec.CommandContext(cmd.Context(), "git", "clone", remote)
			gitCmd.Stdin = os.Stdin
			gitCmd.Stdout = os.Stdout
			gitCmd.Stderr = os.Stderr
			if err := gitCmd.Run(); err != nil {
				return fmt.Errorf("git clone %s: %w", remote, err)
			}

			// For empty repos, git initializes the local repo but doesn't configure
			// the remote tracking or default branch, so we set that up.
			dir := strings.TrimSuffix(path.Base(strings.TrimSuffix(remote, "/")), ".git")
			r, err := git.PlainOpen(dir)
			if err != nil {
				return fmt.Errorf("failed to open cloned repository: %w", err)
			}
			defer func() { _ = r.Close() }()
			head, err := r.Head()
			if err != nil || head == nil {
				if err := setupRepository(r, remote); err != nil {
					return fmt.Errorf("failed to setup local repository: %w", err)
				}
			}
			return nil
		},
	}
}

func setupRepository(r *git.Repository, originURL string) error {
	cfg, err := r.Config()
	if err != nil {
		return fmt.Errorf("failed to get repository config: %w", err)
	}
	cfg.Remotes["origin"] = &config.RemoteConfig{
		Name: "origin",
		URLs: []string{originURL},
	}
	cfg.Branches["main"] = &config.Branch{
		Name:   "main",
		Remote: "origin",
		Merge:  plumbing.Main,
	}
	err = r.Storer.SetConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to set repository config: %w", err)
	}
	// for now we default to main, make this configurable later
	defaultBranch := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.Main)
	err = r.Storer.CheckAndSetReference(defaultBranch, nil)
	if err != nil {
		return fmt.Errorf("failed to set default branch: %w", err)
	}
	return nil
}

func newGetFileCmd(cfg *cliauth.Config) *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "get-file <cluster>/et/<org>/<repo> <path>",
		Short: "Retrieve a file from a repository",
		Long: "Streams the raw bytes of <path> at --ref (default main) to " +
			"stdout. Errors if the ref or path doesn't exist, or the " +
			"server rejects the file as too large.",
		Example: "  # Read a file at the tip of main\n" +
			"  entire-repo get-file aws-us-east-2.entire.io/et/alice/widgets README.md\n\n" +
			"  # Read a file at a tag, piping into another tool\n" +
			"  entire-repo get-file aws-us-east-2.entire.io/et/alice/widgets src/main.go --ref v1.2.0 | grep TODO",
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				_, err := c.RepoGetRawFile(cmd.Context(), repoID, &repov1.RawFileRequest{
					Ref: ref, Path: args[1],
				}, os.Stdout)
				if err != nil {
					return fmt.Errorf("get-file: %w", err)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "main", "branch, tag, or commit hash")
	return cmd
}

func newLsCmd(cfg *cliauth.Config) *cobra.Command {
	var ref, path string
	var recursive bool
	cmd := &cobra.Command{
		Use:   "ls <cluster>/et/<org>/<repo>",
		Short: "List files in a repository tree",
		Long: "Lists files (blobs) in the repository tree at --ref " +
			"(default main). Output is one path per line, sorted; " +
			"directory entries are filtered out. Use --path to scope to " +
			"a subtree. Recursive by default; pass --recursive=false for " +
			"a single-level listing.",
		Example: "  # All files in the repo at main\n" +
			"  entire-repo ls aws-us-east-2.entire.io/et/alice/widgets\n\n" +
			"  # Files under a subtree, at a tag\n" +
			"  entire-repo ls aws-us-east-2.entire.io/et/alice/widgets --path src --ref v1.2.0\n\n" +
			"  # Single-level (top of tree only)\n" +
			"  entire-repo ls aws-us-east-2.entire.io/et/alice/widgets --recursive=false",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				resp, err := c.RepoGetTree(cmd.Context(), repoID, &repov1.GetTreeRequest{
					Ref: ref, Path: path, Recursive: recursive,
				})
				if err != nil {
					return fmt.Errorf("ls: %w", err)
				}
				paths := make([]string, 0, len(resp.Entries))
				for _, e := range resp.Entries {
					if e.Type == repov1.TreeEntryTypeBlob {
						paths = append(paths, e.Path)
					}
				}
				slices.Sort(paths)
				for _, p := range paths {
					fmt.Println(p)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "main", "branch, tag, or commit hash")
	cmd.Flags().StringVar(&path, "path", "", "subtree path")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", true, "list descendants recursively")
	return cmd
}

func newMergeBaseCmd(cfg *cliauth.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge-base <cluster>/et/<org>/<repo> <commit-a> <commit-b>",
		Short: "Find the merge base of two commits",
		Long: "Find the common ancestor of two commits. Each argument may " +
			"be a branch, tag, or commit hash. Prints the resulting " +
			"commit SHA on its own line.",
		Example: "  # Merge base of two branches\n" +
			"  entire-repo merge-base aws-us-east-2.entire.io/et/alice/widgets main feature-x",
		Args: usageArgs(cobra.ExactArgs(3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				mb, err := c.RepoMergeBase(cmd.Context(), repoID, args[1], args[2])
				if err != nil {
					return fmt.Errorf("merge base: %w", err)
				}
				fmt.Println(mb)
				return nil
			})
		},
	}
	return cmd
}

func newLogCmd(cfg *cliauth.Config) *cobra.Command {
	var (
		ref, notRef, path, author  string
		since, until               string
		firstParent, parseTrailers bool
		pageSize                   int
		pageToken                  string
		jsonOut                    bool
	)
	cmd := &cobra.Command{
		Use:   "log <cluster>/et/<org>/<repo>",
		Short: "List commits reachable from a ref",
		Long:  "Walk commit history newest-first. Mirrors git log with support for date/path/author filters and trailer parsing.",
		Example: "  # Last 50 commits on main\n" +
			"  entire-repo log aws-us-east-2.entire.io/et/alice/widgets\n\n" +
			"  # Commits on a feature branch not yet on main\n" +
			"  entire-repo log aws-us-east-2.entire.io/et/alice/widgets --ref feature-x --not-ref main\n\n" +
			"  # Commits touching a path, by a specific author, in a date window\n" +
			"  entire-repo log aws-us-east-2.entire.io/et/alice/widgets --path src/auth --author alice --since 2026-01-01T00:00:00Z",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &repov1.ListCommitsRequest{
				Ref: ref, NotRef: notRef, Path: path, Author: author, PageToken: pageToken,
				FirstParent: firstParent, ParseTrailers: parseTrailers,
				PageSize: int32(pageSize),
			}
			if t, ok, err := parseOptTime(since); err != nil {
				return fmt.Errorf("--since: %w", err)
			} else if ok {
				req.Since = &t
			}
			if t, ok, err := parseOptTime(until); err != nil {
				return fmt.Errorf("--until: %w", err)
			} else if ok {
				req.Until = &t
			}

			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				resp, err := c.RepoListCommits(cmd.Context(), repoID, req)
				if err != nil {
					return fmt.Errorf("log: %w", err)
				}
				if jsonOut {
					return writeJSON(os.Stdout, resp)
				}
				for i := range resp.Commits {
					printCommit(&resp.Commits[i], parseTrailers)
				}
				if resp.NextPageToken != "" {
					fmt.Fprintf(os.Stderr, "next page: --page-token %s\n", resp.NextPageToken)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "main", "branch, tag, or commit hash")
	cmd.Flags().StringVar(&notRef, "not-ref", "", "exclude commits reachable from this ref")
	cmd.Flags().StringVar(&path, "path", "", "only commits touching this path")
	cmd.Flags().StringVar(&author, "author", "", "substring match on author name or email")
	cmd.Flags().StringVar(&since, "since", "", "only commits at or after this RFC 3339 timestamp")
	cmd.Flags().StringVar(&until, "until", "", "only commits before this RFC 3339 timestamp")
	cmd.Flags().BoolVar(&firstParent, "first-parent", false, "follow only the first parent of merges")
	cmd.Flags().BoolVar(&parseTrailers, "parse-trailers", false, "parse and print git trailers")
	cmd.Flags().IntVar(&pageSize, "page-size", 0, "commits per page (0 = server default)")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "cursor from a previous response")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw JSON response")
	return cmd
}

// fileJSON is the JSON shape emitted by `repo files`. Field names are
// lowerCamelCase; bytes are base64-encoded by encoding/json and size remains
// a string for CLI output stability.
type fileJSON struct {
	Path     string `json:"path,omitempty"`
	Content  []byte `json:"content,omitempty"`
	Sha      string `json:"sha,omitempty"`
	Size     string `json:"size,omitempty"`
	NotFound bool   `json:"notFound,omitempty"`
	TooLarge bool   `json:"tooLarge,omitempty"`
}

func newFilesCmd(cfg *cliauth.Config) *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "files <cluster>/et/<org>/<repo> <path>...",
		Short: "Batch read files at a ref",
		Long: "Reads multiple files and emits a JSON object with a `files` " +
			"array, one entry per requested path. Each entry carries " +
			"`path`, `content` (base64), `sha`, and `size`, or a " +
			"`notFound`/`tooLarge` flag. Use this when scripting over " +
			"several files; for a single file's raw bytes, use `repo " +
			"get-file`.",
		Example: "  # Read three files at the tip of main\n" +
			"  entire-repo files aws-us-east-2.entire.io/et/alice/widgets README.md src/main.go go.mod\n\n" +
			"  # Same, at a tag\n" +
			"  entire-repo files aws-us-east-2.entire.io/et/alice/widgets README.md --ref v1.2.0",
		Args: usageArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				var files []fileJSON
				err := c.RepoGetFiles(cmd.Context(), repoID, &repov1.GetFilesRequest{
					Ref: ref, Paths: args[1:],
				}, func(h *repov1.FileHeader, content io.Reader) error {
					f := fileJSON{
						Path:     h.Path,
						Sha:      h.SHA,
						NotFound: h.Status == repov1.FileStatusNotFound,
						TooLarge: h.Status == repov1.FileStatusTooLarge,
					}
					if h.Size != 0 {
						f.Size = strconv.FormatInt(h.Size, 10)
					}
					if h.Status == repov1.FileStatusOK {
						data, err := io.ReadAll(content)
						if err != nil {
							return fmt.Errorf("reading file %s: %w", h.Path, err)
						}
						f.Content = data
					}
					files = append(files, f)
					return nil
				})
				if err != nil {
					return fmt.Errorf("files: %w", err)
				}
				out, err := json.MarshalIndent(struct {
					Files []fileJSON `json:"files"`
				}{Files: files}, "", "  ")
				if err != nil {
					return fmt.Errorf("encoding response: %w", err)
				}
				if _, err := os.Stdout.Write(append(out, '\n')); err != nil {
					return fmt.Errorf("writing output: %w", err)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "main", "branch, tag, or commit hash")
	return cmd
}

func newCompareCmd(cfg *cliauth.Config) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "compare <cluster>/et/<org>/<repo> <base> <head>",
		Short: "Per-file diff between two commits, as JSON",
		Long: "Compare <base> against <head>; each may be a branch, tag, " +
			"or commit hash. Emits a JSON object whose `files` array " +
			"describes the changed files. For raw unified diff output, " +
			"use `repo diff`.",
		Example: "  # File-level summary\n" +
			"  entire-repo compare aws-us-east-2.entire.io/et/alice/widgets main feature-x\n\n" +
			"  # Restrict to a subtree\n" +
			"  entire-repo compare aws-us-east-2.entire.io/et/alice/widgets main feature-x --path src/auth",
		Args: usageArgs(cobra.ExactArgs(3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				resp, err := c.RepoCompare(cmd.Context(), repoID, &repov1.CompareRequest{
					Base: args[1], Head: args[2], Path: path,
				})
				if err != nil {
					return fmt.Errorf("compare: %w", err)
				}
				return writeJSON(os.Stdout, resp)
			})
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "restrict to this path prefix")
	return cmd
}

func newDiffCmd(cfg *cliauth.Config) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "diff <cluster>/et/<org>/<repo> <base> <head>",
		Short: "Raw unified diff between two revisions",
		Long: "Streams a raw unified diff between <base> and <head> to " +
			"stdout. Each may be a branch, tag, or commit hash. Pass " +
			"--path to restrict to a path prefix. For per-file structured " +
			"output, use `repo compare`.",
		Example: "  # Whole-repo diff between two branches\n" +
			"  entire-repo diff aws-us-east-2.entire.io/et/alice/widgets main feature-x\n\n" +
			"  # Limit to a subtree\n" +
			"  entire-repo diff aws-us-east-2.entire.io/et/alice/widgets main feature-x --path src/auth",
		Args: usageArgs(cobra.ExactArgs(3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				err := c.RepoDiff(cmd.Context(), repoID, &repov1.DiffRequest{
					Base: args[1], Head: args[2], Path: path,
				}, os.Stdout)
				if errors.Is(err, client.ErrDiffTruncated) {
					fmt.Fprintln(os.Stderr, "warning: diff truncated at 100 MB")
					return nil
				}
				if err != nil {
					return fmt.Errorf("diff: %w", err)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "restrict to this path prefix")
	return cmd
}

type refListFlags struct {
	search, regex, pageToken string
	pageSize                 int
	jsonOut                  bool
}

func (f *refListFlags) bind(cmd *cobra.Command, plural string) {
	cmd.Flags().StringVar(&f.search, "search", "", "substring filter (^/$ anchors supported)")
	cmd.Flags().StringVar(&f.regex, "regex", "", "RE2 regex filter (mutually exclusive with --search)")
	cmd.Flags().IntVar(&f.pageSize, "page-size", 0, plural+" per page (0 = server default)")
	cmd.Flags().StringVar(&f.pageToken, "page-token", "", "cursor from a previous response")
	cmd.Flags().BoolVar(&f.jsonOut, "json", false, "emit raw JSON response")
}

func printRefList(refs []repov1.Ref, nextToken string) {
	for _, r := range refs {
		fmt.Printf("%s\t%s\n", r.CommitSHA, r.Name)
	}
	if nextToken != "" {
		fmt.Fprintf(os.Stderr, "next page: --page-token %s\n", nextToken)
	}
}

func newBranchesCmd(cfg *cliauth.Config) *cobra.Command {
	var f refListFlags
	cmd := &cobra.Command{
		Use:   "branches <cluster>/et/<org>/<repo>",
		Short: "List branches",
		Long: "List branches. --search does a case-insensitive substring " +
			"match; prefix with `^` or suffix with `$` to anchor. Use " +
			"--regex (RE2) for richer filters; the two flags are mutually " +
			"exclusive.\n\n" +
			"Output is tab-separated commit SHA and ref name; pass --json " +
			"for the full JSON response. When more results remain, a " +
			"next-page cursor is printed to stderr.",
		Example: "  # All branches\n" +
			"  entire-repo branches aws-us-east-2.entire.io/et/alice/widgets\n\n" +
			"  # Branches starting with \"release/\"\n" +
			"  entire-repo branches aws-us-east-2.entire.io/et/alice/widgets --search '^release/'\n\n" +
			"  # Regex filter\n" +
			"  entire-repo branches aws-us-east-2.entire.io/et/alice/widgets --regex '^v\\d+\\.\\d+'",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				resp, err := c.RepoListBranches(cmd.Context(), repoID, &repov1.ListBranchesRequest{
					Search: f.search, Regex: f.regex,
					PageToken: f.pageToken,
					PageSize:  int32(f.pageSize),
				})
				if err != nil {
					return fmt.Errorf("branches: %w", err)
				}
				if f.jsonOut {
					return writeJSON(os.Stdout, resp)
				}
				printRefList(resp.Branches, resp.NextPageToken)
				return nil
			})
		},
	}
	f.bind(cmd, "branches")
	return cmd
}

func newTagsCmd(cfg *cliauth.Config) *cobra.Command {
	var f refListFlags
	cmd := &cobra.Command{
		Use:   "tags <cluster>/et/<org>/<repo>",
		Short: "List tags",
		Long: "List tags. --search does a case-insensitive substring " +
			"match; prefix with `^` or suffix with `$` to anchor. Use " +
			"--regex (RE2) for richer filters; the two flags are mutually " +
			"exclusive.\n\n" +
			"Output is tab-separated commit SHA and ref name; pass --json " +
			"for the full JSON response. When more results remain, a " +
			"next-page cursor is printed to stderr.",
		Example: "  # All tags\n" +
			"  entire-repo tags aws-us-east-2.entire.io/et/alice/widgets\n\n" +
			"  # Tags starting with \"v1.\"\n" +
			"  entire-repo tags aws-us-east-2.entire.io/et/alice/widgets --search '^v1.'",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				resp, err := c.RepoListTags(cmd.Context(), repoID, &repov1.ListTagsRequest{
					Search: f.search, Regex: f.regex,
					PageToken: f.pageToken,
					PageSize:  int32(f.pageSize),
				})
				if err != nil {
					return fmt.Errorf("tags: %w", err)
				}
				if f.jsonOut {
					return writeJSON(os.Stdout, resp)
				}
				printRefList(resp.Tags, resp.NextPageToken)
				return nil
			})
		},
	}
	f.bind(cmd, "tags")
	return cmd
}

func printCommit(c *repov1.Commit, showTrailers bool) {
	sha := c.SHA
	if len(sha) >= 12 {
		sha = sha[:12]
	}
	when := c.Author.Date.Format(time.RFC3339)
	who := c.Author.Name
	title, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
	fmt.Printf("%s  %s  %s  %s\n", sha, when, who, title)
	if !showTrailers || len(c.Trailers) == 0 {
		return
	}
	for _, tr := range c.Trailers {
		fmt.Printf("    %s: %s\n", tr.Key, tr.Value)
	}
}

func writeJSON(w io.Writer, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding response: %w", err)
	}
	if _, err := w.Write(append(out, '\n')); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

func parseOptTime(s string) (time.Time, bool, error) {
	if s == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("invalid RFC 3339 timestamp: %w", err)
	}
	return t, true, nil
}

// parseMergeStrategy maps the CLI --strategy flag to the API enum. Unknown
// strategies are rejected here so we fail fast before the HTTP round-trip.
func parseMergeStrategy(s string) (repov1.MergeStrategy, error) {
	switch s {
	case "merge":
		return repov1.MergeStrategyMerge, nil
	case "no-ff":
		return repov1.MergeStrategyNoFastForward, nil
	case "ff-only":
		return repov1.MergeStrategyFastForwardOnly, nil
	case "squash":
		return repov1.MergeStrategySquash, nil
	default:
		return "", fmt.Errorf("unknown strategy %q (want one of: merge, no-ff, ff-only, squash)", s)
	}
}

// shortSHA returns the first 12 hex chars of sha, or sha itself if shorter.
// Mirrors the git default short-hash length used by printCommit.
func shortSHA(sha string) string {
	if len(sha) >= 12 {
		return sha[:12]
	}
	return sha
}

// snapshotRefTip asks the server to resolve refName to its tip SHA so callers
// can pass it as expected_*_sha to a follow-up mutating RPC. Implemented on
// top of MergeBase (which requires two refs) by passing refName twice; the
// resolved_shas list then contains the same tip at both positions.
func snapshotRefTip(ctx context.Context, c *client.Client, repoID, refName string) (string, error) {
	resp, err := c.RepoMergeBaseFull(ctx, repoID, refName, refName)
	if err != nil {
		return "", fmt.Errorf("snapshot ref tip %q: %w", refName, err)
	}
	resolved := resp.ResolvedSHAs
	if len(resolved) == 0 {
		// Server hasn't populated resolved_shas; leave expected_*_sha
		// empty and let the server decide whether to enforce CAS.
		return "", nil
	}
	return resolved[0], nil
}

func newMergeCmd(cfg *cliauth.Config) *cobra.Command {
	var (
		into     string
		strategy string
		message  string
		jsonOut  bool
		dryRun   bool
	)
	cmd := &cobra.Command{
		Use:   "merge <cluster>/et/<org>/<repo> <head>",
		Short: "Merge a head ref into a base branch (or preview with --dry-run)",
		Long: "Server-side merge of <head> into the branch given by --into. " +
			"--strategy picks the shape: `merge` (default; fast-forward when " +
			"possible, otherwise merge commit), `no-ff` (always create a " +
			"merge commit), `ff-only` (fail unless fast-forward), or " +
			"`squash`. --message overrides the merge/squash commit body; " +
			"empty lets the server generate a git-merge-style default.\n\n" +
			"Optimistic CAS: the CLI snapshots the current tips of base and " +
			"head and sends them as expected_*_sha so a concurrent push " +
			"that moves either ref fails the merge instead of clobbering " +
			"it. On conflict the command exits nonzero and prints the " +
			"conflicted paths.\n\n" +
			"--dry-run previews the outcome (would-fast-forward, would-merge, " +
			"would-squash, would-conflict, already-up-to-date) without " +
			"mutating refs and always exits zero. --message is ignored in " +
			"dry-run mode.",
		Example: "  # Default merge into main\n" +
			"  entire-repo merge aws-us-east-2.entire.io/et/alice/widgets feature-x --into main\n\n" +
			"  # Will this branch merge cleanly?\n" +
			"  entire-repo merge aws-us-east-2.entire.io/et/alice/widgets feature-x --into main --dry-run\n\n" +
			"  # Reject anything that isn't fast-forwardable\n" +
			"  entire-repo merge aws-us-east-2.entire.io/et/alice/widgets feature-x --into main --strategy ff-only\n\n" +
			"  # Squash with a custom commit message\n" +
			"  entire-repo merge aws-us-east-2.entire.io/et/alice/widgets feature-x --into main \\\n" +
			"      --strategy squash --message \"Add widget endpoint\"",
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			start, err := parseMergeStrategy(strategy)
			if err != nil {
				return err
			}
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				if dryRun {
					resp, err := c.RepoDryRunMerge(cmd.Context(), repoID, &repov1.DryRunMergeRequest{
						BaseRef:  into,
						HeadRef:  args[1],
						Strategy: start,
					})
					if err != nil {
						return fmt.Errorf("dry-run merge: %w", err)
					}
					if jsonOut {
						return writeJSON(os.Stdout, resp)
					}
					return printDryRunMergeOutcome(os.Stdout, resp, args[1], into)
				}
				expectedBase, err := snapshotRefTip(cmd.Context(), c, repoID, into)
				if err != nil {
					return err
				}
				expectedHead, err := snapshotRefTip(cmd.Context(), c, repoID, args[1])
				if err != nil {
					return err
				}
				resp, err := c.RepoMerge(cmd.Context(), repoID, &repov1.MergeRequest{
					BaseRef:         into,
					HeadRef:         args[1],
					Strategy:        start,
					ExpectedBaseSHA: expectedBase,
					ExpectedHeadSHA: expectedHead,
					CommitMessage:   message,
				})
				if err != nil {
					return fmt.Errorf("merge: %w", err)
				}
				if jsonOut {
					return writeJSON(os.Stdout, resp)
				}
				return printMergeOutcome(os.Stdout, resp, args[1], into)
			})
		},
	}
	cmd.Flags().StringVar(&into, "into", "", "target base branch (required)")
	cmd.Flags().StringVar(&strategy, "strategy", "merge", "merge strategy: merge, no-ff, ff-only, squash")
	cmd.Flags().StringVar(&message, "message", "", "override the merge/squash commit body (empty = server generates a git-merge-style default)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw JSON response")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview outcome without mutating refs")
	if err := cmd.MarkFlagRequired("into"); err != nil {
		panic(fmt.Sprintf("failed to mark --into required: %v", err))
	}
	return cmd
}

func printMergeOutcome(w io.Writer, resp *repov1.MergeResponse, head, base string) error {
	switch resp.Outcome {
	case repov1.MergeOutcomeConflict:
		paths := resp.ConflictedPaths
		fmt.Fprintf(w, "Conflict on %d file(s) [%s]: %s\n", len(paths), resp.ConflictReason, strings.Join(paths, ", "))
		return errConflict
	case repov1.MergeOutcomeAlreadyUpToDate:
		fmt.Fprintf(w, "Already up-to-date; %s already contains %s\n", base, head)
	case repov1.MergeOutcomeFastForward:
		fmt.Fprintf(w, "Fast-forwarded %s to %s (tip %s)\n", base, head, shortSHA(resp.NewBaseSHA))
	case repov1.MergeOutcomeMergeCommit:
		fmt.Fprintf(w, "Merged %s into %s (merge commit %s, new tip %s)\n", head, base, shortSHA(resp.MergeCommitSHA), shortSHA(resp.NewBaseSHA))
	case repov1.MergeOutcomeSquash:
		fmt.Fprintf(w, "Squashed %s into %s (commit %s, new tip %s)\n", head, base, shortSHA(resp.MergeCommitSHA), shortSHA(resp.NewBaseSHA))
	default:
		fmt.Fprintf(w, "Merge finished with unexpected outcome %s\n", resp.Outcome)
	}
	return nil
}

// errConflict signals that the operation completed with a conflict outcome.
// The CLI surfaces this as a nonzero exit without wrapping it in a fmt error
// to keep the message clean (cobra prints the error by default).
var errConflict = errors.New("conflict")

func printDryRunMergeOutcome(w io.Writer, resp *repov1.DryRunMergeResponse, head, base string) error {
	switch resp.Outcome {
	case repov1.MergeOutcomeConflict:
		paths := resp.ConflictedPaths
		fmt.Fprintf(w, "Would conflict on %d file(s) [%s]: %s\n", len(paths), resp.ConflictReason, strings.Join(paths, ", "))
	case repov1.MergeOutcomeAlreadyUpToDate:
		fmt.Fprintf(w, "Already up-to-date; %s already contains %s\n", base, head)
	case repov1.MergeOutcomeFastForward:
		fmt.Fprintf(w, "Would fast-forward %s to %s\n", base, head)
	case repov1.MergeOutcomeMergeCommit:
		fmt.Fprintf(w, "Would merge %s into %s via merge commit\n", head, base)
	case repov1.MergeOutcomeSquash:
		fmt.Fprintf(w, "Would squash %s into %s\n", head, base)
	default:
		fmt.Fprintf(w, "Dry-run outcome: %s\n", resp.Outcome)
	}
	return nil
}

func newRebaseCmd(cfg *cliauth.Config) *cobra.Command {
	var (
		onto     string
		upstream string
		jsonOut  bool
		dryRun   bool
	)
	cmd := &cobra.Command{
		Use:   "rebase <cluster>/et/<org>/<repo> <branch>",
		Short: "Rebase a branch onto another ref (or preview with --dry-run)",
		Long: "Server-side rebase of <branch> onto --onto. --upstream " +
			"defines the boundary (commits to replay are those reachable " +
			"from <branch> but not <upstream>); when omitted the server " +
			"uses merge-base(branch, onto). The CLI snapshots the current " +
			"tips of branch and onto for optimistic CAS.\n\n" +
			"Per-commit progress streams to stderr (`pick <old> -> <new>`, " +
			"`drop <old>`). If the server reports a conflict it does not " +
			"move the branch; the CLI drains the rest of the stream, " +
			"prints the conflicted paths, and exits nonzero. Successful " +
			"rebases print the new branch tip.\n\n" +
			"--dry-run previews the per-commit outcome (pick/drop/would-" +
			"conflict) without mutating refs and always exits zero.",
		Example: "  # Rebase feature-x onto current main\n" +
			"  entire-repo rebase aws-us-east-2.entire.io/et/alice/widgets feature-x --onto main\n\n" +
			"  # Will the rebase apply cleanly?\n" +
			"  entire-repo rebase aws-us-east-2.entire.io/et/alice/widgets feature-x --onto main --dry-run\n\n" +
			"  # Rebase only the commits since the explicit boundary\n" +
			"  entire-repo rebase aws-us-east-2.entire.io/et/alice/widgets feature-x --onto main --upstream origin/feature-x",
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				if dryRun {
					resp, err := c.RepoDryRunRebase(cmd.Context(), repoID, &repov1.DryRunRebaseRequest{
						Branch:   args[1],
						Onto:     onto,
						Upstream: upstream,
					})
					if err != nil {
						return fmt.Errorf("dry-run rebase: %w", err)
					}
					if jsonOut {
						return writeJSON(os.Stdout, resp)
					}
					printDryRunRebaseOutcome(os.Stdout, resp, args[1], onto)
					return nil
				}
				expectedBranch, err := snapshotRefTip(cmd.Context(), c, repoID, args[1])
				if err != nil {
					return err
				}
				expectedOnto, err := snapshotRefTip(cmd.Context(), c, repoID, onto)
				if err != nil {
					return err
				}
				req := &repov1.RebaseRequest{
					Branch:            args[1],
					Onto:              onto,
					Upstream:          upstream,
					ExpectedBranchSHA: expectedBranch,
					ExpectedOntoSHA:   expectedOnto,
				}
				stream, err := c.RepoRebase(cmd.Context(), repoID, req)
				if err != nil {
					return fmt.Errorf("rebase: %w", err)
				}
				defer func() { _ = stream.Close() }()
				return consumeRebaseStream(stream, req, jsonOut)
			})
		},
	}
	cmd.Flags().StringVar(&onto, "onto", "", "new base ref (required)")
	cmd.Flags().StringVar(&upstream, "upstream", "", "boundary ref; defaults to merge-base(branch, onto)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw JSON array of events")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview outcome without mutating refs")
	if err := cmd.MarkFlagRequired("onto"); err != nil {
		panic(fmt.Sprintf("failed to mark --onto required: %v", err))
	}
	return cmd
}

// consumeRebaseStream drains events from stream and renders progress. In text
// mode each event is printed as it arrives; in JSON mode all events are
// collected and emitted as a JSON array at the end. A terminal
// RebaseConflict returns errConflict so the CLI exits nonzero.
func consumeRebaseStream(stream *client.RepoRebaseStream, req *repov1.RebaseRequest, jsonOut bool) error {
	var collected []repov1.RebaseEvent
	var terminalConflict bool
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("rebase stream: %w", err)
		}
		if jsonOut {
			collected = append(collected, *ev)
			continue
		}
		if err := renderRebaseEvent(os.Stderr, ev, req); err != nil {
			return err
		}
		if ev.Type == repov1.RebaseEventTypeConflict {
			terminalConflict = true
		}
	}
	if jsonOut {
		if err := writeRebaseEventsJSON(os.Stdout, collected); err != nil {
			return err
		}
		for _, ev := range collected {
			if ev.Type == repov1.RebaseEventTypeConflict {
				return errConflict
			}
		}
		return nil
	}
	if terminalConflict {
		return errConflict
	}
	return nil
}

func renderRebaseEvent(w io.Writer, ev *repov1.RebaseEvent, req *repov1.RebaseRequest) error {
	switch ev.Type {
	case repov1.RebaseEventTypeStarted:
		fmt.Fprintf(w, "Rebasing %d commit(s) onto %s...\n", len(ev.CommitsToApply), req.Onto)
	case repov1.RebaseEventTypeApplied:
		if ev.Dropped {
			fmt.Fprintf(w, "  drop %s\n", shortSHA(ev.OriginalSHA))
		} else {
			fmt.Fprintf(w, "  pick %s -> %s\n", shortSHA(ev.OriginalSHA), shortSHA(ev.NewSHA))
		}
	case repov1.RebaseEventTypeConflict:
		fmt.Fprintf(w, "Conflict at %s [%s]: %s\n", shortSHA(ev.CommitSHA), ev.ConflictReason, strings.Join(ev.ConflictedPaths, ", "))
	case repov1.RebaseEventTypeCompleted:
		fmt.Fprintf(w, "Rebase complete. New tip: %s\n", shortSHA(ev.NewBranchSHA))
	default:
		enc, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("unknown rebase event and marshal failed: %w", err)
		}
		fmt.Fprintf(w, "[unknown rebase event] %s\n", enc)
	}
	return nil
}

func writeRebaseEventsJSON(w io.Writer, events []repov1.RebaseEvent) error {
	return writeJSON(w, events)
}

func printDryRunRebaseOutcome(w io.Writer, resp *repov1.DryRunRebaseResponse, branch, onto string) {
	verb := "Would rebase"
	if !resp.WouldSucceed {
		verb = "Would fail to rebase"
	}
	fmt.Fprintf(w, "%s %d commit(s) from %s onto %s (merge-base %s)\n", verb, len(resp.CommitsToApply), branch, onto, shortSHA(resp.MergeBaseSHA))
	for _, p := range resp.Previews {
		switch {
		case p.WouldBeDropped:
			fmt.Fprintf(w, "  drop %s\n", shortSHA(p.OriginalSHA))
		case p.WouldConflict:
			fmt.Fprintf(w, "  conflict %s [%s]: %s\n", shortSHA(p.OriginalSHA), p.ConflictReason, strings.Join(p.ConflictedPaths, ", "))
		default:
			fmt.Fprintf(w, "  pick %s\n", shortSHA(p.OriginalSHA))
		}
	}
}

func newRevertCmd(cfg *cliauth.Config) *cobra.Command {
	var (
		onBranch string
		mainline uint32
		jsonOut  bool
		dryRun   bool
	)
	cmd := &cobra.Command{
		Use:   "revert <cluster>/et/<org>/<repo> <commit>",
		Short: "Revert a commit on a branch (or preview with --dry-run)",
		Long: "Creates a new commit on --on that undoes <commit>. For " +
			"merge commits, --mainline selects the 1-indexed parent whose " +
			"changes are kept (required for merge commits, forbidden " +
			"otherwise). The CLI snapshots the current tip of --on for " +
			"optimistic CAS. On conflict the command exits nonzero without " +
			"moving the branch.\n\n" +
			"--dry-run reports whether the revert would succeed (and any " +
			"conflicted paths) without mutating refs and always exits zero.",
		Example: "  # Revert a regular commit on main\n" +
			"  entire-repo revert aws-us-east-2.entire.io/et/alice/widgets abc123def456 --on main\n\n" +
			"  # Will the revert apply cleanly?\n" +
			"  entire-repo revert aws-us-east-2.entire.io/et/alice/widgets abc123def456 --on main --dry-run\n\n" +
			"  # Revert a merge commit, keeping the first-parent line\n" +
			"  entire-repo revert aws-us-east-2.entire.io/et/alice/widgets abc123def456 --on main --mainline 1",
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, repoPath, err := clidial.ParseRepoArg(args[0])
			if err != nil {
				return err
			}
			return clidial.ConnectForRepo(*cfg, cluster, repoPath, func(c *client.Client, repoID string) error {
				if dryRun {
					resp, err := c.RepoDryRunRevert(cmd.Context(), repoID, &repov1.DryRunRevertRequest{
						CommitSHA: args[1],
						TargetRef: onBranch,
						Mainline:  mainline,
					})
					if err != nil {
						return fmt.Errorf("dry-run revert: %w", err)
					}
					if jsonOut {
						return writeJSON(os.Stdout, resp)
					}
					return printDryRunRevertOutcome(os.Stdout, resp, args[1], onBranch)
				}
				expectedTarget, err := snapshotRefTip(cmd.Context(), c, repoID, onBranch)
				if err != nil {
					return err
				}
				resp, err := c.RepoRevert(cmd.Context(), repoID, &repov1.RevertRequest{
					CommitSHA:         args[1],
					TargetRef:         onBranch,
					ExpectedTargetSHA: expectedTarget,
					Mainline:          mainline,
				})
				if err != nil {
					return fmt.Errorf("revert: %w", err)
				}
				if jsonOut {
					return writeJSON(os.Stdout, resp)
				}
				return printRevertOutcome(os.Stdout, resp, args[1], onBranch)
			})
		},
	}
	cmd.Flags().StringVar(&onBranch, "on", "", "target branch to revert the commit on (required)")
	cmd.Flags().Uint32Var(&mainline, "mainline", 0, "1-indexed parent number; required for merge commits, forbidden otherwise")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw JSON response")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview outcome without mutating refs")
	if err := cmd.MarkFlagRequired("on"); err != nil {
		panic(fmt.Sprintf("failed to mark --on required: %v", err))
	}
	return cmd
}

func printRevertOutcome(w io.Writer, resp *repov1.RevertResponse, commit, branch string) error {
	switch resp.Outcome {
	case repov1.RevertOutcomeConflict:
		paths := resp.ConflictedPaths
		fmt.Fprintf(w, "Conflict reverting %s on %s [%s]: %s\n", shortSHA(commit), branch, resp.ConflictReason, strings.Join(paths, ", "))
		return errConflict
	case repov1.RevertOutcomeAlreadyUpToDate:
		fmt.Fprintf(w, "Already reverted; %s on %s is a no-op\n", shortSHA(commit), branch)
	case repov1.RevertOutcomeReverted:
		fmt.Fprintf(w, "Reverted %s on %s (revert commit %s, new tip %s)\n", shortSHA(commit), branch, shortSHA(resp.RevertCommitSHA), shortSHA(resp.NewTipSHA))
	default:
		fmt.Fprintf(w, "Revert finished with unexpected outcome %s\n", resp.Outcome)
	}
	return nil
}

func printDryRunRevertOutcome(w io.Writer, resp *repov1.DryRunRevertResponse, commit, branch string) error {
	switch resp.Outcome {
	case repov1.RevertOutcomeConflict:
		paths := resp.ConflictedPaths
		fmt.Fprintf(w, "Would conflict reverting %s on %s [%s]: %s\n", shortSHA(commit), branch, resp.ConflictReason, strings.Join(paths, ", "))
	case repov1.RevertOutcomeAlreadyUpToDate:
		fmt.Fprintf(w, "Would be a no-op; %s already reverted on %s\n", shortSHA(commit), branch)
	case repov1.RevertOutcomeReverted:
		fmt.Fprintf(w, "Would revert %s on %s (target tip %s)\n", shortSHA(commit), branch, shortSHA(resp.ResolvedTargetSHA))
	default:
		fmt.Fprintf(w, "Dry-run outcome: %s\n", resp.Outcome)
	}
	return nil
}
