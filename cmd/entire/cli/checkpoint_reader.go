package cli

import "github.com/entireio/cli/cmd/entire/cli/checkpoint"

type committedCheckpointReader interface {
	checkpoint.Reader
	checkpoint.SessionReader
}
