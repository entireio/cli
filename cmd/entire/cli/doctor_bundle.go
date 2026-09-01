package cli

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"
	"github.com/spf13/cobra"
)

func newDoctorBundleCmd() *cobra.Command {
	var outFlag string
	var rawFlag bool

	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Produce a diagnostic bundle (zip) for bug reports — secrets are redacted by default",
		Long: `Produce a zip archive containing logs, settings, and a git snapshot suitable
for attaching to bug reports.

The archive includes:
  - logs/                       (operational logs from .entire/logs/)
  - settings/settings.json and settings/settings.local.json (if present)
  - git-status.txt, git-log.txt, git-remote.txt
  - version.txt with CLI version, Go version, OS/Arch

Redaction:
  By default the bundle redacts known secrets (API keys, credentialed URIs,
  database connection strings, bounded KEY=value credentials) from log files,
  settings JSON, and git command output before zipping. Pass --raw to skip
  redaction; use it only when support has explicitly requested an unredacted
  bundle.

By default the archive is written to a path inside the OS temp directory and
that path is printed to stdout. Use --out to choose a specific path.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			repoRoot, err := paths.WorktreeRoot(ctx)
			if err != nil {
				cmd.SilenceUsage = true
				return errors.New("not a git repository")
			}

			outPath := outFlag
			if outPath == "" {
				outPath = filepath.Join(os.TempDir(), fmt.Sprintf("entire-bundle-%s.zip", time.Now().UTC().Format("20060102-150405")))
			}

			// The routed runtime root (entiredir): globally tracked repos
			// contribute their common-dir logs. A resolution failure must not
			// abort the bundle — this command exists to debug exactly such
			// broken setups — so it degrades to a bundle without .entire
			// entries, with a warning.
			entireRoot, rootErr := entiredir.OpenForRead(ctx)
			switch {
			case errors.Is(rootErr, fs.ErrNotExist):
				entireRoot = nil
			case rootErr != nil:
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not open the runtime directory (%v); bundling without logs and settings.\n", rootErr)
				entireRoot = nil
			}

			if err := writeDoctorBundle(ctx, repoRoot, entireRoot, outPath, rawFlag); err != nil {
				return err
			}

			if rawFlag {
				fmt.Fprintf(cmd.ErrOrStderr(), "Bundle written (RAW — contains unredacted contents): %s\n", outPath)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "Bundle written (redacted): %s\n", outPath)
			}
			fmt.Fprintln(cmd.OutOrStdout(), outPath)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outFlag, "out", "o", "", "Path to write the bundle archive (default: OS temp dir)")
	cmd.Flags().BoolVar(&rawFlag, "raw", false, "Skip secret redaction. The archive will contain raw log lines, settings, and git output. Use only when support has asked for an unredacted bundle.")
	return cmd
}

// writeDoctorBundle writes the diagnostic zip. entireRoot is the caller's
// (routed) runtime root, or nil for none — resolved by the caller so tests
// can hand in a fixture's root; repoRoot anchors the git commands, which are
// worktree-resolved by design.
func writeDoctorBundle(ctx context.Context, repoRoot string, entireRoot *os.Root, outPath string, raw bool) error {
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // user-provided output path is intentional
	if err != nil {
		return fmt.Errorf("create bundle: %w", err)
	}
	if err := out.Chmod(0o600); err != nil {
		_ = out.Close()
		return fmt.Errorf("set bundle permissions: %w", err)
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			_ = out.Close()
		}
	}()

	zw := zip.NewWriter(out)
	zipClosed := false
	defer func() {
		if !zipClosed {
			_ = zw.Close()
		}
	}()

	// Everything the bundle collects from .entire is read through the shared
	// root, so a symlink under .entire cannot pull an arbitrary file into a
	// bundle the user is about to send somewhere. The root is the caller's
	// ROUTED runtime base, so a globally tracked repo contributes its
	// common-dir logs; a repo with no runtime data contributes no log or
	// settings entries and is not an error.
	if entireRoot != nil {
		if err := addDirToZip(zw, entireRoot, logging.LogsName, "logs", raw); err != nil {
			return err
		}

		for _, name := range []string{settings.SettingsName, settings.SettingsLocalName} {
			if err := addFileToZip(zw, entireRoot, name, path.Join("settings", name), raw); err != nil {
				return err
			}
		}
	}

	if err := addCommandOutput(ctx, zw, "git-status.txt", repoRoot, raw, "git", "--no-optional-locks", "status", "--short", "--branch"); err != nil {
		return err
	}
	if err := addCommandOutput(ctx, zw, "git-log.txt", repoRoot, raw, "git", "log", "-n", "50", "--oneline"); err != nil {
		return err
	}
	if err := addCommandOutput(ctx, zw, "git-remote.txt", repoRoot, raw, "git", "remote", "-v"); err != nil {
		return err
	}

	if err := addStringToZip(zw, "entire-refs.txt", entireRefsReport(ctx, repoRoot), raw); err != nil {
		return err
	}

	if err := addStringToZip(zw, "version.txt", versionInfoString(), raw); err != nil {
		return err
	}

	if err := zw.Close(); err != nil {
		return fmt.Errorf("finalize bundle: %w", err)
	}
	zipClosed = true

	if err := out.Close(); err != nil {
		return fmt.Errorf("close bundle: %w", err)
	}
	fileClosed = true

	return nil
}

// entireRefsReport captures entire-related git refs.
// Best-effort: failures are recorded in the report, not returned.
func entireRefsReport(ctx context.Context, repoRoot string) string {
	var sb strings.Builder

	// Broad globs on purpose: refs/heads/entire catches shadow/trails branches,
	// and refs/entire captures custom or legacy Entire refs.
	cmd := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(refname) %(objectname)",
		"refs/heads/entire", "refs/entire", "refs/remotes/origin/entire")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	sb.Write(out)
	if err != nil {
		fmt.Fprintf(&sb, "[error: %v]\n", err)
	}

	return sb.String()
}

func versionInfoString() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Entire CLI %s (%s)\n", versioninfo.Version, versioninfo.Commit)
	fmt.Fprintf(&sb, "Go: %s\n", runtime.Version())
	fmt.Fprintf(&sb, "OS/Arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	return sb.String()
}

func addDirToZip(zw *zip.Writer, root *os.Root, srcDir, archivePrefix string, raw bool) error {
	// Lstat, not Stat. Stat follows a symlink, and so does fs.WalkDir on its
	// own walk root, so a symlinked .entire/logs used to be bundled as whatever
	// it pointed at — presented to whoever reads the bundle as this repo's logs.
	info, err := root.Lstat(srcDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", srcDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Doctor is the command a user runs when the repo is already broken, so
		// this records the finding instead of failing the bundle the way the
		// checkpoint walks do. addFileToZip takes the same line.
		return addStringToZip(zw, zipEntryName(archivePrefix, "SYMLINK.txt"),
			fmt.Sprintf("[skipped: %s is a symlink, not a directory; its target was not bundled]\n", srcDir), raw)
	}
	if !info.IsDir() {
		return nil
	}
	walkErr := fs.WalkDir(root.FS(), srcDir, func(name string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, name)
		if err != nil {
			return fmt.Errorf("rel: %w", err)
		}
		return addFileToZip(zw, root, name, zipEntryName(archivePrefix, rel), raw)
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", srcDir, walkErr)
	}
	return nil
}

func zipEntryName(parts ...string) string {
	cleanParts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		cleanParts = append(cleanParts, filepath.ToSlash(part))
	}
	return path.Join(cleanParts...)
}

func addFileToZip(zw *zip.Writer, root *os.Root, src, archivePath string, raw bool) error {
	f, err := osroot.OpenNoFollow(root, src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		// A bundle that aborts is worse than a bundle with a gap: this is the
		// command a user runs when something is already wrong. The most likely
		// cause here is a symlink under .entire pointing outside it, which the
		// root refuses to follow — before that refusal existed the target was
		// silently bundled instead, so record the reason rather than either
		// including it or failing.
		return addStringToZip(zw, archivePath, fmt.Sprintf("[error: open %s: %v]\n", src, err), raw)
	}
	defer f.Close()

	entryName := zipEntryName(archivePath)
	w, err := zw.Create(entryName)
	if err != nil {
		return fmt.Errorf("zip create %s: %w", entryName, err)
	}

	if raw {
		if _, err := io.Copy(w, f); err != nil {
			return fmt.Errorf("zip copy %s: %w", entryName, err)
		}
		return nil
	}

	contents, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	redacted := redactBundleEntry(entryName, contents)
	if _, err := w.Write(redacted); err != nil {
		return fmt.Errorf("zip write %s: %w", entryName, err)
	}
	return nil
}

func addStringToZip(zw *zip.Writer, archivePath, contents string, raw bool) error {
	entryName := zipEntryName(archivePath)
	w, err := zw.Create(entryName)
	if err != nil {
		return fmt.Errorf("zip create %s: %w", entryName, err)
	}
	body := contents
	if !raw {
		body = string(redactBundleEntry(entryName, []byte(contents)))
	}
	if _, err := io.WriteString(w, body); err != nil {
		return fmt.Errorf("zip write %s: %w", entryName, err)
	}
	return nil
}

func addCommandOutput(ctx context.Context, zw *zip.Writer, archivePath, dir string, raw bool, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		out = append(out, []byte(fmt.Sprintf("\n[error: %v]\n", err))...)
	}
	// addStringToZip applies redaction when raw=false; pass through verbatim otherwise.
	return addStringToZip(zw, archivePath, string(out), raw)
}

// redactBundleEntry chooses a redaction strategy per file shape. JSON / JSONL
// entries get the field-aware redactor (preserves structure, skips ID fields);
// everything else uses the byte-level scrubber.
func redactBundleEntry(entryName string, contents []byte) []byte {
	ext := strings.ToLower(path.Ext(entryName))
	if ext == ".json" || ext == ".jsonl" {
		out, err := redact.JSONLContent(string(contents))
		if err == nil {
			return []byte(out)
		}
		// Fall through to plain redaction if the JSON redactor refuses (malformed input, etc.)
	}
	return redact.Bytes(contents)
}
