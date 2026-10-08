package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// agentHomesFileName is the per-user registry of agent homes, in the user
// config directory.
const agentHomesFileName = "agent_homes.json"

// agentHomesVersion is the registry schema version.
const agentHomesVersion = 1

// maxHomesPerAgent bounds the registry; the least recently used homes are
// evicted first.
const maxHomesPerAgent = 32

// agentHomesFile is the on-disk form of the registry. Each agent's homes are
// listed most recently used first.
type agentHomesFile struct {
	Version int                          `json:"version"`
	Homes   map[types.AgentType][]string `json:"homes,omitempty"`
}

// ErrUntrustedAgentHome is returned, wrapped, by ResolveTrustedHome for a home
// that is neither the agent's active home nor recorded in the per-user
// registry.
var ErrUntrustedAgentHome = errors.New("agent home is not the active home or a recorded one")

// RememberAgentHome records home in the per-user registry as the most recently
// used home of agentType. home must be absolute and exist; its canonical form
// (see filepath.EvalSymlinks) is recorded, so aliases of one directory share
// an entry.
//
// When the registry changes, entries that are no longer directories are
// dropped (entries that cannot be checked are kept) and at most
// maxHomesPerAgent are kept, evicting the least recently used. When home is
// already the most recent entry, the registry is not rewritten. A registry
// that cannot be read or parsed, or has an unsupported version, is not
// overwritten, and its error is returned.
//
// Only a home resolved from the user's environment may be recorded, never one
// read from session state, because ResolveTrustedHome trusts recorded homes.
// The registry is not locked: of two concurrent calls one update may be lost,
// until a later call records it again.
func RememberAgentHome(agentType types.AgentType, home string) error {
	if err := requireAbsoluteHome(home); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(home))
	if err != nil {
		return fmt.Errorf("resolve agent home: %w", err)
	}

	root, err := userdirs.ConfigRoot()
	if err != nil {
		return fmt.Errorf("open config directory: %w", err)
	}
	file, err := readAgentHomesFile(root)
	if err != nil {
		return err
	}
	recorded := file.Homes[agentType]
	if len(recorded) > 0 && sameDir(recorded[0], canonical) {
		return nil
	}

	homes := make([]string, 0, len(recorded)+1)
	homes = append(homes, canonical)
	for _, entry := range recorded {
		if sameDir(entry, canonical) || homeIsGone(entry) {
			continue
		}
		homes = append(homes, entry)
	}
	if len(homes) > maxHomesPerAgent {
		homes = homes[:maxHomesPerAgent]
	}
	if file.Homes == nil {
		file.Homes = make(map[types.AgentType][]string)
	}
	file.Version = agentHomesVersion
	file.Homes[agentType] = homes

	data, err := jsonutil.MarshalIndentWithNewline(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agent homes: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, agentHomesFileName, data, 0o600); err != nil {
		return fmt.Errorf("write agent homes: %w", err)
	}
	return nil
}

// KnownAgentHomes returns the homes recorded for agentType that are still
// directories, most recently used first. It returns nil and no error when the
// config directory or the registry does not exist, and an error when the
// registry cannot be read or parsed or has an unsupported version. It never
// creates the config directory.
func KnownAgentHomes(agentType types.AgentType) ([]string, error) {
	recorded, err := recordedAgentHomes(agentType)
	if err != nil {
		return nil, err
	}
	var homes []string
	for _, home := range recorded {
		if homeExists(home) {
			homes = append(homes, home)
		}
	}
	return homes, nil
}

// ResolveTrustedHome checks that home is provider's active home or a home
// recorded for provider.Type() that is still a directory, comparing canonical
// forms, and returns filepath.Clean(home): the spelling it checked, which a
// caller must use in place of home, since resolving the raw spelling can name
// another directory when it contains ".." after a link. home must be absolute
// and must exist. An untrusted home yields an error wrapping
// ErrUntrustedAgentHome; a home that cannot be resolved, or a registry that
// cannot be read, yields a different error. The active home is trusted even
// when the registry is unusable.
//
// It keeps a home named by session state, which other repositories can supply
// (see entire session adopt), to the homes the user's own environment has
// resolved. It is not a defense against code running as the user, which can
// write the registry itself.
func ResolveTrustedHome(provider HomeLayoutProvider, home string) (string, error) {
	if err := requireAbsoluteHome(home); err != nil {
		return "", err
	}
	checked := filepath.Clean(home)
	canonical, err := filepath.EvalSymlinks(checked)
	if err != nil {
		return "", fmt.Errorf("resolve agent home: %w", err)
	}
	if active, err := provider.SessionHome(); err == nil {
		if activeCanonical, err := filepath.EvalSymlinks(filepath.Clean(active)); err == nil && sameDir(activeCanonical, canonical) {
			return checked, nil
		}
	}
	recorded, err := recordedAgentHomes(provider.Type())
	if err != nil {
		return "", err
	}
	for _, entry := range recorded {
		if sameDir(entry, canonical) && homeExists(entry) {
			return checked, nil
		}
	}
	return "", fmt.Errorf("%q: %w", home, ErrUntrustedAgentHome)
}

// recordedAgentHomes returns the registry's entries for agentType as recorded.
func recordedAgentHomes(agentType types.AgentType) ([]string, error) {
	root, err := userdirs.ConfigRootForRead()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open config directory: %w", err)
	}
	file, err := readAgentHomesFile(root)
	if err != nil {
		return nil, err
	}
	return file.Homes[agentType], nil
}

// homeExists reports whether the recorded home is still a directory.
func homeExists(home string) bool {
	info, err := os.Stat(home)
	return err == nil && info.IsDir()
}

// homeIsGone reports whether the recorded home is positively no longer a
// directory. A home that cannot be checked, for example for lack of
// permission, is kept.
func homeIsGone(home string) bool {
	info, err := os.Stat(home)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return !info.IsDir()
}

func requireAbsoluteHome(home string) error {
	if !filepath.IsAbs(home) {
		return fmt.Errorf("agent home %q is not absolute", home)
	}
	return nil
}

// readAgentHomesFile reads the registry. Only a missing registry reads as
// empty: an unreadable or unsupported one is an error, so it is never
// overwritten.
func readAgentHomesFile(root *os.Root) (agentHomesFile, error) {
	data, err := osroot.ReadFileNoFollow(root, agentHomesFileName)
	if errors.Is(err, os.ErrNotExist) {
		return agentHomesFile{}, nil
	}
	if err != nil {
		return agentHomesFile{}, fmt.Errorf("read agent homes: %w", err)
	}
	var file agentHomesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return agentHomesFile{}, fmt.Errorf("parse agent homes: %w", err)
	}
	if file.Version != agentHomesVersion {
		return agentHomesFile{}, fmt.Errorf("unsupported agent homes version %d", file.Version)
	}
	return file, nil
}
