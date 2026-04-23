package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/x/plugin/objectsigner/auto"
)

func TestHasCustomSSHSignProgram(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  *format.Config
		want bool
	}{
		{
			name: "nil raw config",
			raw:  nil,
			want: false,
		},
		{
			name: "empty raw config",
			raw:  format.New(),
			want: false,
		},
		{
			name: "custom program set",
			raw: func() *format.Config {
				c := format.New()
				c.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")
				return c
			}(),
			want: true,
		},
		{
			name: "default ssh-keygen is not custom",
			raw: func() *format.Config {
				c := format.New()
				c.Section("gpg").Subsection("ssh").SetOption("program", "ssh-keygen")
				return c
			}(),
			want: false,
		},
		{
			name: "gpg section exists but no ssh.program",
			raw: func() *format.Config {
				c := format.New()
				c.Section("gpg").SetOption("format", "ssh")
				return c
			}(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := hasCustomSSHSignProgram(tt.raw)
			if got != tt.want {
				t.Errorf("hasCustomSSHSignProgram() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigMerge_DropsCustomSSHSignProgramFromRaw(t *testing.T) {
	t.Parallel()

	sysCfg := config.NewConfig()
	sysCfg.GPG.Format = string(auto.FormatSSH)
	sysCfg.Raw.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")

	merged := config.Merge(sysCfg, config.NewConfig())

	if !hasCustomSSHSignProgram(sysCfg.Raw) {
		t.Fatal("expected scoped raw config to report custom gpg.ssh.program")
	}

	if hasCustomSSHSignProgram(merged.Raw) {
		t.Fatal("expected merged raw config to lose custom gpg.ssh.program")
	}
}

func TestCustomSSHSignProgramDetection_UsesScopedRawBeforeMerge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		sysRaw    *format.Config
		globalRaw *format.Config
		want      bool
	}{
		{
			name: "system scope custom program",
			sysRaw: func() *format.Config {
				raw := format.New()
				raw.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")
				return raw
			}(),
			globalRaw: format.New(),
			want:      true,
		},
		{
			name:   "global scope custom program",
			sysRaw: format.New(),
			globalRaw: func() *format.Config {
				raw := format.New()
				raw.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")
				return raw
			}(),
			want: true,
		},
		{
			name: "default ssh-keygen in both scopes",
			sysRaw: func() *format.Config {
				raw := format.New()
				raw.Section("gpg").Subsection("ssh").SetOption("program", "ssh-keygen")
				return raw
			}(),
			globalRaw: func() *format.Config {
				raw := format.New()
				raw.Section("gpg").Subsection("ssh").SetOption("program", "ssh-keygen")
				return raw
			}(),
			want: false,
		},
		{
			name:      "no custom program configured",
			sysRaw:    format.New(),
			globalRaw: format.New(),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sysCfg := config.NewConfig()
			sysCfg.GPG.Format = string(auto.FormatSSH)
			sysCfg.Raw = tt.sysRaw

			globalCfg := config.NewConfig()
			globalCfg.GPG.Format = string(auto.FormatSSH)
			globalCfg.Raw = tt.globalRaw

			got := hasCustomSSHSignProgram(sysCfg.Raw) || hasCustomSSHSignProgram(globalCfg.Raw)
			if got != tt.want {
				t.Fatalf("scoped raw detection = %v, want %v", got, tt.want)
			}

			merged := config.Merge(sysCfg, globalCfg)
			if got && hasCustomSSHSignProgram(merged.Raw) {
				t.Fatal("expected merged raw config not to preserve custom gpg.ssh.program")
			}
		})
	}
}

func TestLoadLocalConfig_ReadsCustomSSHSignProgram(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	repoCfg := config.NewConfig()
	repoCfg.GPG.Format = string(auto.FormatSSH)
	repoCfg.Raw.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("git.PlainOpen() error = %v", err)
	}

	if err := repo.SetConfig(repoCfg); err != nil {
		t.Fatalf("repo.SetConfig() error = %v", err)
	}

	localCfg := loadLocalConfig()
	if !hasCustomSSHSignProgram(localCfg.Raw) {
		t.Fatal("expected local config to report custom gpg.ssh.program")
	}
}

func TestObjectSignerConfigMerge_LocalConfigTakesPrecedence(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("git.PlainOpen() error = %v", err)
	}

	localCfg := config.NewConfig()
	localCfg.Commit.GpgSign = config.NewOptBool(true)
	localCfg.GPG.Format = string(auto.FormatSSH)
	localCfg.User.SigningKey = "local-signing-key"
	localCfg.Raw.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")
	if err := repo.SetConfig(localCfg); err != nil {
		t.Fatalf("repo.SetConfig() error = %v", err)
	}

	globalCfg := config.NewConfig()
	globalCfg.Commit.GpgSign = config.NewOptBool(false)
	globalCfg.GPG.Format = "openpgp"
	globalCfg.User.SigningKey = "global-signing-key"

	merged := config.Merge(config.NewConfig(), globalCfg, loadLocalConfig())

	if !merged.Commit.GpgSign.IsTrue() {
		t.Fatal("expected local commit.gpgsign to override global config")
	}
	if got := merged.GPG.Format; got != string(auto.FormatSSH) {
		t.Fatalf("merged GPG.Format = %q, want %q", got, auto.FormatSSH)
	}
	if got := merged.User.SigningKey; got != "local-signing-key" {
		t.Fatalf("merged User.SigningKey = %q, want %q", got, "local-signing-key")
	}
	if !hasCustomSSHSignProgram(loadLocalConfig().Raw) {
		t.Fatal("expected merged setup to see local custom gpg.ssh.program")
	}
}
