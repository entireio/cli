package opencode

import _ "embed"

//go:embed entire_plugin.ts
var pluginTemplate string

// entireCmdPlaceholder is replaced with the actual command during installation.
const entireCmdPlaceholder = "__ENTIRE_CMD__"
