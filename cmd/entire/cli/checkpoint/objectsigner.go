package checkpoint

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/go-git/go-git/v6/config"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/x/plugin/objectsigner/auto"
	programsigner "github.com/go-git/x/plugin/objectsigner/program"
	sshagent "golang.org/x/crypto/ssh/agent"
)

var (
	objectSignerLoader = loadObjectSigner
	scopeName          = map[config.Scope]string{
		config.GlobalScope: "global",
		config.SystemScope: "system",
	}
)

func loadObjectSigner(ctx context.Context) (plugin.Signer, bool) {
	cfgSource, err := plugin.Get(plugin.ConfigLoader())
	if err != nil {
		// No config loader registered; signing not possible.
		return nil, false
	}

	sysCfg := loadScopedConfig(cfgSource, config.SystemScope)
	globalCfg := loadScopedConfig(cfgSource, config.GlobalScope)

	return loadObjectSignerFromConfigs(ctx, sysCfg, globalCfg)
}

func loadObjectSignerFromConfigs(ctx context.Context, sysCfg, globalCfg *config.Config) (plugin.Signer, bool) {
	// Merge system then global so that global settings take precedence.
	merged := config.Merge(sysCfg, globalCfg)

	if !merged.Commit.GpgSign.IsTrue() {
		return nil, false
	}

	if signer, ok := loadCustomProgramSigner(ctx, sysCfg, globalCfg, merged); ok {
		return signer, true
	}

	signer, err := auto.FromConfig(auto.Config{
		SigningKey: merged.User.SigningKey,
		Format:     auto.Format(merged.GPG.Format),
		SSHAgent:   connectSSHAgent(ctx),
	})
	if err != nil {
		logging.Debug(ctx, "failed to create object signer", slog.String("error", err.Error()))
		return nil, false
	}

	return signer, true
}

func loadCustomProgramSigner(ctx context.Context, sysCfg, globalCfg *config.Config, merged config.Config) (plugin.Signer, bool) {
	signFormat := normalizeProgramFormat(merged.GPG.Format)

	// TODO: Replace with merged.GPG.Program once that is surfaced by go-git.
	programName, ok := customSignProgram(signFormat, rawConfig(sysCfg), rawConfig(globalCfg))
	if !ok {
		return nil, false
	}

	signer, err := programsigner.New(signFormat, programName, merged.User.SigningKey)
	if err != nil {
		logging.Debug(ctx, "failed to create object signer from custom program", slog.String("error", err.Error()))
		return nil, false
	}

	logging.Debug(
		ctx,
		"using custom object signer program",
		slog.String("format", string(signFormat)),
		slog.String("program", programName),
	)

	return signer, true
}

func rawConfig(cfg *config.Config) *format.Config {
	if cfg == nil {
		return nil
	}

	return cfg.Raw
}

func normalizeProgramFormat(gitFormat string) programsigner.Format {
	switch auto.Format(gitFormat) {
	case "", auto.FormatOpenPGP:
		return programsigner.FormatOpenPGP
	case auto.FormatSSH:
		return programsigner.FormatSSH
	case auto.Format("x509"):
		return programsigner.FormatX509
	default:
		return programsigner.Format(gitFormat)
	}
}

// customSignProgram returns the effective custom signer program for signFormat.
// Git supports both legacy OpenPGP gpg.program and format-specific
// gpg.<format>.program settings; format-specific values override gpg.program
// within the same scope, and later scopes override earlier scopes.
func customSignProgram(signFormat programsigner.Format, raws ...*format.Config) (string, bool) {
	var programName string
	for _, raw := range raws {
		if raw == nil {
			continue
		}

		if scopedProgram := signProgramFromRaw(signFormat, raw); scopedProgram != "" {
			programName = scopedProgram
		}
	}

	if programName == "" || programName == defaultSignProgram(signFormat) {
		return "", false
	}

	return programName, true
}

func signProgramFromRaw(signFormat programsigner.Format, raw *format.Config) string {
	if raw == nil {
		return ""
	}

	gpgSection := raw.Section("gpg")
	var programName string
	if signFormat == programsigner.FormatOpenPGP {
		programName = gpgSection.Option("program")
	}
	if formatProgram := gpgSection.Subsection(string(signFormat)).Option("program"); formatProgram != "" {
		programName = formatProgram
	}

	return programName
}

func defaultSignProgram(signFormat programsigner.Format) string {
	switch signFormat {
	case programsigner.FormatOpenPGP:
		return "gpg"
	case programsigner.FormatSSH:
		return "ssh-keygen"
	case programsigner.FormatX509:
		return "gpgsm"
	default:
		return ""
	}
}

// connectSSHAgent connects to the SSH agent via SSH_AUTH_SOCK.
// Returns nil if the agent is unavailable.
func connectSSHAgent(ctx context.Context) sshagent.Agent {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil
	}

	return sshagent.NewClient(conn)
}

func loadScopedConfig(source plugin.ConfigSource, scope config.Scope) *config.Config {
	name := scopeName[scope]

	storer, err := source.Load(scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to load %s git config: %v\n", name, err)
		return config.NewConfig()
	}

	cfg, err := storer.Config()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to parse %s git config: %v\n", name, err)
		return config.NewConfig()
	}

	return cfg
}
