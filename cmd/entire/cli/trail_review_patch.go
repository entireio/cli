package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6/plumbing"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
)

// trailReviewPatchAnchor is the pre-image a machine-applicable suggested change
// is pinned to: which file the patch rewrites, what that file hashed to when the
// finding was written, and the exact lines the patch expects to find there. The
// API requires all of it for a unified_diff ("unified_diff change requires
// expected_file_path" and four sibling checks), so these fields are mandatory
// rather than decorative — a patch sent without them is rejected outright.
type trailReviewPatchAnchor struct {
	FilePath  string
	FileHash  string
	StartLine int
	EndLine   int
	Lines     string
}

// resolveTrailReviewPatchAnchor derives the anchor from the patch plus the
// working tree: the diff headers name the file and the old-side line span, and
// the file on disk supplies the hash and the expected lines.
func resolveTrailReviewPatchAnchor(ctx context.Context, patch string) (*trailReviewPatchAnchor, error) {
	target, err := parseTrailReviewPatchTarget(patch)
	if err != nil {
		return nil, err
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	fullPath, ok := safeWorktreeFilePath(root, target.Path)
	if !ok {
		return nil, fmt.Errorf("patch path %s does not resolve inside the worktree", target.Path)
	}
	content, err := os.ReadFile(fullPath) //nolint:gosec // path is constrained to the current worktree root.
	if err != nil {
		return nil, fmt.Errorf("read %s to anchor the suggested change: %w", target.Path, err)
	}
	lines, err := patchAnchorLines(content, target.StartLine, target.EndLine)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target.Path, err)
	}
	// Resolved only once the patch and the file have checked out, so a rejected
	// patch does not pay for opening the repository.
	hash, err := gitBlobHash(content, trailReviewObjectFormat(ctx))
	if err != nil {
		return nil, fmt.Errorf("hash %s: %w", target.Path, err)
	}
	return &trailReviewPatchAnchor{
		FilePath:  target.Path,
		FileHash:  hash,
		StartLine: target.StartLine,
		EndLine:   target.EndLine,
		Lines:     lines,
	}, nil
}

// trailReviewObjectFormat reports the repository's git object format, so that
// expected_file_hash is the blob OID `git hash-object` prints in this repo
// rather than a sha1 value that a sha256 repository would never reproduce.
func trailReviewObjectFormat(ctx context.Context) format.ObjectFormat {
	repo, err := gitrepo.OpenCurrent(ctx)
	if err != nil {
		return format.SHA1
	}
	defer repo.Close()
	cfg, err := repo.Config()
	if err != nil || cfg.Extensions.ObjectFormat == format.UnsetObjectFormat {
		return format.SHA1
	}
	return cfg.Extensions.ObjectFormat
}

// trailReviewPatchTarget is the single file a suggested-change patch rewrites
// and the old-side line span it covers.
type trailReviewPatchTarget struct {
	Path      string
	StartLine int
	EndLine   int
}

// trailReviewHunkRangePattern captures both sides of a unified-diff hunk header:
// "@@ -12,7 +12,8 @@" yields old 12,7 and new 12,8, and an omitted count means a
// single line. Both sides are needed to know how long the hunk body is.
var trailReviewHunkRangePattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// patchDevNull is the path a unified diff uses for "this side does not exist":
// on the old side it means the patch creates the file, on the new side that it
// deletes it.
const patchDevNull = "/dev/null"

// unifiedDiffHunk is the line accounting from one "@@" header.
type unifiedDiffHunk struct {
	OldStart int
	OldCount int
	NewCount int
}

// forEachPatchHeaderLine calls fn for every line of a unified diff that is a
// header rather than hunk content, skipping each hunk body by consuming exactly
// the line counts its "@@" header declares.
//
// Prefix matching alone cannot tell the two apart: deleting a line that starts
// with "-- " (a SQL, Lua or Haskell comment, a CLI flag doc) serializes as
// "--- ...", and adding one that starts with "++ " serializes as "+++ ...".
// Reading those as file headers rejected valid single-file patches and let diff
// content reach path validation. Counting the body is also exactly how git
// itself decides where a hunk ends, so this agrees with what `git apply` will do.
func forEachPatchHeaderLine(patchText string, fn func(line string) error) error {
	scanner := bufio.NewScanner(strings.NewReader(patchText))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var remainingOld, remainingNew int
	for scanner.Scan() {
		line := scanner.Text()
		if remainingOld > 0 || remainingNew > 0 {
			switch {
			case strings.HasPrefix(line, `\`): // "\ No newline at end of file"
			case strings.HasPrefix(line, "-"):
				remainingOld--
			case strings.HasPrefix(line, "+"):
				remainingNew--
			default: // context line, present on both sides
				if remainingOld > 0 {
					remainingOld--
				}
				if remainingNew > 0 {
					remainingNew--
				}
			}
			continue
		}
		if hunk, ok := parseUnifiedDiffHunkRange(line); ok {
			remainingOld, remainingNew = hunk.OldCount, hunk.NewCount
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan patch: %w", err)
	}
	return nil
}

// parseTrailReviewPatchTarget reads the file and line span a unified diff
// targets. A suggestion anchors to exactly one file, so a multi-file patch is
// rejected here with an actionable message instead of as a server 400.
func parseTrailReviewPatchTarget(patch string) (trailReviewPatchTarget, error) {
	var (
		target  trailReviewPatchTarget
		oldPath string
		newPath string
		sawFile bool
		sawHunk bool
	)
	err := forEachPatchHeaderLine(patch, func(line string) error {
		switch {
		case strings.HasPrefix(line, "--- "):
			if sawFile {
				return errors.New("patch targets multiple files; attach one suggested change per file")
			}
			sawFile = true
			oldPath = patchSidePath(line[4:], "a/")
		case strings.HasPrefix(line, "+++ "):
			newPath = patchSidePath(line[4:], "b/")
		default:
			hunk, ok := parseUnifiedDiffHunkRange(line)
			if !ok {
				return nil
			}
			end := hunk.OldStart + hunk.OldCount - 1
			if hunk.OldCount == 0 {
				// A zero-length old range is a pure insertion (diff -u0 output);
				// it anchors on the line the new text is inserted next to.
				end = hunk.OldStart
			}
			if !sawHunk || hunk.OldStart < target.StartLine {
				target.StartLine = hunk.OldStart
			}
			if end > target.EndLine {
				target.EndLine = end
			}
			sawHunk = true
		}
		return nil
	})
	if err != nil {
		return target, err
	}
	if !sawFile {
		return target, errors.New("patch has no '--- <file>' header naming the file it changes")
	}
	if !sawHunk {
		return target, errors.New("patch has no '@@' hunk header")
	}
	// Only a /dev/null old side means creation. A zero start line does not: an
	// insertion above line 1 of an existing file is also "@@ -0,0 +1,N @@".
	if oldPath == patchDevNull {
		return target, fmt.Errorf("patch creates %s; a suggested change must modify an existing file (use --instruction instead)",
			newPath)
	}
	if newPath != "" && newPath != patchDevNull && newPath != oldPath {
		// The old path is gone from the worktree, so anchoring would otherwise
		// fail with an unhelpful "read <old path>" error.
		return target, fmt.Errorf("patch renames %s to %s; a suggested change must modify a single existing file (use --instruction instead)",
			oldPath, newPath)
	}
	// A prepend anchors on the line the new text goes before.
	if target.StartLine < 1 {
		target.StartLine = 1
	}
	if target.EndLine < target.StartLine {
		target.EndLine = target.StartLine
	}
	// The same whole-patch check the apply path runs, so a patch is held to one
	// path-safety standard whether it is being written or applied: this covers
	// the +++ side and any rename/copy headers, not just the --- path we anchor
	// to.
	if err := validateUnifiedDiffPatchPaths(patch); err != nil {
		return target, fmt.Errorf("unsafe patch path: %w", err)
	}
	target.Path = path.Clean(oldPath)
	return target, nil
}

// patchSidePath normalizes one side of a diff header. git prefixes the old side
// with a/ and the new side with b/, and only that one prefix may be stripped: a
// file genuinely living at b/pkg.go is written "--- a/b/pkg.go", so stripping
// both in sequence resolved it to pkg.go and read the wrong file.
func patchSidePath(raw, prefix string) string {
	p := patchHeaderPath(raw)
	if unquoted, err := strconv.Unquote(p); err == nil {
		p = unquoted
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if p == patchDevNull {
		return p
	}
	return strings.TrimPrefix(p, prefix)
}

// parseUnifiedDiffHunkRange extracts the line accounting from a hunk header.
func parseUnifiedDiffHunkRange(line string) (unifiedDiffHunk, bool) {
	match := trailReviewHunkRangePattern.FindStringSubmatch(line)
	if match == nil {
		return unifiedDiffHunk{}, false
	}
	oldStart, err := strconv.Atoi(match[1])
	if err != nil {
		return unifiedDiffHunk{}, false
	}
	oldCount, ok := patchHunkCount(match[2])
	if !ok {
		return unifiedDiffHunk{}, false
	}
	newCount, ok := patchHunkCount(match[4])
	if !ok {
		return unifiedDiffHunk{}, false
	}
	return unifiedDiffHunk{OldStart: oldStart, OldCount: oldCount, NewCount: newCount}, true
}

// patchHunkCount reads one side's line count, which defaults to 1 when the hunk
// header omits it.
func patchHunkCount(raw string) (int, bool) {
	if raw == "" {
		return 1, true
	}
	count, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return count, true
}

// patchAnchorLines returns the file's bytes for lines [start, end] with their
// line endings intact, which is what expected_lines records: the exact slice a
// later apply compares against to tell whether the file has moved on. Unlike
// trailReviewSelectedTextFromWorktree, it must not normalize CRLF — the value is
// a byte-for-byte pre-image, not display text.
func patchAnchorLines(content []byte, start, end int) (string, error) {
	lines := strings.SplitAfter(string(content), "\n")
	// A trailing newline leaves a final empty element that is not a line.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	// end >= start >= 1 holds for every target the parser returns, so this one
	// bound covers both.
	if end > len(lines) {
		return "", fmt.Errorf("patch expects lines %d-%d but the file has %d", start, end, len(lines))
	}
	return strings.Join(lines[start-1:end], ""), nil
}

// gitBlobHash computes the git blob object ID of the given content, so the value
// stored as expected_file_hash matches `git hash-object <file>`.
func gitBlobHash(content []byte, objectFormat format.ObjectFormat) (string, error) {
	hasher := plumbing.NewHasher(objectFormat, plumbing.BlobObject, int64(len(content)))
	if _, err := hasher.Write(content); err != nil {
		return "", fmt.Errorf("write blob content to hasher: %w", err)
	}
	return hasher.Sum().String(), nil
}
