package checkpoint

import "errors"

// ErrSigningDisabled is returned by SignCommit when checkpoint signing is
// disabled in settings.
var ErrSigningDisabled = errors.New("checkpoint signing disabled")
