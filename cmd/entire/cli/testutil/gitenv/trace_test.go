package gitenv

import "testing"

func TestTraceCommands(t *testing.T) {
	IsolateRepository(t)
	commands := TraceCommands(t)
	if calls := commands(); len(calls) != 0 {
		t.Fatalf("got %v before any Git process started, want none", calls)
	}
	root := t.TempDir()
	for count := 1; count <= 2; count++ {
		Run(t, root, "version")
		calls := commands()
		if len(calls) != count {
			t.Fatalf("got %v, want %d process starts", calls, count)
		}
		for _, args := range calls {
			if len(args) != 2 || args[1] != "version" {
				t.Fatalf("unexpected command arguments: %v", args)
			}
		}
	}
}
