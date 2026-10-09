//go:build !windows

package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallFromFishWithMissingPathRunsPostInstallAndShowsFishSetup(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	fixtureDir := t.TempDir()
	archivePath := filepath.Join(fixtureDir, "entire_darwin_arm64.tar.gz")
	writeInstallerArchive(t, archivePath, map[string]string{
		"entire": `#!/bin/sh
case "$1" in
    version)
        exit 0
        ;;
    curl-bash-post-install)
        printf 'shell=%s\nlegacy_shell=%s\nxdg=%s\npath_dir=%s\n' \
            "$ENTIRE_INSTALLER_SHELL" "$SHELL" "$XDG_CONFIG_HOME" "$ENTIRE_INSTALLER_PATH_DIR" \
            > "$HOME/post-install-env"
        ;;
esac
`,
		"git-remote-entire": "#!/bin/sh\nexit 0\n",
	})

	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("read installer archive: %v", err)
	}
	checksumPath := filepath.Join(fixtureDir, "checksums.txt")
	checksum := sha256.Sum256(archiveData)
	if err := os.WriteFile(
		checksumPath,
		[]byte(fmt.Sprintf("%x  entire_darwin_arm64.tar.gz\n", checksum)),
		0o644,
	); err != nil {
		t.Fatalf("write checksums: %v", err)
	}

	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "uname"), `#!/bin/sh
case "$1" in
    -s) printf '%s\n' Darwin ;;
    -m) printf '%s\n' arm64 ;;
esac
`)
	writeExecutable(t, filepath.Join(binDir, "ps"), "#!/bin/sh\nprintf '%s\\n' /opt/homebrew/bin/fish\n")
	writeExecutable(t, filepath.Join(binDir, "curl"), `#!/bin/bash
output=""
url=""
while (($# > 0)); do
    case "$1" in
        -o)
            output="$2"
            shift 2
            ;;
        -H)
            shift 2
            ;;
        *)
            url="$1"
            shift
            ;;
    esac
done

case "$url" in
    */releases/latest)
        printf '%s\n' '{"tag_name":"v1.2.3"}'
        ;;
    */checksums.txt)
        cp "$FIXTURE_CHECKSUM" "$output"
        ;;
    *.tar.gz)
        cp "$FIXTURE_ARCHIVE" "$output"
        ;;
    *)
        printf 'unexpected URL: %s\n' "$url" >&2
        exit 1
        ;;
esac
`)

	xdgConfigHome := filepath.Join(home, "xdg-config")
	cmd := exec.CommandContext(t.Context(), "/bin/bash", installerPath(t))
	cmd.Env = []string{
		"FIXTURE_ARCHIVE=" + archivePath,
		"FIXTURE_CHECKSUM=" + checksumPath,
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SHELL=/bin/zsh",
		"XDG_CONFIG_HOME=" + xdgConfigHome,
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run installer: %v\n%s", err, out)
	}

	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "entire")); err != nil {
		t.Fatalf("installed binary: %v", err)
	}
	postInstallEnv, err := os.ReadFile(filepath.Join(home, "post-install-env"))
	if err != nil {
		t.Fatalf("post-install marker: %v", err)
	}
	wantPostInstallEnv := "shell=fish\nlegacy_shell=fish\nxdg=" + xdgConfigHome + "\npath_dir=" + filepath.Join(home, ".local", "bin") + "\n"
	if string(postInstallEnv) != wantPostInstallEnv {
		t.Fatalf("post-install environment = %q, want %q", postInstallEnv, wantPostInstallEnv)
	}

	output := string(out)
	if !strings.Contains(output, `fish_add_path "$HOME/.local/bin"`) {
		t.Fatalf("installer did not show Fish PATH setup:\n%s", output)
	}
	if strings.Contains(output, "config.fish") {
		t.Fatalf("installer should not tell Fish users to edit config.fish:\n%s", output)
	}
}

func TestInstallerRunsWhenPipedToBash(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(installerPath(t))
	if err != nil {
		t.Fatalf("read installer: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/bash", "-s", "--", "--help")
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=/usr/bin:/bin",
	}
	cmd.Stdin = bytes.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pipe installer to Bash: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Usage: install.sh") {
		t.Fatalf("piped installer did not run main:\n%s", out)
	}
}

func TestDetectUserShell(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		parent     string
		loginShell string
		want       string
	}{
		{
			name:       "parent_fish_overrides_login_zsh",
			parent:     "/opt/homebrew/bin/fish",
			loginShell: "/bin/zsh",
			want:       "fish",
		},
		{
			name:       "padded_parent_fish_overrides_login_zsh",
			parent:     "   /opt/homebrew/bin/fish   ",
			loginShell: "/bin/zsh",
			want:       "fish",
		},
		{
			name:       "login_shell_fallback",
			loginShell: "/usr/bin/fish",
			want:       "fish",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			binDir := t.TempDir()
			psBody := "#!/bin/sh\nexit 1\n"
			if tt.parent != "" {
				psBody = "#!/bin/sh\nprintf '%s\\n' '" + tt.parent + "'\n"
			}
			writeExecutable(t, filepath.Join(binDir, "ps"), psBody)

			got := runInstallerHelper(t, map[string]string{
				"HOME":  t.TempDir(),
				"PATH":  binDir,
				"SHELL": tt.loginShell,
			}, "detect_user_shell")

			if got != tt.want+"\n" {
				t.Fatalf("detect_user_shell output = %q, want %q", got, tt.want+"\n")
			}
		})
	}
}

func TestShowPathSetup_FishUsesNativePersistentCommand(t *testing.T) {
	t.Parallel()

	out := runInstallerHelper(t, map[string]string{
		"HOME":  "/Users/Example User",
		"PATH":  "/usr/bin:/bin",
		"SHELL": "/usr/bin/fish",
	}, "show_path_setup", "fish", "/Users/Example User/.local/bin")

	if !strings.Contains(out, `fish_add_path "$HOME/.local/bin"`) {
		t.Fatalf("Fish PATH instructions do not use fish_add_path:\n%s", out)
	}
	if strings.Contains(out, "config.fish") {
		t.Fatalf("Fish PATH instructions should not edit config.fish:\n%s", out)
	}
	if strings.Contains(out, "Restart your terminal") {
		t.Fatalf("fish_add_path should not require a restart:\n%s", out)
	}
}

func TestShowPathSetup_FishUsesProvidedInstallDir(t *testing.T) {
	t.Parallel()

	out := runInstallerHelper(t, map[string]string{
		"HOME":  "/Users/Example User",
		"PATH":  "/usr/bin:/bin",
		"SHELL": "/usr/bin/fish",
	}, "show_path_setup", "fish", "/opt/entire/bin")

	if !strings.Contains(out, `fish_add_path "/opt/entire/bin"`) {
		t.Fatalf("Fish PATH instructions do not use the provided install directory:\n%s", out)
	}
	if strings.Contains(out, `$HOME/.local/bin`) {
		t.Fatalf("Fish PATH instructions should not hard-code the default install directory:\n%s", out)
	}
}

func TestShowPathSetup_UnknownShellShowsFishAndPOSIXOptions(t *testing.T) {
	t.Parallel()

	out := runInstallerHelper(t, map[string]string{
		"HOME":  "/home/example",
		"PATH":  "/usr/bin:/bin",
		"SHELL": "/usr/bin/elvish",
	}, "show_path_setup", "", "/home/example/.local/bin")

	for _, want := range []string{
		"Fish:",
		`fish_add_path "$HOME/.local/bin"`,
		"Bash, Zsh, and other POSIX-compatible shells:",
		`export PATH="$HOME/.local/bin:$PATH"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PATH instructions missing %q:\n%s", want, out)
		}
	}
}

func runInstallerHelper(t *testing.T, env map[string]string, function string, args ...string) string {
	t.Helper()

	commandArgs := []string{"-c", `source "$1"; shift; "$@"`, "bash", installerPath(t), function}
	commandArgs = append(commandArgs, args...)
	cmd := exec.CommandContext(t.Context(), "/bin/bash", commandArgs...)
	cmd.Env = make([]string, 0, len(env))
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v\n%s", function, err, out)
	}
	return string(out)
}

func installerPath(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate installer test file")
	}
	return filepath.Join(filepath.Dir(filename), "install.sh")
}

func writeInstallerArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()

	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		header := &tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(content)),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write %s header: %v", name, err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatalf("write %s content: %v", name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar archive: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip archive: %v", err)
	}
	if err := os.WriteFile(path, archive.Bytes(), 0o644); err != nil {
		t.Fatalf("write installer archive: %v", err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
}
