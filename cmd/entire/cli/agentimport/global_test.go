package agentimport

import (
	"fmt"
	"os"
	"testing"

	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/go-git/go-git/v6/x/plugin/config"
)

func TestMain(m *testing.M) {
	// Replace go-git's default Auto ConfigLoader, which reads the developer's
	// ~/.gitconfig, with empty global and system configs. Without this every
	// go-git write in this package inherits whatever the host has set: a
	// developer with commit.gpgSign / tag.gpgSign fails each of them with
	// "cannot auto-sign … or register an ObjectSigner plugin", because no
	// signer plugin is registered here. Mirrors the checkpoint, strategy and
	// cli TestMains.
	if err := plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource {
		return config.NewEmpty()
	}); err != nil {
		panic(fmt.Errorf("failed to register config storers: %w", err))
	}

	os.Exit(m.Run())
}
