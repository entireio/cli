package githelper

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleList(t *testing.T) {
	t.Parallel()

	const (
		headSHA = testHeadSHA
		mainSHA = testHeadSHA
		fooSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	tests := []struct {
		name     string
		forPush  bool
		service  string
		firstRef string
		more     []string
		want     string
	}{
		{
			name:     "list with HEAD symref",
			service:  serviceUploadPack,
			firstRef: headSHA + " HEAD\x00multi_ack symref=HEAD:refs/heads/main object-format=sha1\n",
			more: []string{
				mainSHA + " refs/heads/main\n",
				fooSHA + " refs/heads/foo\n",
			},
			want: ":object-format sha1\n" +
				mainSHA + " refs/heads/main\n" +
				fooSHA + " refs/heads/foo\n" +
				"@refs/heads/main HEAD\n\n",
		},
		{
			name:     "list for-push hits receive-pack",
			forPush:  true,
			service:  serviceReceivePack,
			firstRef: mainSHA + " refs/heads/main\x00report-status delete-refs object-format=sha1\n",
			more: []string{
				fooSHA + " refs/heads/foo\n",
			},
			want: ":object-format sha1\n" +
				mainSHA + " refs/heads/main\n" +
				fooSHA + " refs/heads/foo\n\n",
		},
		{
			name:     "detached HEAD",
			service:  serviceUploadPack,
			firstRef: headSHA + " HEAD\x00multi_ack object-format=sha1\n",
			more: []string{
				mainSHA + " refs/heads/main\n",
			},
			want: ":object-format sha1\n" +
				mainSHA + " refs/heads/main\n" +
				headSHA + " HEAD\n\n",
		},
		{
			name:     "multiple symrefs",
			service:  serviceUploadPack,
			firstRef: headSHA + " HEAD\x00multi_ack symref=HEAD:refs/heads/main symref=refs/remotes/origin/HEAD:refs/remotes/origin/main object-format=sha1\n",
			more: []string{
				mainSHA + " refs/heads/main\n",
				fooSHA + " refs/remotes/origin/HEAD\n",
				fooSHA + " refs/remotes/origin/main\n",
			},
			want: ":object-format sha1\n" +
				mainSHA + " refs/heads/main\n" +
				"@refs/remotes/origin/main refs/remotes/origin/HEAD\n" +
				fooSHA + " refs/remotes/origin/main\n" +
				"@refs/heads/main HEAD\n\n",
		},
		{
			name:     "empty repo with unborn HEAD",
			service:  serviceUploadPack,
			firstRef: "0000000000000000000000000000000000000000 capabilities^{}\x00symref=HEAD:refs/heads/main object-format=sha1 multi_ack\n",
			more:     nil,
			want: ":object-format sha1\n" +
				"@refs/heads/main HEAD\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("service"); got != tt.service {
					t.Errorf("service = %s, want %s", got, tt.service)
				}
				var b bytes.Buffer
				b.WriteString(pktLine("# service=" + tt.service + "\n"))
				b.WriteString("0000")
				b.WriteString(pktLine(tt.firstRef))
				for _, ref := range tt.more {
					b.WriteString(pktLine(ref))
				}
				b.WriteString("0000")
				w.Header().Set("Content-Type", "application/x-"+tt.service+"-advertisement")
				_, _ = w.Write(b.Bytes()) //nolint:errcheck // test
			}))
			defer server.Close()

			var out bytes.Buffer
			if err := handleList(context.Background(), testTransport(server), tt.forPush, &out); err != nil {
				t.Fatalf("handleList: %v", err)
			}
			if out.String() != tt.want {
				t.Errorf("got %q, want %q", out.String(), tt.want)
			}
		})
	}
}
