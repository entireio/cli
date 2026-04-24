package cli

import (
	"errors"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/x/plugin/objectsigner/auto"
)

const localSigningKey = "local-signing-key"

func TestIsCustomSSHSignProgram(t *testing.T) {
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

			got := isCustomSSHSignProgram(rawSSHSignProgram(tt.raw))
			if got != tt.want {
				t.Errorf("isCustomSSHSignProgram(rawSSHSignProgram()) = %v, want %v", got, tt.want)
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

	if !isCustomSSHSignProgram(rawSSHSignProgram(sysCfg.Raw)) {
		t.Fatal("expected scoped raw config to report custom gpg.ssh.program")
	}

	if isCustomSSHSignProgram(rawSSHSignProgram(merged.Raw)) {
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

			got := isCustomSSHSignProgram(rawSSHSignProgram(sysCfg.Raw)) || isCustomSSHSignProgram(rawSSHSignProgram(globalCfg.Raw))
			if got != tt.want {
				t.Fatalf("scoped raw detection = %v, want %v", got, tt.want)
			}

			merged := config.Merge(sysCfg, globalCfg)
			if got && isCustomSSHSignProgram(rawSSHSignProgram(merged.Raw)) {
				t.Fatal("expected merged raw config not to preserve custom gpg.ssh.program")
			}
		})
	}
}

func TestEffectiveSSHSignProgram_UsesHighestPrecedenceScope(t *testing.T) {
	t.Parallel()

	sysRaw := format.New()
	sysRaw.Section("gpg").Subsection("ssh").SetOption("program", "/usr/local/bin/custom-system-signer")

	globalRaw := format.New()
	globalRaw.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")

	localRaw := format.New()
	localRaw.Section("gpg").Subsection("ssh").SetOption("program", "ssh-keygen")

	got := effectiveSSHSignProgram(sysRaw, globalRaw, localRaw)
	if got != "ssh-keygen" {
		t.Fatalf("effectiveSSHSignProgram() = %q, want %q", got, "ssh-keygen")
	}

	if isCustomSSHSignProgram(got) {
		t.Fatal("expected highest-precedence ssh-keygen override not to be treated as custom")
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

	localCfg := loadRepoLocalConfig(repo)
	if !isCustomSSHSignProgram(rawSSHSignProgram(localCfg.Raw)) {
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
	localCfg.User.SigningKey = localSigningKey
	localCfg.Raw.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")
	if err := repo.SetConfig(localCfg); err != nil {
		t.Fatalf("repo.SetConfig() error = %v", err)
	}

	globalCfg := config.NewConfig()
	globalCfg.Commit.GpgSign = config.NewOptBool(false)
	globalCfg.GPG.Format = "openpgp"
	globalCfg.User.SigningKey = "global-signing-key"

	merged := config.Merge(config.NewConfig(), globalCfg, loadRepoLocalConfig(repo))

	if !merged.Commit.GpgSign.IsTrue() {
		t.Fatal("expected local commit.gpgsign to override global config")
	}
	if got := merged.GPG.Format; got != string(auto.FormatSSH) {
		t.Fatalf("merged GPG.Format = %q, want %q", got, auto.FormatSSH)
	}
	if got := merged.User.SigningKey; got != localSigningKey {
		t.Fatalf("merged User.SigningKey = %q, want %q", got, localSigningKey)
	}
	if !isCustomSSHSignProgram(rawSSHSignProgram(loadRepoLocalConfig(repo).Raw)) {
		t.Fatal("expected merged setup to see local custom gpg.ssh.program")
	}
}

func TestLoadObjectSignerConfig_IgnoresUpperScopeLoadFailures(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("git.PlainOpen() error = %v", err)
	}

	localCfg := config.NewConfig()
	localCfg.Commit.GpgSign = config.NewOptBool(true)
	localCfg.GPG.Format = string(auto.FormatSSH)
	localCfg.User.SigningKey = localSigningKey
	localCfg.Raw.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")
	if err := repo.SetConfig(localCfg); err != nil {
		t.Fatalf("repo.SetConfig() error = %v", err)
	}

	merged, sshSignProgram := loadObjectSignerConfig(failingConfigSource{}, repo)

	if !merged.Commit.GpgSign.IsTrue() {
		t.Fatal("expected local commit.gpgsign to survive upper-scope load failures")
	}
	if got := merged.GPG.Format; got != string(auto.FormatSSH) {
		t.Fatalf("merged GPG.Format = %q, want %q", got, auto.FormatSSH)
	}
	if got := merged.User.SigningKey; got != localSigningKey {
		t.Fatalf("merged User.SigningKey = %q, want %q", got, localSigningKey)
	}
	if !isCustomSSHSignProgram(sshSignProgram) {
		t.Fatal("expected local custom gpg.ssh.program to survive upper-scope load failures")
	}
}

func TestRegisterObjectSigner_ReturnsNilSignerForLocalCustomSSHProgram(t *testing.T) { //nolint:paralleltest // t.Chdir and plugin registry are process-global
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
	localCfg.User.SigningKey = localSigningKey
	localCfg.Raw.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")
	if err := repo.SetConfig(localCfg); err != nil {
		t.Fatalf("repo.SetConfig() error = %v", err)
	}

	resetPluginEntry("object-signer")
	t.Cleanup(func() {
		resetPluginEntry("object-signer")
		registerObjectSignerOnce = sync.Once{}
	})

	registerObjectSignerOnce = sync.Once{}
	RegisterObjectSigner()

	signer, err := plugin.Get(plugin.ObjectSigner())
	if err != nil {
		t.Fatalf("plugin.Get(object signer) error = %v", err)
	}
	if signer != nil {
		t.Fatal("expected nil signer when local gpg.ssh.program uses a custom signer")
	}
}

type failingConfigSource struct{}

func (failingConfigSource) Load(_ config.Scope) (config.ConfigStorer, error) { //nolint:ireturn // required by plugin.ConfigSource
	return nil, errors.New("boom")
}
