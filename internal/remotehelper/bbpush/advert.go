package bbpush

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"

	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// capBB is the capability that names the threshold in the advertisement
// and that the client echoes, with no value, to send meta 0x03 entries.
const capBB = "entire-bb"

// advertisedRef is one ref of the receive-pack advertisement.
type advertisedRef struct {
	name string
	oid  string
}

// advertisement is a parsed v0/v1 receive-pack ref advertisement.
type advertisement struct {
	// refs keeps the advertised order. send-pack prints its status lines
	// in this order.
	refs []advertisedRef
	// have lists the ".have" object IDs of alternates.
	have []string
	caps map[string]string
}

func (a *advertisement) supports(name string) bool {
	_, ok := a.caps[name]
	return ok
}

// oldOID returns the advertised value of ref, or "" for a ref the server
// does not have.
func (a *advertisement) oldOID(ref string) string {
	for _, r := range a.refs {
		if r.name == ref {
			return r.oid
		}
	}
	return ""
}

// Threshold returns the BB threshold that a receive-pack advertisement
// names with entire-bb. It reports false when the server does not offer
// client chunking.
func Threshold(adv []byte) (int64, bool) {
	a, err := parseAdvertisement(adv)
	if err != nil {
		return 0, false
	}
	return a.threshold()
}

func (a *advertisement) threshold() (int64, bool) {
	v, ok := a.caps[capBB]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// parseAdvertisement reads the ref lines that ReadPostServiceAdvertisement
// returns, as get_remote_heads does: the capabilities ride on the first
// line, "capabilities^{}" stands for an empty repository, and ".have" names
// an alternate's object.
func parseAdvertisement(b []byte) (*advertisement, error) {
	a := &advertisement{caps: map[string]string{}}
	r := bytes.NewReader(b)
	first := true
	for {
		n, line, err := pktline.ReadLine(r)
		if err != nil {
			return nil, fmt.Errorf("read advertisement: %w", err)
		}
		if n == pktline.Flush {
			return a, nil
		}
		text := strings.TrimSuffix(string(line), "\n")
		if first && strings.HasPrefix(text, "version ") {
			continue
		}
		if strings.HasPrefix(text, "shallow ") {
			continue
		}
		head, caps, hasCaps := strings.Cut(text, "\x00")
		if first && hasCaps {
			a.parseCaps(caps)
		}
		first = false
		oid, name, ok := strings.Cut(head, " ")
		if !ok || !isHexOID(oid) {
			return nil, fmt.Errorf("bad advertisement line %q", text)
		}
		switch {
		case name == "capabilities^{}":
		case name == ".have":
			a.have = append(a.have, oid)
		case !plainRefName(name):
			// No valid ref name holds such a byte, and the name would reach
			// the status lines that git reads.
			debuglog.Printf("bbpush: skipping advertised ref %q", name)
		default:
			a.refs = append(a.refs, advertisedRef{name: name, oid: oid})
		}
	}
}

func (a *advertisement) parseCaps(caps string) {
	for _, c := range strings.Fields(caps) {
		k, v, _ := strings.Cut(c, "=")
		if _, seen := a.caps[k]; !seen {
			a.caps[k] = v
		}
	}
}

// plainRefName reports whether name is not empty and holds no space or
// control byte.
func plainRefName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		if c := name[i]; c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isZeroOID(s string) bool {
	return strings.Trim(s, "0") == ""
}

func decodeOID(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("object ID %q: %w", s, err)
	}
	return b, nil
}
