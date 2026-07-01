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
		CheckpointVersion:    DefaultCheckpointVersionSelector,
		CheckpointMinVersion: DefaultCheckpointMinVersionRange,
	}
}

func DefaultCheckpointVersion() string {
	return DefaultCheckpointVersionSelector
}

func Normalize(policy Policy) Policy {
	if policy.CheckpointVersion == "" {
		policy.CheckpointVersion = DefaultCheckpointVersionSelector
	}
	if policy.CheckpointMinVersion == "" {
		policy.CheckpointMinVersion = DefaultCheckpointMinVersionRange
	}
	return policy
}

func ResolvedWriteVersion(policy Policy) (string, error) {
	policy = Normalize(policy)
	return ResolveCheckpointVersionSelector(policy.CheckpointVersion)
}

func ResolvedMetadataVersion(policy Policy) (string, error) {
	version, err := ResolvedWriteVersion(policy)
	if err != nil {
		return "", err
	}
	return MetadataVersionForWriteVersion(version)
}

func ValidatePolicy(policy Policy) error {
	policy = Normalize(policy)

	writeVersion, err := ResolveCheckpointVersionSelector(policy.CheckpointVersion)
	if err != nil {
		return err
	}

	constraint, err := CheckpointMinVersionConstraint(policy.CheckpointMinVersion)
	if err != nil {
		return err
	}
	if !readableVersionSatisfies(constraint) {
		return fmt.Errorf("checkpoint_min_version %q is not readable by this Entire CLI", policy.CheckpointMinVersion)
	}
	if !constraint.Check(mustSemver(writeVersion)) {
		return fmt.Errorf(
			"checkpoint_version %q resolves to %q, which does not satisfy checkpoint_min_version %q",
			policy.CheckpointVersion,
			writeVersion,
			policy.CheckpointMinVersion,
		)
	}

	return nil
}

func RequiresUpgrade(policy Policy) bool {
	policy = Normalize(policy)
	constraint, err := CheckpointMinVersionConstraint(policy.CheckpointMinVersion)
	if err != nil {
		return true
	}
	return !readableVersionSatisfies(constraint)
}

func UnsupportedWrite(policy Policy) bool {
	_, err := ResolvedWriteVersion(policy)
	return err != nil
}

func CanSatisfyPolicy(policy Policy) bool {
	return ValidatePolicy(policy) == nil
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

	writeVersion, err := ResolveCheckpointVersionSelector(policy.CheckpointVersion)
	if err != nil {
		details = append(details, err.Error()+".")
	}

	constraint, err := CheckpointMinVersionConstraint(policy.CheckpointMinVersion)
	if err != nil {
		details = append(details, err.Error()+".")
	} else if !readableVersionSatisfies(constraint) {
		details = append(details, fmt.Sprintf("checkpoint_min_version %q is not readable by this Entire CLI; this CLI can read %q.", policy.CheckpointMinVersion, LogicalCheckpointVersionV1))
	} else if writeVersion != "" && !constraint.Check(mustSemver(writeVersion)) {
		details = append(details, fmt.Sprintf("checkpoint_version %q resolves to %q, which does not satisfy checkpoint_min_version %q.", policy.CheckpointVersion, writeVersion, policy.CheckpointMinVersion))
	}

	return details
}
