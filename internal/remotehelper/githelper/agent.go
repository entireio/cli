package githelper

// Agent is the git agent identifier the helper appends to upload-pack,
// receive-pack, and v2 requests so the server can attribute traffic to
// the remote helper. The CLI entrypoint overwrites it at startup with a
// build-stamped value (see cmd/entire); the default keeps tests and
// standalone use working without that wiring.
var Agent = "git-remote-entire/dev"
