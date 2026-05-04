package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	transcriptcompact "github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/redact"

	"charm.land/lipgloss/v2"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const defaultCheckpointSummaryTimeout = 30 * time.Second

const (
	pagerEnvVar       = "PAGER"
	lessEnvVar        = "LESS"
	lessPagerName     = "less"
	lessRawControlEnv = "LESS=-R"
	windowsGOOS       = "windows"
)

var checkpointSummaryTimeout = defaultCheckpointSummaryTimeout

var generateTranscriptSummary = summarize.GenerateFromTranscript

// errCannotGenerateTemporaryCheckpoint is returned by runExplainCheckpoint when
// --generate is requested for a target that does not match any committed
// checkpoint. runExplainAuto uses errors.Is to detect this case and fall back
// to resolving the target as a git commit ref.
var errCannotGenerateTemporaryCheckpoint = errors.New("cannot generate summary for temporary checkpoint")

type explainCheckpointLookup struct {
	repo                *git.Repository
	v1Store             *checkpoint.GitStore
	v2Store             *checkpoint.V2GitStore
	preferCheckpointsV2 bool
	committed           []checkpoint.CommittedInfo
}

// generateOrRawLabel returns the user-facing verb for the action the user
// requested, used in error messages when a commit target has no trailer.
func generateOrRawLabel(generate bool) string {
	if generate {
		return "generate summary"
	}
	return "show raw transcript"
}

// printNoTrailerMessage renders the friendly message shown when a resolved
// commit has no Entire-Checkpoint trailer in read-only modes. Takes the
// repo so the hash can be abbreviated to the minimum unique length for
// this repo's object set (matching git's --abbrev behavior).
func printNoTrailerMessage(w io.Writer, repo *git.Repository, hash plumbing.Hash) {
	styles := newStatusStyles(w)
	rows := []explainRow{
		{Label: "commit", Value: abbreviateCommitHash(repo, hash)},
		{Label: "reason", Value: "no Entire-Checkpoint trailer"},
		{Label: "hint", Value: "this commit was not created during an Entire session,"},
		{Label: "", Value: "or the trailer was removed"},
	}
	fmt.Fprint(w, styles.renderFailure("No associated Entire checkpoint", rows))
}

// errAmbiguousCommitPrefix is returned by resolveCommitUnambiguous when a
// hex prefix matches more than one commit. Callers use errors.Is to detect
// this case and surface the full wrapped message verbatim.
var errAmbiguousCommitPrefix = errors.New("ambiguous commit prefix")

// commitHashesWithPrefix enumerates all commit hashes in the repo whose
// SHA starts with the given hex prefix. Returns nil when the storer is not
// a *filesystem.Storage or the prefix isn't decodable as hex.
//
// Per PR review (discussion_r3113804961): the reviewer specifically
// suggested repo.Storer.(*filesystem.Storage).HashesWithPrefix followed by
// commit filtering. Using this primitive both in resolution (detect
// ambiguous user input) and in display (dynamically abbreviate shown
// hashes to the minimum unique length).
func commitHashesWithPrefix(repo *git.Repository, prefix string) []plumbing.Hash {
	s, ok := repo.Storer.(*filesystem.Storage)
	if !ok {
		return nil
	}
	// Truncate to even length for byte-aligned hex decoding.
	evenHex := prefix[:len(prefix)&^1]
	decoded, err := hex.DecodeString(evenHex)
	if err != nil || len(decoded) == 0 {
		return nil
	}
	candidates, err := s.HashesWithPrefix(decoded)
	if err != nil {
		return nil
	}
	var commits []plumbing.Hash
	for _, h := range candidates {
		// HashesWithPrefix matches on even byte boundaries; filter the
		// dangling nybble for odd-length prefixes.
		if len(evenHex) != len(prefix) && !strings.HasPrefix(h.String(), prefix) {
			continue
		}
		if _, err := repo.CommitObject(h); err != nil {
			continue
		}
		commits = append(commits, h)
	}
	return commits
}

// resolveCommitUnambiguous resolves a ref to a commit hash, returning
// errAmbiguousCommitPrefix (and the matching hashes) when a hex-prefix input
// matches more than one commit. go-git v6's ResolveRevision silently picks
// the first candidate in ambiguous cases (its source explicitly says "for
// speed purposes don't bother to detect the ambiguity"), which could pick
// the wrong commit. Non-hex refs (HEAD, branch names, HEAD~1) bypass the
// ambiguity check via commitHashesWithPrefix returning nil.
//
// The structured ambiguous return lets callers render a styled failure
// block (with each match's timestamp/session) without re-resolving the
// matches themselves.
func resolveCommitUnambiguous(repo *git.Repository, ref string) (plumbing.Hash, []plumbing.Hash, error) {
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return plumbing.ZeroHash, nil, err //nolint:wrapcheck // caller contextualizes
	}
	matches := commitHashesWithPrefix(repo, ref)
	if len(matches) <= 1 {
		return *hash, nil, nil
	}
	return plumbing.ZeroHash, matches, errAmbiguousCommitPrefix
}

// abbreviateCommitHash returns the shortest prefix of hash unique among
// commit objects in the repo, matching git's --abbrev-commit auto-growth
// so displayed short SHAs stay unambiguous as the repo grows. Falls back
// to a fixed 12-char prefix if the storer doesn't support fast prefix
// lookup, or to the full hash if somehow never unique.
func abbreviateCommitHash(repo *git.Repository, hash plumbing.Hash) string {
	full := hash.String()
	for length := 7; length < len(full); length++ {
		matches := commitHashesWithPrefix(repo, full[:length])
		if matches == nil {
			return full[:12]
		}
		if len(matches) <= 1 {
			return full[:length]
		}
	}
	return full
}

// interaction holds a single prompt and its responses for display.
type interaction struct {
	Prompt    string
	Responses []string // Multiple responses can occur between tool calls
	Files     []string
}

// associatedCommit holds information about a git commit associated with a checkpoint.
type associatedCommit struct {
	SHA      string
	ShortSHA string
	Message  string
	Author   string
	Email    string
	Date     time.Time
}

// checkpointDetail holds detailed information about a checkpoint for display.
type checkpointDetail struct {
	Index            int
	ShortID          string
	Timestamp        time.Time
	IsTaskCheckpoint bool
	Message          string
	// Interactions contains all prompt/response pairs in this checkpoint.
	// Most strategies have one, but shadow condensations may have multiple.
	Interactions []interaction
	// Files is the aggregate list of all files modified (for backwards compat)
	Files []string
}

func newExplainCmd() *cobra.Command {
	var sessionFlag string
	var commitFlag string
	var checkpointFlag string
	var noPagerFlag bool
	var shortFlag bool
	var fullFlag bool
	var rawTranscriptFlag bool
	var generateFlag bool
	var forceFlag bool
	var searchAllFlag bool

	cmd := &cobra.Command{
		Use:   "explain [checkpoint-id | commit-sha]",
		Short: "Explain a session, commit, or checkpoint",
		Long: `Explain provides human-readable context about sessions, commits, and checkpoints.

Use this command to understand what happened during agent-driven development,
either for self-review or to understand a teammate's work.

By default, shows checkpoints on the current branch. Pass a checkpoint ID or
commit SHA as a positional argument to explain a specific item, or use flags.

Viewing specific items:
  entire explain <id-or-sha>           Auto-detects checkpoint ID or commit SHA
  entire explain --checkpoint <id>     Force interpretation as checkpoint ID
  entire explain --commit <ref>        Force interpretation as commit ref

Filtering the list view:
  --session      Filter checkpoints by session ID (or prefix)

Output verbosity levels (when explaining a specific item):
  Default:         Detailed view with scoped prompts (ID, session, tokens, intent, prompts, files)
  --short          Summary only (ID, session, timestamp, tokens, intent)
  --full           Parsed full transcript (all prompts/responses from entire session)
  --raw-transcript Raw transcript file (JSONL format)

Summary generation:
  --generate    Generate an AI summary for the checkpoint
  --force       Regenerate even if a summary already exists (requires --generate)

Performance options:
  --search-all  Remove branch/depth limits when searching for commits (may be slow)

Checkpoint detail view shows:
  - Author of the checkpoint
  - Associated git commits that reference the checkpoint
  - Prompts and responses from the session

Note: --session filters the list view; the positional arg, --commit, and --checkpoint are mutually exclusive.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("accepts at most 1 argument (checkpoint ID or commit SHA), received %d\nHint: use --session to filter the list view, or pass a single checkpoint ID / commit SHA", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check if Entire is disabled
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}

			// Only initialize logging when inside a git worktree to avoid
			// creating .entire/logs/ in arbitrary directories.
			if _, err := paths.WorktreeRoot(cmd.Context()); err == nil {
				logging.SetLogLevelGetter(GetLogLevel)
				if err := logging.Init(cmd.Context(), ""); err == nil {
					defer logging.Close()
				}
			}

			// Positional arg is mutually exclusive with --checkpoint, --commit, --session
			var positional string
			if len(args) > 0 {
				positional = args[0]
				if checkpointFlag != "" || commitFlag != "" || sessionFlag != "" {
					return errors.New("cannot combine positional argument with --checkpoint, --commit, or --session")
				}
			}

			// --generate and --raw-transcript need a specific target — either the
			// positional arg, --checkpoint/-c, or --commit (which forwards to
			// the checkpoint path via the commit's Entire-Checkpoint trailer).
			hasCheckpointTarget := checkpointFlag != "" || commitFlag != "" || positional != ""
			if generateFlag && !hasCheckpointTarget {
				return errors.New("--generate requires a checkpoint ID or commit SHA (positional), --checkpoint/-c, or --commit flag")
			}
			if forceFlag && !generateFlag {
				return errors.New("--force requires --generate flag")
			}
			if rawTranscriptFlag && !hasCheckpointTarget {
				return errors.New("--raw-transcript requires a checkpoint ID or commit SHA (positional), --checkpoint/-c, or --commit flag")
			}

			// Convert short flag to verbose (verbose = !short)
			verbose := !shortFlag
			return runExplain(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), sessionFlag, commitFlag, checkpointFlag, positional, noPagerFlag, verbose, fullFlag, rawTranscriptFlag, generateFlag, forceFlag, searchAllFlag)
		},
	}

	cmd.Flags().StringVar(&sessionFlag, "session", "", "Filter checkpoints by session ID (or prefix)")
	cmd.Flags().StringVar(&commitFlag, "commit", "", "Explain a specific commit (SHA or ref, \"commit-ish\")")
	cmd.Flags().StringVarP(&checkpointFlag, "checkpoint", "c", "", "Explain a specific checkpoint (ID or prefix)")
	cmd.Flags().BoolVar(&noPagerFlag, "no-pager", false, "Disable pager output")
	cmd.Flags().BoolVarP(&shortFlag, "short", "s", false, "Show summary only (omit prompts and files)")
	cmd.Flags().BoolVar(&fullFlag, "full", false, "Show full parsed transcript (all prompts/responses)")
	cmd.Flags().BoolVar(&rawTranscriptFlag, "raw-transcript", false, "Show raw transcript file (JSONL format)")
	cmd.Flags().BoolVar(&generateFlag, "generate", false, "Generate an AI summary for the checkpoint")
	cmd.Flags().BoolVar(&forceFlag, "force", false, "Regenerate summary even if one already exists (requires --generate)")
	cmd.Flags().BoolVar(&searchAllFlag, "search-all", false, "Search all commits (no branch/depth limit, may be slow)")

	// Make --short, --full, and --raw-transcript mutually exclusive
	cmd.MarkFlagsMutuallyExclusive("short", "full", "raw-transcript")
	// --generate and --raw-transcript are incompatible (summary would be generated but not shown)
	cmd.MarkFlagsMutuallyExclusive("generate", "raw-transcript")

	return cmd
}

// runExplain routes to the appropriate explain function based on flags and the
// optional positional target.
func runExplain(ctx context.Context, w, errW io.Writer, sessionID, commitRef, checkpointID, target string, noPager, verbose, full, rawTranscript, generate, force, searchAll bool) error {
	// Count mutually exclusive flags (--commit and --checkpoint are mutually exclusive)
	// --session is now a filter for the list view, not a separate mode
	flagCount := 0
	if commitRef != "" {
		flagCount++
	}
	if checkpointID != "" {
		flagCount++
	}
	// If --session is combined with --commit or --checkpoint, that's still an error
	if sessionID != "" && flagCount > 0 {
		return errors.New("cannot specify multiple of --session, --commit, --checkpoint")
	}
	if flagCount > 1 {
		return errors.New("cannot specify multiple of --session, --commit, --checkpoint")
	}

	// Route to appropriate handler
	if target != "" {
		return runExplainAuto(ctx, w, errW, target, noPager, verbose, full, rawTranscript, generate, force, searchAll)
	}
	if commitRef != "" {
		return runExplainCommit(ctx, w, errW, commitRef, noPager, verbose, full, rawTranscript, generate, force, searchAll)
	}
	if checkpointID != "" {
		return runExplainCheckpoint(ctx, w, errW, checkpointID, noPager, verbose, full, rawTranscript, generate, force, searchAll)
	}

	// Default or with session filter: show list view (optionally filtered by session)
	return runExplainBranchWithFilter(ctx, w, noPager, sessionID)
}

// runExplainAuto resolves a positional target as either a checkpoint ID
// (or prefix) or a git commit ref. Ordering: checkpoint path first (which
// also handles shadow-branch temp checkpoints), falling back to commit
// resolution only on checkpoint.ErrCheckpointNotFound. --generate runs
// an ambiguity pre-check to avoid writing a summary to the wrong
// checkpoint on short-prefix collisions.
func runExplainAuto(ctx context.Context, w, errW io.Writer, target string, noPager, verbose, full, rawTranscript, generate, force, searchAll bool) error {
	stop := startSpinner(errW, "Loading checkpoints")
	lookup, lookupErr := newExplainCheckpointLookup(ctx)
	stop("")
	if generate {
		if err := runExplainAutoAmbiguityGuard(ctx, target, lookup, lookupErr); err != nil {
			return err
		}
	}
	checkpointErr := runExplainCheckpointWithLookup(ctx, w, errW, target, noPager, verbose, full, rawTranscript, generate, force, searchAll, lookup, lookupErr)
	if checkpointErr == nil {
		return nil
	}
	// Fall back to commit resolution ONLY when nothing (committed or temp)
	// matched the target. errCannotGenerateTemporaryCheckpoint signals that
	// we DID match a temp checkpoint but --generate is unsupported for it;
	// falling back to commit in that case would produce a misleading
	// "no trailer" error for the shadow-branch commit.
	if !errors.Is(checkpointErr, checkpoint.ErrCheckpointNotFound) {
		return checkpointErr
	}
	logging.Debug(ctx, "explain auto: checkpoint lookup failed, trying commit fallback",
		slog.String("target", target),
		slog.String("checkpoint_error", checkpointErr.Error()))

	if lookupErr != nil {
		// Composed message beats errors.Join here — the latter renders
		// two lines (one per error) and users act on the first/stale one.
		return fmt.Errorf("no checkpoint matched %q, and commit fallback failed: %w", target, lookupErr)
	}
	hash, ambiguousMatches, resolveErr := resolveCommitUnambiguous(lookup.repo, target)
	if resolveErr != nil {
		if errors.Is(resolveErr, errAmbiguousCommitPrefix) {
			renderAmbiguousPrefixFailure(errW, target, "commits", buildAmbiguousCommitMatches(lookup.repo, ambiguousMatches))
			return NewSilentError(resolveErr)
		}
		logging.Debug(ctx, "explain auto: git ref resolution failed",
			slog.String("target", target),
			slog.String("error", resolveErr.Error()))
		return fmt.Errorf("no checkpoint or commit found matching %q", target)
	}
	commit, commitErr := lookup.repo.CommitObject(hash)
	if commitErr != nil {
		return fmt.Errorf("failed to get commit %s: %w", abbreviateCommitHash(lookup.repo, hash), commitErr)
	}
	cpID, hasCheckpoint := trailers.ParseCheckpoint(commit.Message)
	if !hasCheckpoint {
		// Side-effect modes must error — silently succeeding would leave
		// scripts unable to distinguish "done" from "didn't happen".
		if generate || rawTranscript {
			return fmt.Errorf("cannot %s: commit %s has no Entire-Checkpoint trailer", generateOrRawLabel(generate), abbreviateCommitHash(lookup.repo, hash))
		}
		printNoTrailerMessage(w, lookup.repo, hash)
		return nil
	}
	logging.Debug(ctx, "explain auto: resolved commit to checkpoint via trailer",
		slog.String("target", target),
		slog.String("commit", abbreviateCommitHash(lookup.repo, hash)),
		slog.String("checkpoint_id", cpID.String()))
	return runExplainCheckpointWithLookup(ctx, w, errW, cpID.String(), noPager, verbose, full, rawTranscript, generate, force, searchAll, lookup, nil)
}

// runExplainAutoAmbiguityGuard refuses --generate when the positional
// target resolves as both a git revision and a committed-checkpoint prefix.
// Writing a summary to the wrong checkpoint is destructive; read-only flows
// tolerate the same ambiguity by preferring the checkpoint path.
//
// Best-effort: on repo/list failures we return nil so the main flow
// surfaces the real error instead of double-reporting.
func runExplainAutoAmbiguityGuard(ctx context.Context, target string, lookup *explainCheckpointLookup, lookupErr error) error {
	// Targets longer than a checkpoint ID can't prefix-match one.
	// This is coupled to checkpoint IDs being fixed-width; longer targets
	// cannot be prefixes of committed checkpoint IDs.
	if len(target) > id.ShortIDLength {
		return nil
	}
	if lookupErr != nil {
		logging.Warn(ctx, "explain ambiguity guard degraded: failed to prepare checkpoint lookup",
			"target", target,
			"error", lookupErr)
		return nil
	}
	hash, err := lookup.repo.ResolveRevision(plumbing.Revision(target))
	if err != nil {
		return nil //nolint:nilerr // target isn't a git ref
	}
	if lookup == nil {
		logging.Warn(ctx, "explain ambiguity guard degraded: checkpoint lookup unavailable",
			"target", target)
		return nil
	}
	if lookup.committed == nil {
		logging.Warn(ctx, "explain ambiguity guard degraded: committed checkpoint list unavailable",
			"target", target)
		return nil
	}
	for _, info := range lookup.committed {
		if strings.HasPrefix(info.CheckpointID.String(), target) {
			return fmt.Errorf("ambiguous target %q with --generate: matches both git revision %s and checkpoint prefix (e.g. %s)\nUse --commit <ref> or --checkpoint <id> to disambiguate", target, abbreviateCommitHash(lookup.repo, *hash), info.CheckpointID)
		}
	}
	return nil
}

// runExplainCheckpoint explains a specific checkpoint.
// Supports both committed checkpoints (by checkpoint ID) and temporary checkpoints (by git SHA).
// First tries to match committed checkpoints, then falls back to temporary checkpoints.
// When generate is true, generates an AI summary for the checkpoint.
// When force is true, regenerates even if a summary already exists.
// When rawTranscript is true, outputs only the raw transcript file (JSONL format).
// When searchAll is true, searches all commits without branch/depth limits (used for finding associated commits).
//

func runExplainCheckpoint(ctx context.Context, w, errW io.Writer, checkpointIDPrefix string, noPager, verbose, full, rawTranscript, generate, force, searchAll bool) error {
	return runExplainCheckpointWithLookup(ctx, w, errW, checkpointIDPrefix, noPager, verbose, full, rawTranscript, generate, force, searchAll, nil, nil)
}

func runExplainCheckpointWithLookup(ctx context.Context, w, errW io.Writer, checkpointIDPrefix string, noPager, verbose, full, rawTranscript, generate, force, searchAll bool, lookup *explainCheckpointLookup, lookupErr error) error {
	if lookup == nil {
		var err error
		lookup, err = newExplainCheckpointLookup(ctx)
		if err != nil {
			return err
		}
	} else if lookupErr != nil {
		return lookupErr
	}

	// Collect all matching checkpoint IDs to detect ambiguity
	var matches []id.CheckpointID
	for _, info := range lookup.committed {
		if strings.HasPrefix(info.CheckpointID.String(), checkpointIDPrefix) {
			matches = append(matches, info.CheckpointID)
		}
	}

	// If not found locally, fetch metadata from remote and retry. Reuses
	// resume's getMetadataTree / getV2MetadataTree helpers — they already
	// implement the checkpoint_remote → treeless origin → full origin chain
	// and return a fresh repo handle (which we discard; the post-fetch
	// rebuild via newExplainCheckpointLookup opens its own).
	if len(matches) == 0 {
		stop := startSpinner(errW, "Fetching checkpoint metadata from remote")
		_, _, v1Err := getMetadataTree(ctx)
		v2OK := false
		if lookup.preferCheckpointsV2 {
			if _, _, v2Err := getV2MetadataTree(ctx); v2Err == nil {
				v2OK = true
			}
		}
		stop("")
		if v1Err == nil || v2OK {
			if freshLookup, freshErr := newExplainCheckpointLookup(ctx); freshErr == nil {
				lookup = freshLookup
				for _, info := range lookup.committed {
					if strings.HasPrefix(info.CheckpointID.String(), checkpointIDPrefix) {
						matches = append(matches, info.CheckpointID)
					}
				}
			}
		}
	}

	var fullCheckpointID id.CheckpointID
	switch len(matches) {
	case 0:
		// Check temp checkpoints BEFORE returning errCannotGenerateTemporaryCheckpoint
		// so runExplainAuto can distinguish:
		//   - target matched a real temp checkpoint (sentinel returned, no fallback)
		//   - target matched nothing (ErrCheckpointNotFound, safe to fall back to commit)
		// Previously the --generate path bailed before checking temp checkpoints,
		// which made runExplainAuto fall back to commit resolution for temp
		// checkpoint SHAs and produce a misleading "no trailer" error.
		//
		// --generate and --raw-transcript are mutually exclusive at the flag
		// layer, so rawTranscript is always false when generate is true; the
		// direct-to-w write path inside explainTemporaryCheckpoint is not
		// reachable here and won't leak partial output on error.
		output, found, tempErr := explainTemporaryCheckpoint(ctx, w, errW, lookup.repo, lookup.v1Store, checkpointIDPrefix, verbose, full, rawTranscript)
		if tempErr != nil {
			return tempErr
		}
		if found {
			if generate {
				return fmt.Errorf("%w %s (only committed checkpoints supported)", errCannotGenerateTemporaryCheckpoint, checkpointIDPrefix)
			}
			outputExplainContent(w, output, noPager)
			return nil
		}
		return fmt.Errorf("%w: %s", checkpoint.ErrCheckpointNotFound, checkpointIDPrefix)
	case 1:
		fullCheckpointID = matches[0]
	default:
		// Ambiguous prefix: render styled failure block, return SilentError so
		// main.go does not double-print. Matches the temporary-side and
		// commit-side ambiguity paths.
		ambig := buildAmbiguousCheckpointMatches(matches, lookup.committed)
		renderAmbiguousPrefixFailure(errW, checkpointIDPrefix, "committed checkpoints", ambig)
		return NewSilentError(fmt.Errorf("%w: %s matches %d checkpoints", errAmbiguousCommitPrefix, checkpointIDPrefix, len(matches)))
	}

	// One spinner covers the entire data-loading pipeline: prefetch's
	// missing-blob analysis (which spawns one cat-file -e per blob and
	// can take seconds on a deep checkpoint subtree), the prefetch fetch
	// itself, ResolveCommittedReader's metadata read, session content
	// reads, and getAssociatedCommits' git log walk. Stop strictly before
	// any write to w (stdout) so stderr spinner frames and stdout output
	// never interleave.
	stopLoad := startSpinner(errW, fmt.Sprintf("Loading checkpoint %s", fullCheckpointID))

	resolvedReader, summary, content, err := loadCheckpointForExplain(ctx, errW, lookup, fullCheckpointID, full, generate, rawTranscript)
	if err != nil {
		stopLoad("")
		return err
	}
	v2Reader, isCheckpointsV2 := resolvedReader.(*checkpoint.V2GitStore)

	// Handle summary generation — uses raw transcript.
	if generate {
		stopLoad("") // generation prints its own progress to w/errW
		if err := generateCheckpointSummary(ctx, w, errW, lookup.v1Store, lookup.v2Store, fullCheckpointID, summary, content, force); err != nil {
			return err
		}
		// Reload to get the updated summary. After generation we only need
		// /main data for display, so use the /main-only path for v2.
		stopLoad = startSpinner(errW, fmt.Sprintf("Reloading checkpoint %s", fullCheckpointID))
		if isCheckpointsV2 {
			content, err = readV2ContentFromMain(ctx, v2Reader, fullCheckpointID, summary)
		} else {
			content, err = readLatestSessionContentForExplain(ctx, resolvedReader, fullCheckpointID, summary)
		}
		if err != nil {
			stopLoad("")
			return fmt.Errorf("failed to reload checkpoint: %w", err)
		}
	}

	// Handle raw transcript output
	if rawTranscript {
		stopLoad("")
		rawLog, _, rawErr := checkpoint.ResolveRawSessionLogForCheckpoint(ctx, fullCheckpointID, lookup.v1Store, lookup.v2Store, lookup.preferCheckpointsV2)
		if rawErr != nil {
			return fmt.Errorf("failed to read raw transcript: %w", rawErr)
		}
		if len(rawLog) == 0 {
			return fmt.Errorf("checkpoint %s has no transcript", fullCheckpointID)
		}
		// Output raw transcript directly (no pager, no formatting)
		if _, err = w.Write(rawLog); err != nil {
			return fmt.Errorf("failed to write transcript: %w", err)
		}
		return nil
	}

	// Find associated commits (git commits with matching Entire-Checkpoint trailer)
	associatedCommits, _ := getAssociatedCommits(ctx, lookup.repo, fullCheckpointID, searchAll) //nolint:errcheck // Best-effort

	// Derive author from the first associated commit (the user who made the commit).
	// Fall back to GetCheckpointAuthor (walks entire/checkpoints/v1) for checkpoints
	// not reachable from the current branch.
	var author checkpoint.Author
	if len(associatedCommits) > 0 {
		author = checkpoint.Author{
			Name:  associatedCommits[0].Author,
			Email: associatedCommits[0].Email,
		}
	} else {
		author, _ = lookup.v1Store.GetCheckpointAuthor(ctx, fullCheckpointID) //nolint:errcheck // Author is optional
	}

	// Format and output. Stop spinner BEFORE any write to w to keep stderr
	// frames and stdout content from interleaving.
	stopLoad("")
	output := formatCheckpointOutput(summary, content, fullCheckpointID, associatedCommits, author, verbose, full, w)
	outputExplainContent(w, output, noPager)
	return nil
}

// loadCheckpointForExplain runs prefetchCheckpointBlobs + summary read +
// session content read for the given checkpoint. Extracts the bulk of the
// data-load pipeline out of runExplainCheckpointWithLookup so that
// function stays under maintidx limits. Caller is responsible for the
// surrounding spinner.
func loadCheckpointForExplain(ctx context.Context, errW io.Writer, lookup *explainCheckpointLookup, cpID id.CheckpointID, full, generate, rawTranscript bool) (checkpoint.CommittedReader, *checkpoint.CheckpointSummary, *checkpoint.SessionContent, error) {
	prefetchCheckpointBlobs(ctx, errW, lookup.repo, cpID, lookup.preferCheckpointsV2)

	reader, summary, err := checkpoint.ResolveCommittedReaderForCheckpoint(ctx, cpID, lookup.v1Store, lookup.v2Store, lookup.preferCheckpointsV2)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}

	// Default display modes for v2 checkpoints read only from /main —
	// metadata, prompts, and the compact transcript. The raw transcript
	// on /full/* refs is never needed for human-readable output and may
	// be unavailable (rotated, not fetched).
	needsRawTranscript := full || generate || rawTranscript
	if v2Reader, ok := reader.(*checkpoint.V2GitStore); ok && !needsRawTranscript {
		content, contentErr := readV2ContentFromMain(ctx, v2Reader, cpID, summary)
		if contentErr != nil {
			return nil, nil, nil, fmt.Errorf("failed to read checkpoint content: %w", contentErr)
		}
		return reader, summary, content, nil
	}
	content, contentErr := readLatestSessionContentForExplain(ctx, reader, cpID, summary)
	if contentErr != nil {
		return nil, nil, nil, fmt.Errorf("failed to read checkpoint content: %w", contentErr)
	}
	return reader, summary, content, nil
}

// prefetchCheckpointBlobs navigates to the checkpoint's subtree(s) — v1
// always, v2 when enabled — collects every locally-missing blob, and
// fetches them all in a single `git fetch-pack` invocation per store.
// Best-effort — failure is logged and the read path falls back to the
// FetchingTree's per-File fetcher.
//
// Caller is expected to wrap this with a spinner; both the missing-blob
// analysis (one cat-file -e per blob) and the actual fetch are silent
// inside this function so the caller's spinner provides continuous
// feedback.
func prefetchCheckpointBlobs(ctx context.Context, _ io.Writer, repo *git.Repository, cpID id.CheckpointID, preferV2 bool) {
	v1FT := buildCheckpointFetchingTree(ctx, repo, cpID, "v1", loadV1MetadataRootTree)
	var v2FT *checkpoint.FetchingTree
	if preferV2 {
		v2FT = buildCheckpointFetchingTree(ctx, repo, cpID, "v2", loadV2MainRootTree)
	}

	missingCount := 0
	if v1FT != nil {
		missingCount += len(v1FT.CollectMissingBlobs())
	}
	if v2FT != nil {
		missingCount += len(v2FT.CollectMissingBlobs())
	}
	if missingCount == 0 {
		return
	}
	logging.Debug(ctx, "explain prefetch: fetching missing checkpoint blobs",
		slog.String("checkpoint_id", cpID.String()),
		slog.Int("blob_count", missingCount),
	)

	runPreFetch(ctx, v1FT, cpID, "v1")
	runPreFetch(ctx, v2FT, cpID, "v2")
}

// buildCheckpointFetchingTree navigates to the checkpoint subtree using
// loadRoot and wraps it in a FetchingTree with FetchBlobsByHash. Returns
// nil when the root tree or cp subtree isn't navigable.
func buildCheckpointFetchingTree(ctx context.Context, repo *git.Repository, cpID id.CheckpointID, label string, loadRoot func(*git.Repository) (*object.Tree, error)) *checkpoint.FetchingTree {
	rootTree, err := loadRoot(repo)
	if err != nil {
		return nil
	}
	cpSubtree, err := rootTree.Tree(cpID.Path())
	if err != nil {
		logging.Debug(ctx, "explain prefetch: cp subtree not found",
			slog.String("store", label),
			slog.String("checkpoint_id", cpID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	return checkpoint.NewFetchingTree(ctx, cpSubtree, repo.Storer, FetchBlobsByHash)
}

func runPreFetch(ctx context.Context, ft *checkpoint.FetchingTree, cpID id.CheckpointID, label string) {
	if ft == nil {
		return
	}
	prefetched, err := ft.PreFetch()
	if err != nil {
		logging.Debug(ctx, "explain prefetch: PreFetch failed",
			slog.String("store", label),
			slog.String("checkpoint_id", cpID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	if prefetched > 0 {
		logging.Debug(ctx, "explain prefetch: blobs fetched in one round-trip",
			slog.String("store", label),
			slog.String("checkpoint_id", cpID.String()),
			slog.Int("blob_count", prefetched),
		)
	}
}

func loadV1MetadataRootTree(repo *git.Repository) (*object.Tree, error) {
	if tree, err := strategy.GetMetadataBranchTree(repo); err == nil {
		return tree, nil
	}
	tree, err := strategy.GetRemoteMetadataBranchTree(repo)
	if err != nil {
		return nil, fmt.Errorf("read v1 metadata tree (local + remote-tracking): %w", err)
	}
	return tree, nil
}

func loadV2MainRootTree(repo *git.Repository) (*object.Tree, error) {
	ref, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	if err != nil {
		return nil, fmt.Errorf("v2 /main ref not found: %w", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("read v2 /main commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read v2 /main tree: %w", err)
	}
	return tree, nil
}

func newExplainCheckpointLookup(ctx context.Context) (*explainCheckpointLookup, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}

	v2URL, err := remote.FetchURL(ctx)
	if err != nil {
		logging.Debug(ctx, "explain: using origin for v2 store fetch remote",
			slog.String("error", err.Error()),
		)
		v2URL = ""
	}

	// FetchBlobsByHash uses `git fetch-pack` for blob SHAs (porcelain
	// `git fetch` fails against partial-clone repos with "did not send all
	// necessary objects"). Falls back to a full metadata-branch fetch if
	// fetch-pack also can't reach the blobs.
	v1Store := checkpoint.NewGitStore(repo)
	v1Store.SetBlobFetcher(FetchBlobsByHash)

	v2Store := checkpoint.NewV2GitStore(repo, v2URL)
	v2Store.SetBlobFetcher(FetchBlobsByHash)

	lookup := &explainCheckpointLookup{
		repo:                repo,
		v1Store:             v1Store,
		v2Store:             v2Store,
		preferCheckpointsV2: settings.IsCheckpointsV2Enabled(ctx),
	}

	committed, err := listCommittedForExplain(ctx, lookup.v1Store, lookup.v2Store, lookup.preferCheckpointsV2)
	if err != nil {
		return nil, fmt.Errorf("failed to list checkpoints: %w", err)
	}
	lookup.committed = committed
	return lookup, nil
}

func listCommittedForExplain(ctx context.Context, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, preferCheckpointsV2 bool) ([]checkpoint.CommittedInfo, error) {
	v1Committed, v1Err := v1Store.ListCommitted(ctx)

	if !preferCheckpointsV2 {
		if v1Err != nil {
			return nil, fmt.Errorf("listing v1 checkpoints: %w", v1Err)
		}
		return v1Committed, nil
	}

	v2Committed, v2Err := v2Store.ListCommitted(ctx)
	if v2Err != nil {
		logging.Debug(ctx, "v2 ListCommitted failed, using v1 only",
			slog.String("error", v2Err.Error()),
		)
		if v1Err != nil {
			return nil, fmt.Errorf("listing checkpoints: %w", v1Err)
		}
		return v1Committed, nil
	}

	if v1Err != nil {
		logging.Debug(ctx, "v1 ListCommitted failed, returning v2 only",
			slog.String("error", v1Err.Error()),
		)
		return v2Committed, nil
	}

	// Merge v2 and v1 results so pre-v2 checkpoints remain visible during transition.
	seen := make(map[id.CheckpointID]struct{}, len(v2Committed))
	for _, c := range v2Committed {
		seen[c.CheckpointID] = struct{}{}
	}
	committedCheckpoints := make([]checkpoint.CommittedInfo, 0, len(v2Committed)+len(v1Committed))
	committedCheckpoints = append(committedCheckpoints, v2Committed...)
	for _, c := range v1Committed {
		if _, ok := seen[c.CheckpointID]; !ok {
			committedCheckpoints = append(committedCheckpoints, c)
		}
	}
	return committedCheckpoints, nil
}

func readLatestSessionContentForExplain(ctx context.Context, reader checkpoint.CommittedReader, checkpointID id.CheckpointID, summary *checkpoint.CheckpointSummary) (*checkpoint.SessionContent, error) {
	if summary == nil || len(summary.Sessions) == 0 {
		return nil, checkpoint.ErrCheckpointNotFound
	}

	latestIndex := len(summary.Sessions) - 1
	content, err := reader.ReadSessionContent(ctx, checkpointID, latestIndex)
	if err != nil {
		return nil, fmt.Errorf("reading session %d content: %w", latestIndex, err)
	}
	return content, nil
}

// resolvePromptTree picks the best metadata tree for reading session prompts.
// Prefers v2 when enabled (same sharded layout as v1), falls back to v1.
func resolvePromptTree(v1Tree, v2Tree *object.Tree, preferV2 bool) *object.Tree {
	if preferV2 && v2Tree != nil {
		return v2Tree
	}
	if v1Tree != nil {
		return v1Tree
	}
	return v2Tree // Last resort: use v2 even if not preferred
}

// readV2ContentFromMain reads session content from the v2 /main ref only —
// metadata, prompts, and the compact transcript (transcript.jsonl). This is the
// primary read path for default display modes that don't need the raw transcript
// stored on /full/* refs.
func readV2ContentFromMain(ctx context.Context, v2Reader *checkpoint.V2GitStore, checkpointID id.CheckpointID, summary *checkpoint.CheckpointSummary) (*checkpoint.SessionContent, error) {
	if summary == nil || len(summary.Sessions) == 0 {
		return nil, checkpoint.ErrCheckpointNotFound
	}

	latestIndex := len(summary.Sessions) - 1

	content, err := v2Reader.ReadSessionMetadataAndPrompts(ctx, checkpointID, latestIndex)
	if err != nil {
		return nil, fmt.Errorf("reading session %d metadata: %w", latestIndex, err)
	}

	// ReadSessionMetadataAndPrompts reads the compact transcript from the same
	// session tree. Reset transcript offsets when compact data is present.
	if len(content.Transcript) > 0 {
		content.Metadata.CheckpointTranscriptStart = 0
		content.Metadata.TranscriptLinesAtStart = 0 //nolint:staticcheck // Set for backward compat with older CLI readers
		return content, nil
	}

	// No compact transcript on /main — fall back to the raw transcript on
	// /full/current for the most accurate display before resorting to prompt.txt.
	fullContent, fullErr := v2Reader.ReadSessionContent(ctx, checkpointID, latestIndex)
	if fullErr == nil && len(fullContent.Transcript) > 0 {
		content.Transcript = fullContent.Transcript
		return content, nil
	}

	// Last resort: return metadata + prompts without transcript.
	return content, nil
}

// generateCheckpointSummary generates an AI summary for a checkpoint and persists it.
// The summary is generated from the scoped transcript (only this checkpoint's portion),
// not the entire session transcript.
func generateCheckpointSummary(ctx context.Context, w, errW io.Writer, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, checkpointID id.CheckpointID, cpSummary *checkpoint.CheckpointSummary, content *checkpoint.SessionContent, force bool) error {
	// Check if summary already exists
	if content.Metadata.Summary != nil && !force {
		return renderExplainFailure(errW, "Summary already exists", []explainRow{
			{Label: "id", Value: checkpointID.String()},
			{Label: "try", Value: fmt.Sprintf("entire explain --generate --force %s", checkpointID)},
		}, fmt.Errorf("checkpoint %s already has a summary", checkpointID))
	}

	// Check if transcript exists
	if len(content.Transcript) == 0 {
		return renderExplainFailure(errW, "Checkpoint has no transcript", []explainRow{
			{Label: "id", Value: checkpointID.String()},
		}, fmt.Errorf("checkpoint %s has no transcript to summarize", checkpointID))
	}

	// Scope the transcript to only this checkpoint's portion
	scopedTranscript := scopeTranscriptForCheckpoint(content.Transcript, content.Metadata.GetTranscriptStart(), content.Metadata.Agent)
	if len(scopedTranscript) == 0 {
		return renderExplainFailure(errW, "Checkpoint has no transcript content (scoped)", []explainRow{
			{Label: "id", Value: checkpointID.String()},
		}, fmt.Errorf("checkpoint %s has no transcript content for this checkpoint (scoped)", checkpointID))
	}
	provider, err := resolveCheckpointSummaryProvider(ctx, w)
	if err != nil {
		return fmt.Errorf("failed to resolve summary provider: %w", err)
	}
	scopedTranscript = maybeCompactExternalTranscriptForSummary(ctx, scopedTranscript, content.Metadata.Agent)

	// Generate summary using shared helper
	logging.Info(ctx, "generating checkpoint summary")
	if errW != nil {
		fmt.Fprintln(errW, "Generating checkpoint summary...")
	}

	start := time.Now()
	summary, appliedDeadline, err := generateCheckpointAISummary(ctx, scopedTranscript, cpSummary.FilesTouched, content.Metadata.Agent, provider.Generator)
	if err != nil {
		label, rows, structured := formatCheckpointSummaryError(err, appliedDeadline)
		styles := newStatusStyles(errW)
		fmt.Fprint(errW, styles.renderFailure(label, rows))
		return NewSilentError(structured)
	}
	elapsed := time.Since(start)

	// Persist to both stores; at least one must succeed.
	v1Err := v1Store.UpdateSummary(ctx, checkpointID, summary)
	var v2Err error
	if v2Store != nil {
		v2Err = v2Store.UpdateSummary(ctx, checkpointID, summary)
	}

	switch {
	case v1Err != nil && (v2Store == nil || v2Err != nil):
		// No store succeeded — hard error.
		if v2Err != nil {
			return fmt.Errorf("failed to save summary: v1: %w, v2: %w", v1Err, v2Err)
		}
		return fmt.Errorf("failed to save summary: %w", v1Err)
	case v1Err != nil:
		logging.Debug(ctx, "v1 UpdateSummary failed (v2 succeeded)",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("error", v1Err.Error()),
		)
	case v2Err != nil:
		logging.Debug(ctx, "v2 UpdateSummary failed (v1 succeeded)",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("error", v2Err.Error()),
		)
	}

	styles := newStatusStyles(w)
	rows := summaryProviderRows(provider)
	rows = append(rows, explainRow{Label: "duration", Value: formatSummaryDuration(elapsed)})
	fmt.Fprint(w, styles.renderSuccess(fmt.Sprintf("Summary generated for %s", checkpointID), rows))
	return nil
}

// formatSummaryDuration rounds wall-clock generation time to a human-friendly value.
func formatSummaryDuration(d time.Duration) string {
	return d.Round(100 * time.Millisecond).String()
}

func maybeCompactExternalTranscriptForSummary(ctx context.Context, scopedTranscript []byte, agentType types.AgentType) []byte {
	if transcriptHasSummaryContent(scopedTranscript, agentType) {
		return scopedTranscript
	}

	ag, err := agent.GetByAgentType(agentType)
	if err != nil {
		external.DiscoverAndRegister(ctx)
		ag, err = agent.GetByAgentType(agentType)
	}
	if err != nil || !external.IsExternal(ag) {
		return scopedTranscript
	}

	compactor, ok := agent.AsTranscriptCompactor(ag)
	if !ok {
		return scopedTranscript
	}

	tmpFile, err := os.CreateTemp("", "entire-summary-transcript-*.jsonl")
	if err != nil {
		logging.Debug(ctx, "external summary compaction unavailable",
			slog.String("agent", string(agentType)),
			slog.String("error", err.Error()))
		return scopedTranscript
	}
	tmpPath := tmpFile.Name()
	defer func() {
		if removeErr := os.Remove(tmpPath); removeErr != nil {
			logging.Debug(ctx, "failed to remove temporary summary transcript",
				slog.String("path", tmpPath),
				slog.String("error", removeErr.Error()))
		}
	}()

	if _, err := tmpFile.Write(scopedTranscript); err != nil {
		_ = tmpFile.Close()
		logging.Debug(ctx, "external summary compaction transcript write failed",
			slog.String("agent", string(agentType)),
			slog.String("error", err.Error()))
		return scopedTranscript
	}
	if err := tmpFile.Close(); err != nil {
		logging.Debug(ctx, "external summary compaction transcript close failed",
			slog.String("agent", string(agentType)),
			slog.String("error", err.Error()))
		return scopedTranscript
	}

	compacted, err := compactor.CompactTranscript(ctx, tmpPath)
	if err != nil || compacted == nil || len(compacted.Transcript) == 0 {
		if err != nil {
			logging.Debug(ctx, "external summary compaction failed",
				slog.String("agent", string(agentType)),
				slog.String("error", err.Error()))
		}
		return scopedTranscript
	}

	redacted, err := redact.JSONLBytes(compacted.Transcript)
	if err != nil {
		logging.Debug(ctx, "external summary compaction redaction failed",
			slog.String("agent", string(agentType)),
			slog.String("error", err.Error()))
		return scopedTranscript
	}
	redactedTranscript := redacted.Bytes()
	if !transcriptHasSummaryContent(redactedTranscript, agentType) {
		return scopedTranscript
	}

	logging.Debug(ctx, "using external compact transcript for summary generation",
		slog.String("agent", string(agentType)))
	return redactedTranscript
}

func transcriptHasSummaryContent(transcriptBytes []byte, agentType types.AgentType) bool {
	entries, err := summarize.BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted(transcriptBytes), agentType)
	return err == nil && len(entries) > 0
}

// generateCheckpointAISummary returns the generated summary, the effective
// deadline applied to the underlying call (which may be shorter than
// checkpointSummaryTimeout if the parent context had an earlier deadline),
// and any error. The effective deadline is returned so the caller can render
// the true timeout value in user-facing error messages instead of always
// showing the package default.
func generateCheckpointAISummary(ctx context.Context, scopedTranscript []byte, filesTouched []string, agentType types.AgentType, generator summarize.Generator) (*checkpoint.Summary, time.Duration, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, checkpointSummaryTimeout)
	timeoutDuration := checkpointSummaryTimeout
	if deadline, ok := timeoutCtx.Deadline(); ok {
		timeoutDuration = time.Until(deadline)
	}
	defer cancel()

	// scopedTranscript is either read from checkpoint storage (redacted on
	// write) or replaced by external compact output redacted before use.
	summary, err := generateTranscriptSummary(timeoutCtx, redact.AlreadyRedacted(scopedTranscript), filesTouched, agentType, generator)
	if err != nil {
		// Only classify as ctx cancel/deadline when the error chain actually
		// contains the sentinel. Relying on timeoutCtx.Err() here loses typed
		// errors (e.g. *ClaudeError) when the subprocess returned a real
		// structured failure while timeoutCtx.Err() is non-nil for any reason
		// (parent cancelled, deadline already elapsed, etc.).
		if errors.Is(err, context.Canceled) {
			return nil, timeoutDuration, fmt.Errorf("summary generation canceled: %w", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, timeoutDuration, fmt.Errorf("summary generation timed out after %s: %w", formatSummaryTimeout(timeoutDuration), err)
		}
		return nil, timeoutDuration, err
	}

	return summary, timeoutDuration, nil
}

// formatCheckpointSummaryError maps typed Claude CLI errors and context
// sentinels to a structured failure block: a user-visible label, supporting
// rows, and a structured error suitable for wrapping in NewSilentError.
//
// The styled rendering happens in the caller (generateCheckpointSummary), which
// renders to errW via newStatusStyles(...).renderFailure(label, rows). This
// split keeps the formatting policy in one place (the failure block) while
// letting the caller still return a *SilentError for main.go's exit handling.
func formatCheckpointSummaryError(err error, deadline time.Duration) (string, []explainRow, error) {
	var claudeErr *claudecode.ClaudeError
	switch {
	case errors.As(err, &claudeErr):
		switch claudeErr.Kind { //nolint:exhaustive // ClaudeErrorUnknown handled by default
		case claudecode.ClaudeErrorAuth:
			label := "Claude authentication failed"
			rows := []explainRow{
				{Label: "try", Value: "run `claude login` and retry"},
			}
			if claudeErr.Message != "" {
				rows = append([]explainRow{{Label: "message", Value: claudeErr.Message}}, rows...)
			}
			return label, rows, fmt.Errorf("Claude authentication failed%s", formatMessageSuffix(claudeErr.Message)) //nolint:staticcheck // ST1005: Claude is a proper noun
		case claudecode.ClaudeErrorRateLimit:
			label := "Claude rejected the summary request due to rate limits or quota"
			rows := []explainRow{
				{Label: "try", Value: "wait and retry"},
			}
			if claudeErr.Message != "" {
				rows = append([]explainRow{{Label: "message", Value: claudeErr.Message}}, rows...)
			}
			return label, rows, fmt.Errorf("Claude rejected the summary request due to rate limits or quota%s", formatMessageSuffix(claudeErr.Message)) //nolint:staticcheck // ST1005
		case claudecode.ClaudeErrorConfig:
			label := "Claude rejected the summary request"
			rows := []explainRow{
				{Label: "try", Value: "check your Claude CLI config and selected model"},
			}
			if claudeErr.Message != "" {
				rows = append([]explainRow{{Label: "message", Value: claudeErr.Message}}, rows...)
			}
			return label, rows, fmt.Errorf("Claude rejected the summary request%s", formatMessageSuffix(claudeErr.Message)) //nolint:staticcheck // ST1005
		case claudecode.ClaudeErrorCLIMissing:
			label := "Claude CLI is not installed or not on PATH"
			return label, nil, errors.New("Claude CLI is not installed or not on PATH") //nolint:staticcheck // ST1005
		default:
			label := "Claude failed to generate the summary"
			suffix := formatClaudeErrorSuffix(claudeErr)
			rows := []explainRow{
				{Label: "detail", Value: strings.TrimPrefix(strings.TrimPrefix(suffix, ": "), " ")},
			}
			return label, rows, fmt.Errorf("Claude failed to generate the summary%s", suffix) //nolint:staticcheck // ST1005
		}
	case errors.Is(err, context.DeadlineExceeded):
		// Deliberately provider-neutral: explain --generate supports multiple
		// summary providers (claude-code, codex, gemini, ...), so hardcoding
		// "Claude" / "sonnet" / "Anthropic" here would misdirect users who
		// selected a different provider in .entire/settings.json.
		label := "Summary generation timed out after " + formatSummaryTimeout(deadline)
		rows := []explainRow{
			{Label: "causes", Value: ""},
			{Label: "", Value: "• the selected model is taking longer than expected on a large transcript"},
			{Label: "", Value: "• the summary provider's CLI cannot reach its API (network, VPN, firewall)"},
			{Label: "", Value: "• the provider's API is degraded"},
			{Label: "try", Value: "run the provider CLI directly to confirm it works"},
		}
		return label, rows, fmt.Errorf("summary generation did not return within the %s safety deadline", formatSummaryTimeout(deadline))
	case errors.Is(err, context.Canceled):
		return "Summary generation canceled", nil, errors.New("summary generation canceled")
	default:
		return "Failed to generate summary", []explainRow{{Label: "detail", Value: err.Error()}}, fmt.Errorf("failed to generate summary: %w", err)
	}
}

// formatMessageSuffix formats ": <msg>" when msg is non-empty and "" otherwise.
// Used by the Auth / RateLimit / Config branches of formatCheckpointSummaryError
// to avoid rendering a bare colon when ClaudeError.Message is empty (reachable
// when the CLI envelope is is_error:true with result:null but a real status).
func formatMessageSuffix(msg string) string {
	if msg == "" {
		return ""
	}
	return ": " + msg
}

// formatClaudeErrorSuffix builds a diagnostic suffix for user-facing output
// when we fall through to the default "failed to generate the summary" path.
// Prefers the envelope Message, falls back to HTTP status, then exit code,
// so the user never sees a bare "Claude failed to generate the summary:"
// with nothing after the colon (which happens when Claude returns
// is_error:true with result:null, or when the subprocess crashes with no
// stderr output). ExitCode < 0 means the subprocess did not produce a real
// exit code (e.g. launch failure) — render that as "abnormal termination"
// rather than the misleading "exited with code -1".
func formatClaudeErrorSuffix(e *claudecode.ClaudeError) string {
	if e.Message != "" {
		return ": " + e.Message
	}
	switch {
	case e.APIStatus != 0:
		return fmt.Sprintf(" (Anthropic API returned HTTP %d)", e.APIStatus)
	case e.ExitCode > 0:
		return fmt.Sprintf(" (claude CLI exited with code %d)", e.ExitCode)
	case e.ExitCode < 0:
		return " (claude CLI terminated abnormally — no exit code captured)"
	default:
		return " (no diagnostic detail available from Claude CLI)"
	}
}

func formatSummaryTimeout(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// explainTemporaryCheckpoint finds and formats a temporary checkpoint by shadow commit hash prefix.
// Returns the formatted output, whether the checkpoint was found, and an
// optional error. When err is non-nil, the function has already rendered a
// styled failure block to errW; the caller should wrap and return as
// SilentError without printing again.
// Searches ALL shadow branches, not just the one for current HEAD, to find checkpoints
// created from different base commits (e.g., if HEAD advanced since session start).
// The writer w is used for raw transcript output to bypass the pager.
func explainTemporaryCheckpoint(ctx context.Context, w, errW io.Writer, repo *git.Repository, store *checkpoint.GitStore, shaPrefix string, verbose, full, rawTranscript bool) (string, bool, error) {
	// List temporary checkpoints from ALL shadow branches
	// This ensures we find checkpoints even if HEAD has advanced since the session started
	tempCheckpoints, err := store.ListAllTemporaryCheckpoints(ctx, "", branchCheckpointsLimit)
	if err != nil {
		return "", false, nil //nolint:nilerr // best-effort: caller falls back to ErrCheckpointNotFound when no temp checkpoint is found
	}

	// Find checkpoints matching the SHA prefix - check for ambiguity
	var matches []checkpoint.TemporaryCheckpointInfo
	for _, tc := range tempCheckpoints {
		if strings.HasPrefix(tc.CommitHash.String(), shaPrefix) {
			matches = append(matches, tc)
		}
	}

	if len(matches) == 0 {
		return "", false, nil
	}

	if len(matches) > 1 {
		// Multiple matches: render styled failure block, return SilentError.
		ambiguous := make([]ambiguousMatch, 0, len(matches))
		for _, m := range matches {
			shortID := m.CommitHash.String()
			if len(shortID) > 7 {
				shortID = shortID[:7]
			}
			ambiguous = append(ambiguous, ambiguousMatch{
				ShortID:   shortID,
				Timestamp: m.Timestamp,
				SessionID: m.SessionID,
			})
		}
		renderAmbiguousPrefixFailure(errW, shaPrefix, "temporary checkpoints", ambiguous)
		return "", false, NewSilentError(fmt.Errorf("%w: %s matches %d temporary checkpoints", errAmbiguousCommitPrefix, shaPrefix, len(matches)))
	}

	tc := matches[0]

	// Get shadow commit and tree to read metadata
	shadowCommit, commitErr := repo.CommitObject(tc.CommitHash)
	if commitErr != nil {
		return "", false, nil //nolint:nilerr // best-effort: missing shadow commit is treated as not-found
	}

	shadowTree, treeErr := shadowCommit.Tree()
	if treeErr != nil {
		return "", false, nil //nolint:nilerr // best-effort: missing shadow tree is treated as not-found
	}

	// Read agent type from shadow branch metadata (stored during checkpoint creation)
	agentType := strategy.ReadAgentTypeFromTree(shadowTree, tc.MetadataDir)

	// Handle raw transcript output
	if rawTranscript {
		transcriptBytes, transcriptErr := store.GetTranscriptFromCommit(ctx, tc.CommitHash, tc.MetadataDir, agentType)
		if transcriptErr != nil || len(transcriptBytes) == 0 {
			shortID := tc.CommitHash.String()[:7]
			return "", false, renderExplainFailure(errW, "Checkpoint has no transcript", []explainRow{
				{Label: "id", Value: shortID},
			}, fmt.Errorf("checkpoint %s has no transcript", shortID))
		}
		// Write directly to writer (no pager, no formatting) - matches committed checkpoint behavior
		if _, writeErr := fmt.Fprint(w, string(transcriptBytes)); writeErr != nil {
			return "", false, fmt.Errorf("failed to write transcript: %w", writeErr)
		}
		return "", true, nil
	}

	// Read prompts from shadow branch
	sessionPrompt := strategy.ReadSessionPromptFromTree(shadowTree, tc.MetadataDir)

	// Build output similar to formatCheckpointOutput but for temporary
	var sb strings.Builder
	shortID := tc.CommitHash.String()[:7]
	styles := newStatusStyles(w)

	label := fmt.Sprintf("Checkpoint %s [temporary]", shortID)
	rows := []explainRow{
		{Label: "session", Value: tc.SessionID},
		{Label: "created", Value: tc.Timestamp.Format("2006-01-02 15:04:05")},
	}
	sb.WriteString(styles.renderIdentity(label, "", rows))

	intent := extractIntent(nil, sessionPrompt)
	hint := "Not generated. Temporary checkpoints can be summarized after commit. Run `entire explain --generate` on the resulting commit."
	sb.WriteString(renderExplainBody(w, buildNoSummaryMarkdown(intent, nil, hint)))

	// Transcript section: full shows entire session, verbose shows checkpoint scope
	// For temporary checkpoints, load transcript and compute scope from parent commit
	var fullTranscript []byte
	var scopedTranscript []byte
	if full || verbose {
		fullTranscript, _ = store.GetTranscriptFromCommit(ctx, tc.CommitHash, tc.MetadataDir, agentType) //nolint:errcheck // Best-effort

		if verbose && len(fullTranscript) > 0 {
			// Compute scoped transcript by finding where parent's transcript ended
			// Each shadow branch commit has the full transcript up to that point,
			// so we diff against parent to get just this checkpoint's activity
			scopedTranscript = fullTranscript // Default to full if no parent
			if shadowCommit.NumParents() > 0 {
				if parent, parentErr := shadowCommit.Parent(0); parentErr == nil {
					parentTranscript, _ := store.GetTranscriptFromCommit(ctx, parent.Hash, tc.MetadataDir, agentType) //nolint:errcheck // Best-effort
					if len(parentTranscript) > 0 {
						parentOffset := transcriptOffset(parentTranscript, agentType)
						scopedTranscript = scopeTranscriptForCheckpoint(fullTranscript, parentOffset, agentType)
					}
				}
			}
		}
	}
	if verbose || full {
		label := "Transcript (checkpoint scope)"
		if full {
			label = "Transcript (full session)"
		}
		sb.WriteString("\n")
		sb.WriteString(styles.sectionRule(label, styles.width))
		sb.WriteString("\n")
	}
	appendTranscriptSection(&sb, verbose, full, fullTranscript, scopedTranscript, sessionPrompt, agentType)

	return sb.String(), true, nil
}

// getAssociatedCommits finds git commits that reference the given checkpoint ID.
// Searches commits on the current branch for Entire-Checkpoint trailer matches.
// When searchAll is true, uses full DAG walk with no depth limit (may be slow).
// This finds checkpoint commits on merged feature branches (second parents of merges).
func getAssociatedCommits(ctx context.Context, repo *git.Repository, checkpointID id.CheckpointID, searchAll bool) ([]associatedCommit, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	commits := []associatedCommit{} // Initialize as empty slice, not nil (nil means "not searched")
	targetID := checkpointID.String()

	collectCommit := func(c *object.Commit) {
		fullSHA := c.Hash.String()
		shortSHA := fullSHA
		if len(fullSHA) >= 7 {
			shortSHA = fullSHA[:7]
		}
		commits = append(commits, associatedCommit{
			SHA:      fullSHA,
			ShortSHA: shortSHA,
			Message:  strings.Split(c.Message, "\n")[0],
			Author:   c.Author.Name,
			Email:    c.Author.Email,
			Date:     c.Author.When,
		})
	}

	if searchAll {
		// Full DAG walk: follows all parents of merge commits, no depth limit.
		// This finds checkpoint commits on merged feature branches.
		iter, iterErr := repo.Log(&git.LogOptions{
			From:  head.Hash(),
			Order: git.LogOrderCommitterTime,
		})
		if iterErr != nil {
			return nil, fmt.Errorf("failed to get commit log: %w", iterErr)
		}
		defer iter.Close()

		err = iter.ForEach(func(c *object.Commit) error {
			if err := ctx.Err(); err != nil {
				return err //nolint:wrapcheck // Propagating context cancellation
			}
			cpID, found := trailers.ParseCheckpoint(c.Message)
			if found && cpID.String() == targetID {
				collectCommit(c)
			}
			return nil
		})
	} else {
		// First-parent walk with depth limit and branch filtering.
		// Avoids walking into main's history through merge commit parents.
		reachableFromMain := computeReachableFromMain(ctx, repo)

		err = walkFirstParentCommits(ctx, repo, head.Hash(), commitScanLimit, func(c *object.Commit) error {
			// Once we hit a commit reachable from main on the first-parent chain,
			// all earlier ancestors are also shared-with-main, so stop scanning.
			if reachableFromMain[c.Hash] {
				return errStopIteration
			}

			cpID, found := trailers.ParseCheckpoint(c.Message)
			if found && cpID.String() == targetID {
				collectCommit(c)
			}
			return nil
		})
	}

	if err != nil {
		return nil, fmt.Errorf("error iterating commits: %w", err)
	}

	return commits, nil
}

// scopeTranscriptForCheckpoint slices a transcript to include only the portion
// relevant to a specific checkpoint, starting from the given offset.
// For Claude Code (JSONL), the offset is a line number and we slice by line.
// For Gemini (single JSON blob), the offset is a message index and we slice by message.
func scopeTranscriptForCheckpoint(fullTranscript []byte, startOffset int, agentType types.AgentType) []byte {
	switch agentType {
	case agent.AgentTypeGemini:
		scoped, err := geminicli.SliceFromMessage(fullTranscript, startOffset)
		if err != nil {
			return nil
		}
		return scoped
	case agent.AgentTypeOpenCode:
		scoped, err := opencode.SliceFromMessage(fullTranscript, startOffset)
		if err != nil {
			return nil
		}
		return scoped
	case agent.AgentTypeCodex, agent.AgentTypeClaudeCode, agent.AgentTypeCursor, agent.AgentTypeFactoryAIDroid, agent.AgentTypeUnknown:
		return transcript.SliceFromLine(fullTranscript, startOffset)
	}
	return transcript.SliceFromLine(fullTranscript, startOffset)
}

// extractPromptsFromTranscript extracts user prompts from transcript bytes.
// Returns a slice of prompt strings.
func extractPromptsFromTranscript(transcriptBytes []byte, agentType types.AgentType) []string {
	if len(transcriptBytes) == 0 {
		return nil
	}

	// transcriptBytes is read from checkpoint storage, which redacts on write.
	condensed, err := summarize.BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted(transcriptBytes), agentType)
	if err != nil || len(condensed) == 0 {
		condensed, err = buildCondensedCompactTranscriptEntries(transcriptBytes)
	}
	if err != nil || len(condensed) == 0 {
		return nil
	}

	var prompts []string
	for _, entry := range condensed {
		if entry.Type == summarize.EntryTypeUser && entry.Content != "" {
			prompts = append(prompts, entry.Content)
		}
	}
	return prompts
}

// extractIntent picks the user-facing intent line from available prompt sources.
// Preference: first non-empty entry of scopedPrompts, then first non-empty line
// of fallbackPrompts, then "". Truncates to maxIntentDisplayLength.
func extractIntent(scopedPrompts []string, fallbackPrompts string) string {
	for _, p := range scopedPrompts {
		if p == "" {
			continue
		}
		return strategy.TruncateDescription(p, maxIntentDisplayLength)
	}
	for _, line := range strings.Split(fallbackPrompts, "\n") {
		if line == "" {
			continue
		}
		return strategy.TruncateDescription(line, maxIntentDisplayLength)
	}
	return ""
}

// buildNoSummaryMarkdown renders the body for a checkpoint that does not yet
// have an AI summary. It mirrors the `## Intent` / `## Summary` / `## Files`
// shape of the generated case so the brand markdown renderer can take the same
// path. The italic *summary* paragraph is the affordance pointing the user at
// `--generate` (or, for temporary checkpoints, at committing first).
func buildNoSummaryMarkdown(intent string, files []string, summaryHint string) string {
	var sb strings.Builder

	sb.WriteString("## Intent\n\n")
	if intent == "" {
		sb.WriteString("*(no prompt recorded)*\n\n")
	} else {
		fmt.Fprintf(&sb, "%s\n\n", escapeSummaryText(intent))
	}

	fmt.Fprintf(&sb, "## Summary\n\n*%s*\n", escapeSummaryText(summaryHint))

	if len(files) > 0 {
		fmt.Fprintf(&sb, "\n## Files (%d)\n\n", len(files))
		for _, f := range files {
			fmt.Fprintf(&sb, "- `%s`\n", escapeInlineCodeText(f))
		}
	}

	return sb.String()
}

// ambiguousMatch describes one match in an ambiguous-prefix failure.
// SessionID is optional and only set for temporary-checkpoint matches.
type ambiguousMatch struct {
	ShortID   string
	Timestamp time.Time
	SessionID string
}

// renderAmbiguousPrefixFailure prints a styled failure block describing an
// ambiguous prefix. kind is a noun phrase like "commits" or "temporary
// checkpoints" used in the "matches N <kind>" header row.
func renderAmbiguousPrefixFailure(errW io.Writer, prefix, kind string, matches []ambiguousMatch) {
	styles := newStatusStyles(errW)
	rows := []explainRow{
		{Label: "matches", Value: fmt.Sprintf("%d %s", len(matches), kind)},
	}
	for _, m := range matches {
		ts := ""
		if !m.Timestamp.IsZero() {
			ts = "  " + m.Timestamp.Format("2006-01-02 15:04:05")
		}
		sess := ""
		if m.SessionID != "" {
			sess = "  session " + m.SessionID
		}
		rows = append(rows, explainRow{Label: "", Value: "• " + m.ShortID + ts + sess})
	}
	rows = append(rows, explainRow{Label: "hint", Value: "use a longer prefix or a full SHA"})
	label := fmt.Sprintf("Ambiguous checkpoint prefix %q", prefix)
	fmt.Fprint(errW, styles.renderFailure(label, rows))
}

// renderExplainFailure prints a styled failure block to errW and returns the
// error wrapped as *SilentError so main.go does not double-print. Used at
// every explain call site that has a friendly, structured error to surface.
func renderExplainFailure(errW io.Writer, label string, rows []explainRow, structured error) error {
	fmt.Fprint(errW, newStatusStyles(errW).renderFailure(label, rows))
	return NewSilentError(structured)
}

// buildAmbiguousCommitMatches converts a slice of plumbing.Hash matches
// (from resolveCommitUnambiguous) into ambiguousMatch entries with
// abbreviated short IDs and author timestamps. Caps at 5 entries to keep
// the failure block readable when a short prefix collides on many
// commits.
func buildAmbiguousCommitMatches(repo *git.Repository, hashes []plumbing.Hash) []ambiguousMatch {
	const maxMatches = 5
	matches := make([]ambiguousMatch, 0, len(hashes))
	for i, h := range hashes {
		if i >= maxMatches {
			break
		}
		m := ambiguousMatch{ShortID: abbreviateCommitHash(repo, h)}
		if commit, err := repo.CommitObject(h); err == nil {
			m.Timestamp = commit.Author.When
		}
		matches = append(matches, m)
	}
	return matches
}

// buildAmbiguousCheckpointMatches converts a slice of CheckpointID matches
// into ambiguousMatch entries enriched with timestamps and session IDs from
// the loaded committed-checkpoint listing. Caps at 5 entries to keep the
// failure block readable when a short prefix collides on many checkpoints.
func buildAmbiguousCheckpointMatches(ids []id.CheckpointID, committed []checkpoint.CommittedInfo) []ambiguousMatch {
	const maxMatches = 5
	infoByID := make(map[id.CheckpointID]checkpoint.CommittedInfo, len(committed))
	for _, info := range committed {
		infoByID[info.CheckpointID] = info
	}
	matches := make([]ambiguousMatch, 0, len(ids))
	for i, cpID := range ids {
		if i >= maxMatches {
			break
		}
		m := ambiguousMatch{ShortID: cpID.String()}
		if info, ok := infoByID[cpID]; ok {
			m.Timestamp = info.CreatedAt
			m.SessionID = info.SessionID
		}
		matches = append(matches, m)
	}
	return matches
}

// renderExplainBody routes a markdown body through the brand renderer when
// the writer supports color, and returns the markdown source verbatim
// otherwise. Single point of policy for every explain body section.
func renderExplainBody(w io.Writer, md string) string {
	if !shouldUseColor(w) {
		return md
	}
	rendered, err := defaultRenderTerminalMarkdown(w, md)
	if err != nil {
		logging.Debug(context.Background(), "explain markdown render failed", slog.String("error", err.Error()))
		return md
	}
	return rendered
}

// formatCheckpointOutput formats checkpoint data based on verbosity level.
// When verbose is false: summary only (ID, session, timestamp, tokens, intent).
// When verbose is true: adds files, associated commits, and scoped transcript for this checkpoint.
// When full is true: shows parsed full session transcript instead of scoped transcript.
//
// Transcript scope is controlled by CheckpointTranscriptStart in metadata, which indicates
// where this checkpoint's content begins in the full session transcript.
//
// Author is displayed when available (only for committed checkpoints).
// Associated commits are git commits that reference this checkpoint via Entire-Checkpoint trailer.
func formatCheckpointOutput(summary *checkpoint.CheckpointSummary, content *checkpoint.SessionContent, checkpointID id.CheckpointID, associatedCommits []associatedCommit, author checkpoint.Author, verbose, full bool, w io.Writer) string {
	var sb strings.Builder
	meta := content.Metadata
	styles := newStatusStyles(w)

	// Scope the transcript to this checkpoint's portion
	// If CheckpointTranscriptStart > 0, we slice the transcript to only include
	// content from that point onwards (excluding earlier checkpoint content)
	scopedTranscript := scopeTranscriptForCheckpoint(content.Transcript, meta.GetTranscriptStart(), meta.Agent)

	// Extract prompts from the scoped transcript for intent extraction
	scopedPrompts := extractPromptsFromTranscript(scopedTranscript, meta.Agent)

	sb.WriteString(formatCheckpointHeader(summary, meta, checkpointID, associatedCommits, author, styles))
	sb.WriteString(styles.horizontalRule(styles.width))
	sb.WriteString("\n")

	if meta.Summary != nil {
		md := buildSummaryMarkdown(meta.Summary)
		if verbose || full {
			md += buildFilesMarkdown(meta.FilesTouched)
		}
		if shouldUseColor(w) {
			rendered, err := defaultRenderTerminalMarkdown(w, md)
			if err != nil {
				logging.Debug(context.Background(), "explain markdown render failed", slog.String("error", err.Error()))
				sb.WriteString(md)
			} else {
				sb.WriteString(rendered)
			}
		} else {
			sb.WriteString(md)
		}
	} else {
		intent := extractIntent(scopedPrompts, content.Prompts)

		var files []string
		if verbose || full {
			files = meta.FilesTouched
		}

		hint := fmt.Sprintf("Not generated yet. Run `entire explain --generate %s` to create an AI summary.", checkpointID)
		md := buildNoSummaryMarkdown(intent, files, hint)
		sb.WriteString(renderExplainBody(w, md))
	}

	if verbose || full {
		label := "Transcript (checkpoint scope)"
		if full {
			label = "Transcript (full session)"
		}
		sb.WriteString("\n")
		sb.WriteString(styles.sectionRule(label, styles.width))
		sb.WriteString("\n")
		appendTranscriptSection(&sb, verbose, full, content.Transcript, scopedTranscript, content.Prompts, meta.Agent)
	}

	return sb.String()
}

// appendTranscriptSection appends the appropriate transcript section to the builder
// based on verbosity level. Full mode shows the entire session, verbose shows checkpoint scope.
// fullTranscript is the entire session transcript, scopedContent is either scoped transcript bytes
// or a pre-formatted string (for backwards compat), and scopedFallback is used when scoped parsing fails.
func appendTranscriptSection(sb *strings.Builder, verbose, full bool, fullTranscript, scopedTranscript []byte, scopedFallback string, agentType types.AgentType) {
	switch {
	case full:
		sb.WriteString(formatTranscriptBytes(fullTranscript, "", agentType))

	case verbose:
		sb.WriteString(formatTranscriptBytes(scopedTranscript, scopedFallback, agentType))
	}
}

// formatTranscriptBytes formats transcript bytes into a human-readable string.
// It parses the transcript (JSONL for Claude, JSON for Gemini) and formats it using the condensed format.
// The fallback is used for backwards compatibility when transcript parsing fails or is empty.
func formatTranscriptBytes(transcriptBytes []byte, fallback string, agentType types.AgentType) string {
	if len(transcriptBytes) == 0 {
		if fallback != "" {
			return fallback + "\n"
		}
		return "  (none)\n"
	}

	// transcriptBytes is read from checkpoint storage, which redacts on write.
	condensed, err := summarize.BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted(transcriptBytes), agentType)
	if err != nil || len(condensed) == 0 {
		condensed, err = buildCondensedCompactTranscriptEntries(transcriptBytes)
	}
	if err != nil || len(condensed) == 0 {
		if fallback != "" {
			return fallback + "\n"
		}
		return "  (failed to parse transcript)\n"
	}

	input := summarize.Input{Transcript: condensed}
	return summarize.FormatCondensedTranscript(input)
}

func buildCondensedCompactTranscriptEntries(transcriptBytes []byte) ([]summarize.Entry, error) {
	compactEntries, err := transcriptcompact.BuildCondensedEntries(transcriptBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing compact transcript: %w", err)
	}

	entries := make([]summarize.Entry, 0, len(compactEntries))
	for _, entry := range compactEntries {
		switch entry.Type {
		case "user":
			entries = append(entries, summarize.Entry{Type: summarize.EntryTypeUser, Content: entry.Content})
		case "assistant":
			entries = append(entries, summarize.Entry{Type: summarize.EntryTypeAssistant, Content: entry.Content})
		case "tool":
			entries = append(entries, summarize.Entry{Type: summarize.EntryTypeTool, ToolName: entry.ToolName, ToolDetail: entry.ToolDetail})
		}
	}

	if len(entries) == 0 {
		return nil, errors.New("no parseable compact transcript entries")
	}

	return entries, nil
}

// formatCheckpointHeader builds the metadata block above the summary body.
// When color is enabled, values are styled with the shared status palette;
// otherwise the same compact shape is returned as plain text.
func formatCheckpointHeader(
	summary *checkpoint.CheckpointSummary,
	meta checkpoint.CommittedMetadata,
	cpID id.CheckpointID,
	commits []associatedCommit,
	author checkpoint.Author,
	styles statusStyles,
) string {
	var sb strings.Builder

	headline := "● Checkpoint " + cpID.String()
	if styles.colorEnabled {
		bullet := styles.render(lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")), "●")
		key := styles.render(styles.bold, "Checkpoint")
		val := styles.render(lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")), cpID.String())
		headline = bullet + " " + key + " " + val
	}
	sb.WriteString(headline)
	sb.WriteString("\n")

	writeRow := func(label, value string) {
		paddedLabel := fmt.Sprintf("%-9s", label)
		if styles.colorEnabled {
			paddedLabel = styles.render(styles.dim, paddedLabel)
		}
		fmt.Fprintf(&sb, "  %s%s\n", paddedLabel, value)
	}

	writeRow("session", meta.SessionID)
	writeRow("created", meta.CreatedAt.Format("2006-01-02 15:04:05"))
	if author.Name != "" {
		writeRow("author", fmt.Sprintf("%s <%s>", author.Name, author.Email))
	}

	tokenUsage := meta.TokenUsage
	if tokenUsage == nil && summary != nil {
		tokenUsage = summary.TokenUsage
	}
	if tokenUsage != nil {
		total := tokenUsage.InputTokens + tokenUsage.CacheCreationTokens +
			tokenUsage.CacheReadTokens + tokenUsage.OutputTokens
		tokensVal := formatTokenCount(total)
		if styles.colorEnabled {
			tokensVal = styles.render(styles.yellow, tokensVal)
		}
		writeRow("tokens", tokensVal)
	}

	switch {
	case commits == nil:
	case len(commits) == 0:
		writeRow("commits", "(none on this branch)")
	case len(commits) == 1:
		c := commits[0]
		writeRow("commits", fmt.Sprintf("%s %s", c.ShortSHA, c.Message))
	default:
		writeRow("commits", fmt.Sprintf("(%d)", len(commits)))
		for _, c := range commits {
			fmt.Fprintf(&sb, "           %s %s %s\n",
				c.ShortSHA, c.Date.Format("2006-01-02"), c.Message)
		}
	}

	return sb.String()
}

// buildFilesMarkdown renders touched files as a markdown block for verbose
// and full output when an AI summary is present.
func buildFilesMarkdown(files []string) string {
	if len(files) == 0 {
		return "\n## Files\n\n*(none)*\n"
	}
	var sb strings.Builder
	sb.WriteString("\n## Files\n\n")
	for _, f := range files {
		fmt.Fprintf(&sb, "- `%s`\n", escapeInlineCodeText(f))
	}
	return sb.String()
}

// buildSummaryMarkdown renders a checkpoint AI summary into the brand
// markdown shape used by entire's TTY renderer. The output is also the
// source of truth for non-TTY callers, which write it verbatim.
func buildSummaryMarkdown(s *checkpoint.Summary) string {
	if s == nil {
		return ""
	}
	var sb strings.Builder

	fmt.Fprintf(&sb, "## Intent\n\n%s\n\n", escapeSummaryText(s.Intent))
	fmt.Fprintf(&sb, "## Outcome\n\n%s\n\n", escapeSummaryText(s.Outcome))

	if hasAnyLearning(s.Learnings) {
		sb.WriteString("## Learnings\n\n")
		if len(s.Learnings.Repo) > 0 {
			sb.WriteString("### Repository\n\n")
			for _, item := range s.Learnings.Repo {
				fmt.Fprintf(&sb, "- %s\n", escapeSummaryText(item))
			}
			sb.WriteString("\n")
		}
		if len(s.Learnings.Code) > 0 {
			sb.WriteString("### Code\n\n")
			for _, item := range s.Learnings.Code {
				fmt.Fprintf(&sb, "- %s\n", formatCodeLearning(item))
			}
			sb.WriteString("\n")
		}
		if len(s.Learnings.Workflow) > 0 {
			sb.WriteString("### Workflow\n\n")
			for _, item := range s.Learnings.Workflow {
				fmt.Fprintf(&sb, "- %s\n", escapeSummaryText(item))
			}
			sb.WriteString("\n")
		}
	}

	if len(s.Friction) > 0 {
		sb.WriteString("## Friction\n\n")
		for _, item := range s.Friction {
			fmt.Fprintf(&sb, "- %s\n", escapeSummaryText(item))
		}
		sb.WriteString("\n")
	}

	if len(s.OpenItems) > 0 {
		sb.WriteString("## Open Items\n\n")
		for _, item := range s.OpenItems {
			fmt.Fprintf(&sb, "- %s\n", escapeSummaryText(item))
		}
		sb.WriteString("\n")
	}

	return strings.TrimRight(sb.String(), "\n") + "\n"
}

func hasAnyLearning(l checkpoint.LearningsSummary) bool {
	return len(l.Repo) > 0 || len(l.Code) > 0 || len(l.Workflow) > 0
}

func formatCodeLearning(c checkpoint.CodeLearning) string {
	path := escapeSummaryText(c.Path)
	finding := escapeSummaryText(c.Finding)
	switch {
	case c.Line > 0 && c.EndLine > 0:
		return fmt.Sprintf("`%s:%d-%d` — %s", path, c.Line, c.EndLine, finding)
	case c.Line > 0:
		return fmt.Sprintf("`%s:%d` — %s", path, c.Line, finding)
	default:
		return fmt.Sprintf("`%s` — %s", path, finding)
	}
}

func escapeSummaryText(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "`", "‘")
}

func escapeInlineCodeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "`", "‘")
}

// runExplainDefault shows all checkpoints on the current branch.
// This is the default view when no flags are provided.
func runExplainDefault(ctx context.Context, w io.Writer, noPager bool) error {
	return runExplainBranchDefault(ctx, w, noPager)
}

// branchCheckpointsLimit is the max checkpoints to show in branch view
const branchCheckpointsLimit = 100

// commitScanLimit is how far back to scan git history for checkpoints
const commitScanLimit = 500

// errStopIteration is used to stop commit iteration early
var errStopIteration = errors.New("stop iteration")

// getCurrentWorktreeHash returns the hashed worktree ID for the current working directory.
// This is used to filter shadow branches to only those belonging to this worktree.
func getCurrentWorktreeHash(ctx context.Context) string {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return ""
	}
	worktreeID, err := paths.GetWorktreeID(repoRoot)
	if err != nil {
		return ""
	}
	return checkpoint.HashWorktreeID(worktreeID)
}

// computeReachableFromMain returns a set of commit hashes on the main/default branch's first-parent chain.
// On the default branch itself, returns an empty map (no filtering needed).
// Only first-parent commits are included — commits from side branches merged into main are excluded,
// since those could be feature branch commits that shouldn't be filtered out.
func computeReachableFromMain(ctx context.Context, repo *git.Repository) map[plumbing.Hash]bool {
	reachableFromMain := make(map[plumbing.Hash]bool)

	isOnDefault, _ := strategy.IsOnDefaultBranch(repo)
	if isOnDefault {
		return reachableFromMain // No filtering needed on default branch
	}

	// Resolve main branch hash
	var mainBranchHash plumbing.Hash
	if defaultBranchName := strategy.GetDefaultBranchName(repo); defaultBranchName != "" {
		ref, refErr := repo.Reference(plumbing.ReferenceName("refs/heads/"+defaultBranchName), true)
		if refErr != nil {
			ref, refErr = repo.Reference(plumbing.ReferenceName("refs/remotes/origin/"+defaultBranchName), true)
		}
		if refErr == nil {
			mainBranchHash = ref.Hash()
		}
	}
	if mainBranchHash == plumbing.ZeroHash {
		mainBranchHash = strategy.GetMainBranchHash(repo)
	}
	if mainBranchHash == plumbing.ZeroHash {
		return reachableFromMain
	}

	// Walk main's first-parent chain to build the set
	_ = walkFirstParentCommits(ctx, repo, mainBranchHash, strategy.MaxCommitTraversalDepth, func(c *object.Commit) error { //nolint:errcheck // Best-effort
		reachableFromMain[c.Hash] = true
		return nil
	})

	return reachableFromMain
}

// walkFirstParentCommits walks the first-parent chain starting from `from`,
// calling fn for each commit. It stops after visiting `limit` commits (0 = no limit).
// This avoids the full DAG traversal that repo.Log() does, which follows ALL parents
// of merge commits and can walk into unrelated branch history (e.g., main's full
// history after merging main into a feature branch).
func walkFirstParentCommits(ctx context.Context, repo *git.Repository, from plumbing.Hash, limit int, fn func(*object.Commit) error) error {
	current, err := repo.CommitObject(from)
	if err != nil {
		return fmt.Errorf("failed to get commit %s: %w", from, err)
	}

	for count := 0; limit <= 0 || count < limit; count++ {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // Propagating context cancellation
		}
		if err := fn(current); err != nil {
			if errors.Is(err, errStopIteration) {
				return nil
			}
			return err
		}

		// Follow first parent only (skip merge parents).
		// When there are no parents or parent lookup fails, we've reached the
		// end of the chain — this is a normal termination, not an error.
		if current.NumParents() == 0 {
			return nil
		}
		parentHash := current.Hash
		current, err = current.Parent(0)
		if err != nil {
			return fmt.Errorf("failed to load first parent of commit %s: %w", parentHash, err)
		}
	}
	return nil
}

// getBranchCheckpoints returns checkpoints relevant to the current branch.
// This is strategy-agnostic - it queries checkpoints directly from the checkpoint store.
//
// Behavior:
//   - On feature branches: only show checkpoints unique to this branch (not in main)
//   - On default branch (main/master): show all checkpoints in history (up to limit)
//   - Includes both committed checkpoints (entire/checkpoints/v1) and temporary checkpoints (shadow branches)
func getBranchCheckpoints(ctx context.Context, repo *git.Repository, limit int) ([]strategy.RewindPoint, error) {
	// Warn (once per process) if metadata branches are disconnected
	strategy.WarnIfMetadataDisconnected()

	v1Store := checkpoint.NewGitStore(repo)
	v2URL, err := remote.FetchURL(ctx)
	if err != nil {
		logging.Debug(ctx, "explain: using origin for branch checkpoint v2 store fetch remote",
			slog.String("error", err.Error()),
		)
		v2URL = ""
	}
	v2Store := checkpoint.NewV2GitStore(repo, v2URL)
	preferCheckpointsV2 := settings.IsCheckpointsV2Enabled(ctx)

	// Get all committed checkpoints for lookup (v2-aware with v1 fallback).
	committedInfos, err := listCommittedForExplain(ctx, v1Store, v2Store, preferCheckpointsV2)
	if err != nil {
		committedInfos = nil // Continue without committed checkpoints
	}

	// Build map of checkpoint ID -> committed info
	committedByID := make(map[id.CheckpointID]checkpoint.CommittedInfo)
	for _, info := range committedInfos {
		if !info.CheckpointID.IsEmpty() {
			committedByID[info.CheckpointID] = info
		}
	}

	head, err := repo.Head()
	if err != nil {
		// Unborn HEAD (no commits yet) - return empty list instead of erroring
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return []strategy.RewindPoint{}, nil
		}
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Check if we're on the default branch (needed for getReachableTemporaryCheckpoints)
	isOnDefault, _ := strategy.IsOnDefaultBranch(repo)

	// Fetch metadata trees for reading session prompts (cheap tree lookups).
	// Try v2 /main first, fall back to v1 metadata branch.
	v1MetadataTree, _ := strategy.GetMetadataBranchTree(repo)   //nolint:errcheck // Best-effort
	v2MetadataTree, _ := strategy.GetV2MetadataBranchTree(repo) //nolint:errcheck // Best-effort
	promptTree := resolvePromptTree(v1MetadataTree, v2MetadataTree, preferCheckpointsV2)

	var points []strategy.RewindPoint

	collectCheckpoint := func(c *object.Commit) {
		cpID, found := trailers.ParseCheckpoint(c.Message)
		if !found {
			return
		}
		cpInfo, found := committedByID[cpID]
		if !found {
			return
		}

		message := strings.Split(c.Message, "\n")[0]
		point := strategy.RewindPoint{
			ID:               c.Hash.String(),
			Message:          message,
			Date:             c.Committer.When,
			IsLogsOnly:       true, // Committed checkpoints are logs-only
			CheckpointID:     cpID,
			SessionID:        cpInfo.SessionID,
			IsTaskCheckpoint: cpInfo.IsTask,
			ToolUseID:        cpInfo.ToolUseID,
			Agent:            cpInfo.Agent,
		}
		// Read session prompt from metadata tree (best-effort).
		// Read prompt.txt directly from the latest session subdirectory instead of
		// parsing the full transcript — prompt.txt is tiny vs multi-MB transcripts.
		if promptTree != nil {
			point.SessionPrompt = strategy.ReadLatestSessionPromptFromCommittedTree(promptTree, cpID, cpInfo.SessionCount)
		}

		points = append(points, point)
	}

	if isOnDefault {
		// On the default branch, use full DAG walk to find checkpoint commits
		// on merged feature branches (second parents of merge commits).
		iter, iterErr := repo.Log(&git.LogOptions{
			From:  head.Hash(),
			Order: git.LogOrderCommitterTime,
		})
		if iterErr != nil {
			return nil, fmt.Errorf("failed to get commit log: %w", iterErr)
		}
		defer iter.Close()

		count := 0
		err = iter.ForEach(func(c *object.Commit) error {
			if err := ctx.Err(); err != nil {
				return err //nolint:wrapcheck // Propagating context cancellation
			}
			if count >= commitScanLimit {
				return storer.ErrStop
			}
			count++
			collectCheckpoint(c)
			return nil
		})
	} else {
		// On feature branches, use first-parent walk with branch filtering.
		// This avoids walking into main's full history through merge commit parents.
		reachableFromMain := computeReachableFromMain(ctx, repo)

		err = walkFirstParentCommits(ctx, repo, head.Hash(), commitScanLimit, func(c *object.Commit) error {
			// Once we hit a commit reachable from main on the first-parent chain,
			// all earlier ancestors are also shared-with-main, so stop scanning.
			if reachableFromMain[c.Hash] {
				return errStopIteration
			}
			collectCheckpoint(c)
			return nil
		})
	}

	if err != nil {
		return nil, fmt.Errorf("error iterating commits: %w", err)
	}

	// Get temporary checkpoints from ALL shadow branches whose base commit is reachable from HEAD.
	tempPoints := getReachableTemporaryCheckpoints(ctx, repo, v1Store, head.Hash(), isOnDefault, limit)
	points = append(points, tempPoints...)

	// Sort by date, most recent first
	sort.Slice(points, func(i, j int) bool {
		return points[i].Date.After(points[j].Date)
	})

	// Apply limit
	if len(points) > limit {
		points = points[:limit]
	}

	return points, nil
}

// getReachableTemporaryCheckpoints returns temporary checkpoints from shadow branches
// whose base commit is reachable from the given HEAD hash and that belong to this worktree.
// For default branches, all shadow branches for this worktree are included.
// For feature branches, only shadow branches whose base commit is in HEAD's history are included.
func getReachableTemporaryCheckpoints(ctx context.Context, repo *git.Repository, store *checkpoint.GitStore, headHash plumbing.Hash, isOnDefault bool, limit int) []strategy.RewindPoint {
	var points []strategy.RewindPoint

	// Compute current worktree's hash for filtering shadow branches
	currentWorktreeHash := getCurrentWorktreeHash(ctx)

	shadowBranches, _ := store.ListTemporary(ctx) //nolint:errcheck // Best-effort
	for _, sb := range shadowBranches {
		// Filter by worktree: only show shadow branches belonging to this worktree.
		// Skip filtering if currentWorktreeHash is empty (error computing it) to avoid
		// accidentally filtering out ALL shadow branches.
		_, branchWorktreeHash, parsed := checkpoint.ParseShadowBranchName(sb.BranchName)
		if currentWorktreeHash != "" && parsed && branchWorktreeHash != "" && branchWorktreeHash != currentWorktreeHash {
			continue
		}

		// Check if this shadow branch's base commit is reachable from current HEAD
		if !isShadowBranchReachable(ctx, repo, sb.BaseCommit, headHash, isOnDefault) {
			continue
		}

		// List checkpoints from this shadow branch
		tempCheckpoints, _ := store.ListCheckpointsForBranch(ctx, sb.BranchName, "", limit) //nolint:errcheck // Best-effort
		for _, tc := range tempCheckpoints {
			point := convertTemporaryCheckpoint(repo, tc)
			if point != nil {
				points = append(points, *point)
			}
		}
	}

	return points
}

// isShadowBranchReachable checks if a shadow branch's base commit is reachable from HEAD.
// For default branches, all shadow branches are considered reachable.
// For feature branches, we check if any commit with the base commit prefix is in HEAD's history.
func isShadowBranchReachable(ctx context.Context, repo *git.Repository, baseCommit string, headHash plumbing.Hash, isOnDefault bool) bool {
	// For default branch: all shadow branches are potentially relevant
	if isOnDefault {
		return true
	}

	// Check if base commit hash prefix matches any commit in HEAD's first-parent chain
	found := false
	_ = walkFirstParentCommits(ctx, repo, headHash, commitScanLimit, func(c *object.Commit) error { //nolint:errcheck // Best-effort
		if strings.HasPrefix(c.Hash.String(), baseCommit) {
			found = true
			return errStopIteration
		}
		return nil
	})

	return found
}

// convertTemporaryCheckpoint converts a TemporaryCheckpointInfo to a RewindPoint.
// Returns nil if the checkpoint should be skipped (no tree changes or can't be read).
//
// Filtering uses hasAnyChanges (O(1) tree hash comparison) rather than hasCodeChanges
// (O(files) full diff). This means metadata-only checkpoints (.entire/ changes without
// code changes) are kept — only true no-ops (identical tree as parent) are dropped.
// This trade-off is intentional for list-view performance.
func convertTemporaryCheckpoint(repo *git.Repository, tc checkpoint.TemporaryCheckpointInfo) *strategy.RewindPoint {
	shadowCommit, commitErr := repo.CommitObject(tc.CommitHash)
	if commitErr != nil {
		return nil
	}

	// Skip no-op commits where the tree is identical to the parent's.
	// Note: this keeps metadata-only changes (e.g. transcript updates in .entire/)
	// since those produce a different tree hash. See hasAnyChanges godoc.
	if !hasAnyChanges(shadowCommit) {
		return nil
	}

	// Read session prompt from the shadow branch commit's tree (not from entire/checkpoints/v1)
	// Temporary checkpoints store their metadata in the shadow branch, not in entire/checkpoints/v1
	var sessionPrompt string
	shadowTree, treeErr := shadowCommit.Tree()
	if treeErr == nil {
		sessionPrompt = strategy.ReadSessionPromptFromTree(shadowTree, tc.MetadataDir)
	}

	return &strategy.RewindPoint{
		ID:               tc.CommitHash.String(),
		Message:          tc.Message,
		MetadataDir:      tc.MetadataDir,
		Date:             tc.Timestamp,
		IsTaskCheckpoint: tc.IsTaskCheckpoint,
		ToolUseID:        tc.ToolUseID,
		SessionID:        tc.SessionID,
		SessionPrompt:    sessionPrompt,
		IsLogsOnly:       false, // Temporary checkpoints can be fully rewound
	}
}

// runExplainBranchWithFilter shows checkpoints on the current branch, optionally filtered by session.
// This is strategy-agnostic - it queries checkpoints directly.
func runExplainBranchWithFilter(ctx context.Context, w io.Writer, noPager bool, sessionFilter string) error {
	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Get current branch name
	branchName := strategy.GetCurrentBranchName(repo)
	if branchName == "" {
		// Detached HEAD state or unborn HEAD - try to use short commit hash if possible
		head, headErr := repo.Head()
		if headErr != nil {
			// Unborn HEAD (no commits yet) - treat as empty history instead of erroring
			if errors.Is(headErr, plumbing.ErrReferenceNotFound) {
				branchName = "HEAD (no commits yet)"
			} else {
				return fmt.Errorf("failed to get HEAD: %w", headErr)
			}
		} else {
			branchName = "HEAD (" + head.Hash().String()[:7] + ")"
		}
	}

	// Get checkpoints for this branch (strategy-agnostic)
	points, err := getBranchCheckpoints(ctx, repo, branchCheckpointsLimit)
	if err != nil {
		// If context was cancelled (e.g. user hit Ctrl+C), exit silently
		if ctx.Err() != nil {
			return NewSilentError(ctx.Err())
		}
		// Log the error but continue with empty list so user sees helpful message
		logging.Warn(ctx, "failed to get branch checkpoints", "error", err)
		points = nil
	}

	// Format output
	output := formatBranchCheckpoints(w, branchName, points, sessionFilter)

	outputExplainContent(w, output, noPager)
	return nil
}

// runExplainBranchDefault shows all checkpoints on the current branch grouped by date.
// This is a convenience wrapper that calls runExplainBranchWithFilter with no filter.
func runExplainBranchDefault(ctx context.Context, w io.Writer, noPager bool) error {
	return runExplainBranchWithFilter(ctx, w, noPager, "")
}

// outputExplainContent outputs content with optional pager support.
func outputExplainContent(w io.Writer, content string, noPager bool) {
	if noPager {
		fmt.Fprint(w, content)
	} else {
		outputWithPager(w, content)
	}
}

// runExplainCommit looks up the checkpoint associated with a commit.
// Extracts the Entire-Checkpoint trailer and delegates to checkpoint detail view.
// If no trailer found, shows a message indicating no associated checkpoint.
func runExplainCommit(ctx context.Context, w, errW io.Writer, commitRef string, noPager, verbose, full, rawTranscript, generate, force, searchAll bool) error {
	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Resolve the commit reference, erroring on hex-prefix ambiguity
	// instead of silently picking the first matching commit.
	hash, ambiguousMatches, err := resolveCommitUnambiguous(repo, commitRef)
	if err != nil {
		if errors.Is(err, errAmbiguousCommitPrefix) {
			renderAmbiguousPrefixFailure(errW, commitRef, "commits", buildAmbiguousCommitMatches(repo, ambiguousMatches))
			return NewSilentError(err)
		}
		return renderExplainFailure(errW, "Commit not found", []explainRow{
			{Label: "ref", Value: commitRef},
		}, fmt.Errorf("commit not found: %s", commitRef))
	}

	commit, err := repo.CommitObject(hash)
	if err != nil {
		return fmt.Errorf("failed to get commit: %w", err)
	}

	// Extract Entire-Checkpoint trailer
	checkpointID, hasCheckpoint := trailers.ParseCheckpoint(commit.Message)
	if !hasCheckpoint {
		// Side-effect modes must error so scripts can distinguish "done"
		// from "didn't happen"; read-only modes print a friendly message.
		if generate || rawTranscript {
			return fmt.Errorf("cannot %s: commit %s has no Entire-Checkpoint trailer", generateOrRawLabel(generate), abbreviateCommitHash(repo, hash))
		}
		printNoTrailerMessage(w, repo, hash)
		return nil
	}

	// Delegate to checkpoint detail view, forwarding the full flag set so
	// --generate / --raw-transcript / --force work via --commit as well.
	return runExplainCheckpoint(ctx, w, errW, checkpointID.String(), noPager, verbose, full, rawTranscript, generate, force, searchAll)
}

// formatSessionInfo formats session information for display.
//
// NOTE: This function has no production caller — `entire explain --session`
// flows through formatBranchCheckpoints (the list view filtered by session),
// not through here. It is kept for tests that exercise the per-checkpoint
// markdown body shape used elsewhere; restyling it for the brand format was
// not worth the diff. If the CLI ever grows a session-detail surface, revisit.
func formatSessionInfo(session *strategy.Session, sourceRef string, checkpoints []checkpointDetail) string {
	var sb strings.Builder

	// Session header
	fmt.Fprintf(&sb, "Session: %s\n", session.ID)
	fmt.Fprintf(&sb, "Strategy: %s\n", session.Strategy)

	if !session.StartTime.IsZero() {
		fmt.Fprintf(&sb, "Started: %s\n", session.StartTime.Format("2006-01-02 15:04:05"))
	}

	if sourceRef != "" {
		fmt.Fprintf(&sb, "Source Ref: %s\n", sourceRef)
	}

	fmt.Fprintf(&sb, "Checkpoints: %d\n", len(checkpoints))

	// Checkpoint details
	for _, cp := range checkpoints {
		sb.WriteString("\n")

		// Checkpoint header
		taskMarker := ""
		if cp.IsTaskCheckpoint {
			taskMarker = " [Task]"
		}
		fmt.Fprintf(&sb, "─── Checkpoint %d [%s] %s%s ───\n",
			cp.Index, cp.ShortID, cp.Timestamp.Format("2006-01-02 15:04"), taskMarker)
		sb.WriteString("\n")

		// Display all interactions in this checkpoint
		for i, inter := range cp.Interactions {
			// For multiple interactions, add a sub-header
			if len(cp.Interactions) > 1 {
				fmt.Fprintf(&sb, "### Interaction %d\n\n", i+1)
			}

			// Prompt section
			if inter.Prompt != "" {
				sb.WriteString("## Prompt\n\n")
				sb.WriteString(inter.Prompt)
				sb.WriteString("\n\n")
			}

			// Response section
			if len(inter.Responses) > 0 {
				sb.WriteString("## Responses\n\n")
				sb.WriteString(strings.Join(inter.Responses, "\n\n"))
				sb.WriteString("\n\n")
			}

			// Files modified for this interaction
			if len(inter.Files) > 0 {
				fmt.Fprintf(&sb, "Files Modified (%d):\n", len(inter.Files))
				for _, file := range inter.Files {
					fmt.Fprintf(&sb, "  - %s\n", file)
				}
				sb.WriteString("\n")
			}
		}

		// If no interactions, show message and/or files
		if len(cp.Interactions) == 0 {
			// Show commit message as summary when no transcript available
			if cp.Message != "" {
				sb.WriteString(cp.Message)
				sb.WriteString("\n\n")
			}
			// Show aggregate files if available
			if len(cp.Files) > 0 {
				fmt.Fprintf(&sb, "Files Modified (%d):\n", len(cp.Files))
				for _, file := range cp.Files {
					fmt.Fprintf(&sb, "  - %s\n", file)
				}
			}
		}
	}

	return sb.String()
}

// pagerLookupEnv is overridable for tests so pager env-gate behavior can be
// asserted without depending on the host's PAGER / LESS settings.
var pagerLookupEnv = os.Getenv

// buildPagerCmd constructs the pager subprocess and injects LESS=-R when the
// default Unix pager is less and the user has not customized PAGER or LESS.
func buildPagerCmd(ctx context.Context) (*exec.Cmd, string) {
	pager := pagerLookupEnv(pagerEnvVar)
	if pager == "" {
		if runtime.GOOS == windowsGOOS {
			pager = "more"
		} else {
			pager = lessPagerName
		}
	}

	cmd := exec.CommandContext(ctx, pager)
	if pager == lessPagerName && pagerLookupEnv(pagerEnvVar) == "" && pagerLookupEnv(lessEnvVar) == "" {
		cmd.Env = upsertEnv(os.Environ(), lessEnvVar, "-R")
	}
	return cmd, pager
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	entry := prefix + value
	result := make([]string, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			if !replaced {
				result = append(result, entry)
				replaced = true
			}
			continue
		}
		result = append(result, e)
	}
	if !replaced {
		result = append(result, entry)
	}
	return result
}

// outputWithPager outputs content through a pager if stdout is a terminal and content is long.
func outputWithPager(w io.Writer, content string) {
	// Check if we're writing to stdout and it's a terminal
	if f, ok := w.(*os.File); ok && f == os.Stdout && interactive.IsTerminalWriter(w) {
		// Get terminal height
		_, height, err := term.GetSize(int(f.Fd())) //nolint:gosec // G115: same as above
		if err != nil {
			height = 24 // Default fallback
		}

		// Count lines in content
		lineCount := strings.Count(content, "\n")

		// Use pager if content exceeds terminal height
		if lineCount > height-2 {
			// Use context.Background() intentionally — pagers are interactive
			// processes that handle signals (including SIGINT) themselves.
			// Using the cancellable ctx would cause exec.CommandContext to
			// SIGKILL the pager on Ctrl+C, preventing it from restoring
			// terminal state (raw mode, echo, etc.).
			cmd, _ := buildPagerCmd(context.Background())
			cmd.Stdin = strings.NewReader(content)
			cmd.Stdout = f
			cmd.Stderr = os.Stderr

			if err := cmd.Run(); err != nil {
				// Fallback to direct output if pager fails
				fmt.Fprint(w, content)
			}
			return
		}
	}

	// Direct output for non-terminal or short content
	fmt.Fprint(w, content)
}

// Constants for formatting output
const (
	// maxIntentDisplayLength is the maximum length for intent text before truncation
	maxIntentDisplayLength = 80
	// maxMessageDisplayLength is the maximum length for checkpoint messages before truncation
	maxMessageDisplayLength = 80
	// maxPromptDisplayLength is the maximum length for session prompts before truncation
	maxPromptDisplayLength = 60
	// checkpointIDDisplayLength is the number of characters to show from checkpoint IDs
	checkpointIDDisplayLength = 12
)

// formatBranchCheckpoints formats checkpoint information for a branch.
// Groups commits by checkpoint ID and shows the prompt for each checkpoint.
// If sessionFilter is non-empty, only shows checkpoints matching that session ID (or prefix).
func formatBranchCheckpoints(w io.Writer, branchName string, points []strategy.RewindPoint, sessionFilter string) string {
	var sb strings.Builder
	styles := newStatusStyles(w)

	// Filter by session if specified (must happen before counting)
	if sessionFilter != "" {
		var filtered []strategy.RewindPoint
		for _, p := range points {
			if p.SessionID == sessionFilter || strings.HasPrefix(p.SessionID, sessionFilter) {
				filtered = append(filtered, p)
			}
		}
		points = filtered
	}

	// Group by checkpoint ID so the count matches the rendered group count
	groups := groupByCheckpointID(points)

	branchRows := []explainRow{
		{Label: "branch", Value: branchName},
	}
	if sessionFilter != "" {
		branchRows = append(branchRows, explainRow{Label: "session", Value: sessionFilter})
	}
	branchRows = append(branchRows, explainRow{Label: "checkpoints", Value: strconv.Itoa(len(groups))})

	sb.WriteString(styles.metadataRows(branchRows))
	sb.WriteString("\n")

	if len(groups) == 0 {
		sb.WriteString("No checkpoints found on this branch.\n")
		sb.WriteString("Checkpoints will appear here after you save changes during an agent session.\n")
		return sb.String()
	}

	// Output each checkpoint group
	for _, group := range groups {
		formatCheckpointGroup(&sb, group, styles)
		sb.WriteString("\n")
	}

	return sb.String()
}

// checkpointGroup represents a group of commits sharing the same checkpoint ID.
type checkpointGroup struct {
	checkpointID string
	prompt       string
	isTemporary  bool // true if any commit is not logs-only (can be rewound)
	isTask       bool // true if this is a task checkpoint
	commits      []commitEntry
}

// commitEntry represents a single git commit within a checkpoint.
type commitEntry struct {
	date    time.Time
	gitSHA  string // short git SHA
	message string
}

// groupByCheckpointID groups rewind points by their checkpoint ID.
// Returns groups sorted by latest commit timestamp (most recent first).
func groupByCheckpointID(points []strategy.RewindPoint) []checkpointGroup {
	if len(points) == 0 {
		return nil
	}

	// Build map of checkpoint ID -> group
	groupMap := make(map[string]*checkpointGroup)
	var order []string // Track insertion order for stable iteration

	for _, point := range points {
		// Determine the checkpoint ID to use for grouping
		cpID := point.CheckpointID.String()
		if cpID == "" {
			// Temporary checkpoints: group by session ID to preserve per-session prompts
			// Use session ID prefix for readability (format: YYYY-MM-DD-uuid)
			cpID = point.SessionID
			if cpID == "" {
				cpID = "temporary" // Fallback if no session ID
			}
		}

		group, exists := groupMap[cpID]
		if !exists {
			group = &checkpointGroup{
				checkpointID: cpID,
				prompt:       point.SessionPrompt,
				isTemporary:  !point.IsLogsOnly,
				isTask:       point.IsTaskCheckpoint,
			}
			groupMap[cpID] = group
			order = append(order, cpID)
		}

		// Short git SHA (7 chars)
		gitSHA := point.ID
		if len(gitSHA) > 7 {
			gitSHA = gitSHA[:7]
		}

		group.commits = append(group.commits, commitEntry{
			date:    point.Date,
			gitSHA:  gitSHA,
			message: point.Message,
		})

		// Update flags - if any commit is temporary/task, the group is too
		if !point.IsLogsOnly {
			group.isTemporary = true
		}
		if point.IsTaskCheckpoint {
			group.isTask = true
		}
		// Update prompt if the group's prompt is empty but this point has one
		if group.prompt == "" && point.SessionPrompt != "" {
			group.prompt = point.SessionPrompt
		}
	}

	// Sort commits within each group by date (most recent first)
	for _, group := range groupMap {
		sort.Slice(group.commits, func(i, j int) bool {
			return group.commits[i].date.After(group.commits[j].date)
		})
	}

	// Build result slice in order, then sort by latest commit
	result := make([]checkpointGroup, 0, len(order))
	for _, cpID := range order {
		result = append(result, *groupMap[cpID])
	}

	// Sort groups by latest commit timestamp (most recent first)
	sort.Slice(result, func(i, j int) bool {
		// Each group's commits are already sorted, so first commit is latest
		if len(result[i].commits) == 0 {
			return false
		}
		if len(result[j].commits) == 0 {
			return true
		}
		return result[i].commits[0].date.After(result[j].commits[0].date)
	})

	return result
}

// formatCheckpointGroup formats a single checkpoint group for display.
// The list view headline puts the checkpoint ID first (in bold orange),
// followed by indicators and the prompt — which cascades from
// SessionPrompt → latest commit message → dimmed `(no prompt recorded)`.
func formatCheckpointGroup(sb *strings.Builder, group checkpointGroup, styles statusStyles) {
	cpID := group.checkpointID
	if len(cpID) > checkpointIDDisplayLength {
		cpID = cpID[:checkpointIDDisplayLength]
	}

	// Indicators (Task / temporary). Skip [temporary] when cpID already says so.
	var indicators []string
	if group.isTask {
		indicators = append(indicators, "[Task]")
	}
	if group.isTemporary && cpID != "temporary" {
		indicators = append(indicators, "[temporary]")
	}

	// Prompt cascade: SessionPrompt → latest commit message → dimmed placeholder.
	// Quote user prompts; commit subjects render bare.
	var promptText string
	var promptIsPlaceholder bool
	switch {
	case group.prompt != "":
		promptText = fmt.Sprintf("%q", strategy.TruncateDescription(group.prompt, maxPromptDisplayLength))
	case len(group.commits) > 0 && group.commits[0].message != "":
		promptText = strategy.TruncateDescription(group.commits[0].message, maxPromptDisplayLength)
	default:
		promptText = "(no prompt recorded)"
		promptIsPlaceholder = true
	}
	if promptIsPlaceholder {
		promptText = styles.render(styles.dim, promptText)
	}

	// Build suffix: "[Task]  [temporary]  <prompt>" with two-space separators.
	parts := append([]string{}, indicators...)
	parts = append(parts, promptText)
	suffix := strings.Join(parts, "  ")

	sb.WriteString(styles.listIdentityBullet(cpID, suffix))

	// List commits under this checkpoint.
	for _, commit := range group.commits {
		dateTimeStr := commit.date.Format("01-02 15:04")
		message := strategy.TruncateDescription(commit.message, maxMessageDisplayLength)
		fmt.Fprintf(sb, "  %s (%s) %s\n", dateTimeStr, commit.gitSHA, message)
	}
}

// countLines counts the number of lines in a byte slice.
// For JSONL content (where each line ends with \n), this returns the line count.
// Empty content returns 0.
func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	count := 0
	for _, b := range content {
		if b == '\n' {
			count++
		}
	}
	return count
}

// transcriptOffset returns the appropriate offset for scoping a transcript.
// For Claude Code (JSONL), this is the line count. For Gemini (JSON), this is the message count.
func transcriptOffset(transcriptBytes []byte, agentType types.AgentType) int {
	switch agentType {
	case agent.AgentTypeGemini:
		t, err := geminicli.ParseTranscript(transcriptBytes)
		if err != nil {
			return 0
		}
		return len(t.Messages)
	case agent.AgentTypeClaudeCode, agent.AgentTypeOpenCode, agent.AgentTypeCursor, agent.AgentTypeFactoryAIDroid, agent.AgentTypeUnknown:
		return countLines(transcriptBytes)
	}
	return countLines(transcriptBytes)
}

// hasCodeChanges returns true if the commit has changes to non-metadata files.
// Uses a full tree diff to distinguish code changes from .entire/ metadata-only changes.
// Returns false only if the commit has a parent AND only modified .entire/ metadata files.
//
// WARNING: This is expensive via go-git (resolves many tree/blob objects from packfiles).
// For list views with many checkpoints, use hasAnyChanges instead.
func hasCodeChanges(commit *object.Commit) bool {
	// First commit on shadow branch captures working copy state - always meaningful
	if commit.NumParents() == 0 {
		return true
	}

	parent, err := commit.Parent(0)
	if err != nil {
		return true // Can't check, assume meaningful
	}

	commitTree, err := commit.Tree()
	if err != nil {
		return true
	}

	parentTree, err := parent.Tree()
	if err != nil {
		return true
	}

	changes, err := parentTree.Diff(commitTree)
	if err != nil {
		return true
	}

	// Check if any non-metadata file was changed
	for _, change := range changes {
		name := change.To.Name
		if name == "" {
			name = change.From.Name
		}
		// Skip .entire/ metadata files
		if !strings.HasPrefix(name, ".entire/") {
			return true
		}
	}

	return false
}

// hasAnyChanges is a lightweight alternative to hasCodeChanges that compares
// tree hashes without doing a full diff. Returns true if the commit's tree
// differs from its parent's tree. This may include metadata-only changes,
// but is O(1) instead of O(files) — suitable for list views.
func hasAnyChanges(commit *object.Commit) bool {
	if commit.NumParents() == 0 {
		return true
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return true
	}
	return commit.TreeHash != parent.TreeHash
}
