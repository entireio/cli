package githelper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/remotehelper/replicas"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

// testTransport builds a transport.Proxy pointed at a single test
// server for /et/owner/repo.
func testTransport(server *httptest.Server) *transport.Proxy {
	return transport.New(transport.Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: []string{server.URL},
			EntryURL:     server.URL,
			ClusterHost:  hostOf(server.URL),
			RepoPath:     "owner/repo",
		},
		Path: "/et/owner/repo",
	})
}

func hostOf(u string) string {
	// Strip scheme.
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexByte(u, ':'); i >= 0 {
		u = u[:i]
	}
	return u
}

// pktLine encodes a string as a git pkt-line.
func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func TestRun_Capabilities(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode Mode
		want string
	}{
		{"default", ModeConnect, "connect\noption\n\n"},
		{"stateless", ModeStateless, "stateless-connect\npush\noption\n\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			defer server.Close()
			stdin := strings.NewReader("capabilities\n\n")
			var stdout bytes.Buffer
			if err := Run(context.Background(), testTransport(server), tt.mode, stdin, &stdout); err != nil {
				t.Fatalf("Run failed: %v", err)
			}
			if stdout.String() != tt.want {
				t.Errorf("output = %q, want %q", stdout.String(), tt.want)
			}
		})
	}
}

func TestRun_OptionCommandReplies(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	stdin := strings.NewReader("option dry-run true\noption weird true\n\n")
	var stdout bytes.Buffer
	if err := Run(context.Background(), testTransport(server), ModeConnect, stdin, &stdout); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	got := stdout.String()
	if !strings.Contains(got, "ok\n") {
		t.Errorf("missing ok reply: %q", got)
	}
	if !strings.Contains(got, "unsupported\n") {
		t.Errorf("missing unsupported reply: %q", got)
	}
}

// TestRun_ConnectUploadPackUsesV0: in connect mode, git sends `connect
// git-upload-pack` and expects a v0/v1 bidirectional dialogue: refs
// advertisement, then wants + flush, then alternating have/ACK rounds
// terminated by `done`.
func TestRun_ConnectUploadPackUsesV0(t *testing.T) {
	t.Parallel()
	refsAdvertisement := pktLine("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\x00multi_ack agent=test\n") + "0000"
	packResponse := "0008NAK\nPACK..."

	var posts [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			if r.URL.Query().Get("service") != serviceUploadPack {
				t.Errorf("service = %s", r.URL.Query().Get("service"))
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, pktLine("# service=git-upload-pack\n")+"0000"+refsAdvertisement)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, serviceUploadPack):
			body, _ := io.ReadAll(r.Body) //nolint:errcheck // test
			posts = append(posts, body)
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			fmt.Fprint(w, packResponse)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	wantPkt := pktLine("want aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa multi_ack\n")
	donePkt := pktLine("done\n")
	stdin := strings.NewReader("connect git-upload-pack\n" + wantPkt + "0000" + donePkt)
	var stdout bytes.Buffer

	if err := Run(context.Background(), testTransport(server), ModeConnect, stdin, &stdout); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	wantOut := "\n" + refsAdvertisement + packResponse
	if stdout.String() != wantOut {
		t.Errorf("stdout = %q, want %q", stdout.String(), wantOut)
	}
	if len(posts) != 1 {
		t.Fatalf("expected 1 POST (final round with done), got %d", len(posts))
	}
	wantBody := wantPkt + "0000" + donePkt
	if string(posts[0]) != wantBody {
		t.Errorf("POST body = %q, want %q", posts[0], wantBody)
	}
}
