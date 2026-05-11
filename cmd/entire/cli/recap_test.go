package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/recap"
)

const recapTestAgentCodex = "codex"

func TestRecapFlags_RangeKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		flags recapFlags
		want  recap.RangeKey
	}{
		{"default_day", recapFlags{}, recap.RangeDay},
		{"day", recapFlags{day: true}, recap.RangeDay},
		{"week", recapFlags{week: true}, recap.RangeWeek},
		{"month", recapFlags{month: true}, recap.RangeMonth},
		{"90d", recapFlags{d90: true}, recap.Range90d},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.flags.rangeKey(); got != c.want {
				t.Errorf("rangeKey() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRecapFlags_Mode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		view string
		want recap.ViewMode
	}{
		{"", recap.ViewBoth},
		{"both", recap.ViewBoth},
		{"you", recap.ViewYou},
		{"me", recap.ViewYou},
		{"team", recap.ViewTeam},
		{"contributors", recap.ViewTeam},
	}
	for _, c := range cases {
		t.Run(c.view, func(t *testing.T) {
			t.Parallel()
			if got := (&recapFlags{view: c.view}).mode(); got != c.want {
				t.Errorf("mode() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRecapCmd_RegistersStaticFlags(t *testing.T) {
	t.Parallel()
	cmd := newRecapCmd()
	for _, name := range []string{"day", "week", "month", "90", "agent", "view", "color", "static", "insecure-http-auth"} {
		if flag := cmd.Flag(name); flag == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

func TestRecapFlags_UseTUI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		flags      recapFlags
		terminal   bool
		canPrompt  bool
		accessible bool
		want       bool
	}{
		{name: "terminal default", terminal: true, canPrompt: true, want: true},
		{name: "non terminal static", terminal: false, canPrompt: true, want: false},
		{name: "cannot prompt static", terminal: true, canPrompt: false, want: false},
		{name: "static flag", flags: recapFlags{static: true}, terminal: true, canPrompt: true, want: false},
		{name: "accessible static", terminal: true, canPrompt: true, accessible: true, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.flags.useTUI(c.terminal, c.canPrompt, c.accessible); got != c.want {
				t.Fatalf("useTUI() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRecapFlags_AgentName(t *testing.T) {
	t.Parallel()
	if got := (&recapFlags{}).agentName(); got != recap.AgentAll {
		t.Errorf("default agent = %q, want all", got)
	}
	if got := (&recapFlags{agent: " Codex "}).agentName(); got != recapTestAgentCodex {
		t.Errorf("agent = %q, want %s", got, recapTestAgentCodex)
	}
}

func TestRecapFlags_ColorEnabled(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	got, err := (&recapFlags{color: "always"}).colorEnabled(&out)
	if err != nil {
		t.Fatalf("colorEnabled(always) error = %v", err)
	}
	if !got {
		t.Fatal("colorEnabled(always) = false, want true")
	}

	got, err = (&recapFlags{color: "never"}).colorEnabled(&out)
	if err != nil {
		t.Fatalf("colorEnabled(never) error = %v", err)
	}
	if got {
		t.Fatal("colorEnabled(never) = true, want false")
	}

	got, err = (&recapFlags{color: "auto"}).colorEnabled(&out)
	if err != nil {
		t.Fatalf("colorEnabled(auto) error = %v", err)
	}
	if got {
		t.Fatal("colorEnabled(auto non-tty) = true, want false")
	}

	if _, err := (&recapFlags{color: "rainbow"}).colorEnabled(&out); err == nil {
		t.Fatal("colorEnabled(invalid) error = nil, want error")
	}
}

func TestKeyringReadError_PreservesCauseAndMatchesAs(t *testing.T) {
	t.Parallel()

	cause := errors.New("keychain locked")
	err := error(&keyringReadError{Cause: cause})

	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is should match wrapped cause; got false for %v", err)
	}
	var keyringErr *keyringReadError
	if !errors.As(err, &keyringErr) {
		t.Fatalf("errors.As should extract *keyringReadError; got false for %v", err)
	}
	if !errors.Is(keyringErr.Cause, cause) {
		t.Fatalf("Cause = %v, want %v", keyringErr.Cause, cause)
	}
	if !strings.Contains(err.Error(), "keychain locked") {
		t.Fatalf("Error() should include cause text; got %q", err.Error())
	}
}

func TestRunRecap_PrerequisiteErrorsUseErrorWriter(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	var out bytes.Buffer
	var errOut bytes.Buffer
	err := runRecap(context.Background(), &out, &errOut, &recapFlags{})
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("error = %T %v, want SilentError", err, err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errOut.String(), "Not a git repository") {
		t.Fatalf("stderr missing git prerequisite message: %q", errOut.String())
	}
}

func TestHandleRecapFetchError_UnauthorizedPromptsLogin(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := handleRecapFetchError(&out, &api.HTTPError{
		StatusCode: http.StatusUnauthorized,
		Message:    "Token expired",
	})
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("error = %T %v, want SilentError", err, err)
	}
	if !strings.Contains(out.String(), "Run `entire login` to re-authenticate.") {
		t.Fatalf("output missing re-authentication prompt: %q", out.String())
	}
}

func TestHandleRecapFetchError_PrintsMappedMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "bad request",
			err: &api.HTTPError{
				StatusCode: http.StatusBadRequest,
				Message:    "since must be on or before until",
			},
			want: "invalid recap time range",
		},
		{
			name: "missing account",
			err: &api.HTTPError{
				StatusCode: http.StatusNotFound,
				Message:    "User not found",
			},
			want: "could not find your account",
		},
		{
			name: "server failure",
			err: &api.HTTPError{
				StatusCode: http.StatusInternalServerError,
				Message:    "Failed to build recap",
			},
			want: "entire.io could not build the recap",
		},
		{
			name: "network",
			err:  &net.DNSError{Name: "entire.io", Err: "no such host"},
			want: "Could not reach entire.io",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := handleRecapFetchError(&out, c.err)
			var silent *SilentError
			if !errors.As(err, &silent) {
				t.Fatalf("error = %T %v, want SilentError", err, err)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Fatalf("output missing %q: %q", c.want, out.String())
			}
		})
	}
}

func TestRecapLoadErrorMessage_HTTPStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "bad recap time range",
			err: &api.HTTPError{
				StatusCode: http.StatusBadRequest,
				Message:    "since must be on or before until",
			},
			want: []string{
				"Entire sent an invalid recap time range.",
				"update Entire CLI",
				"HTTP 400",
				"since must be on or before until",
			},
		},
		{
			name: "missing server user",
			err: &api.HTTPError{
				StatusCode: http.StatusNotFound,
				Message:    "User not found",
			},
			want: []string{
				"entire.io could not find your account",
				"entire logout",
				"entire login",
				"HTTP 404",
				"User not found",
			},
		},
		{
			name: "server failure",
			err: &api.HTTPError{
				StatusCode: http.StatusInternalServerError,
				Message:    "Failed to build recap",
			},
			want: []string{
				"entire.io could not build the recap",
				"retry",
				"HTTP 500",
				"Failed to build recap",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := recapLoadErrorMessage(fmt.Errorf("me/recap: %w", c.err))
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Fatalf("message missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestRecapLoadErrorMessage_NetworkError(t *testing.T) {
	t.Parallel()

	dnsErr := &net.DNSError{Name: "entire.io", Err: "no such host"}
	got := recapLoadErrorMessage(fmt.Errorf("me/recap get: %w", dnsErr))
	for _, want := range []string{
		"Could not reach entire.io",
		"Check your internet connection",
		"ENTIRE_API_BASE_URL",
		"no such host",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("message missing %q:\n%s", want, got)
		}
	}
}

func TestRecapLoadErrorMessage_DNSNotFound(t *testing.T) {
	t.Parallel()

	nxdomain := &net.DNSError{Name: "no-token-here.example.com", Err: "no such host", IsNotFound: true}
	got := recapLoadErrorMessage(fmt.Errorf("me/recap get: %w", nxdomain))
	if strings.Contains(got, "Check your internet connection") {
		t.Fatalf("NXDOMAIN should not blame internet connection:\n%s", got)
	}
	for _, want := range []string{
		"Could not resolve API host",
		"no-token-here.example.com",
		"ENTIRE_API_BASE_URL",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("message missing %q:\n%s", want, got)
		}
	}
}

func TestRecapLoadErrorMessage_ContextCancellation(t *testing.T) {
	t.Parallel()

	canceled := fmt.Errorf("me/recap get: %w", &url.Error{
		Op:  "Get",
		URL: "https://entire.io/api/v1/me/recap",
		Err: context.Canceled,
	})
	got := recapLoadErrorMessage(canceled)
	if strings.Contains(got, "Could not reach entire.io") {
		t.Fatalf("cancellation should not be reported as a network failure:\n%s", got)
	}
	if !strings.Contains(got, "Recap request was canceled") {
		t.Fatalf("message missing cancellation explanation:\n%s", got)
	}
}

func TestRecapLoadErrorMessage_ContextDeadlineExceeded(t *testing.T) {
	t.Parallel()

	deadline := fmt.Errorf("me/recap get: %w", &url.Error{
		Op:  "Get",
		URL: "https://entire.io/api/v1/me/recap",
		Err: context.DeadlineExceeded,
	})
	got := recapLoadErrorMessage(deadline)
	if strings.Contains(got, "Could not reach entire.io") {
		t.Fatalf("timeout should not be reported as a generic network failure:\n%s", got)
	}
	if !strings.Contains(got, "Recap request timed out") {
		t.Fatalf("message missing timeout explanation:\n%s", got)
	}
}
