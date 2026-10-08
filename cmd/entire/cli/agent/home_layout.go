package agent

import "path/filepath"

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

// StoreContaining returns the store beneath home that contains path, and
// reports whether there is one. The comparison is lexical and folds case on
// Windows.
func (l HomeLayout) StoreContaining(home, path string) (string, bool) {
	for _, store := range l.StoresUnder(home) {
		if pathHasDirPrefix(path, store) {
			return store, true
		}
	}
	return "", false
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
