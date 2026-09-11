package redact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	goredact "github.com/lastpersonlabs/goredact"
)

// ErrScannerDegraded is returned by JSONLBytes/JSONLBytesWithPrivacyFilter
// when the goredact engine failed at runtime and betterleaks is disabled:
// the sole scanner produced no coverage, so transcript writes must fail
// rather than persist under-scanned content. Callers distinguish it from
// JSON-parse errors with errors.Is and must NOT fall back to redact.Bytes.
var ErrScannerDegraded = errors.New("redaction scanner degraded: goredact scan failed with betterleaks disabled")

// ScannersConfig selects which scanner engines run in detectAllLayers.
type ScannersConfig struct {
	Betterleaks bool
	Goredact    bool
}

type scannerState struct {
	betterleaks bool
	engine      *goredact.Engine // non-nil iff goredact enabled
}

var (
	// scannerConfig is an atomic.Pointer because detectAllLayers reads it on
	// the hot per-leaf path (unlike the RWMutex-guarded configs in
	// custom.go/pii.go, whose configs are read once per call batch).
	scannerConfig   atomic.Pointer[scannerState]
	scannerDegraded atomic.Bool

	// goredactScanMu serializes goredact scans. OnFinding is fixed at engine
	// construction and findings carry no per-call correlation, so a shared
	// engine routes findings through this swapped collector.
	goredactScanMu      sync.Mutex
	goredactScanRegions *[]taggedRegion

	goredactErrLogged atomic.Bool
)

// defaultScanners is the unconfigured state: betterleaks-only, identical
// to the pre-feature pipeline.
var defaultScanners = &scannerState{betterleaks: true}

// ConfigureScanners installs the scanner selection and eagerly constructs
// the goredact engine when enabled. Call once at process startup after
// loading settings; an error means the caller must fail the operation —
// proceeding would run a scanner set the configuration did not choose.
// Thread-safe; replaces prior config and resets the degradation state.
func ConfigureScanners(cfg ScannersConfig) error {
	state := &scannerState{betterleaks: cfg.Betterleaks}
	if cfg.Goredact {
		engine, err := goredact.New(goredact.Config{
			Profile: goredact.ProfileBalanced,
			OnFinding: func(f goredact.Finding) {
				// goredactScanMu is held by detectGoredact for the duration
				// of the Redact call that invokes this.
				if goredactScanRegions == nil {
					// Callback outside a scan (a library regression to
					// async/post-return delivery): flag unknown coverage
					// instead of panicking inside the checkpoint write path.
					scannerDegraded.Store(true)
					return
				}
				*goredactScanRegions = append(*goredactScanRegions,
					taggedRegion{region: region{int(f.Start), int(f.End)}})
			},
		})
		if err != nil {
			return fmt.Errorf("construct goredact engine: %w", err)
		}
		state.engine = engine
	}
	scannerConfig.Store(state)
	scannerDegraded.Store(false)
	goredactErrLogged.Store(false)
	return nil
}

func getScanners() *scannerState {
	if s := scannerConfig.Load(); s != nil {
		return s
	}
	return defaultScanners
}

// scannerDegradedSole reports the fail-the-write condition: a goredact
// runtime failure occurred and betterleaks is not covering.
func scannerDegradedSole() bool {
	return scannerDegraded.Load() && !getScanners().betterleaks
}

// detectGoredact runs the goredact engine over s and returns its findings
// as tagged regions, filtered through the stack-wide placeholder policy.
// Returns nil when goredact is not enabled.
func detectGoredact(s string) []taggedRegion {
	st := getScanners()
	if st.engine == nil || s == "" {
		return nil
	}
	goredactScanMu.Lock()
	defer goredactScanMu.Unlock()
	var regions []taggedRegion
	goredactScanRegions = &regions
	defer func() { goredactScanRegions = nil }()
	// Background context + in-memory reader + io.Discard engineer out every
	// documented Redact error source; an error here is a library bug.
	if _, err := st.engine.Redact(context.Background(), io.Discard, strings.NewReader(s)); err != nil {
		if goredactErrLogged.CompareAndSwap(false, true) {
			slog.Warn("goredact scan failed; flagging degradation",
				componentAttr, slog.String("error", err.Error()))
		}
		scannerDegraded.Store(true)
		return nil
	}
	out := regions[:0]
	for _, r := range regions {
		// Out-of-bounds or inverted offsets are a library-contract violation;
		// flag unknown coverage instead of panicking. Deliberately keeps the
		// remaining regions (each is independently bounds-checked, so using
		// them only adds redaction): under a sole scanner the sentinel fails
		// the write regardless, and under both engines betterleaks carries
		// coverage — dropping valid regions here would only redact less.
		if r.start < 0 || r.end > len(s) || r.start > r.end {
			scannerDegraded.Store(true)
			continue
		}
		// Defensive: goredact's validators already reject placeholder shapes;
		// kept so the stack-wide policy holds even if a future rule set stops
		// doing so.
		if isPlaceholderSecretValue(s[r.start:r.end]) {
			continue
		}
		out = append(out, r)
	}
	return out
}
