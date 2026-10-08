package agent

import (
	"fmt"
	"path"
	"path/filepath"
)

// HomeLayout describes where an agent keeps session transcripts beneath its
// per-user home directory.
type HomeLayout struct {
	// Stores lists the session stores relative to the home, as
	// slash-separated paths, with the store GetSessionDir resolves into
	// first. A store may hold per-project subdirectories.
	Stores []string
}

// StoresUnder returns the absolute session stores beneath home, in the order
// of l.Stores.
func (l HomeLayout) StoresUnder(home string) []string {
	stores := make([]string, len(l.Stores))
	for i, store := range l.Stores {
		stores[i] = filepath.Join(home, filepath.FromSlash(store))
	}
	return stores
}

// Holds reports whether path is one of the stores beneath home or lies in one.
// The comparison is lexical and folds case on Windows.
func (l HomeLayout) Holds(home, path string) bool {
	for _, store := range l.StoresUnder(home) {
		if pathHasDirPrefix(path, store) {
			return true
		}
	}
	return false
}

// RepoHomeLayout returns provider's home layout narrowed to the sessions of
// repoPath. The session directory GetSessionDir resolves for repoPath must lie
// in one of the active home's stores; its path within that store is appended
// to every store. For an agent with a directory per project, such as Claude
// Code, the result names that project's directory in each store; for one
// without, such as Codex, it is the full layout. It reports false when the
// session directory lies outside every store of the active home, as for a Pi
// store relocated with PI_CODING_AGENT_SESSION_DIR or under a test override of
// the session directory.
func RepoHomeLayout(provider HomeLayoutProvider, repoPath string) (HomeLayout, bool, error) {
	active, err := provider.SessionHome()
	if err != nil {
		return HomeLayout{}, false, fmt.Errorf("resolve %s home: %w", provider.Type(), err)
	}
	dir, err := provider.GetSessionDir(repoPath)
	if err != nil {
		return HomeLayout{}, false, fmt.Errorf("resolve %s session directory: %w", provider.Type(), err)
	}
	dir = filepath.Clean(dir)
	layout := provider.HomeLayout()
	for _, store := range layout.StoresUnder(filepath.Clean(active)) {
		if !pathHasDirPrefix(dir, store) {
			continue
		}
		rel, err := filepath.Rel(store, dir)
		if err != nil || !filepath.IsLocal(rel) {
			return HomeLayout{}, false, nil //nolint:nilerr // a directory Rel cannot place within the store lies outside it
		}
		narrowed := HomeLayout{Stores: make([]string, len(layout.Stores))}
		for i, s := range layout.Stores {
			narrowed.Stores[i] = path.Join(s, filepath.ToSlash(rel))
		}
		return narrowed, true, nil
	}
	return HomeLayout{}, false, nil
}

// HomeLayoutProvider is implemented by built-in agents that keep session
// transcripts in one or more stores beneath a per-user home directory, such as
// Codex's live and archived session stores beneath CODEX_HOME.
type HomeLayoutProvider interface {
	Agent

	// SessionHome returns the agent's active home directory, resolved from
	// the same settings GetSessionDir uses but ignoring Entire's test
	// overrides of the session directory.
	SessionHome() (string, error)

	// HomeLayout returns where the agent keeps sessions beneath its home.
	HomeLayout() HomeLayout
}

// AsHomeLayoutProvider returns the agent as a HomeLayoutProvider if it is a
// built-in agent that implements the interface.
func AsHomeLayoutProvider(ag Agent) (HomeLayoutProvider, bool) {
	return builtinCapability[HomeLayoutProvider](ag)
}
