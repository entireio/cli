package checkpointpolicy

import (
	"errors"
	"fmt"
)

var errUnsupportedCheckpointVersion = errors.New("not supported by this Entire CLI")

// IsUnsupportedCheckpointVersionError reports whether err means checkpoint metadata uses an unsupported version.
func IsUnsupportedCheckpointVersionError(err error) bool {
	return errors.Is(err, errUnsupportedCheckpointVersion)
}

// ValidateCheckpointMetadataVersion rejects checkpoint metadata versions this CLI does not support.
func ValidateCheckpointMetadataVersion(checkpointID, version string) error {
	logicalVersion, err := LogicalVersionForCheckpointMetadata(version)
	if err != nil {
		return fmt.Errorf("checkpoint %s has invalid checkpoint_version %q: %w", checkpointID, version, err)
	}
	if !IsSupportedCheckpointVersion(logicalVersion) {
		return unsupportedCheckpointVersionError{
			CheckpointID: checkpointID,
			Version:      version,
		}
	}
	return nil
}

type unsupportedCheckpointVersionError struct {
	CheckpointID string
	Version      string
}

func (e unsupportedCheckpointVersionError) Error() string {
	return fmt.Sprintf("checkpoint %s uses unsupported checkpoint_version %q: %v", e.CheckpointID, e.Version, errUnsupportedCheckpointVersion)
}

func (e unsupportedCheckpointVersionError) Unwrap() error {
	return errUnsupportedCheckpointVersion
}
