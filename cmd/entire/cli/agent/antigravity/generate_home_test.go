package antigravity

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func newTestTextGenerationAuth(t *testing.T) textGenerationAuth {
	t.Helper()
	home := t.TempDir()
	configDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return textGenerationAuth{
		userHome:  home,
		configDir: configDir,
		// Explicit fake ADC overrides any real inherited credential pointer
		// when a generation test executes its mock process.
		adcCredentials: filepath.Join(home, "fake-adc.json"),
	}
}

func writeTextGenerationFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func prepareTestHome(t *testing.T, auth textGenerationAuth) (string, []string) {
	t.Helper()
	home := t.TempDir()
	env, err := isolatedHomeEnv(home, auth)
	if err != nil {
		t.Fatalf("isolatedHomeEnv: %v", err)
	}
	return home, env
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%q should be absent (stat error: %v)", path, err)
	}
}

func TestIsolatedHomeEnv_OnlyCarriesAuthentication(t *testing.T) {
	t.Parallel()
	auth := newTestTextGenerationAuth(t)
	writeTextGenerationFixture(t, filepath.Join(auth.configDir, agyFileTokenName), "FAKE_TOKEN")
	writeTextGenerationFixture(t, filepath.Join(auth.configDir, agySettingsFileName), `{
  "modelProvider": "gemini",
  "title": {"type": "command", "command": "must-not-run"},
  "trustedWorkspaces": ["/somewhere"],
  "mcpServers": {"unsafe": {"command": "must-not-run"}}
}`)
	writeTextGenerationFixture(t, filepath.Join(auth.userHome, ".gemini", "config", "hooks.json"), `{"unsafe":"must-not-run"}`)
	writeTextGenerationFixture(t, filepath.Join(auth.userHome, ".gemini", "config", "skills", "unsafe.md"), "must-not-load")
	keychainName := filepath.Join("Library", "Keychains", "login.keychain-db")
	writeTextGenerationFixture(t, filepath.Join(auth.userHome, keychainName), "FAKE_KEYCHAIN")

	home, env := prepareTestHome(t, auth)
	for _, entry := range []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		adcCredentialsEnvVar + "=" + auth.adcCredentials,
	} {
		if !slices.Contains(env, entry) {
			t.Errorf("missing isolated environment entry %q", entry)
		}
	}
	if runtime.GOOS == "windows" && !slices.Contains(env, "USERPROFILE="+home) {
		t.Error("USERPROFILE must point at the isolated home on Windows")
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	settings, err := os.ReadFile(filepath.Join(dir, agySettingsFileName))
	if err != nil || string(settings) != `{"modelProvider":"gemini"}` {
		t.Fatalf("isolated settings = (%q, %v), want only Gemini API-key mode", settings, err)
	}
	token, err := os.ReadFile(filepath.Join(dir, agyFileTokenName))
	if err != nil || string(token) != "FAKE_TOKEN" {
		t.Fatalf("isolated token = (%q, %v), want the fake token", token, err)
	}
	assertMissing(t, filepath.Join(home, ".gemini", "config"))
	if runtime.GOOS == "darwin" {
		target, err := os.Readlink(filepath.Join(home, keychainName))
		if err != nil || target != filepath.Join(auth.userHome, keychainName) {
			t.Fatalf("keychain link = (%q, %v), want the fake keychain file", target, err)
		}
	} else {
		assertMissing(t, filepath.Join(home, "Library"))
	}
}

func TestIsolatedHomeEnv_WritesNoSettingsWithoutAPIKeyMode(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"no file":        "",
		"no provider":    `{"title": {"type": "command", "command": "x"}}`,
		"other provider": `{"modelProvider": "$(touch /tmp/x)"}`,
		"malformed":      `{"modelProvider":`,
		"symlinked":      "symlink",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			auth := newTestTextGenerationAuth(t)
			settingsPath := filepath.Join(auth.configDir, agySettingsFileName)
			switch content {
			case "":
			case "symlink":
				// Even a link staying inside the config root is refused.
				target := filepath.Join(auth.configDir, "elsewhere.json")
				writeTextGenerationFixture(t, target, `{"modelProvider":"gemini"}`)
				if err := os.Symlink(filepath.Base(target), settingsPath); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("symlinks unavailable: %v", err)
					}
					t.Fatal(err)
				}
			default:
				writeTextGenerationFixture(t, settingsPath, content)
			}
			home, _ := prepareTestHome(t, auth)
			assertMissing(t, filepath.Join(home, ".gemini"))
		})
	}
}

func TestIsolatedHomeEnv_RefusesASymlinkedFileToken(t *testing.T) {
	t.Parallel()
	auth := newTestTextGenerationAuth(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	writeTextGenerationFixture(t, target, "FAKE_TOKEN")
	if err := os.Symlink(target, filepath.Join(auth.configDir, agyFileTokenName)); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	home, _ := prepareTestHome(t, auth)
	assertMissing(t, filepath.Join(home, ".gemini"))
}

func TestLinkLoginKeychain_OnlyLinksTheFixtureFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("production only links the login keychain on macOS")
	}
	realHome := t.TempDir()
	home := t.TempDir()
	if err := linkLoginKeychain(home, realHome); err != nil {
		t.Fatalf("missing keychain: %v", err)
	}
	assertMissing(t, filepath.Join(home, "Library"))
	name := filepath.Join("Library", "Keychains", "login.keychain-db")
	source := filepath.Join(realHome, name)
	writeTextGenerationFixture(t, source, "FAKE_KEYCHAIN")
	if err := linkLoginKeychain(home, realHome); err != nil {
		t.Fatalf("link fake keychain: %v", err)
	}
	target, err := os.Readlink(filepath.Join(home, name))
	if err != nil || target != source {
		t.Fatalf("keychain link = (%q, %v), want only %q", target, err, source)
	}
	// The parent is a real isolated directory, not a link into the source home.
	info, err := os.Lstat(filepath.Join(home, "Library", "Keychains"))
	if err != nil || !info.IsDir() {
		t.Fatalf("isolated keychain parent is not a directory (error: %v)", err)
	}
}

func TestResolveTextGenerationAuth_RefusedConfigDoesNotFallBack(t *testing.T) {
	auth := newTestTextGenerationAuth(t)
	t.Setenv("HOME", auth.userHome)
	t.Setenv("USERPROFILE", auth.userHome)
	t.Setenv(configDirEnv, "relative-config")
	t.Setenv(adcCredentialsEnvVar, "")
	// A refused override must not silently carry credentials from the default
	// directory, even though that directory exists in our synthetic home.
	writeTextGenerationFixture(t, filepath.Join(auth.configDir, agyFileTokenName), "DEFAULT_TOKEN")
	resolved := resolveTextGenerationAuth()
	if resolved.configDir != "" {
		t.Fatalf("refused config resolved to %q", resolved.configDir)
	}
	home, _ := prepareTestHome(t, resolved)
	assertMissing(t, filepath.Join(home, ".gemini"))
}

func TestADCCredentialsEnv_OnlyUsesExplicitSources(t *testing.T) {
	t.Parallel()
	for _, explicit := range []bool{false, true} {
		name := "default file"
		if explicit {
			name = "explicit file"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			auth := newTestTextGenerationAuth(t)
			if !explicit {
				auth.adcCredentials = ""
			}
			want := auth.adcCredentials
			if !explicit {
				if got := adcCredentialsEnv(auth); !slices.Equal(got, []string{adcCredentialsEnvVar + "="}) {
					t.Fatalf("missing default file must clear ambient ADC, got %q", got)
				}
				want = filepath.Join(auth.userHome, adcCredentialsFile)
				writeTextGenerationFixture(t, want, `{}`)
			}
			got := adcCredentialsEnv(auth)
			if !explicit && runtime.GOOS == "windows" {
				if !slices.Equal(got, []string{adcCredentialsEnvVar + "="}) {
					t.Fatalf("Windows default ADC must clear the ambient pointer and keep using APPDATA, got %q", got)
				}
			} else if !slices.Equal(got, []string{adcCredentialsEnvVar + "=" + want}) {
				t.Fatalf("ADC override = %q, want only %q", got, want)
			}
		})
	}
}

func TestIsolatedHomeEnv_EmptySourcesDoNotFallBack(t *testing.T) {
	t.Parallel()
	home, env := prepareTestHome(t, textGenerationAuth{})
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty sources populated the isolated home: %v (error: %v)", entries, err)
	}
	if !slices.Contains(env, adcCredentialsEnvVar+"=") {
		t.Errorf("empty sources must explicitly clear ambient ADC, got %q", env)
	}
}

func TestIsolatedHomeEnv_IgnoresAmbientCredentialSources(t *testing.T) {
	// Synthetic ambient credentials stand in for the developer's files. None
	// may be discovered when the caller injects independent empty sources.
	ambient := newTestTextGenerationAuth(t)
	t.Setenv("HOME", ambient.userHome)
	t.Setenv("USERPROFILE", ambient.userHome)
	t.Setenv(configDirEnv, ambient.configDir)
	t.Setenv(adcCredentialsEnvVar, ambient.adcCredentials)
	writeTextGenerationFixture(t, filepath.Join(ambient.configDir, agyFileTokenName), "AMBIENT_TOKEN")
	writeTextGenerationFixture(t, filepath.Join(ambient.configDir, agySettingsFileName), `{"modelProvider":"gemini"}`)
	writeTextGenerationFixture(t, filepath.Join(ambient.userHome, "Library", "Keychains", "login.keychain-db"), "AMBIENT_KEYCHAIN")
	writeTextGenerationFixture(t, filepath.Join(ambient.userHome, adcCredentialsFile), `{}`)

	home, env := prepareTestHome(t, textGenerationAuth{})
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("ambient credentials populated the isolated home: %v (error: %v)", entries, err)
	}
	for _, entry := range env {
		if strings.Contains(entry, ambient.userHome) {
			t.Errorf("ambient credential location leaked into overrides: %q", entry)
		}
	}
}
