package strategy

import (
	"strconv"
	"sync"
	"testing"
)

// stubStrategy is a minimal Strategy implementation for registry tests.
// All methods are no-ops; we only need a concrete type that satisfies the interface
// so that Get() can instantiate it via the factory.
type stubStrategy struct {
	name string
}

func (s *stubStrategy) Name() string                                     { return s.name }
func (s *stubStrategy) Description() string                              { return "stub: " + s.name }
func (s *stubStrategy) ValidateRepository() error                        { return nil }
func (s *stubStrategy) SaveChanges(_ SaveContext) error                  { return nil }
func (s *stubStrategy) SaveTaskCheckpoint(_ TaskCheckpointContext) error { return nil }
func (s *stubStrategy) GetRewindPoints(_ int) ([]RewindPoint, error)     { return nil, nil }
func (s *stubStrategy) Rewind(_ RewindPoint) error                       { return nil }
func (s *stubStrategy) CanRewind() (bool, string, error)                 { return true, "", nil }
func (s *stubStrategy) PreviewRewind(_ RewindPoint) (*RewindPreview, error) {
	return nil, ErrNotImplemented
}
func (s *stubStrategy) GetTaskCheckpoint(_ RewindPoint) (*TaskCheckpoint, error) {
	return nil, ErrNotImplemented
}
func (s *stubStrategy) GetTaskCheckpointTranscript(_ RewindPoint) ([]byte, error) { return nil, nil }
func (s *stubStrategy) GetSessionInfo() (*SessionInfo, error) {
	return nil, ErrNoSession
}
func (s *stubStrategy) EnsureSetup() error                            { return nil }
func (s *stubStrategy) GetMetadataRef(_ Checkpoint) string            { return "" }
func (s *stubStrategy) GetSessionMetadataRef(_ string) string         { return "" }
func (s *stubStrategy) GetSessionContext(_ string) string             { return "" }
func (s *stubStrategy) GetCheckpointLog(_ Checkpoint) ([]byte, error) { return nil, nil }

// newStubFactory returns a Factory that produces a stubStrategy with the given name.
func newStubFactory(name string) Factory {
	return func() Strategy {
		return &stubStrategy{name: name}
	}
}

// cleanupRegistryEntry removes a test-registered strategy on test cleanup.
func cleanupRegistryEntry(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, name)
		registryMu.Unlock()
	})
}

func TestRegister_AddsStrategy(t *testing.T) {
	t.Parallel()

	const name = "test-register-adds"
	Register(name, newStubFactory(name))
	cleanupRegistryEntry(t, name)

	s, err := Get(name)
	if err != nil {
		t.Fatalf("Get(%q) returned unexpected error: %v", name, err)
	}
	if s.Name() != name {
		t.Errorf("expected strategy name %q, got %q", name, s.Name())
	}
}

func TestGet_ReturnsErrorForUnknown(t *testing.T) {
	t.Parallel()

	_, err := Get("no-such-strategy-exists")
	if err == nil {
		t.Fatal("Get() for unregistered name should return an error, got nil")
	}
}

func TestGet_ReturnsRegisteredStrategy(t *testing.T) {
	t.Parallel()

	// The init() functions register manual-commit and auto-commit.
	// Verify that we can retrieve both and they return the correct names.
	tests := []struct {
		name string
	}{
		{name: StrategyNameManualCommit},
		{name: StrategyNameAutoCommit},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, err := Get(tc.name)
			if err != nil {
				t.Fatalf("Get(%q) returned unexpected error: %v", tc.name, err)
			}
			if s == nil {
				t.Fatalf("Get(%q) returned nil strategy", tc.name)
			}
			if s.Name() != tc.name {
				t.Errorf("expected Name() = %q, got %q", tc.name, s.Name())
			}
		})
	}
}

func TestList_ReturnsAllRegistered(t *testing.T) {
	t.Parallel()

	// Register two additional test strategies.
	extras := []string{"test-list-alpha", "test-list-beta"}
	for _, name := range extras {
		Register(name, newStubFactory(name))
		cleanupRegistryEntry(t, name)
	}

	names := List()

	// Build a lookup set from the returned names.
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}

	// The list must contain both the init()-registered strategies and our extras.
	expected := append([]string{StrategyNameManualCommit, StrategyNameAutoCommit}, extras...)
	for _, want := range expected {
		if !nameSet[want] {
			t.Errorf("List() missing expected strategy %q; got %v", want, names)
		}
	}

	// Verify sorted order.
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("List() not sorted: %q >= %q at index %d", names[i-1], names[i], i)
		}
	}
}

func TestDefault_ReturnsDefaultStrategy(t *testing.T) {
	t.Parallel()

	s := Default()
	if s == nil {
		t.Fatal("Default() returned nil")
	}
	if s.Name() != DefaultStrategyName {
		t.Errorf("Default() returned strategy %q, expected %q", s.Name(), DefaultStrategyName)
	}
}

func TestRegister_OverwritesExisting(t *testing.T) {
	t.Parallel()

	const name = "test-overwrite"

	// Register with the first factory.
	Register(name, newStubFactory("first"))
	// Overwrite with a second factory.
	Register(name, newStubFactory("second"))
	cleanupRegistryEntry(t, name)

	s, err := Get(name)
	if err != nil {
		t.Fatalf("Get(%q) returned unexpected error: %v", name, err)
	}
	if s.Name() != "second" {
		t.Errorf("expected overwritten strategy Name() = %q, got %q", "second", s.Name())
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	const iterations = 100

	// Register a strategy that concurrent goroutines will read.
	const name = "test-concurrent"
	Register(name, newStubFactory(name))
	cleanupRegistryEntry(t, name)

	var wg sync.WaitGroup

	// Spawn goroutines that call Get concurrently.
	for range iterations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Get(name)
			if err != nil {
				t.Errorf("concurrent Get(%q) error: %v", name, err)
				return
			}
			if s.Name() != name {
				t.Errorf("concurrent Get(%q) returned Name() = %q", name, s.Name())
			}
		}()
	}

	// Spawn goroutines that call List concurrently.
	for range iterations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names := List()
			if len(names) == 0 {
				t.Error("concurrent List() returned empty slice")
			}
		}()
	}

	// Spawn goroutines that call Register concurrently with unique keys.
	for i := range iterations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uniqueName := "test-concurrent-reg-" + strconv.Itoa(i)
			Register(uniqueName, newStubFactory(uniqueName))
		}()
	}

	wg.Wait()

	// Clean up the concurrent registration entries.
	registryMu.Lock()
	for i := range iterations {
		delete(registry, "test-concurrent-reg-"+strconv.Itoa(i))
	}
	registryMu.Unlock()
}
