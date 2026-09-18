package cli

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

// releaseDepsRecorder builds a releaseDeps whose calls are recorded, matching
// the shape of promptDepsRecorder in doctor_unattributed_authors_test.go.
type releaseDepsRecorder struct {
	loggedIn   bool
	releaseErr error

	releaseCalls    []string
	invalidateCalls int
}

func (r *releaseDepsRecorder) deps() releaseDeps {
	return releaseDeps{
		loggedIn: func(context.Context) bool { return r.loggedIn },
		release: func(_ context.Context, email string) error {
			r.releaseCalls = append(r.releaseCalls, email)
			return r.releaseErr
		},
		invalidate: func(context.Context) { r.invalidateCalls++ },
	}
}

func TestReleaseAlias_Success_PrintsLimitation(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	rec := &releaseDepsRecorder{loggedIn: true}

	err := runReleaseAlias(context.Background(), &out, &errOut, "me@h.local", rec.deps())

	if err != nil {
		t.Fatalf("runReleaseAlias() error = %v, want nil", err)
	}
	want := "✓ Released me@h.local. Commits already linked stay linked; new ones will not be. Another account may now claim this address.\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}
	if rec.invalidateCalls != 1 {
		t.Errorf("invalidateCalls = %d, want 1", rec.invalidateCalls)
	}
	if len(rec.releaseCalls) != 1 || rec.releaseCalls[0] != "me@h.local" {
		t.Errorf("releaseCalls = %v, want [me@h.local]", rec.releaseCalls)
	}
}

func TestReleaseAlias_NotDeclared_Typed404(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	rec := &releaseDepsRecorder{
		loggedIn:   true,
		releaseErr: &coreapi.ErrorModelStatusCode{StatusCode: http.StatusNotFound},
	}

	err := runReleaseAlias(context.Background(), &out, &errOut, "me@h.local", rec.deps())

	assertReleaseFailed(t, err, &out, &errOut, "me@h.local is not a linked author address on your account\n")
	if len(rec.releaseCalls) != 1 {
		t.Errorf("releaseCalls = %v, want exactly one call", rec.releaseCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidateCalls = %d, want 0", rec.invalidateCalls)
	}
}

func TestReleaseAlias_NotAvailable_PlainText404(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	rec := &releaseDepsRecorder{
		loggedIn:   true,
		releaseErr: errors.New("decode response: default (code 404): unexpected Content-Type: text/plain"),
	}

	err := runReleaseAlias(context.Background(), &out, &errOut, "me@h.local", rec.deps())

	assertReleaseFailed(t, err, &out, &errOut, "Linking isn't available on this Entire yet.\n")
	if len(rec.releaseCalls) != 1 {
		t.Errorf("releaseCalls = %v, want exactly one call", rec.releaseCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidateCalls = %d, want 0", rec.invalidateCalls)
	}
}

func TestReleaseAlias_OtherError(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	rec := &releaseDepsRecorder{
		loggedIn:   true,
		releaseErr: errors.New("dial tcp: connection refused"),
	}

	err := runReleaseAlias(context.Background(), &out, &errOut, "me@h.local", rec.deps())

	assertReleaseFailed(t, err, &out, &errOut, "Could not release me@h.local: dial tcp: connection refused\n")
	if len(rec.releaseCalls) != 1 {
		t.Errorf("releaseCalls = %v, want exactly one call", rec.releaseCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidateCalls = %d, want 0", rec.invalidateCalls)
	}
}

func TestReleaseAlias_Conflict_UsesAPIError(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	statusErr := &coreapi.ErrorModelStatusCode{
		StatusCode: http.StatusConflict,
		Response:   coreapi.ErrorModel{Detail: coreapi.NewOptString("account is being deleted")},
	}
	rec := &releaseDepsRecorder{loggedIn: true, releaseErr: statusErr}

	err := runReleaseAlias(context.Background(), &out, &errOut, "me@h.local", rec.deps())

	assertReleaseFailed(t, err, &out, &errOut, "Could not release me@h.local: "+coreapi.APIError(statusErr)+"\n")
	if len(rec.releaseCalls) != 1 {
		t.Errorf("releaseCalls = %v, want exactly one call", rec.releaseCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidateCalls = %d, want 0", rec.invalidateCalls)
	}
}

func TestReleaseAlias_LoggedOut_Hint(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	rec := &releaseDepsRecorder{loggedIn: false}

	err := runReleaseAlias(context.Background(), &out, &errOut, "me@h.local", rec.deps())

	assertReleaseFailed(t, err, &out, &errOut, "Run `entire login` first\n")
	if len(rec.releaseCalls) != 0 {
		t.Errorf("release should not be called when logged out, got %v", rec.releaseCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidateCalls = %d, want 0", rec.invalidateCalls)
	}
}

func TestReleaseAlias_NotReservedHost_Refused(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	rec := &releaseDepsRecorder{loggedIn: true}

	err := runReleaseAlias(context.Background(), &out, &errOut, "bob@company.com", rec.deps())

	assertReleaseFailed(t, err, &out, &errOut, "Only reserved-host addresses (.local, .localdomain, …) can be linked or released\n")
	if len(rec.releaseCalls) != 0 {
		t.Errorf("release should not be called for a non-reserved-host address, got %v", rec.releaseCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidateCalls = %d, want 0", rec.invalidateCalls)
	}
}

func TestReleaseAlias_Empty_Refused(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	rec := &releaseDepsRecorder{loggedIn: true}

	err := runReleaseAlias(context.Background(), &out, &errOut, "   ", rec.deps())

	assertReleaseFailed(t, err, &out, &errOut, "Only reserved-host addresses (.local, .localdomain, …) can be linked or released\n")
	if len(rec.releaseCalls) != 0 {
		t.Errorf("release should not be called for an empty address, got %v", rec.releaseCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidateCalls = %d, want 0", rec.invalidateCalls)
	}
}

func TestReleaseAlias_NormalizesAddress(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	rec := &releaseDepsRecorder{loggedIn: true}

	err := runReleaseAlias(context.Background(), &out, &errOut, "Me@H.local.", rec.deps())

	if err != nil {
		t.Fatalf("runReleaseAlias() error = %v, want nil", err)
	}
	if len(rec.releaseCalls) != 1 || rec.releaseCalls[0] != "me@h.local." {
		t.Errorf("releaseCalls = %v, want [me@h.local.]", rec.releaseCalls)
	}
	if !strings.Contains(out.String(), "me@h.local.") {
		t.Errorf("stdout = %q, want the normalized address", out.String())
	}
}

// assertSilentError fails the test unless err is a non-nil *SilentError.
func assertSilentError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("runReleaseAlias() error = nil, want a *SilentError")
	}
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Fatalf("runReleaseAlias() error = %v (%T), want *SilentError", err, err)
	}
}

// assertReleaseFailed asserts the failure-path contract every runReleaseAlias
// error case shares: a *SilentError, nothing printed to stdout (the message
// went to stderr instead), and the exact stderr line.
func assertReleaseFailed(t *testing.T, err error, out, errOut *strings.Builder, wantStderr string) {
	t.Helper()
	assertSilentError(t, err)
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if errOut.String() != wantStderr {
		t.Errorf("stderr = %q, want %q", errOut.String(), wantStderr)
	}
}

// TestDoctor_ReleaseAndForceMutuallyExclusive checks that --release and
// --force cannot be combined, and that --release alone is unaffected.
// Asserted at the ValidateFlagGroups level (rather than a full Execute())
// because MarkFlagsMutuallyExclusive's enforcement runs from cobra's
// flag-group validation, which ValidateFlagGroups triggers directly without
// needing a git repo for doctor's PersistentPreRunE/PreRunE to succeed
// against.
func TestDoctor_ReleaseAndForceMutuallyExclusive(t *testing.T) {
	t.Parallel()
	cmd := newDoctorCmd()
	if err := cmd.Flags().Parse([]string{"--release", "me@h.local", "--force"}); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	err := cmd.ValidateFlagGroups()

	if err == nil {
		t.Fatal("ValidateFlagGroups() error = nil, want a mutually-exclusive-flags error")
	}
	if !strings.Contains(err.Error(), "release") || !strings.Contains(err.Error(), "force") {
		t.Errorf("ValidateFlagGroups() error = %q, want it to mention both release and force", err.Error())
	}
}

// TestDoctor_ReleaseAlone_NotMutuallyExclusive is the positive counterpart to
// TestDoctor_ReleaseAndForceMutuallyExclusive: --release by itself must not
// trip the mutual-exclusion validation.
func TestDoctor_ReleaseAlone_NotMutuallyExclusive(t *testing.T) {
	t.Parallel()
	cmd := newDoctorCmd()
	if err := cmd.Flags().Parse([]string{"--release", "me@h.local"}); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if err := cmd.ValidateFlagGroups(); err != nil {
		t.Errorf("ValidateFlagGroups() error = %v, want nil", err)
	}
}

// TestDoctor_ReleaseEmptyString_StillChanged documents the Changed-based gate
// in newDoctorCmd's RunE: `entire doctor --release ""` must still count as an
// explicit --release (Flags().Changed("release") true, value ""), so RunE
// routes it into runReleaseAlias — which then refuses the empty address —
// rather than silently falling through to the normal scan the way a
// releaseFlag != "" gate would.
func TestDoctor_ReleaseEmptyString_StillChanged(t *testing.T) {
	t.Parallel()
	cmd := newDoctorCmd()
	if err := cmd.Flags().Parse([]string{"--release", ""}); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if !cmd.Flags().Changed("release") {
		t.Error("Flags().Changed(\"release\") = false, want true for an explicit --release \"\"")
	}
	got, err := cmd.Flags().GetString("release")
	if err != nil {
		t.Fatalf("GetString(\"release\") error = %v", err)
	}
	if got != "" {
		t.Errorf("release flag value = %q, want empty", got)
	}
}
