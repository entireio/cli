package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/x/plugin/objectsigner/auto"
	"golang.org/x/crypto/ssh/agent"
)

var registerObjectSignerOnce sync.Once

func RegisterObjectSigner() {
	registerObjectSignerOnce.Do(func() {
		//nolint:errcheck,gosec // best-effort; if registration fails, commits are left unsigned
		plugin.Register(plugin.ObjectSigner(), func() plugin.Signer {
			cfgSource, err := plugin.Get(plugin.ConfigLoader())
			if err != nil {
				// No config loader registered; signing not possible.
				return nil
			}

			sysCfg := loadScopedConfig(cfgSource, config.SystemScope)
			globalCfg := loadScopedConfig(cfgSource, config.GlobalScope)

			// Merge system then global so that global settings take precedence.
			merged := config.Merge(sysCfg, globalCfg)

			if !merged.Commit.GpgSign.IsTrue() {
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
