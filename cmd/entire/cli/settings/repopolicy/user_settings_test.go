package repopolicy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeUserSettingsForTest(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, UserSettingsFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadUserSettings_MissingMalformedAndStrict(t *testing.T) {
	t.Run("missing is unconfigured", func(t *testing.T) {
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		settings, err := LoadUserSettings(t.Context())
		if err != nil {
			t.Fatalf("LoadUserSettings: %v", err)
		}
		if settings.Global != nil {
			t.Fatalf("missing file Global = %+v, want nil", settings.Global)
		}
	})

	t.Run("malformed fails closed", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", dir)
		writeUserSettingsForTest(t, dir, `{not json`)
		if _, err := LoadUserSettings(t.Context()); err == nil {
			t.Fatal("malformed settings must return an error")
		}
	})

	t.Run("unknown fields fail strict decoding", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", dir)
		writeUserSettingsForTest(t, dir, `{"global":{"enabled":true,"exclud_paths":["/private/**"]}}`)
		if _, err := LoadUserSettings(t.Context()); err == nil {
			t.Fatal("unknown settings fields must return an error")
		}
	})

	t.Run("trailing junk fails strict decoding", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", dir)
		writeUserSettingsForTest(t, dir, `{"global":{"enabled":true}} trailing`)
		if _, err := LoadUserSettings(t.Context()); err == nil {
			t.Fatal("trailing junk must return an error")
		}
	})

	t.Run("second JSON value fails strict decoding", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", dir)
		writeUserSettingsForTest(t, dir, `{"global":{"enabled":true}} {}`)
		if _, err := LoadUserSettings(t.Context()); err == nil {
			t.Fatal("a second JSON value must return an error")
		}
	})
}

func TestValidateGlobalConfig_InvalidExclusionsFailClosed(t *testing.T) {
	t.Parallel()

	config := &GlobalConfig{
		Enabled:        true,
		ExcludePaths:   []string{"relative/**"},
		ExcludeOrigins: []string{"["},
	}
	problems := ValidateGlobalPatterns(config)
	if len(problems) != 2 {
		t.Fatalf("ValidateGlobalPatterns problems = %v, want two invalid exclusions", problems)
	}

	resolved := false
	policy, err := ClassifyGlobalConfig(t.Context(), config, func(context.Context) (Repository, error) {
		resolved = true
		return Repository{}, nil
	})
	if err == nil {
		t.Fatal("invalid exclusions must return an error")
	}
	if resolved {
		t.Fatal("invalid exclusions must fail closed before repository resolution")
	}
	if policy.Active || policy.InactiveReason != InactiveReasonGlobalExcluded {
		t.Fatalf("policy = %+v, want inactive global exclusion", policy)
	}
}

func TestModifyUserSettings_ConcurrentTrustWritesDoNotClobber(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	if err := ModifyUserSettings(ctx, func(settings *UserSettings) error {
		settings.Global = &GlobalConfig{Enabled: true}
		return nil
	}); err != nil {
		t.Fatalf("initial ModifyUserSettings: %v", err)
	}

	const writes = 25
	errs := make(chan error, 2)
	go func() {
		for i := range writes {
			err := ModifyUserSettings(ctx, func(settings *UserSettings) error {
				settings.Global.TrustedOrigins = append(settings.Global.TrustedOrigins, "github.com/acme/repo-"+string(rune('a'+i)))
				return nil
			})
			if err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	go func() {
		for i := range writes {
			err := ModifyUserSettings(ctx, func(settings *UserSettings) error {
				settings.Global.TrustedPaths = append(settings.Global.TrustedPaths, "/srv/repo-"+string(rune('a'+i)))
				return nil
			})
			if err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("ModifyUserSettings: %v", err)
		}
	}

	settings, err := LoadUserSettings(ctx)
	if err != nil {
		t.Fatalf("LoadUserSettings: %v", err)
	}
	if got := len(settings.Global.TrustedOrigins); got != writes {
		t.Fatalf("trusted origins = %d, want %d", got, writes)
	}
	if got := len(settings.Global.TrustedPaths); got != writes {
		t.Fatalf("trusted paths = %d, want %d", got, writes)
	}
}
