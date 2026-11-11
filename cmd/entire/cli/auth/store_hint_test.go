package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/auth-go/tokenmanager"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// Structural markers of the two remedies; the underlying D-Bus error can never
// contain them, so they stay meaningful if the prose changes.
const (
	providerRemedyMarker  = "Secret Service provider"
	fileStoreRemedyMarker = "=file entire login"
)

// pinMachineShape makes the advice deterministic: no CI, and a PATH with no
// Secret Service provider on it (GitHub Actions sets CI, and developer
// machines may have gnome-keyring installed).
func pinMachineShape(t *testing.T) {
	t.Helper()
	t.Setenv("CI", "")
	t.Setenv("PATH", t.TempDir())
}

func serviceMissingChain() error {
	missing := &tokenstore.KeyringServiceMissingError{Cause: errors.New("The name org.freedesktop.secrets was not provided by any .service files")}
	return fmt.Errorf("read access token: %w", missing)
}

// A missing Secret Service provider names the login and its server and offers
// both ways forward, the file store one with the paths tokenstore reads.
func TestContextAuthError_KeyringServiceMissing(t *testing.T) {
	// Not parallel: the advice depends on process-global env.
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv(tokenstore.BackendEnvVar, "")
	t.Setenv(tokenstore.PathEnvVar, "")
	pinMachineShape(t)

	c := &contexts.Context{Name: "alice@core", CoreURL: "https://core.example/"}
	err := contextAuthError(c, serviceMissingChain())
	if !errors.Is(err, tokenstore.ErrKeyringServiceMissing) {
		t.Fatalf("error = %v, want ErrKeyringServiceMissing", err)
	}
	got := err.Error()
	for _, want := range []string{
		`cannot read the saved login for "alice@core" (https://core.example): no Secret Service provider`,
		"org.freedesktop.secrets was not provided",
		providerRemedyMarker,
		tokenstore.BackendEnvVar + fileStoreRemedyMarker,
		"log in again",
		tokenstore.ConfigFilePath(),
		tokenstore.FileBackendPath(),
		tokenstore.PathEnvVar,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("error is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "read access token") {
		t.Errorf("error repeats the store-read chain:\n%s", got)
	}
}

// With the file store already selected, suggesting it again would be
// nonsense; the provider remedy stays.
func TestContextAuthError_KeyringServiceMissing_FileStoreSelected(t *testing.T) {
	// Not parallel: mutates process-global env.
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv(tokenstore.BackendEnvVar, "file")
	pinMachineShape(t)

	c := &contexts.Context{Name: "alice@core", CoreURL: "https://core.example"}
	got := contextAuthError(c, serviceMissingChain()).Error()
	if !strings.Contains(got, providerRemedyMarker) {
		t.Errorf("error is missing the provider remedy:\n%s", got)
	}
	if strings.Contains(got, fileStoreRemedyMarker) || strings.Contains(got, tokenstore.PathEnvVar) {
		t.Errorf("error suggests the file store although it is selected:\n%s", got)
	}
}

// A missing provider wins over any re-login sentinel in the same chain: with
// the keyring unreadable, "expired" or "not logged in" is a guess.
func TestContextAuthError_KeyringServiceMissingWinsOverReauthSentinels(t *testing.T) {
	// Not parallel: the advice depends on process-global env.
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv(tokenstore.BackendEnvVar, "")

	c := &contexts.Context{Name: "alice@core", CoreURL: "https://core.example"}
	for _, sentinel := range []error{tokenmanager.ErrNotLoggedIn, tokenmanager.ErrReauthRequired} {
		err := contextAuthError(c, fmt.Errorf("load: %w", errors.Join(sentinel, serviceMissingChain())))
		if !errors.Is(err, tokenstore.ErrKeyringServiceMissing) {
			t.Fatalf("%v: error = %v, want ErrKeyringServiceMissing", sentinel, err)
		}
		if errors.Is(err, sentinel) {
			t.Errorf("%v: error %q still reports the re-login sentinel", sentinel, err)
		}
	}
}

// End to end through the refreshing provider: the store's error reaches the
// user as the reworded message, not the raw D-Bus chain.
func TestNewRefreshingLoginProvider_KeyringServiceMissing(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv(tokenstore.BackendEnvVar, "")
	pinMachineShape(t)
	t.Cleanup(tokenstore.UseMissingSecretServiceForTesting())

	svc := tokenstore.CoreKeyringService("https://core.example")
	c := &contexts.Context{Name: "alice@core", CoreURL: "https://core.example", Handle: "alice", KeychainService: svc}
	provider, err := NewRefreshingLoginProvider(c, failRoundTripper(t), false)
	if err != nil {
		t.Fatalf("NewRefreshingLoginProvider: %v", err)
	}
	_, err = provider(t.Context())
	if !errors.Is(err, tokenstore.ErrKeyringServiceMissing) {
		t.Fatalf("error = %v, want ErrKeyringServiceMissing", err)
	}
	if errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("error %v matches ErrNotLoggedIn, so callers add the plain login hint", err)
	}
	if got := err.Error(); !strings.HasPrefix(got, `cannot read the saved login for "alice@core"`) || !strings.Contains(got, fileStoreRemedyMarker) {
		t.Errorf("error = %q, want the reworded message with the file-store remedy", got)
	}
}

// The remedies are ordered by what the machine looks like, and neither is
// dropped unless it cannot apply.
func TestKeyringServiceMissingAdvice_Order(t *testing.T) {
	t.Parallel()

	const (
		start   = "is installed but not answering"
		install = "install and start a Secret Service provider"
	)
	tests := []struct {
		name             string
		provider         string
		ci               bool
		fileStoreCanHelp bool
		wantFirst        string // remedy that must come first
		want             []string
		wantNot          []string
	}{
		{name: "desktop: provider installed", provider: "gnome-keyring-daemon", fileStoreCanHelp: true,
			wantFirst: "gnome-keyring-daemon " + start, want: []string{fileStoreRemedyMarker}, wantNot: []string{install}},
		{name: "desktop, file store selected", provider: "kwalletd6",
			wantFirst: "kwalletd6 " + start, wantNot: []string{fileStoreRemedyMarker}},
		{name: "server: no provider found", fileStoreCanHelp: true,
			wantFirst: fileStoreRemedyMarker, want: []string{install}, wantNot: []string{start}},
		{name: "server, file store selected",
			wantFirst: install, wantNot: []string{fileStoreRemedyMarker, start}},
		{name: "CI: file store only", ci: true, provider: "gnome-keyring-daemon", fileStoreCanHelp: true,
			wantFirst: fileStoreRemedyMarker, wantNot: []string{start, install}},
		{name: "CI, file store selected keeps some advice", ci: true,
			wantFirst: install, wantNot: []string{fileStoreRemedyMarker}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := keyringServiceMissingAdvice(tt.provider, tt.ci, tt.fileStoreCanHelp)
			first := strings.Index(got, tt.wantFirst)
			if first < 0 {
				t.Fatalf("advice is missing %q:\n%s", tt.wantFirst, got)
			}
			for _, w := range tt.want {
				if i := strings.Index(got, w); i < 0 || i < first {
					t.Errorf("advice must contain %q after %q:\n%s", w, tt.wantFirst, got)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("advice must not contain %q:\n%s", w, got)
				}
			}
		})
	}
}

// The PATH scan finds a provider in an absolute PATH directory and ignores
// relative entries, which resolve against the caller's repository.
func TestInstalledSecretServiceProvider(t *testing.T) {
	// Not parallel: mutates PATH and the working directory.
	bin := t.TempDir()
	if got := withPath(t, bin); got != "" {
		t.Fatalf("empty PATH dir: provider = %q, want none", got)
	}

	writeExecutable(t, filepath.Join(bin, "kwalletd6"))
	writeExecutable(t, filepath.Join(bin, "gnome-keyring-daemon"))
	if got := withPath(t, bin); got != "gnome-keyring-daemon" {
		t.Errorf("provider = %q, want gnome-keyring-daemon (first in preference order)", got)
	}

	repo := t.TempDir()
	writeExecutable(t, filepath.Join(repo, "keepassxc"))
	t.Chdir(repo)
	if got := withPath(t, "."+string(os.PathListSeparator)+"relative"); got != "" {
		t.Errorf("relative PATH entries: provider = %q, want none", got)
	}

	if err := os.Mkdir(filepath.Join(repo, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "dir", "keepassxc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := withPath(t, filepath.Join(repo, "dir")); got != "" {
		t.Errorf("directory named like a provider: provider = %q, want none", got)
	}
}

func withPath(t *testing.T, path string) string {
	t.Helper()
	t.Setenv("PATH", path)
	return installedSecretServiceProvider()
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
