package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// A tracked core.hooksPath hook lives in the worktree. The old install moved it
// to an untracked <hook>.pre-entire and wrote Entire's wrapper at the tracked
// path. Committing that wrapper, without the backup, drops the project's hook
// in every other clone. Shell hooks are edited in place instead. Anything
// else tracked is left untouched: rewriting it would repeat that loss.
const (
	trackedSpliceBegin = "# Entire CLI hooks (tracked splice) begin"
	trackedSpliceEnd   = "# Entire CLI hooks (tracked splice) end"

	// hookOriginalsDir is under GIT_DIR, not the worktree, so the snapshot is
	// never something a later commit can pick up by accident.
	hookOriginalsDir = "entire-hook-originals"
)

const (
	postRewriteInvocation          = `hooks git post-rewrite "$1" 2>/dev/null || true`
	postRewriteInvocationWithStdin = `hooks git post-rewrite "$1" < "$_entire_stdin" 2>/dev/null || true`
)

var (
	errTrackedHookNotShell = errors.New("tracked hook is not a shell script")
	errTrackedHookSymlink  = errors.New("tracked hook is a symbolic link")
)

// shellShebang matches a shell interpreter on one shebang line: /bin/sh,
// /bin/bash -e, /usr/bin/env bash, dash, ash. It does not match python or a
// name that merely starts with a shell (ship).
var shellShebang = regexp.MustCompile(`^#![ \t]*(?:/usr/bin/env[ \t]+)?(?:[^\r\n]*/)?(?:sh|bash|dash|ash)(?:[ \t]|$)`)

// indexedPlan is what install should do with one hook that is in the index.
// handled false means the ordinary backup-and-write path still applies.
type indexedPlan struct {
	handled        bool
	content        string
	snapshot       []byte
	rel            string
	firstSplice    bool
	repaired       bool
	sidecar        []byte
	dropSidecar    bool
	sidecarTracked bool
}

type indexPath struct {
	top     string
	rel     string
	tracked bool
}

// isTrackedSplice reports whether data is a hook this file edited in place.
// The begin line contains entireHookMarker, so callers must check this before
// treating every marker as hookOurs. hookOurs is deleted on uninstall, and a
// splice has no .pre-entire backup to put back.
func isTrackedSplice(data []byte) bool {
	return bytes.Contains(data, []byte(trackedSpliceBegin)) && bytes.Contains(data, []byte(trackedSpliceEnd))
}

func isShellHook(data []byte) bool {
	if len(data) == 0 || bytes.Contains(data, []byte{0}) {
		return false
	}
	line, _, _ := bytes.Cut(data, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return shellShebang.Match(line)
}

func notShellError(rel string) error {
	return fmt.Errorf("%w: %s: Entire will not rewrite a tracked hook that is not a shell script, because moving it would create an untracked %s file and committing the wrapper would drop the hook for every other clone. Point core.hooksPath at an untracked directory such as .git/hooks, or run Entire from a shell hook that then execs the script",
		errTrackedHookNotShell, rel, filepath.Base(rel)+backupSuffix)
}

// rejectIndexedHooksEntireWillNotEdit fails the install before any hook is
// written. pre-push is last, so a mid-loop refusal would already have created
// the other hook files.
func rejectIndexedHooksEntireWillNotEdit(ctx context.Context, root *os.Root, hooksDir string, specs []hookSpec) error {
	for _, spec := range specs {
		if _, err := planIndexedHook(ctx, root, hooksDir, spec); err != nil {
			return err
		}
	}
	return nil
}

func installIndexedHook(ctx context.Context, root *os.Root, hooksDir string, spec hookSpec) (bool, bool, error) {
	plan, err := planIndexedHook(ctx, root, hooksDir, spec)
	if err != nil {
		return false, false, err
	}
	if !plan.handled {
		return false, false, nil
	}
	if len(plan.snapshot) > 0 {
		if err := saveHookOriginal(ctx, spec.name, plan.snapshot); err != nil {
			return true, false, err
		}
	}
	wrote, err := writeHookFile(root, spec.name, plan.content)
	if err != nil {
		return true, false, err
	}
	if plan.dropSidecar {
		if err := dropMatchingSidecar(ctx, root, hooksDir, spec.name, plan.sidecar); err != nil {
			return true, wrote, err
		}
	}
	if wrote {
		writeIndexedNotice(plan, spec.name)
		logging.Info(ctx, "edited tracked git hook in place", slog.String("hook", spec.name))
	} else if plan.sidecarTracked {
		fmt.Fprintf(stderrWriter, "[entire] %s%s is tracked, so Entire left it in place. It no longer runs.\n", spec.name, backupSuffix)
	}
	return true, wrote, nil
}

func writeIndexedNotice(plan indexedPlan, name string) {
	switch {
	case plan.firstSplice:
		fmt.Fprintf(stderrWriter, "[entire] %s is tracked, so Entire edited it in place. The project's script still runs after Entire, and no %s%s file was created. Commit this edit so other clones keep both. A copy of the original is in the git directory at %s/%s.\n",
			plan.rel, name, backupSuffix, hookOriginalsDir, name)
	case plan.repaired:
		fmt.Fprintf(stderrWriter, "[entire] %s had Entire's wrapper in the tracked file. Entire folded the project's script back into it.\n", plan.rel)
		if plan.dropSidecar {
			fmt.Fprintf(stderrWriter, "[entire] Removed the untracked %s%s copy.\n", name, backupSuffix)
		}
	}
	if plan.sidecarTracked {
		fmt.Fprintf(stderrWriter, "[entire] %s%s is tracked, so Entire left it in place. It no longer runs.\n", name, backupSuffix)
	}
}

func planIndexedHook(ctx context.Context, root *os.Root, hooksDir string, spec hookSpec) (indexedPlan, error) {
	loc, err := locateHook(ctx, hooksDir, spec.name)
	if err != nil {
		return indexedPlan{}, err
	}
	if !loc.tracked {
		return indexedPlan{}, nil
	}
	if err := refuseTrackedSymlink(root, loc.rel); err != nil {
		return indexedPlan{}, err
	}
	class, classErr := classifyExistingHook(root, spec.name)
	if classErr != nil {
		return indexedPlan{}, unclassifiableHookError(hooksDir, spec.name, classErr)
	}
	switch class {
	case hookSpliced:
		return planRefreshSplice(ctx, root, hooksDir, spec, loc)
	case hookForeign:
		return planSpliceForeign(root, spec, loc)
	case hookAbsent:
		return planSpliceAbsent(ctx, spec, loc)
	case hookOurs:
		return planRepairOurs(ctx, root, hooksDir, spec, loc)
	default:
		return indexedPlan{}, nil
	}
}

func refuseTrackedSymlink(root *os.Root, rel string) error {
	info, err := root.Lstat(filepath.Base(rel))
	if err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s: Entire will not replace a tracked symbolic link, because the replacement is a regular file and committing it changes the hook for every other clone. Replace the link with the script it points at, or point core.hooksPath at an untracked directory such as .git/hooks",
			errTrackedHookSymlink, rel)
	}
	return nil
}

func planSpliceForeign(root *os.Root, spec hookSpec, loc indexPath) (indexedPlan, error) {
	data, err := osroot.ReadFileNoFollow(root, spec.name)
	if err != nil {
		return indexedPlan{}, fmt.Errorf("read %s: %w", loc.rel, err)
	}
	if !isShellHook(data) {
		return indexedPlan{}, notShellError(loc.rel)
	}
	next, err := spliceTrackedHook(spec.name, spec.content, data)
	if err != nil {
		return indexedPlan{}, err
	}
	return indexedPlan{
		handled:     true,
		content:     next,
		snapshot:    data,
		rel:         loc.rel,
		firstSplice: true,
	}, nil
}

func planRefreshSplice(ctx context.Context, root *os.Root, hooksDir string, spec hookSpec, loc indexPath) (indexedPlan, error) {
	data, err := osroot.ReadFileNoFollow(root, spec.name)
	if err != nil {
		return indexedPlan{}, fmt.Errorf("read %s: %w", loc.rel, err)
	}
	next, err := spliceTrackedHook(spec.name, spec.content, data)
	if err != nil {
		return indexedPlan{}, err
	}
	stripped, ok := stripTrackedSplice(string(data))
	if !ok {
		return indexedPlan{}, fmt.Errorf("tracked splice in %s is missing its end marker", loc.rel)
	}
	plan := indexedPlan{
		handled:  true,
		content:  next,
		snapshot: []byte(stripped),
		rel:      loc.rel,
	}
	sideName := spec.name + backupSuffix
	side, sideErr := osroot.ReadFileNoFollow(root, sideName)
	// A missing or different sidecar is not this splice's backup, so leave it.
	// An unreadable file must not fail the install: the repair can still
	// fold the project's script back into the tracked hook.
	sameSidecar := sideErr == nil && bytes.Equal(side, []byte(stripped))
	if !sameSidecar {
		return plan, nil
	}
	sideLoc, err := locateHook(ctx, hooksDir, sideName)
	if err != nil {
		return indexedPlan{}, err
	}
	if sideLoc.tracked {
		plan.sidecarTracked = true
		return plan, nil
	}
	plan.sidecar = side
	plan.dropSidecar = true
	return plan, nil
}

func planSpliceAbsent(ctx context.Context, spec hookSpec, loc indexPath) (indexedPlan, error) {
	blob, err := gitIndexBlob(ctx, loc)
	if err != nil {
		return indexedPlan{}, err
	}
	if isTrackedSplice(blob) || (isShellHook(blob) && !bytes.Contains(blob, []byte(entireHookMarker))) {
		next, err := spliceTrackedHook(spec.name, spec.content, blob)
		if err != nil {
			return indexedPlan{}, err
		}
		snap := blob
		if isTrackedSplice(blob) {
			stripped, ok := stripTrackedSplice(string(blob))
			if !ok {
				return indexedPlan{}, fmt.Errorf("tracked splice in %s is missing its end marker", loc.rel)
			}
			snap = []byte(stripped)
		}
		return indexedPlan{
			handled:     true,
			content:     next,
			snapshot:    snap,
			rel:         loc.rel,
			firstSplice: !isTrackedSplice(blob),
		}, nil
	}
	if bytes.Contains(blob, []byte(entireHookMarker)) {
		return indexedPlan{}, nil
	}
	return indexedPlan{}, notShellError(loc.rel)
}

func planRepairOurs(ctx context.Context, root *os.Root, hooksDir string, spec hookSpec, loc indexPath) (indexedPlan, error) {
	blob, err := gitIndexBlob(ctx, loc)
	if err != nil {
		return indexedPlan{}, err
	}
	sideName := spec.name + backupSuffix
	side, sideErr := osroot.ReadFileNoFollow(root, sideName)
	useSide := sideErr == nil && isShellHook(side) && !bytes.Contains(side, []byte(entireHookMarker))

	var user []byte
	switch {
	case useSide:
		user = side
	case isTrackedSplice(blob):
		user = blob
	case isShellHook(blob) && !bytes.Contains(blob, []byte(entireHookMarker)):
		user = blob
	case bytes.Contains(blob, []byte(entireHookMarker)):
		return indexedPlan{}, nil
	default:
		return indexedPlan{}, notShellError(loc.rel)
	}

	next, err := spliceTrackedHook(spec.name, spec.content, user)
	if err != nil {
		return indexedPlan{}, err
	}
	snap := user
	if isTrackedSplice(user) {
		stripped, ok := stripTrackedSplice(string(user))
		if !ok {
			return indexedPlan{}, fmt.Errorf("tracked splice in %s is missing its end marker", loc.rel)
		}
		snap = []byte(stripped)
	}
	plan := indexedPlan{
		handled:  true,
		content:  next,
		snapshot: snap,
		rel:      loc.rel,
		repaired: true,
	}
	if !useSide {
		return plan, nil
	}
	plan.sidecar = side
	sideLoc, err := locateHook(ctx, hooksDir, sideName)
	if err != nil {
		return indexedPlan{}, err
	}
	if sideLoc.tracked {
		plan.sidecarTracked = true
	} else {
		plan.dropSidecar = true
	}
	return plan, nil
}

func dropMatchingSidecar(ctx context.Context, root *os.Root, hooksDir, hook string, want []byte) error {
	name := hook + backupSuffix
	loc, err := locateHook(ctx, hooksDir, name)
	if err != nil {
		return err
	}
	if loc.tracked {
		fmt.Fprintf(stderrWriter, "[entire] %s is tracked, so Entire left it in place. It no longer runs.\n", name)
		return nil
	}
	got, err := osroot.ReadFileNoFollow(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if !bytes.Equal(got, want) {
		fmt.Fprintf(stderrWriter, "[entire] Warning: %s changed, so Entire left it in place.\n", name)
		return nil
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// spliceTrackedHook inserts Entire's commands after the shebang and leaves the
// rest of the script in the same file. A second call refreshes the block and
// keeps later edits. Line endings follow the file Entire is editing.
func spliceTrackedHook(name, specContent string, original []byte) (string, error) {
	if isTrackedSplice(original) {
		stripped, ok := stripTrackedSplice(string(original))
		if !ok {
			return "", fmt.Errorf("tracked splice in %s is missing its end marker", name)
		}
		original = []byte(stripped)
	}
	if !isShellHook(original) {
		return "", notShellError(name)
	}
	nl := "\n"
	if bytes.Contains(original, []byte("\r\n")) {
		nl = "\r\n"
	}
	shebang, rest, found := strings.Cut(string(original), "\n")
	if !found {
		rest = ""
	}
	shebang = strings.TrimRight(shebang, "\r")
	block := spliceBlock(name, specContent, nl)
	var b strings.Builder
	b.Grow(len(shebang) + len(nl) + len(block) + len(rest))
	b.WriteString(shebang)
	b.WriteString(nl)
	b.WriteString(block)
	b.WriteString(rest)
	return b.String(), nil
}

func spliceBlock(name, specContent, nl string) string {
	body := specContent
	if strings.HasPrefix(body, "#!") {
		if _, after, ok := strings.Cut(body, "\n"); ok {
			body = after
		}
	}
	if name == postRewriteHook {
		body = postRewriteSpliceBody(body)
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.TrimRight(body, "\n")
	if nl != "\n" {
		body = strings.ReplaceAll(body, "\n", nl)
	}
	return trackedSpliceBegin + nl + body + nl + trackedSpliceEnd + nl
}

// postRewriteSpliceBody saves stdin for the user's script without installing a
// trap. A trap here would replace one the project's hook already set.
func postRewriteSpliceBody(body string) string {
	body = strings.Replace(body, postRewriteInvocation, postRewriteInvocationWithStdin, 1)
	return strings.Join([]string{
		`_entire_stdin="$(mktemp "${TMPDIR:-/tmp}/entire-post-rewrite.XXXXXX")"`,
		`cat > "$_entire_stdin" || true`,
		body,
		`exec < "$_entire_stdin" || true`,
		`rm -f "$_entire_stdin"`,
	}, "\n")
}

// stripTrackedSplice removes the delimited block and the single newline that
// ends it. The shebang line and everything after the block stay, including
// edits made after install.
func stripTrackedSplice(content string) (string, bool) {
	begin := strings.Index(content, trackedSpliceBegin)
	if begin < 0 {
		return content, false
	}
	rest := content[begin+len(trackedSpliceBegin):]
	end := strings.Index(rest, trackedSpliceEnd)
	if end < 0 {
		return content, false
	}
	after := rest[end+len(trackedSpliceEnd):]
	switch {
	case strings.HasPrefix(after, "\r\n"):
		after = after[2:]
	case strings.HasPrefix(after, "\n"):
		after = after[1:]
	case strings.HasPrefix(after, "\r"):
		after = strings.TrimPrefix(after[1:], "\n")
	}
	return content[:begin] + after, true
}

func stripTrackedSpliceFile(root *os.Root, name string) error {
	data, err := osroot.ReadFileNoFollow(root, name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	stripped, ok := stripTrackedSplice(string(data))
	if !ok {
		return fmt.Errorf("%s has no tracked splice to remove", name)
	}
	if _, err := writeHookFile(root, name, stripped); err != nil {
		return err
	}
	return nil
}

func saveHookOriginal(ctx context.Context, name string, data []byte) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("refusing to snapshot hook name %q", name)
	}
	gitDir, err := GetGitDir(ctx)
	if err != nil {
		return fmt.Errorf("locate git directory to snapshot %s: %w", name, err)
	}
	dir := filepath.Join(gitDir, hookOriginalsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", hookOriginalsDir, err)
	}
	dest := filepath.Join(dir, name)
	// Name is one of the fixed hook names, and dest is that name under GIT_DIR.
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // snapshot path is GIT_DIR plus a fixed hook name
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", name, err)
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		if rerr := os.Remove(dest); rerr != nil {
			return fmt.Errorf("snapshot %s: %w (removing the partial copy: %w)", name, werr, rerr)
		}
		return fmt.Errorf("snapshot %s: %w", name, werr)
	}
	if cerr != nil {
		return fmt.Errorf("snapshot %s: %w", name, cerr)
	}
	return nil
}

func locateHook(ctx context.Context, hooksDir, name string) (indexPath, error) {
	top, err := gitWorktreeTop(ctx)
	if err != nil {
		return indexPath{}, err
	}
	dir, err := canonicalDir(hooksDir)
	if err != nil {
		return indexPath{}, err
	}
	rel, inside := relInside(top, filepath.Join(dir, name))
	if !inside {
		return indexPath{top: top}, nil
	}
	slash := filepath.ToSlash(rel)
	tracked, err := gitTracked(ctx, top, slash)
	if err != nil {
		return indexPath{}, err
	}
	return indexPath{top: top, rel: slash, tracked: tracked}, nil
}

func gitWorktreeTop(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
		}
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w (%s)", err, msg)
	}
	top := filepath.Clean(filepath.FromSlash(strings.TrimSpace(string(out))))
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = filepath.Clean(resolved)
	}
	return top, nil
}

func canonicalDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve hooks directory: %w", err)
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	return abs, nil
}

func relInside(top, abs string) (string, bool) {
	if rel, ok := relInsideFold(top, abs, false); ok {
		return rel, true
	}
	if runtime.GOOS == goosWindows {
		return relInsideFold(top, abs, true)
	}
	return "", false
}

func relInsideFold(top, abs string, fold bool) (string, bool) {
	a, b := top, abs
	if fold {
		a, b = strings.ToLower(top), strings.ToLower(abs)
	}
	rel, err := filepath.Rel(a, b)
	if err != nil || !isInsideRel(rel) {
		return "", false
	}
	return rel, true
}

func isInsideRel(rel string) bool {
	if rel == "." || rel == "" || filepath.IsAbs(rel) {
		return false
	}
	parent := ".." + string(filepath.Separator)
	return rel != ".." && !strings.HasPrefix(rel, parent)
}

func gitTracked(ctx context.Context, top, slashRel string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--error-unmatch", "--", slashRel)
	cmd.Dir = top
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git ls-files -- %s: %w", slashRel, err)
}

func gitIndexBlob(ctx context.Context, loc indexPath) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "show", ":"+loc.rel)
	cmd.Dir = loc.top
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("git show :%s: %w", loc.rel, err)
		}
		return nil, fmt.Errorf("git show :%s: %w (%s)", loc.rel, err, msg)
	}
	return out, nil
}
