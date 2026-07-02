package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// OPFDecision is the resolved gate for a single pre-push OPF run.
type OPFDecision int

const (
	OPFRun   OPFDecision = iota // run the rewrite, push 8-layer
	OPFSkip                     // skip the rewrite, push 7-layer
	OPFAbort                    // cancel the push entirely (Ctrl-C / non-TTY abort)
)

const envOPF = "ENTIRE_OPF"

// resolveOPFDecision picks the run/skip/abort gate. Precedence (highest
// first): ENTIRE_OPF env var → settings.PromptDefault → interactive
// prompt → non-TTY fallback (run).
//
// Pure logic — prompter is only called when the user needs to decide.
// Tests inject a fake.
func resolveOPFDecision(env, promptDefault string, hasTTY bool, prompter func() (OPFDecision, error)) (OPFDecision, error) {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "yes":
		return OPFRun, nil
	case "no":
		return OPFSkip, nil
	}
	switch strings.ToLower(strings.TrimSpace(promptDefault)) {
	case settings.OPFPromptNever:
		return OPFSkip, nil
	case settings.OPFPromptAlways:
		return OPFRun, nil
	}
	if !hasTTY {
		// Non-interactive context: run OPF (matches the user's "if
		// enabled, just run" preference). The caller emits a progress
		// line at run time so scripted output isn't silent.
		return OPFRun, nil
	}
	return prompter()
}

// resolveOPFDecisionForPrePush is the production wiring: reads env +
// settings + TTY, calls askOPFPrompt when interactive, emits a stderr
// progress line on the non-TTY auto-run path. errOut receives the
// progress line (only printed when we'll actually run + the caller is
// non-interactive).
func resolveOPFDecisionForPrePush(ctx context.Context, opf *settings.OPFSettings, errOut io.Writer) (OPFDecision, error) {
	hasTTY := interactive.CanPromptInteractively()
	promptDefault := ""
	if opf != nil {
		promptDefault = opf.PromptDefault
	}
	d, err := resolveOPFDecision(
		os.Getenv(envOPF),
		promptDefault,
		hasTTY,
		func() (OPFDecision, error) { return askOPFPrompt(ctx, isAccessibleMode()) },
	)
	if err != nil {
		return OPFAbort, err
	}
	if d == OPFRun && !hasTTY {
		fmt.Fprintln(errOut, "→ OpenAI Privacy Filter: scanning checkpoints before push (may take ~30s)…")
	}
	return d, nil
}

// askOPFPrompt shows the 3-option huh form. Ctrl-C / SIGINT returns
// OPFAbort. Selecting "Always" persists prompt_default=always to
// .entire/settings.local.json so future pushes don't ask.
//
// Style matches other entire CLI prompts: Dracula theme via
// huh.ThemeDracula (the same theme cli.NewAccessibleForm applies for
// callers in the cli package). Strategy can't import cli (cycle), so
// we apply the theme inline.
func askOPFPrompt(ctx context.Context, accessible bool) (OPFDecision, error) {
	const (
		choiceYes    = "yes"
		choiceNo     = "no"
		choiceAlways = "always"
	)
	choice := choiceYes
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Run OpenAI Privacy Filter on these checkpoints?").
				Description("Adds ~30s but redacts names/PII the regex layers can't catch. Ctrl-C to cancel the push.").
				Options(
					huh.NewOption("Yes — run OPF this push", choiceYes),
					huh.NewOption("No — skip OPF, push as-is", choiceNo),
					huh.NewOption("Always — run OPF on every push from now on", choiceAlways),
				).
				Value(&choice),
		),
	).WithTheme(huh.ThemeFunc(huh.ThemeDracula))
	if accessible {
		form = form.WithAccessible(true)
	}
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return OPFAbort, nil
		}
		return OPFAbort, fmt.Errorf("opf prompt: %w", err)
	}
	if choice == choiceAlways {
		if err := persistOPFPromptDefaultAlways(ctx); err != nil {
			logging.Warn(ctx, "failed to persist OPF prompt_default=always",
				slog.String("error", err.Error()))
		}
	}
	if choice == choiceNo {
		return OPFSkip, nil
	}
	return OPFRun, nil
}

// persistOPFPromptDefaultAlways writes
// redaction.openai_privacy_filter.prompt_default = "always" to
// .entire/settings.local.json, preserving any unrelated fields by
// using a generic JSON-map round-trip.
func persistOPFPromptDefaultAlways(ctx context.Context) error {
	path, raw, _, err := settings.LoadLocalRaw(ctx)
	if err != nil {
		return fmt.Errorf("load local settings: %w", err)
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	redactionRaw := readSubObject(ctx, raw, "redaction")
	opfRaw := readSubObject(ctx, redactionRaw, "openai_privacy_filter")

	val, err := json.Marshal(settings.OPFPromptAlways)
	if err != nil {
		return fmt.Errorf("marshal prompt_default: %w", err)
	}
	opfRaw["prompt_default"] = val

	if err := writeSubObject(redactionRaw, "openai_privacy_filter", opfRaw); err != nil {
		return err
	}
	if err := writeSubObject(raw, "redaction", redactionRaw); err != nil {
		return err
	}
	if err := settings.SaveLocalRaw(path, raw); err != nil {
		return fmt.Errorf("save local settings: %w", err)
	}
	return nil
}

func readSubObject(ctx context.Context, parent map[string]json.RawMessage, key string) map[string]json.RawMessage {
	sub := map[string]json.RawMessage{}
	data, ok := parent[key]
	if !ok {
		return sub
	}
	// Malformed sub-object → fresh map (we'll overwrite the slot).
	if err := json.Unmarshal(data, &sub); err != nil {
		logging.Warn(ctx, "malformed OPF prompt settings object; overwriting with fresh object",
			slog.String("key", key),
			slog.String("error", err.Error()),
		)
		return map[string]json.RawMessage{}
	}
	return sub
}

func writeSubObject(parent map[string]json.RawMessage, key string, sub map[string]json.RawMessage) error {
	data, err := json.Marshal(sub)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", key, err)
	}
	parent[key] = data
	return nil
}
