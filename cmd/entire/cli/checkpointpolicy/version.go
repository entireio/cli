package checkpointpolicy

import (
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

var errUnsupportedVersion = errors.New("not read-supported by this Entire CLI")

func IsUnsupportedVersion(err error) bool {
	return errors.Is(err, errUnsupportedVersion)
}

func EnsureCanReadVersion(checkpointID, version string) error {
	if version == "" {
		version = checkpoint.CheckpointVersionBranchV1
	}

	format, err := ParseFormat(version)
	if err != nil {
		return fmt.Errorf("checkpoint %s has invalid checkpoint_version %q: %w", checkpointID, version, err)
	}
	if !CanRead(format) {
		return unsupportedVersionError{
			CheckpointID: checkpointID,
			Version:      version,
		}
	}
	return nil
}

type unsupportedVersionError struct {
	CheckpointID string
	Version      string
}

func (e unsupportedVersionError) Error() string {
	return fmt.Sprintf("checkpoint %s uses unsupported checkpoint_version %q: %v", e.CheckpointID, e.Version, errUnsupportedVersion)
}

func (e unsupportedVersionError) Unwrap() error {
	return errUnsupportedVersion
}
