package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// agentHomesFileName stores homes independently resolved at session start.
// Repository metadata must never populate this per-user registry.
const agentHomesFileName = "agent_homes.json"

// agentHomesVersion is the registry schema version. Unknown versions are ignored.
const agentHomesVersion = 1

// maxHomesPerAgent bounds cold searches; least recently used homes are evicted first.
const maxHomesPerAgent = 32

// agentHomesFile is the on-disk shape of agentHomesFileName. Homes are
// recorded least-recently-used first per agent type.
type agentHomesFile struct {
	Version int                          `json:"version"`
	Homes   map[types.AgentType][]string `json:"homes,omitempty"`
}

// RememberAgentHome records an independently resolved absolute home. Repeated
// entries move to the end. Concurrent writers may lose an update; later session
// starts retry it. Only session/turn start may record independently resolved homes.
func RememberAgentHome(agentType types.AgentType, home string) error {
	if !filepath.IsAbs(home) {
		return fmt.Errorf("agent home must be absolute, got %q", home)
	}
	clean := filepath.Clean(home)
	// Snapshot the directory observed now. A developer may later retarget a
	// home alias; that must not erase the independently observed old home.
	if canonical, err := filepath.EvalSymlinks(clean); err == nil {
		clean = canonical
	} else if !errors.Is(err, os.ErrNotExist) {
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
	homes := file.Homes[agentType]
	updated := make([]string, 0, len(homes)+1)
	for _, candidate := range homes {
		if candidate == clean {
			continue
		}
		info, statErr := os.Stat(candidate)
		if (statErr == nil && info.IsDir()) || (statErr != nil && !errors.Is(statErr, os.ErrNotExist)) {
			updated = append(updated, candidate)
		}
	}
	updated = append(updated, clean)
	if slices.Equal(homes, updated) {
		return nil
	}
	homes = updated
	if len(homes) > maxHomesPerAgent {
		// Keep the most recently used maxHomesPerAgent entries.
		homes = slices.Clone(homes[len(homes)-maxHomesPerAgent:])
	}
	if file.Homes == nil {
		file.Homes = make(map[types.AgentType][]string)
	}
	file.Homes[agentType] = homes
	file.Version = agentHomesVersion

	data, err := jsonutil.MarshalIndentWithNewline(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agent homes: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, agentHomesFileName, data, 0o600); err != nil {
		return fmt.Errorf("write agent homes: %w", err)
	}
	return nil
}

// KnownAgentHomes returns existing recorded homes, least-recently-used first. Missing,
// malformed, or unsupported registry files return nil. Probes retain no roots.
func KnownAgentHomes(agentType types.AgentType) []string {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		// No config directory yet (ConfigRootForRead must not create one) —
		// nothing has ever been recorded.
		return nil
	}
	file, err := readAgentHomesFile(root)
	if err != nil {
		return nil
	}
	homes := file.Homes[agentType]
	if len(homes) == 0 {
		return nil
	}

	existing := make([]string, 0, len(homes))
	for _, home := range homes {
		info, statErr := os.Stat(home)
		if statErr != nil || !info.IsDir() {
			continue
		}
		existing = append(existing, home)
	}
	return existing
}

// ResolveTrustedSessionHome returns home's canonical spelling if it matches
// the agent's current home or its per-user registry. Repository metadata cannot
// grant trust. Missing, relative, and unrecognized homes are rejected.
func ResolveTrustedSessionHome(provider AgentHomeProvider, home string) (string, error) {
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("agent home must be absolute: %s", home)
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("resolve agent home: %w", err)
	}
	candidates := KnownAgentHomes(provider.Type())
	if active, activeErr := provider.SessionHome(); activeErr == nil {
		candidates = append(candidates, active)
	}
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) {
			continue
		}
		canonical, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr != nil {
			continue
		}
		if canonical == resolved || (runtime.GOOS == hookWrapperOSWindows && strings.EqualFold(canonical, resolved)) {
			return canonical, nil
		}
	}
	return "", fmt.Errorf("unrecognized agent home: %s", home)
}

// ResolveTrustedTranscript authorizes a home and returns canonical home/path
// coordinates. Links below the home are refused, including dangling links;
// missing transcripts are allowed and remain confined when read later.
func ResolveTrustedTranscript(provider AgentHomeProvider, home, path string) (transcriptPath, agentHome string, err error) {
	resolver, err := NewTrustedTranscriptResolver(provider, home)
	if err != nil {
		return "", "", err
	}
	return resolver.Resolve(path)
}

// TrustedTranscriptResolver reuses home provenance for one operation while
// checking each transcript's layout and symlinks independently. Do not cache it
// across operations: the per-user registry and active home can change.
type TrustedTranscriptResolver struct {
	provider      AgentHomeProvider
	home          string
	canonicalHome string
}

// NewTrustedTranscriptResolver authorizes a home against independently resolved
// homes. Repository metadata alone cannot construct a trusted resolver.
func NewTrustedTranscriptResolver(provider AgentHomeProvider, home string) (*TrustedTranscriptResolver, error) {
	home = filepath.Clean(home)
	canonicalHome, err := ResolveTrustedSessionHome(provider, home)
	if err != nil {
		return nil, err
	}
	return &TrustedTranscriptResolver{provider: provider, home: home, canonicalHome: canonicalHome}, nil
}

// Resolve returns canonical home/path coordinates after checking this path.
// Missing transcripts are accepted; links below the home are always refused.
func (r *TrustedTranscriptResolver) Resolve(path string) (transcriptPath, agentHome string, err error) {
	home, canonicalHome := r.home, r.canonicalHome
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve transcript path: %w", err)
	}
	rel, ok := TranscriptNameUnderHome(absPath, home)
	if !ok {
		rel, ok = TranscriptNameUnderHome(absPath, canonicalHome)
	}
	canonicalPath := filepath.Join(canonicalHome, rel)
	if !ok || !HomeConfinesTranscript(r.provider, canonicalHome, canonicalPath) {
		return "", "", fmt.Errorf("transcript %s is not confined to the %s session layout", path, r.provider.Name())
	}
	return canonicalPath, canonicalHome, nil
}

// HomeConfinesTranscript reports whether home may be recorded for
// transcriptPath: the agent's layout places the transcript beneath home and a
// confined path can reach it. It checks layout and links, not provenance or
// permission to open an existing transcript.
func HomeConfinesTranscript(provider AgentHomeProvider, home, transcriptPath string) bool {
	return home != "" && provider.SessionPathUnder(home, transcriptPath) &&
		TranscriptReadableUnderHome(transcriptPath, home)
}

// readAgentHomesFile treats only a missing registry as empty. Invalid or
// unreadable registries must not be overwritten by the next session start.
func readAgentHomesFile(root *os.Root) (agentHomesFile, error) {
	data, err := osroot.ReadFileNoFollow(root, agentHomesFileName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return agentHomesFile{}, nil
		}
		return agentHomesFile{}, fmt.Errorf("read agent homes: %w", err)
	}
	var file agentHomesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return agentHomesFile{}, fmt.Errorf("parse agent homes: %w", err)
	}
	if file.Version != agentHomesVersion {
		return agentHomesFile{}, fmt.Errorf("unsupported agent homes version: %d", file.Version)
	}
	return file, nil
}
