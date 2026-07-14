package githelper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- fake transports for fault injection ----------------------------------

// fakeTransport is a Transport stub that returns user-supplied bodies
// and errors for each method. It captures every ServiceRPC body so
// tests can verify "what reached the wire" — the invariant we care
// about for push acknowledgement.
type fakeTransport struct {
	infoRefsResp   func() (io.ReadCloser, error)
	infoRefsV2Resp func() (io.ReadCloser, error)
	serviceRPCResp func(service string, body []byte) (io.ReadCloser, error)

	rpcCalls []rpcCall
}

type rpcCall struct {
	Service string
	Body    []byte
}

func (f *fakeTransport) InfoRefs(_ context.Context, _ string) (io.ReadCloser, error) {
	if f.infoRefsResp == nil {
		return nil, errors.New("fakeTransport: no info/refs handler")
	}
	return f.infoRefsResp()
}

func (f *fakeTransport) InfoRefsV2(_ context.Context) (io.ReadCloser, error) {
	if f.infoRefsV2Resp == nil {
		return nil, errors.New("fakeTransport: no v2 info/refs handler")
	}
	return f.infoRefsV2Resp()
}

func (f *fakeTransport) ServiceRPC(_ context.Context, service string, body io.ReadSeeker, _ ...func(*http.Request)) (io.ReadCloser, error) {
	var buf []byte
	if body != nil {
		_, _ = body.Seek(0, io.SeekStart) //nolint:errcheck // test
		buf, _ = io.ReadAll(body)         //nolint:errcheck // test
	}
	f.rpcCalls = append(f.rpcCalls, rpcCall{Service: service, Body: buf})
	if f.serviceRPCResp == nil {
		return nil, errors.New("fakeTransport: no service-RPC handler")
	}
	return f.serviceRPCResp(service, buf)
}

func (f *fakeTransport) ErrorBaseURL() string { return "https://test.invalid/repo" }

// stringRC wraps a string as a ReadCloser.
func stringRC(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

// ---- helpers --------------------------------------------------------------

// serviceAnnouncement returns the smart-HTTP info/refs body for the
// given service: announcement pkt-line, flush, then refLines, then
// trailing flush.
func serviceAnnouncement(service string, refLines ...string) string {
	var b strings.Builder
	b.WriteString(pktLine("# service=" + service + "\n"))
	b.WriteString("0000")
	for _, r := range refLines {
		b.WriteString(pktLine(r))
	}
	b.WriteString("0000")
	return b.String()
}

// ---- fault-injection tests -----------------------------------------------

// TestHandleStatelessConnect_FallbackOnNonV2: a non-v2
// advertisement (e.g. plain HTTP error masked as 200 with garbage)
// must end in "fallback\n" without issuing any POST. The remote
// helper contract is that we explicitly say "fallback" instead of
// claiming we can speak the version.
func TestHandleStatelessConnect_FallbackOnNonV2(t *testing.T) {
	t.Parallel()
	ft := &fakeTransport{
		infoRefsV2Resp: func() (io.ReadCloser, error) {
			return stringRC(pktLine("version 1\n") + "0000"), nil
		},
	}
	var out bytes.Buffer
	if err := handleStatelessConnect(context.Background(), ft, serviceUploadPack, strings.NewReader(""), &out); err != nil {
		t.Fatalf("handleStatelessConnect: %v", err)
	}
	if out.String() != "fallback\n" {
		t.Errorf("output = %q, want fallback\\n", out.String())
	}
	if len(ft.rpcCalls) != 0 {
		t.Errorf("ServiceRPC called %d times on fallback; must be zero", len(ft.rpcCalls))
	}
}

// TestHandleStatelessConnect_InfoRefsErrorSurfacesNoOutput: a failure
// at the info/refs probe must error without writing anything to
// stdout — there is no half-advertisement state that git can
// proceed from.
func TestHandleStatelessConnect_InfoRefsErrorSurfacesNoOutput(t *testing.T) {
	t.Parallel()
	ft := &fakeTransport{
		infoRefsV2Resp: func() (io.ReadCloser, error) {
			return nil, errors.New("simulated server failure")
		},
	}
	var out bytes.Buffer
	err := handleStatelessConnect(context.Background(), ft, serviceUploadPack, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if out.Len() != 0 {
		t.Errorf("expected empty stdout on error, got %q", out.String())
	}
}

// TestHandleConnect_TruncatedReceivePackRequestErrors: a
// receive-pack POST body that has command pkt-lines but no
// terminating flush must NOT be forwarded as-is. ReadReceivePackRequest
// surfaces the truncation, handleConnect refuses to POST a partial
// request — otherwise the server would either reject or, worse,
// accept it as a valid empty-update batch.
func TestHandleConnect_TruncatedReceivePackRequestErrors(t *testing.T) {
	t.Parallel()
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain

	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldSHA+" "+ref+"\x00 report-status\n")), nil
		},
	}
	// Command pkt-line, then EOF — no flush.
	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	stdin := strings.NewReader(pktLine(cmd))

	var out bytes.Buffer
	err := handleConnect(context.Background(), ft, serviceReceivePack, stdin, &out)
	if err == nil {
		t.Fatal("expected error on truncated request")
	}
	if len(ft.rpcCalls) != 0 {
		t.Errorf("ServiceRPC called %d times despite truncation — must be zero", len(ft.rpcCalls))
	}
}

// TestHandleConnect_EmptyReceivePackRequestNoPOST: when git closes
// stdin before sending any command, we must NOT issue a POST. An
// empty receive-pack POST is technically valid and would be
// acknowledged by some servers as "nothing changed" — but git's
// transport layer expects no network traffic at all in that case.
func TestHandleConnect_EmptyReceivePackRequestNoPOST(t *testing.T) {
	t.Parallel()
	oldSHA := strings.Repeat("a", 40)
	ref := testRefMain

	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldSHA+" "+ref+"\x00 report-status\n")), nil
		},
	}
	var out bytes.Buffer
	if err := handleConnect(context.Background(), ft, serviceReceivePack, strings.NewReader(""), &out); err != nil {
		t.Fatalf("handleConnect: %v", err)
	}
	if len(ft.rpcCalls) != 0 {
		t.Errorf("ServiceRPC called %d times for empty stdin — must be zero", len(ft.rpcCalls))
	}
}

// TestHandleConnect_ServiceRPCFailurePropagates: an HTTP failure on
// the receive-pack POST must surface a non-nil error to the caller.
// The helper must NOT swallow the error and emit a fake "ok" line —
// that would tell git the push succeeded when in fact nothing
// reached the server.
func TestHandleConnect_ServiceRPCFailurePropagates(t *testing.T) {
	t.Parallel()
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain

	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldSHA+" "+ref+"\x00 report-status\n")), nil
		},
		serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
			return nil, errors.New("502 Bad Gateway")
		},
	}
	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	var stdin bytes.Buffer
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")
	stdin.WriteString("PACK fake")

	var out bytes.Buffer
	err := handleConnect(context.Background(), ft, serviceReceivePack, &stdin, &out)
	if err == nil {
		t.Fatal("expected error to propagate to caller")
	}
	if strings.Contains(out.String(), "ok ") {
		t.Errorf("output contains spurious 'ok': %q", out.String())
	}
}

// flakyBody wraps an io.Reader and returns ErrUnexpectedEOF after
// allowing some bytes through. Used to simulate a server closing the
// HTTP response stream mid-pack.
type flakyBody struct {
	src    io.Reader
	limit  int
	served int
}

func (f *flakyBody) Read(p []byte) (int, error) {
	if f.served >= f.limit {
		return 0, io.ErrUnexpectedEOF
	}
	remaining := f.limit - f.served
	if remaining < len(p) {
		p = p[:remaining]
	}
	n, err := f.src.Read(p)
	f.served += n
	return n, err
}

func (f *flakyBody) Close() error { return nil }

// TestHandleConnect_TruncatedResponseSurfacesError: when the server's
// response stream is cut short mid-stream, the io.Copy to stdout
// errors and handleConnect must return that error. Otherwise git
// would see a partial pack response and could mis-report success.
func TestHandleConnect_TruncatedResponseSurfacesError(t *testing.T) {
	t.Parallel()
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain

	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldSHA+" "+ref+"\x00 report-status\n")), nil
		},
		serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
			// 1KB available, ten bytes of garbage, then ErrUnexpectedEOF.
			return &flakyBody{src: strings.NewReader(strings.Repeat("x", 1024)), limit: 10}, nil
		},
	}
	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	var stdin bytes.Buffer
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")
	stdin.WriteString("PACK fake")

	var out bytes.Buffer
	err := handleConnect(context.Background(), ft, serviceReceivePack, &stdin, &out)
	if err == nil {
		t.Fatal("expected error on truncated response")
	}
}

// ---- HTTP-level fault injection ------------------------------------------

// TestHandleConnect_Real5xxNoSpuriousAck: against a real HTTP server
// that 502s on every POST, the helper must surface the failure and
// emit no successful-push markers.
func TestHandleConnect_Real5xxNoSpuriousAck(t *testing.T) {
	t.Parallel()
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			_, _ = w.Write([]byte(serviceAnnouncement(serviceReceivePack, //nolint:errcheck // test
				oldSHA+" "+ref+"\x00 report-status\n")))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, serviceReceivePack):
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "upstream dead")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	var stdin bytes.Buffer
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")
	stdin.WriteString("PACK fake")

	var out bytes.Buffer
	err := handleConnect(context.Background(), testTransport(server), serviceReceivePack, &stdin, &out)
	if err == nil {
		t.Fatal("expected error on 502")
	}
	for _, marker := range []string{"unpack ok", "\nok ", "ok refs/"} {
		if strings.Contains(out.String(), marker) {
			t.Errorf("stdout contains spurious success marker %q: %s", marker, out.String())
		}
	}
}

// TestHandleConnect_ServerHangsTimesOutViaCtx: a server that holds
// the response open forever must NOT leave the helper blocked once
// the context cancels. The Transport interface promises to surface
// ctx cancellation, so handleConnect just has to thread it through.
//
// Cleanup order matters here: the hanging handler can't return until
// `hold` closes, and httptest.Server.Close blocks on all in-flight
// requests. We therefore close `hold` first via the early defer
// (LIFO: registered last, runs first) so server.Close has a chance
// to return.
func TestHandleConnect_ServerHangsTimesOutViaCtx(t *testing.T) {
	t.Parallel()
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain

	hold := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			_, _ = w.Write([]byte(serviceAnnouncement(serviceReceivePack, //nolint:errcheck // test
				oldSHA+" "+ref+"\x00 report-status\n")))
		default:
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}
	}))
	defer server.Close()
	defer close(hold) // runs before server.Close — unblocks the handler

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	var stdin bytes.Buffer
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")
	stdin.WriteString("PACK fake")

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- handleConnect(ctx, testTransport(server), serviceReceivePack, &stdin, &out)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected error on ctx cancel")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not return after ctx timeout")
	}
}
