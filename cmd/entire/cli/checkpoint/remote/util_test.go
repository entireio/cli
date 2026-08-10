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
			name:         "entire:// origin without token derives mirror checkpoint url on same cluster",
			originURL:    "entire://app.entire.io/gh/acme/app",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "entire://app.entire.io/gh/acme/checkpoints",
		},
		{
			name:         "entire:// origin with forge not matching provider routes to provider checkpoint url (ssh default)",
			originURL:    "entire://app.entire.io/et/acme/app",
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

//nolint:maintidx // table-driven: the score tracks the size of the case table (data), not branching logic; splitting the table would scatter closely related URL-resolution cases
func TestPushURL(t *testing.T) {
	tests := []struct {
		name              string
		originURL         string
		originPushURL     string
		pushRemote        string
		pushURL           string
		extraPushURLs     []string
		settingsJSON      string
		settingsLocalJSON string
		token             string
		wantURL           string
		wantEnabled       bool
		wantErr           bool
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
			name:         "entire:// origin derives mirror checkpoint url on same cluster",
			originURL:    "entire://app.entire.io/gh/acme/app",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "entire://app.entire.io/gh/acme/checkpoints",
			wantEnabled:  true,
		},
		{
			name:         "entire:// origin with forge not matching provider routes to provider checkpoint url (ssh default)",
			originURL:    "entire://app.entire.io/et/acme/app",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "git@github.com:acme/checkpoints.git",
			wantEnabled:  true,
		},
		{
			name:         "entire:// origin with different owner disables checkpoint push url",
			originURL:    "entire://app.entire.io/gh/fork/app",
			pushRemote:   "origin",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "entire://app.entire.io/gh/fork/app",
			wantEnabled:  false,
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
		{
			// A differently-owned push destination reads as inherited even when
			// origin's owner matches, because that combination is exactly the
			// "cloned the base repo, added my fork" contributor topology and is
			// indistinguishable from this one (own repo + backup remote in
			// another org) using local git config alone. Erring toward "not
			// ours" keeps transcripts out of repositories we cannot confirm we
			// own; pushes to origin still route to the checkpoint repo, and
			// ENT-1451's pre-push gate blocks checkpoint sync on pushes to a
			// non-elected remote anyway.
			name:         "differently owned push remote reads as inherited even when origin owner matches",
			originURL:    "https://github.com/acme/app.git",
			pushRemote:   "backup",
			pushURL:      "https://github.com/otherorg/backup.git",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "https://github.com/acme/app.git",
			wantEnabled:  false,
		},
		{
			// git fans out to every push URL on the git-branch backend, so
			// ownership has to hold for the whole set: a differently-owned mirror
			// in SECOND position would otherwise read as ours and receive session
			// transcripts. The same-owner URL is deliberately first, so a
			// first-URL-only check would pass this.
			//
			// Two extraPushURLs, not one: the first `pushurl` REPLACES the
			// remote's url as a push destination (git's push_url_of_remote), so a
			// single --push --add yields a one-URL remote, not a two-URL one.
			name:          "differently owned second push url reads as inherited",
			originURL:     "https://github.com/acme/app.git",
			pushRemote:    "mirror",
			pushURL:       "https://github.com/acme/app.git",
			extraPushURLs: []string{"https://github.com/acme/app.git", "https://github.com/otherorg/mirror.git"},
			settingsJSON:  `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:       "https://github.com/acme/app.git",
			wantEnabled:   false,
		},
		{
			// Same shape, every push URL owned by the checkpoint owner: still ours.
			name:          "multiple same-owner push urls keep the checkpoint remote",
			originURL:     "https://github.com/acme/app.git",
			pushRemote:    "mirror",
			pushURL:       "https://github.com/acme/app.git",
			extraPushURLs: []string{"https://github.com/acme/app.git", "https://github.com/acme/mirror.git"},
			settingsJSON:  `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:       "https://github.com/acme/checkpoints.git",
			wantEnabled:   true,
		},
		{
			// The regression this change exists for: a contributor who cloned
			// the UPSTREAM base repo and added their own fork. Origin belongs to
			// upstream, so an origin-only ownership check read the inherited
			// committed setting as ours and aimed the contributor's session
			// transcripts at the upstream project's checkpoint repo.
			name:         "committed setting is inherited when origin is upstream and we push to our fork",
			originURL:    "https://github.com/acme/app.git",
			pushRemote:   "myfork",
			pushURL:      "https://github.com/contributor/app.git",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "https://github.com/acme/app.git",
			wantEnabled:  false,
		},
		{
			// Same topology, but the developer confirmed the checkpoint repo is
			// theirs by putting it in the gitignored settings.local.json. That
			// provenance short-circuit still wins — it is the escape hatch for
			// every case the owner comparison has to refuse.
			name:              "settings.local.json still wins in the fork topology",
			originURL:         "https://github.com/acme/app.git",
			pushRemote:        "myfork",
			pushURL:           "https://github.com/contributor/app.git",
			settingsJSON:      `{"enabled":true}`,
			settingsLocalJSON: `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:           "https://github.com/acme/checkpoints.git",
			wantEnabled:       true,
		},
		{
			// The escape hatch for a checkpoint repo owned by a different account
			// or org than origin: settings.local.json is gitignored and per-clone,
			// so a setting there cannot have been inherited by forking.
			name:              "checkpoint remote from settings.local.json is honored despite mismatched origin owner",
			originURL:         "https://github.com/fork/app.git",
			pushRemote:        "origin",
			settingsJSON:      `{"enabled":true}`,
			settingsLocalJSON: `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:           "https://github.com/acme/checkpoints.git",
			wantEnabled:       true,
		},
		{
			// Regression: ownership is decided by origin, but a repo can have no
			// remote named origin at all. The push remote is then the only identity
			// it has, and a matching owner must keep the configured checkpoint
			// remote rather than silently falling back.
			name:         "no origin remote falls back to the push remote owner and keeps the checkpoint remote",
			pushRemote:   "upstream",
			pushURL:      "https://github.com/acme/app.git",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "https://github.com/acme/checkpoints.git",
			wantEnabled:  true,
		},
		{
			// The same topology with a mismatched owner still reads as inherited.
			name:         "no origin remote with a differently owned push remote stays disabled",
			pushRemote:   "upstream",
			pushURL:      "https://github.com/fork/app.git",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:      "https://github.com/fork/app.git",
			wantEnabled:  false,
		},
		{
			// Transport comes from where the push actually goes: a remote with a
			// pushurl pushes there, not to its (fetch) url. Reading the plain url
			// would derive https here.
			name:          "checkpoint url derives transport from the push url, not the fetch url",
			originURL:     "https://github.com/acme/app.git",
			originPushURL: "git@github.com:acme/app.git",
			pushRemote:    "origin",
			settingsJSON:  `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			wantURL:       "git@github.com:acme/checkpoints.git",
			wantEnabled:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			if tt.originURL != "" {
				runGit(t, repoDir, "remote", "add", "origin", tt.originURL)
			}
			if tt.originPushURL != "" {
				runGit(t, repoDir, "remote", "set-url", "--push", "origin", tt.originPushURL)
			}
			if tt.pushURL != "" {
				runGit(t, repoDir, "remote", "add", tt.pushRemote, tt.pushURL)
			}
			for _, extra := range tt.extraPushURLs {
				runGit(t, repoDir, "remote", "set-url", "--push", "--add", tt.pushRemote, extra)
			}
			writeSettings(t, repoDir, tt.settingsJSON)
			if tt.settingsLocalJSON != "" {
				writeLocalSettings(t, repoDir, tt.settingsLocalJSON)
			}
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

// TestPushURL_EntireOriginDerivesMirrorURL reproduces the real-world setup:
// origin migrated to an entire:// URL (forge-prefixed /gh/owner/repo) with a
// github checkpoint_remote. Checkpoints must follow origin through the
// push-through mirror on the same cluster — even when leftover direct github
// remotes (e.g. URL-named promisor entries from filtered fetches) exist. The
// exception is a checkpoint token, which is an HTTPS credential for the
// provider host and therefore forces direct provider HTTPS.
func TestPushURL_EntireOriginDerivesMirrorURL(t *testing.T) {
	const entireOrigin = "entire://aws-ap-southeast-2.entire.io/gh/entireio/cli"
	const mirrorCheckpointURL = "entire://aws-ap-southeast-2.entire.io/gh/entireio/cli-checkpoints"
	tests := []struct {
		name        string
		githubURL   string
		token       string
		wantURL     string
		wantEnabled bool
	}{
		{
			name:        "existing ssh github remote does not divert checkpoints off the mirror",
			githubURL:   "git@github.com:entireio/cli.git",
			wantURL:     mirrorCheckpointURL,
			wantEnabled: true,
		},
		{
			name:        "existing https github remote does not divert checkpoints off the mirror",
			githubURL:   "https://github.com/entireio/cli.git",
			wantURL:     mirrorCheckpointURL,
			wantEnabled: true,
		},
		{
			name:        "entire origin alone derives mirror checkpoint url",
			wantURL:     mirrorCheckpointURL,
			wantEnabled: true,
		},
		{
			name:        "token forces https on the provider host",
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

// writeLocalSettings writes .entire/settings.local.json — the gitignored
// per-developer override, which is what marks a checkpoint_remote as this
// developer's own rather than inherited from an upstream project.
func writeLocalSettings(t *testing.T, repoDir, content string) {
	t.Helper()
	entireDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", entireDir, err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(settings.local.json) error = %v", err)
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
			name:           "entire push remote keeps cluster and forge",
			pushRemoteURL:  "entire://aws-ap-southeast-2.entire.io/gh/org/main-repo",
			checkpointRepo: "org/checkpoints",
			want:           "entire://aws-ap-southeast-2.entire.io/gh/org/checkpoints",
		},
		{
			name:           "entire push remote with non-standard port",
			pushRemoteURL:  "entire://cluster.example.com:8443/gh/org/main-repo",
			checkpointRepo: "org/checkpoints",
			want:           "entire://cluster.example.com:8443/gh/org/checkpoints",
		},
		{
			name:           "entire push remote with forge not matching provider",
			pushRemoteURL:  "entire://aws-ap-southeast-2.entire.io/et/org/main-repo",
			checkpointRepo: "org/checkpoints",
			wantDeriveErr:  true,
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
