package external

import (
	"maps"
	"regexp"
	"sync"
)

var (
	callerEnvMu   sync.Mutex
	callerEnvVars = map[string]string{} // variable name -> agent type
)

// callerEnvVarPattern accepts plain environment variable names only.
var callerEnvVarPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)

func recordCallerEnvVars(names []string, agentType string) {
	callerEnvMu.Lock()
	defer callerEnvMu.Unlock()
	for _, name := range names {
		if callerEnvVarPattern.MatchString(name) {
			callerEnvVars[name] = agentType
		}
	}
}

// CallerEnvVars returns the caller variables declared by registered external
// agents, mapped to each agent's display type.
func CallerEnvVars() map[string]string {
	callerEnvMu.Lock()
	defer callerEnvMu.Unlock()
	return maps.Clone(callerEnvVars)
}
