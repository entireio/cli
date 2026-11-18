package bbpush

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Options are the push options that git sets on the helper and that this
// path implements. The caller sends a push with any other option through
// git send-pack.
type Options struct {
	DryRun bool
	Atomic bool
	// PushOptions are the --push-option values.
	PushOptions []string
	// CAS holds the force-with-lease values in "<ref>:<expected oid>" form,
	// as git's transport-helper sends them.
	CAS []string
	// PushCertIfAsked is --signed=if-asked. The push is signed only when
	// the remote advertises push-cert, and only send-pack signs, so such a
	// remote makes Push fall back with ErrNeedsSendPack.
	PushCertIfAsked bool
	// Quiet is git push --quiet. Push then prints no summary line.
	Quiet bool
}

// command is one ref update of the push.
type command struct {
	ref string
	// old is the advertised value, or the zero ID for a new ref.
	old string
	// new is the zero ID for a deletion.
	new string
}

// refspec is one "push" line of git's helper protocol:
// [+]<src>:<dst>, or :<dst> to delete.
type refspec struct {
	force bool
	src   string
	dst   string
}

func parseRefspec(s string) (refspec, error) {
	var r refspec
	if rest, ok := strings.CutPrefix(s, "+"); ok {
		r.force, s = true, rest
	}
	src, dst, ok := strings.Cut(s, ":")
	switch {
	case !ok:
		return r, fmt.Errorf("refspec %q has no destination", s)
	case !strings.HasPrefix(dst, "refs/") || strings.ContainsAny(dst, "*: \t\n\x00"):
		return r, fmt.Errorf("refspec %q has destination %q", s, dst)
	case strings.HasPrefix(src, "-") || strings.ContainsAny(src, "*\n\x00"):
		return r, fmt.Errorf("refspec %q has source %q", s, src)
	}
	r.src, r.dst = src, dst
	return r, nil
}

// parseCAS reads the force-with-lease values. Each names a full ref and a
// full object ID; a lease that git would resolve from a tracking ref never
// reaches the helper.
func parseCAS(values []string, oidHexLen int) (map[string]string, error) {
	leases := make(map[string]string, len(values))
	for _, v := range values {
		ref, oid, ok := strings.Cut(v, ":")
		if !ok || !strings.HasPrefix(ref, "refs/") || len(oid) != oidHexLen || !isHexOID(oid) {
			return nil, fmt.Errorf("force-with-lease value %q", v)
		}
		leases[ref] = oid
	}
	return leases, nil
}

// planCommands turns the refspecs into ref updates, with the checks that
// send-pack makes before it sends anything (set_ref_status_for_push). git
// already made the same checks against the same advertisement, so a refspec
// that one of them would reject does not normally reach the helper. When one
// does, planCommands fails, and the caller lets send-pack report it in its
// own words.
func planCommands(ctx context.Context, repo *localRepo, adv *advertisement, specs []string, opts Options) ([]command, error) {
	leases, err := parseCAS(opts.CAS, repo.oidHexLen)
	if err != nil {
		return nil, err
	}
	parsed := make([]refspec, len(specs))
	var names []string
	seen := map[string]bool{}
	for i, s := range specs {
		if parsed[i], err = parseRefspec(s); err != nil {
			return nil, err
		}
		if seen[parsed[i].dst] {
			return nil, fmt.Errorf("two refspecs update %s", parsed[i].dst)
		}
		seen[parsed[i].dst] = true
		if parsed[i].src != "" {
			names = append(names, parsed[i].src)
		}
		if old := adv.oldOID(parsed[i].dst); old != "" {
			names = append(names, old)
		}
	}
	objects, err := batchCheck(ctx, names)
	if err != nil {
		return nil, err
	}
	cmds := make([]command, 0, len(parsed))
	for _, r := range parsed {
		c, err := planCommand(ctx, repo, adv, r, leases, objects)
		if err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	return cmds, nil
}

func planCommand(ctx context.Context, repo *localRepo, adv *advertisement, r refspec, leases map[string]string, objects map[string]object) (command, error) {
	zero := repo.zeroOID()
	c := command{ref: r.dst, old: adv.oldOID(r.dst), new: zero}
	if c.old == "" {
		c.old = zero
	}
	if r.src == "" {
		if c.old == zero {
			return c, fmt.Errorf("%s: deletes a ref that the remote does not have", r.dst)
		}
		if !adv.supports("delete-refs") {
			return c, errors.New("the remote does not take deletions")
		}
		return c, nil
	}
	obj, ok := objects[r.src]
	if !ok {
		return c, fmt.Errorf("%s: source %q does not resolve", r.dst, r.src)
	}
	c.new = obj.oid
	if c.new == c.old {
		return c, fmt.Errorf("%s is up to date", r.dst)
	}
	force := r.force
	if expect, ok := leases[r.dst]; ok {
		if expect != c.old {
			return c, fmt.Errorf("%s: stale lease", r.dst)
		}
		force = true
	}
	if force || c.old == zero {
		return c, nil
	}
	return c, checkFastForward(ctx, c, objects)
}

// checkFastForward applies the "must fast-forward" rules of an update that
// is not forced.
func checkFastForward(ctx context.Context, c command, objects map[string]object) error {
	if strings.HasPrefix(c.ref, "refs/tags/") {
		return fmt.Errorf("%s already exists", c.ref)
	}
	if _, ok := objects[c.old]; !ok {
		return fmt.Errorf("%s: the remote value %s is not in the local repository", c.ref, c.old)
	}
	ff, err := isAncestor(ctx, c.old, c.new)
	if err != nil {
		return err
	}
	if !ff {
		return fmt.Errorf("%s: not a fast-forward", c.ref)
	}
	return nil
}

// requestCaps returns the capabilities of the first command line, in
// send-pack's order, each with a leading space. It fails when send-pack
// would refuse the push before it sends a request, or would sign it. quiet
// is send-pack's choice when it shows no progress: when stderr is not a
// terminal.
func requestCaps(adv *advertisement, repo *localRepo, opts Options, quiet bool) (string, error) {
	if opts.PushCertIfAsked && adv.supports("push-cert") {
		return "", fmt.Errorf("the remote asks for a signed push: %w", ErrNeedsSendPack)
	}
	if !adv.supports("report-status") {
		return "", errors.New("the remote does not offer report-status")
	}
	format, ok := adv.caps["object-format"]
	if !ok {
		format = "sha1"
	}
	if format != repo.objectFormat {
		return "", fmt.Errorf("the remote uses object format %s, the repository %s", format, repo.objectFormat)
	}
	if opts.Atomic && !adv.supports("atomic") {
		return "", errors.New("the remote does not take atomic pushes")
	}
	if len(opts.PushOptions) > 0 && !adv.supports("push-options") {
		return "", errors.New("the remote does not take push options")
	}
	var b strings.Builder
	b.WriteString(" report-status")
	if adv.supports("side-band-64k") {
		b.WriteString(" side-band-64k")
	}
	if quiet && adv.supports("quiet") {
		b.WriteString(" quiet")
	}
	if opts.Atomic {
		b.WriteString(" atomic")
	}
	if len(opts.PushOptions) > 0 {
		b.WriteString(" push-options")
	}
	if adv.supports("object-format") {
		b.WriteString(" object-format=" + repo.objectFormat)
	}
	if adv.supports("agent") {
		b.WriteString(" agent=" + repo.agent)
	}
	b.WriteString(" " + capBB)
	return b.String(), nil
}
