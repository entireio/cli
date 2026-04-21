package remote

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
)

func TestFetchURL(t *testing.T) {
	tests := []struct {
		name         string
		originURL    string
		settingsJSON string
		token        string
		wantURL      string
		wantErr      bool
	}{
		{
			name:         "checkpoint remote with token and https origin returns https checkpoint url",
			originURL:    "https://github.com/acme/app.git",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			token:        "secret-token",
			wantURL:      "https://github.com/acme/checkpoints.git",
		},
		{
			name:         "checkpoint remote with token and ssh origin returns https checkpoint url",
			originURL:    "git@github.com:acme/app.git",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			token:        "secret-token",
			wantURL:      "https://github.com/acme/checkpoints.git",
		},
		{
			name:         "checkpoint remote without token and https origin reuses https",
			originURL:    "https://github.com/acme/app.git",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "https://github.com/acme/checkpoints.git",
		},
		{
			name:         "checkpoint remote without token and ssh origin reuses ssh",
			originURL:    "git@github.com:acme/app.git",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "git@github.com:acme/checkpoints.git",
		},
		{
			name:         "no checkpoint remote with https origin returns origin url",
			originURL:    "https://github.com/acme/app.git",
			settingsJSON: `{"enabled":true}`,
			wantURL:      "https://github.com/acme/app.git",
		},
		{
			name:         "no checkpoint remote with ssh origin returns origin url",
			originURL:    "git@github.com:acme/app.git",
			settingsJSON: `{"enabled":true}`,
			wantURL:      "git@github.com:acme/app.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			runGit(t, repoDir, "remote", "add", "origin", tt.originURL)
			writeSettings(t, repoDir, tt.settingsJSON)
			t.Chdir(repoDir)
			if tt.token != "" {
				t.Setenv(checkpointTokenEnvVar, tt.token)
			}

			got, err := FetchURL(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatal("FetchURL() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchURL() error = %v", err)
			}
			if got != tt.wantURL {
				t.Fatalf("FetchURL() = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

func TestFetchURL_ErrorsWhenOriginMissing(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	_, err := FetchURL(context.Background())
	if err == nil {
		t.Fatal("FetchURL() error = nil, want error")
	}
}

func TestFetchURL_EdgeCases(t *testing.T) {
	tests := []struct {
		name         string
		addOrigin    bool
		originURL    string
		settingsJSON string
		token        string
		wantURL      string
		wantErr      bool
	}{
		{
			name:         "unsupported origin protocol without token falls back to origin",
			addOrigin:    true,
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "",
		},
		{
			name:         "unsupported origin protocol with token returns https checkpoint url",
			addOrigin:    true,
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			token:        "secret-token",
			wantURL:      "https://github.com/acme/checkpoints.git",
		},
		{
			name:         "missing origin with token returns https checkpoint url",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			token:        "secret-token",
			wantURL:      "https://github.com/acme/checkpoints.git",
		},
		{
			name:         "malformed settings with token falls back to origin because checkpoint remote config is unavailable",
			addOrigin:    true,
			settingsJSON: `{`,
			token:        "secret-token",
			wantURL:      "",
		},
		{
			name:         "malformed settings with token and ssh origin returns https origin url",
			originURL:    "git@github.com:acme/app.git",
			settingsJSON: `{`,
			token:        "secret-token",
			wantURL:      "https://github.com/acme/app.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)

			originURL := tt.originURL
			if tt.addOrigin {
				originDir := t.TempDir()
				initBareRepo(t, originDir)
				originURL = fileURL(originDir)
			}
			if originURL != "" {
				runGit(t, repoDir, "remote", "add", "origin", originURL)
			}

			writeSettings(t, repoDir, tt.settingsJSON)
			t.Chdir(repoDir)
			if tt.token != "" {
				t.Setenv(checkpointTokenEnvVar, tt.token)
			}

			got, err := FetchURL(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatal("FetchURL() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchURL() error = %v", err)
			}

			wantURL := tt.wantURL
			if wantURL == "" {
				wantURL = originURL
			}
			if got != wantURL {
				t.Fatalf("FetchURL() = %q, want %q", got, wantURL)
			}
		})
	}
}

func TestPushURL(t *testing.T) {
	tests := []struct {
		name         string
		originURL    string
		pushRemote   string
		pushURL      string
		settingsJSON string
		token        string
		wantURL      string
		wantEnabled  bool
		wantErr      bool
	}{
		{
			name:         "no checkpoint remote falls back to origin https url and reports disabled",
			originURL:    "https://github.com/acme/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true}`,
			wantURL:      "https://github.com/acme/app.git",
			wantEnabled:  false,
		},
		{
			name:         "no checkpoint remote falls back to origin ssh url and reports disabled",
			originURL:    "git@github.com:acme/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true}`,
			wantURL:      "git@github.com:acme/app.git",
			wantEnabled:  false,
		},
		{
			name:         "configured checkpoint remote with https push remote uses https",
			originURL:    "https://github.com/acme/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "https://github.com/acme/checkpoints.git",
			wantEnabled:  true,
		},
		{
			name:         "configured checkpoint remote with ssh push remote uses ssh",
			originURL:    "git@github.com:acme/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "git@github.com:acme/checkpoints.git",
			wantEnabled:  true,
		},
		{
			name:         "token forces https for push url with ssh remote",
			originURL:    "git@github.com:acme/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			token:        "push-token",
			wantURL:      "https://github.com/acme/checkpoints.git",
			wantEnabled:  true,
		},
		{
			name:         "token keeps https for push url with https remote",
			originURL:    "https://github.com/acme/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			token:        "push-token",
			wantURL:      "https://github.com/acme/checkpoints.git",
			wantEnabled:  true,
		},
		{
			name:         "different push remote owner disables checkpoint push url",
			originURL:    "https://github.com/fork/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "https://github.com/fork/app.git",
			wantEnabled:  false,
		},
		{
			name:         "missing push remote falls back to origin when checkpoint remote configured",
			originURL:    "https://github.com/acme/app.git",
			pushRemote:   "upstream",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "https://github.com/acme/app.git",
			wantEnabled:  false,
		},
		{
			name:         "no checkpoint remote falls back to requested push remote when origin missing",
			pushRemote:   "upstream",
			pushURL:      "https://github.com/acme/app.git",
			settingsJSON: `{"enabled":true}`,
			wantURL:      "https://github.com/acme/app.git",
			wantEnabled:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			if tt.originURL != "" {
				runGit(t, repoDir, "remote", "add", "origin", tt.originURL)
			}
			if tt.pushURL != "" {
				runGit(t, repoDir, "remote", "add", tt.pushRemote, tt.pushURL)
			}
			writeSettings(t, repoDir, tt.settingsJSON)
			t.Chdir(repoDir)
			if tt.token != "" {
				t.Setenv(checkpointTokenEnvVar, tt.token)
			}

			gotURL, gotEnabled, err := PushURL(context.Background(), tt.pushRemote)
			if tt.wantErr {
				if err == nil {
					t.Fatal("PushURL() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("PushURL() error = %v", err)
			}
			if gotEnabled != tt.wantEnabled {
				t.Fatalf("PushURL() enabled = %v, want %v", gotEnabled, tt.wantEnabled)
			}
			if gotURL != tt.wantURL {
				t.Fatalf("PushURL() URL = %q, want %q", gotURL, tt.wantURL)
			}
		})
	}
}

func TestPushURL_ErrorsWhenNoCheckpointRemoteAndOriginMissing(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	writeSettings(t, repoDir, `{"enabled":true}`)
	t.Chdir(repoDir)

	_, _, err := PushURL(context.Background(), "origin")
	if err == nil {
		t.Fatal("PushURL() error = nil, want error")
	}
}

func TestConfigured_MalformedSettingsTreatedAsNotConfigured(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	writeSettings(t, repoDir, `{`)
	t.Chdir(repoDir)

	configured := Configured(context.Background())
	if configured {
		t.Fatal("Configured() = true, want false")
	}
}

func writeSettings(t *testing.T, repoDir, content string) {
	t.Helper()
	entireDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", entireDir, err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(settings.json) error = %v", err)
	}
}

func TestRunGitHelperUsesGitCLI(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	cmd := exec.CommandContext(context.Background(), "git", "rev-parse", "--git-dir")
	cmd.Dir = repoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git rev-parse failed: %v\nOutput: %s", err, output)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\nOutput: %s", args, err, output)
	}
}

func initBareRepo(t *testing.T, repoDir string) {
	t.Helper()
	if _, err := git.PlainInit(repoDir, true); err != nil {
		t.Fatalf("PlainInit(%s, bare) error = %v", repoDir, err)
	}
}

func fileURL(path string) string {
	return "file://" + filepath.ToSlash(path)
}
