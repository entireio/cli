package githelper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestHandleConnect_DeleteBranch(t *testing.T) {
	t.Parallel()
	oldSHA := testHeadSHA
	zeroSHA := "0000000000000000000000000000000000000000"
	ref := testRefFeatureBranch

	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			announcement := "# service=git-receive-pack\n"
			fmt.Fprintf(w, "%04x%s", len(announcement)+4, announcement)
			fmt.Fprint(w, "0000")
			refLine := oldSHA + " " + ref + "\x00 report-status delete-refs\n"
			fmt.Fprintf(w, "%04x%s", len(refLine)+4, refLine)
			fmt.Fprint(w, "0000")

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, serviceReceivePack):
			receivedBody, _ = io.ReadAll(r.Body) //nolint:errcheck // test
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			fmt.Fprint(w, pktLine("unpack ok\n"))
			fmt.Fprint(w, pktLine("ok "+ref+"\n"))
			fmt.Fprint(w, "0000")

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	var stdin bytes.Buffer
	cmd := oldSHA + " " + zeroSHA + " " + ref + "\x00 report-status delete-refs\n"
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")

	var stdout bytes.Buffer
	err := handleConnect(context.Background(), testTransport(server), serviceReceivePack, &stdin, &stdout)
	if err != nil {
		t.Fatalf("handleConnect failed: %v", err)
	}

	body := string(receivedBody)
	if !strings.Contains(body, oldSHA) {
		t.Errorf("server body missing old SHA")
	}
	if !strings.Contains(body, zeroSHA) {
		t.Errorf("server body missing zero SHA")
	}
	if !strings.Contains(body, ref) {
		t.Errorf("server body missing ref name")
	}

	output := stdout.String()
	if !strings.Contains(output, "unpack ok") {
		t.Errorf("expected 'unpack ok' in output, got %q", output)
	}
}

// receivePackServer returns an httptest.Server that handles info/refs
// and git-receive-pack. It captures the X-Entire-Push-Size header
// from the POST request into the returned pointer.
func receivePackServer(t *testing.T, refLine, ref string) (*httptest.Server, *string) {
	t.Helper()
	var receivedPushSize string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			announcement := "# service=git-receive-pack\n"
			fmt.Fprintf(w, "%04x%s", len(announcement)+4, announcement)
			fmt.Fprint(w, "0000")
			fmt.Fprintf(w, "%04x%s", len(refLine)+4, refLine)
			fmt.Fprint(w, "0000")

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, serviceReceivePack):
			receivedPushSize = r.Header.Get("X-Entire-Push-Size")
			_, _ = io.ReadAll(r.Body) //nolint:errcheck // test
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			fmt.Fprint(w, pktLine("unpack ok\n"))
			fmt.Fprint(w, pktLine("ok "+ref+"\n"))
			fmt.Fprint(w, "0000")

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return server, &receivedPushSize
}

func TestHandleConnect_PushSizeHeader(t *testing.T) {
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

	expectedSize := len(pktLine(cmd)) + 4 + len(packData)

	var stdout bytes.Buffer
	err := handleConnect(context.Background(), testTransport(server), serviceReceivePack, &stdin, &stdout)
	if err != nil {
		t.Fatalf("handleConnect failed: %v", err)
	}

	if *receivedPushSize == "" {
		t.Fatal("X-Entire-Push-Size header not sent")
	}
	want := strconv.Itoa(expectedSize)
	if *receivedPushSize != want {
		t.Errorf("X-Entire-Push-Size = %s, want %s", *receivedPushSize, want)
	}
}

func TestHandleConnect_DeletePushSizeHeader(t *testing.T) {
	t.Parallel()
	oldSHA := testHeadSHA
	zeroSHA := "0000000000000000000000000000000000000000"
	ref := testRefFeatureBranch

	refLine := oldSHA + " " + ref + "\x00 report-status delete-refs\n"
	server, receivedPushSize := receivePackServer(t, refLine, ref)
	defer server.Close()

	var stdin bytes.Buffer
	cmd := oldSHA + " " + zeroSHA + " " + ref + "\x00 report-status delete-refs\n"
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")

	expectedSize := strconv.Itoa(len(pktLine(cmd)) + 4)

	var stdout bytes.Buffer
	err := handleConnect(context.Background(), testTransport(server), serviceReceivePack, &stdin, &stdout)
	if err != nil {
		t.Fatalf("handleConnect failed: %v", err)
	}

	if *receivedPushSize != expectedSize {
		t.Errorf("X-Entire-Push-Size = %s, want %s", *receivedPushSize, expectedSize)
	}
}
