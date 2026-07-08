package fsstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	cp "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

// backendType is the registry type name for the filesystem reference backend.
const backendType = "fs"

// config is the fsstore backend's settings "config" block.
type config struct {
	Path string `json:"path"`
}

var registerOnce sync.Once

// registerForTesting registers the fsstore backend so tests can select it as a
// checkpoint mirror. It lives in a _test.go file on purpose: the production
// fsstore package exposes no way to add itself to the registry, so a production
// binary can never resolve the "fs" backend. Registration is process-wide and
// idempotent (checkpoint.Register panics on duplicates).
func registerForTesting() {
	registerOnce.Do(func() {
		checkpoint.Register(backendType, factory)
	})
}

//nolint:ireturn // must return the contract interface to satisfy checkpoint.Factory
func factory(_ context.Context, _ checkpoint.OpenEnv, cfg json.RawMessage) (cp.PersistentStore, error) {
	var c config
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, fmt.Errorf("fsstore: invalid config: %w", err)
		}
	}
	if c.Path == "" {
		return nil, errors.New("fsstore: config.path is required")
	}
	return New(c.Path), nil
}
