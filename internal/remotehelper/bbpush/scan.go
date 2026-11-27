package bbpush

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
)

// largeBlob is a blob of the push at or over the threshold.
type largeBlob struct {
	oid  string
	size int64
}

// revisions returns the rev-list input of the push: each new value, and each
// advertised value that the local repository holds as a negative.
func revisions(cmds []command, zero string, negatives []string) []byte {
	var b bytes.Buffer
	for _, c := range cmds {
		if c.new != zero {
			b.WriteString(c.new + "\n")
		}
	}
	for _, n := range negatives {
		b.WriteString("^" + n + "\n")
	}
	return b.Bytes()
}

// localNegatives returns the advertised ref values and ".have" IDs that the
// local repository holds. send-pack excludes the same objects.
func localNegatives(ctx context.Context, adv *advertisement) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(oid string) {
		if !isZeroOID(oid) && !seen[oid] {
			seen[oid] = true
			ids = append(ids, oid)
		}
	}
	for _, r := range adv.refs {
		add(r.oid)
	}
	for _, h := range adv.have {
		add(h)
	}
	objects, err := batchCheck(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range ids {
		if _, ok := objects[id]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// findLargeBlobs lists the blobs of the push at or over threshold, ordered
// by size and then object ID, so the order does not depend on the walk. The
// filter is the one that the pack uses, so the pack holds every other
// object.
func findLargeBlobs(ctx context.Context, revs []byte, threshold int64, stderr io.Writer) ([]largeBlob, error) {
	omitted := map[string]bool{}
	err := streamLines(ctx, revs, stderr, func(line string) {
		if oid, ok := strings.CutPrefix(line, "~"); ok {
			omitted[oid] = true
		}
	}, "rev-list", "--objects", "--stdin", "--filter=blob:limit="+strconv.FormatInt(threshold, 10), "--filter-print-omitted")
	if err != nil || len(omitted) == 0 {
		return nil, err
	}
	ids := make([]string, 0, len(omitted))
	for oid := range omitted {
		ids = append(ids, oid)
	}
	objects, err := batchCheck(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]largeBlob, 0, len(ids))
	for _, oid := range ids {
		obj, ok := objects[oid]
		switch {
		case !ok || obj.typ != "blob" || obj.size < threshold:
			return nil, fmt.Errorf("omitted object %s is not a blob of at least %d bytes", oid, threshold)
		case obj.size > bbchunk.MaxObjectSize:
			return nil, fmt.Errorf("blob %s is %d bytes, over the %d-byte chunked blob limit", oid, obj.size, bbchunk.MaxObjectSize)
		}
		out = append(out, largeBlob{oid: oid, size: obj.size})
	}
	slices.SortFunc(out, func(a, b largeBlob) int {
		return cmp.Or(cmp.Compare(a.size, b.size), cmp.Compare(a.oid, b.oid))
	})
	return out, nil
}

// streamLines runs git with stdin and calls fn for each line of its output.
// git's own messages go to stderr.
func streamLines(ctx context.Context, stdin []byte, stderr io.Writer, fn func(string), args ...string) error {
	cmd := gitCommand(ctx, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git %s stdout: %w", args[0], err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git %s: %w", args[0], err)
	}
	r := bufio.NewReaderSize(out, 64<<10)
	var readErr error
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			fn(strings.TrimSuffix(line, "\n"))
		}
		if err != nil {
			if err != io.EOF {
				readErr = fmt.Errorf("read git %s: %w", args[0], err)
				killProcess(cmd)
			}
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return readErr
}
