package checkpointpolicy

import (
	"fmt"
	"strings"
)

type Policy struct {
	CheckpointVersion    string `json:"checkpoint_version,omitempty"`
	CheckpointMinVersion string `json:"checkpoint_min_version,omitempty"`
}

func DefaultPolicy() Policy {
	return Policy{
		CheckpointVersion:    CheckpointVersionBranchV1,
		CheckpointMinVersion: CheckpointVersionBranchV1,
	}
}

func DefaultCheckpointVersion() string {
	return CheckpointVersionBranchV1
}

func Normalize(policy Policy) Policy {
	if policy.CheckpointVersion == "" {
		policy.CheckpointVersion = DefaultCheckpointVersion()
	}
	if policy.CheckpointMinVersion == "" {
		policy.CheckpointMinVersion = CheckpointVersionBranchV1
	}
	return policy
}

func ValidatePolicy(policy Policy) error {
	policy = Normalize(policy)

	version, err := ParseFormat(policy.CheckpointVersion)
	if err != nil {
		return fmt.Errorf("checkpoint_version: %w", err)
	}
	if !CanWrite(version) {
		return fmt.Errorf("checkpoint_version %q is not supported by this Entire CLI", policy.CheckpointVersion)
	}

	minVersion, err := ParseFormat(policy.CheckpointMinVersion)
	if err != nil {
		return fmt.Errorf("checkpoint_min_version: %w", err)
	}
	if !CanRead(minVersion) {
		return fmt.Errorf("checkpoint_min_version %q is not supported by this Entire CLI", policy.CheckpointMinVersion)
	}
	if Compare(minVersion, version) > 0 {
		return fmt.Errorf("checkpoint_min_version %q is newer than checkpoint_version %q", policy.CheckpointMinVersion, policy.CheckpointVersion)
	}

	return nil
}

func RequiresUpgrade(policy Policy) bool {
	policy = Normalize(policy)
	minVersion, err := ParseFormat(policy.CheckpointMinVersion)
	if err != nil {
		return true
	}
	return !CanRead(minVersion)
}

func UnsupportedWrite(policy Policy) bool {
	policy = Normalize(policy)
	version, err := ParseFormat(policy.CheckpointVersion)
	if err != nil {
		return true
	}
	return !CanWrite(version)
}

func CanSatisfyPolicy(policy Policy) bool {
	return !UnsupportedWrite(policy) && !RequiresUpgrade(policy)
}

// UnsupportedPolicyMessage renders the upgrade advice for a policy this CLI
// cannot satisfy. commandShell names the shell updateCommand has to run in
// (versioncheck.UpdateCommandShell), and is "" when any shell will do: the
// command is printed for the user to paste, and on Windows it is a PowerShell
// one-liner that misbehaves in cmd.exe or bash, so an unnamed shell is not
// enough to act on.
func UnsupportedPolicyMessage(policy Policy, updateCommand, commandShell string) string {
	if CanSatisfyPolicy(policy) {
		return ""
	}

	upgrade := "Upgrade Entire, then rerun the command:"
	if commandShell != "" {
		upgrade = fmt.Sprintf("Upgrade Entire by running the following in %s, then rerun the command:", commandShell)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[entire] This repository requires checkpoint support newer than this Entire CLI.\n[entire] %s\n[entire]   %s\n", upgrade, updateCommand)
	details := unsupportedPolicyDetails(policy)
	if len(details) == 0 {
		return b.String()
	}
	b.WriteString("[entire] Details:\n")
	for _, detail := range details {
		fmt.Fprintf(&b, "[entire]   %s\n", detail)
	}
	return b.String()
}

func unsupportedPolicyDetails(policy Policy) []string {
	policy = Normalize(policy)
	var details []string

	version, err := ParseFormat(policy.CheckpointVersion)
	if err != nil {
		details = append(details, fmt.Sprintf("checkpoint_version %q is invalid: %v.", policy.CheckpointVersion, err))
	} else if !CanWrite(version) {
		details = append(details, fmt.Sprintf("checkpoint_version %q is not writable by this Entire CLI; this CLI defaults to %q.", policy.CheckpointVersion, DefaultCheckpointVersion()))
	}

	minVersion, err := ParseFormat(policy.CheckpointMinVersion)
	if err != nil {
		details = append(details, fmt.Sprintf("checkpoint_min_version %q is invalid: %v.", policy.CheckpointMinVersion, err))
	} else if !CanRead(minVersion) {
		details = append(details, fmt.Sprintf("checkpoint_min_version %q is not readable by this Entire CLI; this CLI can read %q.", policy.CheckpointMinVersion, DefaultCheckpointVersion()))
	}

	return details
}
