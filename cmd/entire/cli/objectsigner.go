package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/go-git/go-git/v6/config"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/x/plugin/objectsigner/auto"
	"golang.org/x/crypto/ssh/agent"
)

var registerObjectSignerOnce sync.Once
var objectSignerLoader = loadObjectSigner

func RegisterObjectSigner() {
	registerObjectSignerOnce.Do(func() {
		signer, ok := objectSignerLoader()
		if !ok {
			return
		}

		//nolint:errcheck,gosec // best-effort; if registration fails, commits are left unsigned
		plugin.Register(plugin.ObjectSigner(), func() plugin.Signer {
			return signer
		})
	})
}

func loadObjectSigner() (plugin.Signer, bool) {
	cfgSource, err := plugin.Get(plugin.ConfigLoader())
	if err != nil {
		// No config loader registered; signing not possible.
		return nil, false
	}

	sysCfg := loadScopedConfig(cfgSource, config.SystemScope)
	globalCfg := loadScopedConfig(cfgSource, config.GlobalScope)

	// Merge system then global so that global settings take precedence.
	merged := config.Merge(sysCfg, globalCfg)

	if !merged.Commit.GpgSign.IsTrue() {
		return nil, false
	}

	// When a custom gpg.ssh.program is configured (e.g. 1Password's
	// op-ssh-sign), signing happens via an external binary that go-git
	// cannot invoke. Skip native signing silently — checkpoint commits
	// will be unsigned, which is acceptable since signing is best-effort.
	// The default program is "ssh-keygen", which works with go-git's
	// native SSH agent signing and does not need to be skipped.
	if auto.Format(merged.GPG.Format) == auto.FormatSSH && hasCustomSSHSignProgram(merged.Raw) {
		logging.Debug(context.Background(), "skipping native SSH commit signing: custom gpg.ssh.program is configured")
		return nil, false
	}

	signer, err := auto.FromConfig(auto.Config{
		SigningKey: merged.User.SigningKey,
		Format:     auto.Format(merged.GPG.Format),
		SSHAgent:   connectSSHAgent(),
	})
	if err != nil {
		logging.Debug(context.Background(), "failed to create object signer", "error", err.Error())
		return nil, false
	}

	return signer, true
}

// connectSSHAgent connects to the SSH agent via SSH_AUTH_SOCK.
// Returns nil if the agent is unavailable.
func connectSSHAgent() agent.Agent {
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

	program := raw.Section("gpg").Subsection("ssh").Option("program")

	return program != "" && program != "ssh-keygen"
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
