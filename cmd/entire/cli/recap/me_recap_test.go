package recap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestFetchMeRecap_ReturnsTypedUnauthorizedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := w.Write([]byte(`{"error":"Token expired"}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	t.Setenv(api.BaseURLEnvVar, server.URL)

	_, err := FetchMeRecap(
		context.Background(),
		api.NewClient("expired-token"),
		time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC),
		"",
		0,
	)
	if err == nil {
		t.Fatal("FetchMeRecap error = nil, want unauthorized error")
	}
	if !api.IsHTTPErrorStatus(err, http.StatusUnauthorized) {
		t.Fatalf("FetchMeRecap error = %v, want typed 401", err)
	}
	if !strings.Contains(err.Error(), "Token expired") {
		t.Fatalf("FetchMeRecap error = %v, want token-expired message", err)
	}
}
