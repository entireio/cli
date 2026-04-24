package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/x/plugin/objectsigner/auto"
	"golang.org/x/crypto/ssh/agent"
)

var registerObjectSignerOnce sync.Once
var loadLocalConfigFunc = loadLocalConfig

func RegisterObjectSigner() {
	registerObjectSignerOnce.Do(func() {
		var localCfgOnce sync.Once
		var localCfg *config.Config

		//nolint:errcheck,gosec // best-effort; if registration fails, commits are left unsigned
		plugin.Register(plugin.ObjectSigner(), func() plugin.Signer {
			cfgSource, err := plugin.Get(plugin.ConfigLoader())
			if err != nil {
				// No config loader registered; signing not possible.
				return nil
			}

			sysCfg := loadScopedConfig(cfgSource, config.SystemScope)
			globalCfg := loadScopedConfig(cfgSource, config.GlobalScope)
			localCfgOnce.Do(func() {
				localCfg = loadLocalConfigFunc()
			})
			sshSignProgram := effectiveSSHSignProgram(sysCfg.Raw, globalCfg.Raw, localCfg.Raw)

			// Merge system, global, then local so local settings take precedence.
			merged := config.Merge(sysCfg, globalCfg, localCfg)

			if !merged.Commit.GpgSign.IsTrue() {
				return nil
			}

			// When a custom gpg.ssh.program is configured (e.g. 1Password's
			// op-ssh-sign), signing happens via an external binary that go-git
			// cannot invoke. Skip native signing silently — checkpoint commits
			// will be unsigned, which is acceptable since signing is best-effort.
			// The default program is "ssh-keygen", which works with go-git's
			// native SSH agent signing and does not need to be skipped.
			if auto.Format(merged.GPG.Format) == auto.FormatSSH && isCustomSSHSignProgram(sshSignProgram) {
				logging.Debug(context.Background(), "skipping native SSH commit signing: custom gpg.ssh.program is configured")
				return nil
			}

			cfg := auto.Config{
				SigningKey: merged.User.SigningKey,
				Format:     auto.Format(merged.GPG.Format),
				SSHAgent:   connectSSHAgent(),
			}

			signer, err := auto.FromConfig(cfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to create object signer: %v\n", err)
				return nil
			}

			return signer
		})
	})
}

// connectSSHAgent connects to the SSH agent via SSH_AUTH_SOCK.
// Returns nil if the agent is unavailable.
func connectSSHAgent() agent.Agent { //nolint:ireturn // must return the ssh agent interface
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}

	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "unix", sock)
	if err != nil {
		return nil
	}

	return agent.NewClient(conn)
}

var scopeName = map[config.Scope]string{
	config.GlobalScope: "global",
	config.SystemScope: "system",
}

// hasCustomSSHSignProgram checks whether gpg.ssh.program is set to a
// non-default value in the raw config. The git default is "ssh-keygen",
// which works with go-git's native SSH agent signing. Custom programs
// (e.g. 1Password's op-ssh-sign) use a separate signing mechanism that
// go-git cannot invoke.
// go-git's Config struct does not expose this field, so we read it directly.
func hasCustomSSHSignProgram(raw *format.Config) bool {
	if raw == nil {
		return false
	}

	return isCustomSSHSignProgram(raw.Section("gpg").Subsection("ssh").Option("program"))
}

func isCustomSSHSignProgram(program string) bool {
	return program != "" && program != "ssh-keygen"
}

func effectiveSSHSignProgram(raws ...*format.Config) string {
	for i := len(raws) - 1; i >= 0; i-- {
		raw := raws[i]
		if raw == nil {
			continue
		}

		program := raw.Section("gpg").Subsection("ssh").Option("program")
		if program != "" {
			return program
		}
	}

	return ""
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

func loadLocalConfig() *config.Config {
	repo, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return config.NewConfig()
	}

	cfg, err := repo.Storer.Config()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to parse local git config: %v\n", err)
		return config.NewConfig()
	}

	return cfg
}
