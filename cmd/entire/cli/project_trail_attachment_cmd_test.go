package cli

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const trailAttachmentTestID = "01ARZ3NDEKTSV4RRFFQ69G5ATT"

// writeTestPNG writes a w×h PNG named name into a temp dir and returns its path.
func writeTestPNG(t *testing.T, name string, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))))
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

type trailAttachmentTestServer struct {
	// storageStatus answers the PUT when set; expiresAt overrides the
	// upload URL's expiry.
	storageStatus int
	expiresAt     string
	// completeStatus fails /complete when set; listing then answers it.
	completeStatus int
	requests       []string
	intent         map[string]any
	idemKey        string
	uploaded       []byte
	uploadType     string
	uploadAuth     string
}

// serveTrailAttachments fakes the cell's attachment routes on project trail 42
// and a storage server for the presigned PUT.
func serveTrailAttachments(t *testing.T, listing []map[string]any) *trailAttachmentTestServer {
	t.Helper()
	s := &trailAttachmentTestServer{}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests = append(s.requests, "storage "+r.Method)
		s.uploadType, s.uploadAuth = r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		var err error
		s.uploaded, err = io.ReadAll(r.Body)
		assert.NoError(t, err)
		if s.storageStatus != 0 {
			w.WriteHeader(s.storageStatus)
		}
	}))
	t.Cleanup(storage.Close)
	collection := projectTrailTestPath + "/attachments"
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == projectTrailTestPath:
			assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			return
		case strings.HasPrefix(r.URL.Path, collection):
			s.requests = append(s.requests, r.Method+" "+strings.TrimPrefix(r.URL.Path, projectTrailTestPath))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == collection:
			s.idemKey = r.Header.Get("Idempotency-Key")
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&s.intent))
			expires := s.expiresAt
			if expires == "" {
				expires = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			}
			w.WriteHeader(http.StatusCreated)
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": trailAttachmentTestID,
				"upload": map[string]any{"url": storage.URL + "/put?sig=x", "expires_at": expires, "max_bytes": 10485760}}))
		case r.Method == http.MethodPost && r.URL.Path == collection+"/"+trailAttachmentTestID+"/complete" && s.completeStatus != 0:
			http.Error(w, `{"error":"gateway timeout"}`, s.completeStatus)
		case r.Method == http.MethodPost && r.URL.Path == collection+"/"+trailAttachmentTestID+"/complete":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"attachment": map[string]any{"id": trailAttachmentTestID, "filename": "shot.png",
				"content_type": "image/png", "size_bytes": len(s.uploaded), "is_ready": true}}))
		case r.Method == http.MethodGet && r.URL.Path == collection:
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"attachments": listing}))
		case r.Method == http.MethodDelete && r.URL.Path == collection+"/"+trailAttachmentTestID:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected attachment request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	return s
}

func trailAttachmentArgs(args ...string) []string {
	return append(append([]string{"attachment"}, args...), "--trail", projectTrailTestID, "--project", "gh/acme")
}

func TestTrailAttachmentAdd_UploadsAndPrintsTheReference(t *testing.T) {
	// Serial: replaces global clients.
	s := serveTrailAttachments(t, nil)
	path := writeTestPNG(t, "shot.png", 3, 2)
	want, err := os.ReadFile(path)
	require.NoError(t, err)

	out, _, err := executeProjectTrailTest(t, trailAttachmentArgs("add", path)...)
	require.NoError(t, err)
	assert.Equal(t, []string{"POST /attachments", "storage PUT", "POST /attachments/" + trailAttachmentTestID + "/complete"}, s.requests)
	assert.Equal(t, map[string]any{"filename": "shot.png", "content_type": "image/png", "width": float64(3), "height": float64(2)}, s.intent)
	assert.NotEmpty(t, s.idemKey)
	assert.Equal(t, want, s.uploaded)
	assert.Equal(t, "image/png", s.uploadType, "the declared type")
	assert.Empty(t, s.uploadAuth, "the presigned URL never gets the caller's token")
	assert.Contains(t, out, "![image](attachment:"+trailAttachmentTestID+")")

	out, _, err = executeProjectTrailTest(t, trailAttachmentArgs("add", path, "--json")...)
	require.NoError(t, err)
	var got struct {
		Attachment struct {
			ID string `json:"id"`
		} `json:"attachment"`
		Markdown string `json:"markdown"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, trailAttachmentTestID, got.Attachment.ID)
	assert.Equal(t, "![image](attachment:"+trailAttachmentTestID+")", got.Markdown)
}

func TestTrailAttachmentAdd_RefusesFilesBeforeAnyRequest(t *testing.T) {
	// Serial: replaces global clients.
	s := serveTrailAttachments(t, nil)
	dir := t.TempDir()
	svg := filepath.Join(dir, "logo.png")
	require.NoError(t, os.WriteFile(svg, []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), 0o600))
	empty := filepath.Join(dir, "empty.png")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	big := filepath.Join(dir, "big.png")
	require.NoError(t, os.WriteFile(big, make([]byte, trailAttachmentMaxBytes+1), 0o600))

	for path, says := range map[string]string{
		svg:                           "only PNG, JPEG, GIF, and WebP images",
		empty:                         "is empty",
		big:                           "limited to",
		dir:                           "not a regular file",
		filepath.Join(dir, "missing"): "no such file",
	} {
		_, _, err := executeProjectTrailTest(t, trailAttachmentArgs("add", path)...)
		require.ErrorContains(t, err, says, path)
	}
	assert.Empty(t, s.requests, "a refused file registers nothing")
}

func TestTrailAttachmentAdd_RefusesAPlaintextUploadURL(t *testing.T) {
	t.Parallel()
	err := putTrailAttachmentBytes(t.Context(), "http://storage.example/put", trailAttachmentFile{data: []byte("x"), contentType: "image/png"})
	require.ErrorContains(t, err, "refusing a non-HTTPS upload URL")
}

func TestTrailAttachmentList(t *testing.T) {
	// Serial: replaces global clients.
	serveTrailAttachments(t, []map[string]any{
		{"id": trailAttachmentTestID, "filename": "shot.png", "content_type": "image/png", "size_bytes": 1234, "is_ready": true},
		{"id": "01ARZ3NDEKTSV4RRFFQ69G5ATB", "filename": "half.png", "content_type": "image/png", "size_bytes": 0, "is_ready": false},
	})
	out, _, err := executeProjectTrailTest(t, trailAttachmentArgs("list")...)
	require.NoError(t, err)
	assert.Regexp(t, trailAttachmentTestID+`\s+shot.png\s+image/png\s+1234\s+true\s+!\[image\]\(attachment:`+trailAttachmentTestID+`\)`, out)
	assert.Regexp(t, `half.png\s+image/png\s+0\s+false\s*\n`, out, "an unfinished upload has no reference")

	out, _, err = executeProjectTrailTest(t, trailAttachmentArgs("list", "--json")...)
	require.NoError(t, err)
	assert.Contains(t, out, `"attachments"`)
}

func TestTrailAttachmentRemove(t *testing.T) {
	// Serial: replaces global clients.
	s := serveTrailAttachments(t, nil)
	_, _, err := executeProjectTrailTest(t, trailAttachmentArgs("remove", trailAttachmentTestID)...)
	require.ErrorContains(t, err, "requires --force")
	for _, id := range []string{"", ".", "..", "shot.png"} {
		_, _, err := executeProjectTrailTest(t, trailAttachmentArgs("remove", id, "--force")...)
		require.ErrorContains(t, err, "expected an attachment ID", "id %q", id)
	}
	assert.Empty(t, s.requests)

	out, _, err := executeProjectTrailTest(t, trailAttachmentArgs("remove", trailAttachmentTestID, "--force")...)
	require.NoError(t, err)
	assert.Equal(t, []string{"DELETE /attachments/" + trailAttachmentTestID}, s.requests)
	assert.Contains(t, out, "Deleted attachment "+trailAttachmentTestID)
}

func TestTrailAttachmentAdd_RemovesTheRegisteredAttachmentWhenTheUploadFails(t *testing.T) {
	// Serial: replaces global clients.
	path := writeTestPNG(t, "shot.png", 3, 2)
	for name, setup := range map[string]func(*trailAttachmentTestServer){
		"storage refuses": func(s *trailAttachmentTestServer) { s.storageStatus = http.StatusForbidden },
	} {
		s := serveTrailAttachments(t, nil)
		setup(s)
		_, _, err := executeProjectTrailTest(t, trailAttachmentArgs("add", path)...)
		require.Error(t, err, name)
		assert.Equal(t, "DELETE /attachments/"+trailAttachmentTestID, s.requests[len(s.requests)-1], "%s: the unfinished attachment is removed", name)
		assert.NotContains(t, s.requests, "POST /attachments/"+trailAttachmentTestID+"/complete", name)
	}
}

func TestTrailAttachmentAdd_LeavesTheUploadURLExpiryToStorage(t *testing.T) {
	// Serial: replaces global clients. An expiry already past by the local
	// clock is still tried: the clock may run ahead of the server's.
	s := serveTrailAttachments(t, nil)
	s.expiresAt = "2020-01-01T00:00:00Z"
	_, _, err := executeProjectTrailTest(t, trailAttachmentArgs("add", writeTestPNG(t, "shot.png", 3, 2))...)
	require.NoError(t, err)
	assert.Contains(t, s.requests, "storage PUT")
}

func TestTrailAttachmentAdd_AFailedCompletionNeverDeletesAFinishedImage(t *testing.T) {
	// Serial: replaces global clients.
	path := writeTestPNG(t, "shot.png", 3, 2)
	ready := map[string]any{"id": trailAttachmentTestID, "filename": "shot.png", "content_type": "image/png", "is_ready": true}
	notReady := map[string]any{"id": trailAttachmentTestID, "filename": "shot.png", "content_type": "image/png", "is_ready": false}

	// The server finished it before the response was lost: success, kept.
	s := serveTrailAttachments(t, []map[string]any{ready})
	s.completeStatus = http.StatusGatewayTimeout
	out, _, err := executeProjectTrailTest(t, trailAttachmentArgs("add", path)...)
	require.NoError(t, err)
	assert.Contains(t, out, "![image](attachment:"+trailAttachmentTestID+")")
	assert.NotContains(t, s.requests, "DELETE /attachments/"+trailAttachmentTestID)

	// Still unfinished: removed, and the failure reported.
	s = serveTrailAttachments(t, []map[string]any{notReady})
	s.completeStatus = http.StatusGatewayTimeout
	_, _, err = executeProjectTrailTest(t, trailAttachmentArgs("add", path)...)
	require.ErrorContains(t, err, "complete attachment")
	assert.Equal(t, "DELETE /attachments/"+trailAttachmentTestID, s.requests[len(s.requests)-1])
}
