package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAntigravityIdentity(t *testing.T) {
	t.Parallel()
	a := &Antigravity{}
	if got, want := a.Name(), "antigravity"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if got, want := a.Binary(), "agy"; got != want {
		t.Errorf("Binary() = %q, want %q", got, want)
	}
	if got, want := a.EntireAgent(), "antigravity"; got != want {
		t.Errorf("EntireAgent() = %q, want %q", got, want)
	}
	if a.TimeoutMultiplier() <= 0 {
		t.Errorf("TimeoutMultiplier() = %v, want > 0", a.TimeoutMultiplier())
	}
}

func TestAntigravityDefaultConcurrencyIsSerial(t *testing.T) {
	t.Parallel()

	if antigravityDefaultConcurrency != 1 {
		t.Fatalf("antigravityDefaultConcurrency = %d, want 1", antigravityDefaultConcurrency)
	}
}

func TestAntigravityModelUsesDefault(t *testing.T) {
	t.Setenv("E2E_ANTIGRAVITY_MODEL", "")

	if got := antigravityModel(); got != "Gemini 3.5 Flash (Low)" {
		t.Fatalf("antigravityModel() = %q, want stable default Gemini 3.5 Flash (Low)", got)
	}
}

func TestAntigravityModelReadsEnv(t *testing.T) {
	t.Setenv("E2E_ANTIGRAVITY_MODEL", "Gemini 3.1 Pro (Low)")

	if got := antigravityModel(); got != "Gemini 3.1 Pro (Low)" {
		t.Fatalf("antigravityModel() = %q, want env override", got)
	}
}

func TestAntigravityBootstrapRequiresADCInCI(t *testing.T) {
	t.Setenv("CI", "true")
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	err := (&Antigravity{}).Bootstrap()
	if err == nil {
		t.Fatal("Bootstrap() error = nil, want missing ADC error")
	}
	if !strings.Contains(err.Error(), "ANTIGRAVITY_GOOGLE_APPLICATION_CREDENTIALS_JSON") {
		t.Fatalf("Bootstrap() error = %q, want CI secret guidance", err)
	}
}

func TestAntigravityBootstrapAcceptsReadableADCFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CI", "true")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)

	if err := (&Antigravity{}).Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}
}

func TestAntigravityBootstrapRejectsUnreadableADCFile(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))

	err := (&Antigravity{}).Bootstrap()
	if err == nil {
		t.Fatal("Bootstrap() error = nil, want missing credentials file error")
	}
	if !strings.Contains(err.Error(), "GOOGLE_APPLICATION_CREDENTIALS") {
		t.Fatalf("Bootstrap() error = %q, want credentials path context", err)
	}
}

func TestAntigravityBootstrapAllowsLocalOAuthWithoutADC(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	if err := (&Antigravity{}).Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil for local OAuth fallback", err)
	}
}

func TestAntigravityPromptArgsIncludeModelAndWorkspace(t *testing.T) {
	t.Parallel()

	args, displayArgs := antigravityPromptArgsFromEnv("make a file", "/repo", "gemini-2.5-pro", nil)

	if len(args) < 2 || !strings.Contains(args[1], "make a file") || !strings.Contains(args[1], "/repo") {
		t.Fatalf("prompt arg = %#v, want workspace-scoped prompt containing original request and repo dir", args)
	}
	want := []string{
		"-p", args[1],
		"--model", "gemini-2.5-pro",
		"--dangerously-skip-permissions",
		"--new-project",
		"--add-dir", "/repo",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	if !slices.Contains(displayArgs, "--model") || !slices.Contains(displayArgs, "gemini-2.5-pro") {
		t.Fatalf("displayArgs should include model flag, got %#v", displayArgs)
	}
}

func TestAntigravityWorkspacePromptPreservesRequestedGitOperations(t *testing.T) {
	t.Parallel()

	prompt := antigravityWorkspacePrompt("create docs/red.md, then git add and commit it", "/repo")

	required := []string{
		"Complete every requested operation before responding",
		"including every git command or commit mentioned in the request",
		"If the request asks for a commit, run a shell command such as git add",
		"Do not claim a file was committed until the git command has completed successfully",
		"Do not run verification commands such as list_dir or view_file unless requested",
		"If the request has numbered steps, complete every numbered step in order",
		"For multi-step requests with file contents and git commands, use shell commands when that is the most direct way to preserve order",
		"Do not create artifacts for repository files",
	}
	for _, want := range required {
		if !strings.Contains(prompt, want) {
			t.Fatalf("workspace prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestAntigravityPromptArgsIncludeProjectFromADCJSON(t *testing.T) {
	t.Parallel()

	credentialsPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"project_id":"entire-e2e"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	args, displayArgs := antigravityPromptArgsFromEnv("make a file", "/repo", "gemini-2.5-pro", []string{
		"GOOGLE_APPLICATION_CREDENTIALS=" + credentialsPath,
		"GOOGLE_CLOUD_PROJECT=",
	})

	projectFlag := slices.Index(args, "--project")
	if projectFlag == -1 || projectFlag+1 >= len(args) || args[projectFlag+1] != "entire-e2e" {
		t.Fatalf("args = %#v, want --project entire-e2e from ADC JSON", args)
	}
	if !slices.Contains(displayArgs, "--project") || !slices.Contains(displayArgs, "entire-e2e") {
		t.Fatalf("displayArgs should include project flag, got %#v", displayArgs)
	}
}

func TestAntigravityPromptEnvUsesADCWithIsolatedHomeWhenCredentialsProvided(t *testing.T) {
	t.Parallel()

	base := []string{
		"PATH=/bin",
		"ENTIRE_TEST_TTY=1",
		"HOME=/real-home",
		"USE_ADC=0",
		"GOOGLE_APPLICATION_CREDENTIALS=/creds.json",
	}

	got := antigravityPromptEnvFrom(base, "/tmp/repo")

	if _, ok := envValue(got, "ENTIRE_TEST_TTY"); ok {
		t.Fatalf("ENTIRE_TEST_TTY should be stripped, env=%#v", got)
	}
	if gotHome, _ := envValue(got, "HOME"); gotHome != "/tmp/repo-antigravity-home" {
		t.Fatalf("HOME = %q, want isolated test home", gotHome)
	}
	if gotADC, _ := envValue(got, "USE_ADC"); gotADC != "1" {
		t.Fatalf("USE_ADC = %q, want 1", gotADC)
	}
	if gotCreds, _ := envValue(got, "GOOGLE_APPLICATION_CREDENTIALS"); gotCreds != "/creds.json" {
		t.Fatalf("GOOGLE_APPLICATION_CREDENTIALS = %q, want preserved credentials path", gotCreds)
	}
	if countEnv(got, "HOME") != 1 || countEnv(got, "USE_ADC") != 1 {
		t.Fatalf("HOME and USE_ADC should be upserted once, env=%#v", got)
	}
}

func TestAntigravityPromptEnvSetsGoogleCloudProjectFromADCJSON(t *testing.T) {
	t.Parallel()

	credentialsPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"project_id":"entire-e2e"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"PATH=/bin",
		"HOME=/real-home",
		"GOOGLE_APPLICATION_CREDENTIALS=" + credentialsPath,
	}

	got := antigravityPromptEnvFrom(base, "/tmp/repo")

	if gotProject, _ := envValue(got, "GOOGLE_CLOUD_PROJECT"); gotProject != "entire-e2e" {
		t.Fatalf("GOOGLE_CLOUD_PROJECT = %q, want project_id from ADC JSON", gotProject)
	}
}

func TestAntigravitySessionEnvSetsGoogleCloudProjectFromADCJSON(t *testing.T) {
	t.Parallel()

	credentialsPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"project_id":"entire-e2e"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"PATH=/bin",
		"HOME=/real-home",
		"GOOGLE_APPLICATION_CREDENTIALS=" + credentialsPath,
	}

	envArgs, _ := antigravitySessionEnv(base, "/tmp/repo")

	if gotProject, _ := envValue(envArgs, "GOOGLE_CLOUD_PROJECT"); gotProject != "entire-e2e" {
		t.Fatalf("GOOGLE_CLOUD_PROJECT = %q, want project_id from ADC JSON", gotProject)
	}
}

func TestAntigravityPromptEnvKeepsRealHomeWithoutADCCredentials(t *testing.T) {
	t.Parallel()

	base := []string{
		"PATH=/bin",
		"HOME=/real-home",
		"USE_ADC=1",
	}

	got := antigravityPromptEnvFrom(base, "/tmp/repo")

	if gotHome, _ := envValue(got, "HOME"); gotHome != "/real-home" {
		t.Fatalf("HOME = %q, want real home when GOOGLE_APPLICATION_CREDENTIALS is absent", gotHome)
	}
	if gotADC, _ := envValue(got, "USE_ADC"); gotADC != "1" {
		t.Fatalf("USE_ADC = %q, want existing value preserved", gotADC)
	}
}

func TestAntigravityStartupClassifierTreatsTrustSelectorAsConfirmation(t *testing.T) {
	t.Parallel()

	content := `Accessing workspace:

/private/tmp/repo

Do you trust the contents of this project?

Antigravity may read files and run commands in this folder.

> Yes, I trust this folder
  No, exit`

	if !antigravityNeedsStartupConfirmation(content) {
		t.Fatal("trust selector should require confirmation")
	}
	if antigravityReadyForPrompt(content) {
		t.Fatal("trust selector should not be treated as the ready prompt")
	}
}

func TestAntigravityStartupClassifierAcceptsReadyPrompt(t *testing.T) {
	t.Parallel()

	content := `Accessing workspace:

/private/tmp/repo

>`

	if antigravityNeedsStartupConfirmation(content) {
		t.Fatal("ready prompt should not require confirmation")
	}
	if !antigravityReadyForPrompt(content) {
		t.Fatal("ready prompt should be accepted")
	}
}

func TestAntigravityIsTransientErrorRecognizesErrorText(t *testing.T) {
	t.Parallel()

	a := &Antigravity{}
	err := stringError("antigravity transcript contains ERROR_MESSAGE error_code=429: model API is currently overloaded")
	if !a.IsTransientError(Output{}, err) {
		t.Fatal("IsTransientError() = false, want true for transient error text")
	}
}

func TestAntigravityQuotaExhaustedIsFatalNotTransient(t *testing.T) {
	t.Parallel()

	// agy wraps the hard quota wall in "overloaded" + "429" + "RESOURCE_EXHAUSTED",
	// all of which the transient patterns would otherwise match. The quota-reached
	// marker must win so the harness fails fast instead of restarting for ~53h.
	out := Output{Stderr: "The model API is currently overloaded: RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 53h35m43s."}
	if (&Antigravity{}).IsTransientError(out, stringError(out.Stderr)) {
		t.Fatal("Individual quota reached must be fatal (non-retryable), not a transient restart")
	}
}

func TestAntigravityEntitlementWallsAreFatal(t *testing.T) {
	t.Parallel()

	for _, msg := range []string{
		"PERMISSION_DENIED (code 403): Cloud Code Private API ... reason: SERVICE_DISABLED",
		"AUTH_PERMISSION_DENIED subject: 110002",
		"error getting token source: You are not logged into Antigravity.",
	} {
		if (&Antigravity{}).IsTransientError(Output{Stderr: msg}, stringError(msg)) {
			t.Errorf("config/entitlement wall must be fatal, not transient: %q", msg)
		}
	}
}

func TestAntigravityGenuineOverloadStillRetries(t *testing.T) {
	t.Parallel()

	// A real transient overload with no quota/entitlement marker must still retry.
	out := Output{Stderr: "The model API is currently overloaded and may experience intermittent errors. (503 UNAVAILABLE)"}
	if !(&Antigravity{}).IsTransientError(out, stringError(out.Stderr)) {
		t.Fatal("genuine transient overload (no quota/auth marker) should still retry")
	}
}

func TestAntigravityMissingCommitIsNotTransient(t *testing.T) {
	t.Parallel()

	a := &Antigravity{}
	err := stringError("antigravity did not create requested commit; HEAD unchanged")
	if a.IsTransientError(Output{}, err) {
		t.Fatal("missing commit should fail the prompt, not restart the whole scenario as a transient API error")
	}
}

func TestAntigravityPromptTranscriptTransientMatchesCurrentPrompt(t *testing.T) {
	t.Parallel()

	brainDir := t.TempDir()
	path := filepath.Join(brainDir, "conv-123", ".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	data := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>\nmake docs/example.md\n</USER_REQUEST>","status":"DONE"}` + "\n" +
		`{"step_index":1,"source":"SYSTEM","type":"ERROR_MESSAGE","error":"The model API is currently overloaded and may experience intermittent errors.","error_code":429,"status":"DONE"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := antigravityPromptTranscriptTransient(brainDir, "make docs/example.md", time.Now().Add(-time.Minute))
	if !ok {
		t.Fatal("antigravityPromptTranscriptTransient() ok = false, want true")
	}
	if got == "" {
		t.Fatal("antigravityPromptTranscriptTransient() message is empty")
	}
}

func TestAntigravityPromptTranscriptTransientIgnoresRecoveredInvalidToolCall(t *testing.T) {
	t.Parallel()

	brainDir := t.TempDir()
	path := filepath.Join(brainDir, "conv-123", ".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	data := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>\nmake docs/example.md\n</USER_REQUEST>","status":"DONE"}` + "\n" +
		`{"step_index":1,"source":"SYSTEM","type":"ERROR_MESSAGE","error":"There was a problem parsing the tool call. Error Message: model output error: invalid tool call error (invalid_args)","status":"DONE"}` + "\n" +
		`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","content":"Done.","status":"DONE"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, ok := antigravityPromptTranscriptTransient(brainDir, "make docs/example.md", time.Now().Add(-time.Minute)); ok {
		t.Fatalf("antigravityPromptTranscriptTransient() = %q, true; want no match for recovered tool error", got)
	}
}

func TestAntigravityRawToolCallOutputRecognized(t *testing.T) {
	t.Parallel()

	cases := []string{
		`<|start|>assistant<|channel|>commentary to=functions.run_command <|constrain|>json<|message|>{"CommandLine":"mkdir -p docs"}<|call|>`,
		`<|channel|>commentary<|message|>{"CommandLine":"git add docs/red.md && git commit -m \"Add red doc\"","toolAction":"Committing file"}`,
	}
	for _, content := range cases {
		if !antigravityRawToolCallOutput(content) {
			t.Fatalf("antigravityRawToolCallOutput() = false for %q, want true", content)
		}
		if !(&Antigravity{}).IsTransientError(Output{Stdout: content}, stringError("antigravity emitted raw tool call output")) {
			t.Fatalf("IsTransientError() = false for %q, want raw tool output to be retryable", content)
		}
	}
}

func TestAntigravityPromptTranscriptTransientIgnoresOtherPrompt(t *testing.T) {
	t.Parallel()

	brainDir := t.TempDir()
	path := filepath.Join(brainDir, "conv-123", ".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	data := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>\nchange another file\n</USER_REQUEST>","status":"DONE"}` + "\n" +
		`{"step_index":1,"source":"SYSTEM","type":"ERROR_MESSAGE","error":"The model API is currently overloaded and may experience intermittent errors.","error_code":429,"status":"DONE"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, ok := antigravityPromptTranscriptTransient(brainDir, "make docs/example.md", time.Now().Add(-time.Minute)); ok {
		t.Fatalf("antigravityPromptTranscriptTransient() = %q, true; want no match", got)
	}
}

func TestAntigravityAuthenticationRequiredRecognized(t *testing.T) {
	t.Parallel()

	content := `Authentication required. Please visit the URL to log in:
  https://accounts.google.com/o/oauth2/auth?state=abc

Waiting for authentication (timeout 30s)...
Or, paste the authorization code here and press Enter:
Error: authentication timed out.`
	if !antigravityAuthenticationRequired(content) {
		t.Fatal("antigravityAuthenticationRequired() = false, want true")
	}
	if (&Antigravity{}).IsTransientError(Output{Stdout: content}, nil) {
		t.Fatal("authentication timeout should not be treated as transient")
	}
}

func TestAntigravityAuthenticationRequiredRecognizesLowercasePrintModeError(t *testing.T) {
	t.Parallel()

	content := "Error: authentication required. Run 'agy' to log in."
	if !antigravityAuthenticationRequired(content) {
		t.Fatal("antigravityAuthenticationRequired() = false, want true for lowercase print-mode auth error")
	}
}

type stringError string

func (e stringError) Error() string { return string(e) }

func countEnv(env []string, key string) int {
	count := 0
	for _, entry := range env {
		if len(entry) > len(key)+1 && entry[:len(key)+1] == key+"=" {
			count++
		}
	}
	return count
}

func TestAntigravitySessionCLIArgsIncludeWorkspaceAndProject(t *testing.T) {
	t.Parallel()

	args := antigravitySessionCLIArgsFromEnv("/repo", "gemini-2.5-pro", []string{
		"GOOGLE_CLOUD_PROJECT=entire-e2e",
	})

	want := []string{
		"agy",
		"--model", "gemini-2.5-pro",
		"--dangerously-skip-permissions",
		"--new-project",
		"--add-dir", "/repo",
		"--project", "entire-e2e",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestAntigravityInteractiveSessionWrapsSend(t *testing.T) {
	t.Parallel()

	rec := &recordingSession{}
	session := &antigravityInteractiveSession{Session: rec, dir: "/repo"}

	if err := session.Send("create docs/red.md"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(rec.inputs) != 1 {
		t.Fatalf("recorded inputs = %#v, want one", rec.inputs)
	}
	if !strings.Contains(rec.inputs[0], "Use the workspace at /repo") {
		t.Fatalf("wrapped prompt missing workspace: %q", rec.inputs[0])
	}
	if !strings.Contains(rec.inputs[0], "Request:\ncreate docs/red.md") {
		t.Fatalf("wrapped prompt missing original request: %q", rec.inputs[0])
	}
}

func TestAntigravityInteractiveSessionRetriesRawToolCallOutput(t *testing.T) {
	t.Parallel()

	raw := `<|channel|>commentary<|message|>{"CommandLine":"git add hello.txt && git commit -m Add","toolAction":"Creating commit"}`
	rec := &recordingSession{waits: []string{raw, "done\n>"}}
	session := &antigravityInteractiveSession{Session: rec, dir: "/repo"}

	if err := session.Send("create hello.txt and commit it"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	content, err := session.WaitFor(">", time.Second)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if content != "done\n>" {
		t.Fatalf("WaitFor content = %q, want final retry content", content)
	}
	if len(rec.inputs) != 2 {
		t.Fatalf("recorded inputs = %#v, want original send plus retry", rec.inputs)
	}
	if !strings.Contains(rec.inputs[1], "Request:\ncreate hello.txt and commit it") {
		t.Fatalf("retry prompt missing original request: %q", rec.inputs[1])
	}
}

func TestAntigravityInteractiveSessionRetriesRequestedCommitWhenHeadUnchanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGitForAntigravityTest(t, dir, "init")
	runGitForAntigravityTest(t, dir, "config", "user.name", "E2E Test")
	runGitForAntigravityTest(t, dir, "config", "user.email", "e2e@test.local")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForAntigravityTest(t, dir, "add", "README.md")
	runGitForAntigravityTest(t, dir, "commit", "-m", "initial commit")

	rec := &recordingSession{waits: []string{"claimed committed\n>", "done\n>"}}
	session := &antigravityInteractiveSession{Session: rec, dir: dir}

	if err := session.Send("create hello.txt and commit it"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	content, err := session.WaitFor(">", time.Second)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if content != "done\n>" {
		t.Fatalf("WaitFor content = %q, want retry content", content)
	}
	if len(rec.inputs) != 2 {
		t.Fatalf("recorded inputs = %#v, want original send plus commit retry", rec.inputs)
	}
	if !strings.Contains(rec.inputs[1], "HEAD has not changed") {
		t.Fatalf("commit retry prompt missing HEAD guidance: %q", rec.inputs[1])
	}
	if !strings.Contains(rec.inputs[1], "Do not modify repository file contents") {
		t.Fatalf("commit retry prompt should forbid content edits: %q", rec.inputs[1])
	}
	if !strings.Contains(rec.inputs[1], "Previous request:\ncreate hello.txt and commit it") {
		t.Fatalf("commit retry prompt missing original request: %q", rec.inputs[1])
	}
}

func runGitForAntigravityTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestAntigravityShouldRetryMissingCommit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGitForAntigravityTest(t, dir, "init")
	runGitForAntigravityTest(t, dir, "config", "user.name", "E2E Test")
	runGitForAntigravityTest(t, dir, "config", "user.email", "e2e@test.local")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForAntigravityTest(t, dir, "add", "README.md")
	runGitForAntigravityTest(t, dir, "commit", "-m", "initial commit")
	head := antigravityGitHead(dir)

	if !antigravityShouldRetryMissingCommit("create docs/red.md and commit it", dir, head) {
		t.Fatal("antigravityShouldRetryMissingCommit() = false, want true when commit requested and HEAD unchanged")
	}
	if antigravityShouldRetryMissingCommit("create docs/red.md but do not commit", dir, head) {
		t.Fatal("antigravityShouldRetryMissingCommit() = true, want false when prompt says not to commit")
	}

	if err := os.WriteFile(filepath.Join(dir, "next.md"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForAntigravityTest(t, dir, "add", "next.md")
	runGitForAntigravityTest(t, dir, "commit", "-m", "next commit")
	if antigravityShouldRetryMissingCommit("create docs/red.md and commit it", dir, head) {
		t.Fatal("antigravityShouldRetryMissingCommit() = true, want false after HEAD changed")
	}
}

type recordingSession struct {
	inputs []string
	waits  []string
}

func (r *recordingSession) Send(input string) error {
	r.inputs = append(r.inputs, input)
	return nil
}

func (r *recordingSession) WaitFor(string, time.Duration) (string, error) {
	if len(r.waits) == 0 {
		return "", nil
	}
	content := r.waits[0]
	r.waits = r.waits[1:]
	return content, nil
}
func (r *recordingSession) Capture() string { return "" }
func (r *recordingSession) Close() error    { return nil }
