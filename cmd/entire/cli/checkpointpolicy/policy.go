package checkpointpolicy

import (
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

type Policy struct {
	CheckpointVersion    string `json:"checkpoint_version,omitempty"`
	CheckpointMinVersion string `json:"checkpoint_min_version,omitempty"`
}

func DefaultPolicy() Policy {
	return Policy{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	}
}

func DefaultCheckpointVersion() string {
	return checkpoint.CheckpointVersionBranchV1
}

func Normalize(policy Policy) Policy {
	if policy.CheckpointVersion == "" {
		policy.CheckpointVersion = DefaultCheckpointVersion()
	}
	if policy.CheckpointMinVersion == "" {
		policy.CheckpointMinVersion = checkpoint.CheckpointVersionBranchV1
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

func UnsupportedPolicyMessage(policy Policy, updateCommand string) string {
	if CanSatisfyPolicy(policy) {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[entire] This repository requires checkpoint support newer than this Entire CLI.\n[entire] Upgrade Entire, then rerun the command:\n[entire]   %s\n", updateCommand)
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
