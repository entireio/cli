package githelper

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/internal/remotehelper/bbpush"
)

// Options accumulates `option <name> <value>` commands from git and
// translates the ones we can act on into `git send-pack` flags. The
// fields mirror what upstream's transport-helper.c:set_common_push_options
// forwards before a push: dry-run, atomic, signed (push-cert),
// force-with-lease (cas), force-if-includes, push-option, plus the
// per-list object-format option and the verbosity of standard_options.
// Unknown options are answered with "unsupported", which transport-helper
// treats as "ignore me".
type Options struct {
	dryRun          bool
	atomic          bool
	pushCert        string // "true" / "if-asked" / ""
	forceIfIncludes bool
	pushOptions     []string
	cas             []string // --force-with-lease=<spec>
	invalidCAS      bool
	// quiet is git's --quiet: transport-helper sends "verbosity 0". It
	// stops the summary line of a client-chunked push. send-pack does not
	// get it.
	quiet bool
}

// Set applies one option and returns the helper-protocol reply line.
// Booleans use the literal "true"/"false" upstream emits
// (transport-helper.c:set_helper_option). String options pass through
// quote_c_style there, so a value containing '"', '\' or (under
// core.quotePath) non-ASCII bytes arrives C-quoted. send-pack parses
// its flags literally, so cas and push-option are unquoted here the
// same way remote-curl.c:set_option does. A malformed quoted value is
// answered with "error invalid value". transport-helper ignores that
// reply for cas and still sends the forced refspec, so a rejected cas
// also makes PushError refuse the push.
func (o *Options) Set(name, value string) string {
	switch name {
	case "dry-run":
		o.dryRun = value == optionValueTrue
	case "atomic":
		o.atomic = value == optionValueTrue
	case "pushcert", "push-cert":
		// git's transport-helper sends "pushcert" (TRANS_OPT_PUSH_CERT).
		o.pushCert = value
	case "force-if-includes":
		o.forceIfIncludes = value == optionValueTrue
	case "push-option":
		v, ok := unquoteOptionValue(value)
		if !ok {
			return optionReplyInvalid
		}
		o.pushOptions = append(o.pushOptions, v)
	case "cas":
		v, ok := unquoteOptionValue(value)
		if !ok {
			o.invalidCAS = true
			return optionReplyInvalid
		}
		o.cas = append(o.cas, v)
	case "verbosity":
		o.quiet = value == "0"
	case "object-format":
		// Accepted so transport-helper can negotiate object-format
		// support. send-pack learns the actual format from the ref
		// advertisement.
	default:
		return "unsupported"
	}
	return "ok"
}

// errInvalidCAS refuses a push whose lease could not be parsed. Pushing
// without it would turn a --force-with-lease push into a plain force push.
var errInvalidCAS = errors.New("refusing to push: invalid force-with-lease value")

// PushError reports whether the accumulated options must stop a push.
func (o *Options) PushError() error {
	if o.invalidCAS {
		return errInvalidCAS
	}
	return nil
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

// errBBUnsupportedOption names a push option that the client-chunked push
// does not implement.
var errBBUnsupportedOption = errors.New("an option needs send-pack")

// bbPushOptions returns the options of a client-chunked push. It fails for
// a signed push, which only send-pack implements. A push signed "if-asked"
// stays on the client-chunked path unless the remote asks for a signature.
// force-if-includes needs nothing here: git checks it before it runs the
// helper.
func (o *Options) bbPushOptions() (bbpush.Options, error) {
	if o.pushCert == optionValueTrue {
		return bbpush.Options{}, fmt.Errorf("push-cert %s: %w", o.pushCert, errBBUnsupportedOption)
	}
	return bbpush.Options{
		DryRun:          o.dryRun,
		Atomic:          o.atomic,
		PushOptions:     o.pushOptions,
		CAS:             o.cas,
		PushCertIfAsked: o.pushCert == "if-asked",
		Quiet:           o.quiet,
	}, nil
}

// optionReplyInvalid is remote-curl.c's reply when set_option fails.
const optionReplyInvalid = "error invalid value"

// unquoteOptionValue returns value unchanged unless it starts with '"'.
// A quoted value is unquoted as by quote.c:unquote_c_style, which
// accepts the escapes quote_c_style emits: \a \b \f \n \r \t \v,
// \\, \" and three-digit octal with a leading digit of 0-3. Like
// remote-curl.c, anything after the closing quote is ignored.
func unquoteOptionValue(value string) (string, bool) {
	if !strings.HasPrefix(value, `"`) {
		return value, true
	}
	var b strings.Builder
	b.Grow(len(value))
	for i := 1; i < len(value); {
		c := value[i]
		i++
		switch c {
		case '"':
			return b.String(), true
		case 0:
			return "", false
		case '\\':
		default:
			b.WriteByte(c)
			continue
		}
		if i >= len(value) {
			return "", false
		}
		c = value[i]
		i++
		switch c {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\', '"':
			b.WriteByte(c)
		case '0', '1', '2', '3':
			if i+2 > len(value) || !isOctal(value[i]) || !isOctal(value[i+1]) {
				return "", false
			}
			b.WriteByte((c-'0')<<6 | (value[i]-'0')<<3 | (value[i+1] - '0'))
			i += 2
		default:
			return "", false
		}
	}
	return "", false
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
