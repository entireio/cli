package telemetry

import "testing"

func TestBuildCheckpointPolicyBlockedPayload_Unsupported(t *testing.T) {
	t.Parallel()
	payload := BuildCheckpointPolicyBlockedPayload(CheckpointPolicyBlockedEvent{
		Hook:                 "post-commit",
		HookType:             PolicyBlockedHookTypeGit,
		Reason:               PolicyBlockedReasonUnsupported,
		Outcome:              PolicyBlockedOutcomeSkipped,
		CheckpointVersion:    "v2",
		CheckpointMinVersion: "v2",
	}, "1.2.3")
	if payload == nil {
		t.Fatal("BuildCheckpointPolicyBlockedPayload returned nil")
		return
	}
	if payload.Event != "checkpoint_policy_blocked" {
		t.Errorf("Event = %q, want %q", payload.Event, "checkpoint_policy_blocked")
	}
	if payload.DistinctID == "" {
		t.Error("DistinctID must be set to the machine ID")
	}
	checks := map[string]any{
		"hook":                   "post-commit",
		"hook_type":              "git",
		"reason":                 "policy_unsupported",
		"outcome":                "skipped",
		"checkpoint_version":     "v2",
		"checkpoint_min_version": "v2",
		"cli_version":            "1.2.3",
	}
	for k, want := range checks {
		if got := payload.Properties[k]; got != want {
			t.Errorf("Properties[%q] = %v, want %v", k, got, want)
		}
	}
	if _, ok := payload.Properties["agent"]; ok {
		t.Error("git-hook payload must not include 'agent'")
	}
}

func TestBuildCheckpointPolicyBlockedPayload_UnreadableOmitsVersions(t *testing.T) {
	t.Parallel()
	payload := BuildCheckpointPolicyBlockedPayload(CheckpointPolicyBlockedEvent{
		Hook:     "session-start",
		HookType: PolicyBlockedHookTypeAgent,
		Reason:   PolicyBlockedReasonUnreadable,
		Outcome:  PolicyBlockedOutcomeSkipped,
		Agent:    "claude-code",
	}, "1.2.3")
	if payload == nil {
		t.Fatal("BuildCheckpointPolicyBlockedPayload returned nil")
		return
	}
	if got := payload.Properties["agent"]; got != "claude-code" {
		t.Errorf("Properties[agent] = %v, want %q", got, "claude-code")
	}
	if _, ok := payload.Properties["checkpoint_version"]; ok {
		t.Error("unreadable payload must omit 'checkpoint_version'")
	}
	if _, ok := payload.Properties["checkpoint_min_version"]; ok {
		t.Error("unreadable payload must omit 'checkpoint_min_version'")
	}
	if got := payload.Properties["outcome"]; got != "skipped" {
		t.Errorf("Properties[outcome] = %v, want %q", got, "skipped")
	}
}
