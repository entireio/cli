package remote

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
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
		{
			name:         "token drops ssh port when coercing ssh origin to https",
			originURL:    "ssh://git@git.example.com:2222/acme/app.git",
			settingsJSON: `{"enabled":true}`,
			token:        "secret-token",
			wantURL:      "https://git.example.com/acme/app.git",
		},
		{
			name:         "token preserves https port when source is already https",
			originURL:    "https://git.example.com:8443/acme/app.git",
			settingsJSON: `{"enabled":true}`,
			token:        "secret-token",
			wantURL:      "https://git.example.com:8443/acme/app.git",
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
				t.Setenv(CheckpointTokenEnvVar, tt.token)
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
			name:         "unsupported origin protocol without token routes to provider checkpoint url (ssh default)",
			addOrigin:    true,
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "git@github.com:acme/checkpoints.git",
		},
		{
			name:         "entire:// origin without token routes to provider checkpoint url (ssh default)",
			originURL:    "entire://app.entire.io/gh/acme/app",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "git@github.com:acme/checkpoints.git",
		},
		{
			name:         "non-derivable origin with unknown provider falls back to origin",
			originURL:    "entire://app.entire.io/gh/acme/app",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"bitbucket","repo":"acme/checkpoints"}}}`,
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
				t.Setenv(CheckpointTokenEnvVar, tt.token)
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
			name:         "token forces https for origin fallback when no checkpoint remote is configured",
			originURL:    "git@github.com:acme/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true}`,
			token:        "push-token",
			wantURL:      "https://github.com/acme/app.git",
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
			name:         "token drops ssh port when coercing ssh origin to https",
			originURL:    "ssh://git@git.example.com:2222/acme/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			token:        "push-token",
			wantURL:      "https://git.example.com/acme/checkpoints.git",
			wantEnabled:  true,
		},
		{
			name:         "token preserves https port when source is already https",
			originURL:    "https://git.example.com:8443/acme/app.git",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			token:        "push-token",
			wantURL:      "https://git.example.com:8443/acme/checkpoints.git",
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
			name:         "entire:// origin routes to provider checkpoint url (ssh default)",
			originURL:    "entire://app.entire.io/gh/acme/app",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "git@github.com:acme/checkpoints.git",
			wantEnabled:  true,
		},
		{
			name:         "file:// origin routes to provider checkpoint url (ssh default)",
			originURL:    "file:///acme/app",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "git@github.com:acme/checkpoints.git",
			wantEnabled:  true,
		},
		{
			name:         "non-derivable origin with unknown provider falls back to origin",
			originURL:    "entire://app.entire.io/gh/acme/app",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"bitbucket","repo":"acme/checkpoints"}}}`,
			wantURL:      "entire://app.entire.io/gh/acme/app",
			wantEnabled:  false,
		},
		{
			name:         "token with entire:// origin routes to provider host not origin host",
			originURL:    "entire://app.entire.io/gh/acme/app",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			token:        "push-token",
			wantURL:      "https://github.com/acme/checkpoints.git",
			wantEnabled:  true,
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
				t.Setenv(CheckpointTokenEnvVar, tt.token)
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

// TestPushURL_EntireOriginReusesProviderRemoteScheme reproduces the real-world
// setup: origin migrated to an entire:// URL (forge-prefixed /gh/owner/repo)
// with a github checkpoint_remote. The checkpoint URL must route to github
// rather than fall back to the entire:// origin, reusing the auth/scheme the
// repo had for that endpoint — a token forces HTTPS, then an existing remote
// on the provider host, then defaulting to SSH.
func TestPushURL_EntireOriginReusesProviderRemoteScheme(t *testing.T) {
	const entireOrigin = "entire://aws-eu-central-1.entire.io/gh/entireio/cli"
	tests := []struct {
		name        string
		githubURL   string
		token       string
		wantURL     string
		wantEnabled bool
	}{
		{
			name:        "ssh github remote yields ssh checkpoint url",
			githubURL:   "git@github.com:entireio/cli.git",
			wantURL:     "git@github.com:entireio/cli-checkpoints.git",
			wantEnabled: true,
		},
		{
			name:        "https github remote yields https checkpoint url",
			githubURL:   "https://github.com/entireio/cli.git",
			wantURL:     "https://github.com/entireio/cli-checkpoints.git",
			wantEnabled: true,
		},
		{
			name:        "no signal defaults to ssh",
			wantURL:     "git@github.com:entireio/cli-checkpoints.git",
			wantEnabled: true,
		},
		{
			name:        "token forces https over existing ssh remote",
			githubURL:   "git@github.com:entireio/cli.git",
			token:       "ci-token",
			wantURL:     "https://github.com/entireio/cli-checkpoints.git",
			wantEnabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			runGit(t, repoDir, "remote", "add", "origin", entireOrigin)
			if tt.githubURL != "" {
				runGit(t, repoDir, "remote", "add", "github", tt.githubURL)
			}
			writeSettings(t, repoDir, `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"entireio/cli-checkpoints"}}}`)
			t.Chdir(repoDir)
			if tt.token != "" {
				t.Setenv(CheckpointTokenEnvVar, tt.token)
			}

			gotURL, gotEnabled, err := PushURL(context.Background(), "origin")
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

// TestDeriveCheckpointURLFromInfo covers the push-remote to checkpoint-remote
// URL mapping (previously exercised cross-package via the removed
// DeriveCheckpointURL wrapper).
func TestDeriveCheckpointURLFromInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		pushRemoteURL  string
		checkpointRepo string
		want           string
		wantParseErr   bool
		wantDeriveErr  bool
	}{
		{
			name:           "SSH push remote",
			pushRemoteURL:  "git@github.com:org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "git@github.com:org/checkpoints.git",
		},
		{
			name:           "HTTPS push remote",
			pushRemoteURL:  "https://github.com/org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "https://github.com/org/checkpoints.git",
		},
		{
			name:           "SSH protocol push remote",
			pushRemoteURL:  "ssh://git@github.com/org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "git@github.com:org/checkpoints.git",
		},
		{
			name:           "different host",
			pushRemoteURL:  "git@github.example.com:org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "git@github.example.com:org/checkpoints.git",
		},
		{
			name:           "HTTPS with non-standard port",
			pushRemoteURL:  "https://git.example.com:8443/org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "https://git.example.com:8443/org/checkpoints.git",
		},
		{
			name:           "SSH protocol with non-standard port",
			pushRemoteURL:  "ssh://git@git.example.com:2222/org/main-repo.git",
			checkpointRepo: "org/checkpoints",
			want:           "ssh://git@git.example.com:2222/org/checkpoints.git",
		},
		{
			name:          "invalid push remote",
			pushRemoteURL: "not-a-url",
			wantParseErr:  true,
		},
		{
			name:           "unsupported protocol",
			pushRemoteURL:  "file:///tmp/repo.git",
			checkpointRepo: "org/checkpoints",
			wantDeriveErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info, err := ParseURL(tt.pushRemoteURL)
			if tt.wantParseErr {
				if err == nil {
					t.Fatalf("ParseURL(%q) = nil error, want parse error", tt.pushRemoteURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURL(%q) error = %v", tt.pushRemoteURL, err)
			}

			config := &settings.CheckpointRemoteConfig{Provider: "github", Repo: tt.checkpointRepo}
			got, err := deriveCheckpointURLFromInfo(info, config)
			if tt.wantDeriveErr {
				if err == nil {
					t.Fatalf("deriveCheckpointURLFromInfo(%q) = %q, nil error; want error", tt.pushRemoteURL, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("deriveCheckpointURLFromInfo(%q) error = %v", tt.pushRemoteURL, err)
			}
			if got != tt.want {
				t.Errorf("deriveCheckpointURLFromInfo(%q) = %q, want %q", tt.pushRemoteURL, got, tt.want)
			}
		})
	}
}
