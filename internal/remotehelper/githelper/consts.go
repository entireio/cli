package githelper

// Git smart-HTTP service names exchanged over the remote-helper
// protocol. Extracted to constants so the repeated literals across the
// connect/stateless/push paths (and their tests) satisfy goconst.
const (
	serviceUploadPack  = "git-upload-pack"
	serviceReceivePack = "git-receive-pack"
)

// optionValueTrue is git's literal boolean "true" as sent on the
// `option <name> true` helper-protocol line (transport-helper.c).
const optionValueTrue = "true"
