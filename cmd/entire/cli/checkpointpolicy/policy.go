package checkpointpolicy

import (
	"fmt"
	"slices"
	"strings"
)

type Policy struct {
	CheckpointVersion string `json:"checkpoint_version,omitempty"`
}

func DefaultPolicy() Policy {
	return Policy{
		CheckpointVersion: DefaultCheckpointVersionSelector,
	}
}

func Normalize(policy Policy) Policy {
	if policy.CheckpointVersion == "" {
		policy.CheckpointVersion = DefaultCheckpointVersionSelector
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

	_, err := ResolveCheckpointVersionSelector(policy.CheckpointVersion)
	return err
}

func UnsupportedWrite(policy Policy) bool {
	_, err := ResolvedWriteVersion(policy)
	return err != nil
}

func CanSatisfyPolicy(policy Policy) bool {
	return ValidatePolicy(policy) == nil
}

func PolicyEnablesFeature(policy Policy, feature CheckpointFeature) bool {
	policy = Normalize(policy)
	version, err := resolveCheckpointVersionSelector(policy.CheckpointVersion, supportedCheckpointVersions)
	if err != nil {
		return false
	}
	return slices.Contains(version.features, feature)
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

	_, err := ResolveCheckpointVersionSelector(policy.CheckpointVersion)
	if err != nil {
		details = append(details, err.Error()+".")
	}

	return details
}
