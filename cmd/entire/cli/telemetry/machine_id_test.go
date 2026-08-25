package telemetry

import (
	"errors"
	"sync"
	"testing"
)

// testMachineID is the value the stub resolver returns.
const testMachineID = "machine-abc"

// withMachineIDResolver installs a resolver and clears the memoized value,
// restoring both afterwards.
//
// Swapping package state is only safe because a caller cannot be parallel: Go
// resumes parallel top-level tests after every sequential one has finished, so
// the parallel payload-builder tests in this package never overlap this window.
// The t.Setenv call enforces that rather than documenting it — Go panics with
// "test using t.Setenv or t.Chdir can not use t.Parallel" if anyone adds
// t.Parallel() to a test that calls this, instead of it silently becoming a
// data race on the globals below.
func withMachineIDResolver(t *testing.T, fn func() (string, error)) {
	t.Helper()
	t.Setenv("ENTIRE_TEST_MACHINE_ID_SEAM", "1")
	prev := machineIDResolver
	machineIDResolver = fn
	resetMachineIDCacheForTest()
	t.Cleanup(func() {
		machineIDResolver = prev
		resetMachineIDCacheForTest()
	})
}

// The contract that matters: payload builders run in loops, so the platform
// lookup must happen once per process no matter how many payloads are built.
// On macOS each miss is an `ioreg` subprocess (~11.8ms), which is what made a
// 20-event batch cost 218ms of blocking hook time.
func TestTelemetryMachineID_ResolvesOncePerProcess(t *testing.T) {
	var calls int
	withMachineIDResolver(t, func() (string, error) {
		calls++
		return testMachineID, nil
	})

	inv := SkillInvocation{Skill: "entire", Agent: "claude-code",
		Signal: "prompt_slash_command", EventType: "prompt_invocation"}
	for range 20 {
		if p := BuildSkillEventPayload(inv, true, "1.0.0"); p == nil {
			t.Fatal("BuildSkillEventPayload returned nil")
		}
	}
	// Other event types share the same cache.
	if p := BuildPluginEventPayload("entire-review", true, "1.0.0"); p == nil {
		t.Fatal("BuildPluginEventPayload returned nil")
	}

	if calls != 1 {
		t.Errorf("platform lookup ran %d times, want 1", calls)
	}
}

func TestTelemetryMachineID_ReturnsCachedValue(t *testing.T) {
	withMachineIDResolver(t, func() (string, error) { return testMachineID, nil })

	first, err := telemetryMachineID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := telemetryMachineID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != testMachineID || second != first {
		t.Errorf("got %q then %q, want both %q", first, second, testMachineID)
	}
}

// A failed lookup is cached deliberately: callers treat it as "no payload", and
// retrying per event would reintroduce the per-event subprocess.
func TestTelemetryMachineID_CachesFailureAndDropsPayload(t *testing.T) {
	var calls int
	wantErr := errors.New("ioreg unavailable")
	withMachineIDResolver(t, func() (string, error) {
		calls++
		return "", wantErr
	})

	inv := SkillInvocation{Skill: "entire", Agent: "claude-code",
		Signal: "prompt_slash_command", EventType: "prompt_invocation"}
	for range 5 {
		if p := BuildSkillEventPayload(inv, true, "1.0.0"); p != nil {
			t.Fatal("expected nil payload when the machine ID is unavailable")
		}
	}
	if calls != 1 {
		t.Errorf("platform lookup ran %d times, want 1", calls)
	}
	if _, err := telemetryMachineID(); !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want %v", err, wantErr)
	}
}

// sync.OnceValues must serialize concurrent first callers to a single lookup.
func TestTelemetryMachineID_ConcurrentCallersResolveOnce(t *testing.T) {
	var mu sync.Mutex
	var calls int
	withMachineIDResolver(t, func() (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return testMachineID, nil
	})

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := telemetryMachineID(); err != nil || id != testMachineID {
				t.Errorf("got (%q, %v)", id, err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("platform lookup ran %d times under concurrency, want 1", calls)
	}
}
