package bbpush

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// The helper runs in the repository that git pushes from: git sets GIT_DIR
// for it, and every git command here inherits the environment.

// gitCommand returns a git command for this package. Every git process of a
// push starts here. GIT_NO_LAZY_FETCH stops a partial clone from fetching
// missing objects during the scan: a missing object makes the push fall back
// to send-pack instead.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	return cmd
}

// gitOutput runs git with stdin and returns its standard output.
func gitOutput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := gitCommand(ctx, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// object is the answer of `git cat-file --batch-check` for one name.
type object struct {
	oid  string
	typ  string
	size int64
}

// batchCheck resolves each name to its object. A name that does not resolve
// has no entry.
func batchCheck(ctx context.Context, names []string) (map[string]object, error) {
	if len(names) == 0 {
		return map[string]object{}, nil
	}
	var in bytes.Buffer
	for _, n := range names {
		if strings.ContainsAny(n, "\n\r") {
			return nil, fmt.Errorf("object name %q holds a line break", n)
		}
		in.WriteString(n)
		in.WriteByte('\n')
	}
	out, err := gitOutput(ctx, in.Bytes(), "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(names) {
		return nil, fmt.Errorf("cat-file answered %d lines for %d names", len(lines), len(names))
	}
	res := make(map[string]object, len(names))
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) != 3 || !isHexOID(f[0]) {
			continue
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cat-file size %q: %w", f[2], err)
		}
		res[names[i]] = object{oid: f[0], typ: f[1], size: size}
	}
	return res, nil
}

// isAncestor reports whether commit a is an ancestor of commit b. It fails
// when either is not a commit.
func isAncestor(ctx context.Context, a, b string) (bool, error) {
	cmd := gitCommand(ctx, "merge-base", "--is-ancestor", a, b)
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	default:
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", a, b, err)
	}
}

// localRepo is what a push needs to know about the repository.
type localRepo struct {
	objectFormat string
	// oidHexLen is 40 for SHA-1 and 64 for SHA-256.
	oidHexLen int
	agent     string
}

func openLocalRepo(ctx context.Context) (*localRepo, error) {
	out, err := gitOutput(ctx, nil, "rev-parse", "--show-object-format", "--is-shallow-repository")
	if err != nil {
		return nil, err
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return nil, fmt.Errorf("git rev-parse answered %q", out)
	}
	if f[1] != "false" {
		// send-pack sends the shallow boundary, which this path does not.
		return nil, errors.New("the repository is shallow")
	}
	r := &localRepo{objectFormat: f[0]}
	switch r.objectFormat {
	case "sha1":
		r.oidHexLen = 40
	case "sha256":
		r.oidHexLen = 64
	default:
		return nil, fmt.Errorf("object format %q", r.objectFormat)
	}
	r.agent, err = gitAgent(ctx)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *localRepo) zeroOID() string { return strings.Repeat("0", r.oidHexLen) }

// gitAgent returns the agent value that git sends: GIT_USER_AGENT, or
// "git/<version>" with every byte that is not printable ASCII, and every
// space, replaced by a dot.
func gitAgent(ctx context.Context) (string, error) {
	agent := os.Getenv("GIT_USER_AGENT")
	if agent == "" {
		out, err := gitOutput(ctx, nil, "version")
		if err != nil {
			return "", err
		}
		agent = "git/" + strings.TrimPrefix(strings.TrimSpace(string(out)), "git version ")
	}
	b := []byte(agent)
	for i, c := range b {
		if c <= ' ' || c >= 0x7f {
			b[i] = '.'
		}
	}
	return string(b), nil
}

// blobSource reads blobs from one `git cat-file --batch` process.
type blobSource struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
	// broken is set after a failed read: the position in the output is
	// unknown, so Close stops the process instead of waiting for it.
	broken bool
}

func openBlobSource(ctx context.Context, stderr io.Writer) (*blobSource, error) {
	cmd := gitCommand(ctx, "cat-file", "--batch")
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("cat-file stdin: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cat-file stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cat-file: %w", err)
	}
	return &blobSource{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 256<<10)}, nil
}

// read streams blob oid of size bytes to fn. It fails when the object is
// not a blob of that size.
func (s *blobSource) read(oid string, size int64, fn func(io.Reader) error) error {
	err := s.readBlob(oid, size, fn)
	if err != nil {
		s.broken = true
	}
	return err
}

func (s *blobSource) readBlob(oid string, size int64, fn func(io.Reader) error) error {
	if _, err := io.WriteString(s.in, oid+"\n"); err != nil {
		return fmt.Errorf("cat-file request: %w", err)
	}
	header, err := s.out.ReadString('\n')
	if err != nil {
		return fmt.Errorf("cat-file header of %s: %w", oid, err)
	}
	if want := fmt.Sprintf("%s blob %d\n", oid, size); header != want {
		return fmt.Errorf("cat-file answered %q for blob %s of %d bytes", strings.TrimSpace(header), oid, size)
	}
	content := io.LimitReader(s.out, size)
	ferr := fn(content)
	if _, err := io.Copy(io.Discard, content); err != nil {
		return errors.Join(ferr, fmt.Errorf("cat-file content of %s: %w", oid, err))
	}
	if ferr != nil {
		return ferr
	}
	if b, err := s.out.ReadByte(); err != nil || b != '\n' {
		return fmt.Errorf("cat-file content of %s does not end with a newline", oid)
	}
	return nil
}

func (s *blobSource) Close() error {
	_ = s.in.Close()
	if s.broken {
		killProcess(s.cmd)
	}
	if err := s.cmd.Wait(); err != nil {
		return fmt.Errorf("cat-file: %w", err)
	}
	return nil
}

// killProcess stops a git process whose output the caller no longer reads,
// so that Wait does not block on a full pipe.
func killProcess(cmd *exec.Cmd) {
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		debuglog.Printf("bbpush: kill git %s: %v", cmd.Args[1], err)
	}
}
