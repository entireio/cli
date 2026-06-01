package githelper

// Options accumulates `option <name> <value>` commands from git and
// translates the ones we can act on into `git send-pack` flags. The
// fields mirror what upstream's transport-helper.c:set_common_push_options
// forwards before a push: dry-run, atomic, signed (push-cert),
// force-with-lease (cas), force-if-includes, push-option, plus the
// per-list object-format option. Unknown options are answered with
// "unsupported", which transport-helper treats as "ignore me".
type Options struct {
	dryRun          bool
	atomic          bool
	pushCert        string // "true" / "if-asked" / ""
	forceIfIncludes bool
	pushOptions     []string
	cas             []string // --force-with-lease=<spec>
}

// Set applies one option and returns the helper-protocol reply line.
// The value is taken verbatim — booleans use the literal "true"/"false"
// upstream emits (transport-helper.c:357-361); string options use
// quote_c_style output, which our consumers (send-pack) accept on
// input in the same form. We intentionally do not attempt to unquote,
// so values containing spaces or backslashes round-trip correctly.
func (o *Options) Set(name, value string) string {
	switch name {
	case "dry-run":
		o.dryRun = value == optionValueTrue
	case "atomic":
		o.atomic = value == optionValueTrue
	case "push-cert":
		o.pushCert = value
	case "force-if-includes":
		o.forceIfIncludes = value == optionValueTrue
	case "push-option":
		o.pushOptions = append(o.pushOptions, value)
	case "cas":
		o.cas = append(o.cas, value)
	case "object-format":
		// Accepted so transport-helper can negotiate object-format
		// support. send-pack learns the actual format from the ref
		// advertisement.
	default:
		return "unsupported"
	}
	return "ok"
}

// SendPackArgs returns the `git send-pack` flag set corresponding to
// the accumulated options. Returned slice is fresh; callers can mutate
// it.
func (o *Options) SendPackArgs() []string {
	var args []string
	if o.dryRun {
		args = append(args, "--dry-run")
	}
	if o.atomic {
		args = append(args, "--atomic")
	}
	switch o.pushCert {
	case optionValueTrue:
		args = append(args, "--signed=true")
	case "if-asked":
		args = append(args, "--signed=if-asked")
	}
	if o.forceIfIncludes {
		args = append(args, "--force-if-includes")
	}
	for _, po := range o.pushOptions {
		args = append(args, "--push-option="+po)
	}
	for _, cas := range o.cas {
		args = append(args, "--force-with-lease="+cas)
	}
	return args
}
