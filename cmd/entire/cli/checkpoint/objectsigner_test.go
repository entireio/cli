package checkpoint

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/config"
	programsigner "github.com/go-git/x/plugin/objectsigner/program"
)

func TestCustomSignProgram(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rawConfig  string
		format     programsigner.Format
		wantName   string
		wantCustom bool
	}{
		{
			name:       "nil raw config",
			format:     programsigner.FormatSSH,
			wantCustom: false,
		},
		{
			name:       "empty raw config",
			rawConfig:  "",
			format:     programsigner.FormatSSH,
			wantCustom: false,
		},
		{
			name: "custom ssh program",
			rawConfig: `[gpg "ssh"]
	program = op-ssh-sign
`,
			format:     programsigner.FormatSSH,
			wantName:   "op-ssh-sign",
			wantCustom: true,
		},
		{
			name: "default ssh program is not custom",
			rawConfig: `[gpg "ssh"]
	program = ssh-keygen
`,
			format:     programsigner.FormatSSH,
			wantCustom: false,
		},
		{
			name: "legacy openpgp program",
			rawConfig: `[gpg]
	program = gpg2
`,
			format:     programsigner.FormatOpenPGP,
			wantName:   "gpg2",
			wantCustom: true,
		},
		{
			name: "legacy openpgp program is ignored for ssh",
			rawConfig: `[gpg]
	program = gpg2
`,
			format:     programsigner.FormatSSH,
			wantCustom: false,
		},
		{
			name: "default openpgp program is not custom",
			rawConfig: `[gpg]
	program = gpg
`,
			format:     programsigner.FormatOpenPGP,
			wantCustom: false,
		},
		{
			name: "format program overrides legacy program",
			rawConfig: `[gpg]
	program = gpg2
[gpg "openpgp"]
	program = gpg-custom
`,
			format:     programsigner.FormatOpenPGP,
			wantName:   "gpg-custom",
			wantCustom: true,
		},
		{
			name: "x509 format program",
			rawConfig: `[gpg "x509"]
	program = gpgsm-custom
`,
			format:     programsigner.FormatX509,
			wantName:   "gpgsm-custom",
			wantCustom: true,
		},
		{
			name: "default x509 program is not custom",
			rawConfig: `[gpg "x509"]
	program = gpgsm
`,
			format:     programsigner.FormatX509,
			wantCustom: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg *config.Config
			if tt.rawConfig != "" {
				cfg = readSignerTestConfig(t, tt.rawConfig)
			}

			gotName, gotCustom := customSignProgram(tt.format, rawConfig(cfg))
			if gotCustom != tt.wantCustom {
				t.Fatalf("customSignProgram() custom = %v, want %v", gotCustom, tt.wantCustom)
			}
			if gotName != tt.wantName {
				t.Fatalf("customSignProgram() name = %q, want %q", gotName, tt.wantName)
			}
		})
	}
}

func TestCustomSignProgram_GlobalOverridesSystem(t *testing.T) {
	t.Parallel()

	systemCfg := readSignerTestConfig(t, `[gpg "ssh"]
	program = op-ssh-sign
`)
	globalCfg := readSignerTestConfig(t, `[gpg "ssh"]
	program = ssh-keygen
`)

	programName, ok := customSignProgram(programsigner.FormatSSH, systemCfg.Raw, globalCfg.Raw)
	if ok {
		t.Fatalf("customSignProgram() custom = true, want false with global default override; program %q", programName)
	}
}

func TestLoadObjectSignerFromConfigs_UsesCustomProgram(t *testing.T) {
	t.Parallel()

	globalCfg := readSignerTestConfig(t, `[user]
	signingKey = ABC123
[commit]
	gpgSign = true
[gpg]
	format = openpgp
	program = go
`)

	signer, ok := loadObjectSignerFromConfigs(context.Background(), config.NewConfig(), globalCfg)
	if !ok {
		t.Fatal("loadObjectSignerFromConfigs() ok = false, want true")
	}
	if signer == nil {
		t.Fatal("loadObjectSignerFromConfigs() returned nil signer")
	}
}

func readSignerTestConfig(t *testing.T, content string) *config.Config {
	t.Helper()

	cfg, err := config.ReadConfig(strings.NewReader(content))
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}

	return cfg
}
