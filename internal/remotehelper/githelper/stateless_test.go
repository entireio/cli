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
)

func TestHandleStatelessConnect_UploadPack(t *testing.T) {
	t.Parallel()
	advertisement := pktLine("version 2\n") + pktLine("agent=test\n") + pktLine("ls-refs=unborn\n") + "0000"
	requestBody := pktLine("command=ls-refs\n") + "0000"
	responseBody := pktLine("refs/heads/main HEAD\n") + "0000"

	var posts [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			if r.URL.Query().Get("service") != serviceUploadPack {
				t.Errorf("service = %q", r.URL.Query().Get("service"))
			}
			if r.Header.Get("Git-Protocol") != "version=2" {
				t.Errorf("Git-Protocol = %q", r.Header.Get("Git-Protocol"))
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, advertisement)

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, serviceUploadPack):
			if r.Header.Get("Git-Protocol") != "version=2" {
				t.Errorf("Git-Protocol = %q", r.Header.Get("Git-Protocol"))
			}
			body, _ := io.ReadAll(r.Body) //nolint:errcheck // test
			posts = append(posts, body)
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			fmt.Fprint(w, responseBody)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	input := strings.NewReader(requestBody)

	var stdout bytes.Buffer
	if err := handleStatelessConnect(context.Background(), testTransport(server), serviceUploadPack, input, &stdout); err != nil {
		t.Fatalf("handleStatelessConnect failed: %v", err)
	}

	if len(posts) != 1 {
		t.Fatalf("POST count = %d, want 1", len(posts))
	}
	if string(posts[0]) != requestBody {
		t.Errorf("POST body = %q, want %q", posts[0], requestBody)
	}

	want := "\n" + advertisement + responseBody + "0002"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestHandleStatelessConnect_AmendsClientAgent(t *testing.T) {
	t.Parallel()
	advertisement := pktLine("version 2\n") + pktLine("agent=test\n") + pktLine("ls-refs=unborn\n") + "0000"
	requestBody := pktLine("command=ls-refs\n") + pktLine("agent=git/2.54.0\n") + "0000"
	wantBody := pktLine("command=ls-refs\n") +
		pktLine("agent=git/2.54.0 "+Agent+"\n") +
		"0000"

	var post []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, advertisement)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, serviceUploadPack):
			post, _ = io.ReadAll(r.Body) //nolint:errcheck // test
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			fmt.Fprint(w, "0000")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	if err := handleStatelessConnect(context.Background(), testTransport(server), serviceUploadPack, strings.NewReader(requestBody), &stdout); err != nil {
		t.Fatalf("handleStatelessConnect failed: %v", err)
	}
	if string(post) != wantBody {
		t.Errorf("POST body = %q, want %q", post, wantBody)
	}
}

func TestHandleStatelessConnect_EmptyBundleURIResponseSynthesizesFlush(t *testing.T) {
	t.Parallel()
	advertisement := pktLine("version 2\n") + pktLine("agent=test\n") + pktLine("bundle-uri\n") + "0000"
	requestBody := pktLine("command=bundle-uri\n") + "0000"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, advertisement)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, serviceUploadPack):
			body, _ := io.ReadAll(r.Body) //nolint:errcheck // test
			if string(body) != requestBody {
				t.Errorf("POST body = %q, want %q", body, requestBody)
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	if err := handleStatelessConnect(context.Background(), testTransport(server), serviceUploadPack, strings.NewReader(requestBody), &stdout); err != nil {
		t.Fatalf("handleStatelessConnect failed: %v", err)
	}

	want := "\n" + advertisement + "00000002"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestHandleStatelessConnect_FallbackForUnsupportedService(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	if err := handleStatelessConnect(context.Background(), nil, "git-archive", strings.NewReader(""), &stdout); err != nil {
		t.Fatalf("handleStatelessConnect failed: %v", err)
	}
	if stdout.String() != "fallback\n" {
		t.Errorf("stdout = %q, want fallback", stdout.String())
	}
}

func TestHandleStatelessConnect_ReceivePack(t *testing.T) {
	t.Parallel()
	oldSHA := testHeadSHA
	newSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ref := testRefMain
	packData := "PACK fake pack data here"

	refLine := oldSHA + " " + ref + "\x00 report-status\n"
	server, receivedPushSize := receivePackServer(t, refLine, ref)
	defer server.Close()

	var stdin bytes.Buffer
	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")
	stdin.WriteString(packData)

	var stdout bytes.Buffer
	if err := handleStatelessConnect(context.Background(), testTransport(server), serviceReceivePack, &stdin, &stdout); err != nil {
		t.Fatalf("handleStatelessConnect failed: %v", err)
	}

	if !strings.HasPrefix(stdout.String(), "\n") {
		t.Errorf("stdout should start with stateless-connect success line, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "unpack ok") {
		t.Errorf("expected receive-pack response in stdout, got %q", stdout.String())
	}
	if *receivedPushSize == "" {
		t.Fatal("X-Entire-Push-Size header not sent")
	}
}
