package cli

import (
	"testing"

	"github.com/go-git/go-git/v6/config"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/x/plugin/objectsigner/auto"
)

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

	got := effectiveSSHSignProgram(sysRaw, globalRaw)
	if got != "/Applications/1Password.app/Contents/MacOS/op-ssh-sign" {
		t.Fatalf("effectiveSSHSignProgram() = %q, want %q", got, "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")
	}

	if !isCustomSSHSignProgram(got) {
		t.Fatal("expected highest-precedence global override to be treated as custom")
	}
}
