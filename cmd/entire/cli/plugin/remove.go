package plugin

import (
	"errors"
	"fmt"
	"os"
)

// Remove deletes the plugin with the given bare name. Local plugins (symlinks)
// remove only the symlink, not the source directory. Returns an error if the
// plugin is not installed.
func (m *Manager) Remove(name string) error {
	p, err := m.Find(name)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("plugin %q is not installed", name)
	}

	// Local plugins are symlinks — os.Remove unlinks without touching the
	// target. RemoveAll on a symlink would also just unlink it on Unix, but
	// os.Remove is the right tool for clarity.
	if p.Kind == KindLocal {
		if err := os.Remove(p.Dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove local plugin: %w", err)
		}
		return nil
	}

	if err := os.RemoveAll(p.Dir); err != nil {
		return fmt.Errorf("remove plugin: %w", err)
	}
	return nil
}
