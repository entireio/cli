package strategy

import "errors"

const (
	lefthookLocalConfigName = "lefthook-local.yml"
	lefthookLocalDir        = ".lefthook-local"
	lefthookScriptName      = "entire.sh"
	lefthookOwnedMarker     = "entire-cli-owned:lefthook:v1"
)

var (
	ErrLefthookAmbiguous          = errors.New("ambiguous Lefthook configuration")
	ErrLefthookOwnedEntryConflict = errors.New("lefthook entire.sh entry is not owned by Entire")
	errLefthookFileUnchanged      = errors.New("lefthook artifact is unchanged")
)

var lefthookMainConfigNames = lefthookConfigNames(false)
var lefthookLocalConfigNames = lefthookConfigNames(true)

type hookManagerDetectionError struct {
	manager hookManager
	err     error
}

func (e *hookManagerDetectionError) Error() string { return e.err.Error() }
func (e *hookManagerDetectionError) Unwrap() error { return e.err }
