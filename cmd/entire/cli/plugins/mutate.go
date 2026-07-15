package plugins

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	lua "github.com/yuin/gopher-lua"
)

// Mutating hooks differ from observer hooks in that their return values affect
// CLI behavior. They are additionally capability-gated and have deterministic
// multi-plugin semantics: plugins are consulted in load order (user-dir plugins
// before repo-local, discovery order within each), which is stable across runs.
//
// Ordering vs built-in behavior:
//   - prepare_commit_msg: plugin trailers are appended AFTER the strategy's own
//     Entire-Checkpoint trailer, so the built-in linkage trailer is never
//     displaced. Multiple plugins' trailers are appended in load order.
//   - pre_push: the plugin veto runs BEFORE the built-in OPF rewrite and
//     checkpoint-ref push, so a veto short-circuits that work. A veto aborts the
//     user's push (non-zero hook exit); the built-in OPF abort path is
//     unaffected and independent.

// FireCommitMsg dispatches the prepare_commit_msg mutating hook and returns the
// trailer strings contributed by plugins granted the commit_msg capability, in
// load order. A plugin that registered the hook without the capability is
// skipped (it cannot mutate the commit message). Errors and panics are isolated
// and drop only that callback's contribution.
func (r *Registry) FireCommitMsg(ctx context.Context, payload map[string]any) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	logCtx := logging.WithComponent(ctx, "plugins")

	var out []string
	for _, p := range r.plugins {
		cbs := p.callbacks[HookPrepareCommitMsg]
		if len(cbs) == 0 {
			continue
		}
		if !p.Grant.HasCapability(settings.PluginCapabilityCommitMsg) {
			logging.Debug(logCtx, "skip prepare_commit_msg: capability not granted",
				slog.String("plugin", p.Manifest.Name))
			continue
		}
		for _, cb := range cbs {
			if s, ok := r.callString(ctx, logCtx, p, HookPrepareCommitMsg, cb, payload); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// FirePrePush dispatches the pre_push hook. Every plugin's callback runs (its
// observer side effects), but only a plugin granted the pre_push capability may
// veto: returning false (with an optional reason string) aborts the push. The
// first veto in load order supplies the reported reason; all callbacks still
// run. Returns a non-nil error when the push is vetoed.
func (r *Registry) FirePrePush(ctx context.Context, payload map[string]any) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	logCtx := logging.WithComponent(ctx, "plugins")

	var vetoErr error
	for _, p := range r.plugins {
		cbs := p.callbacks[HookPrePush]
		if len(cbs) == 0 {
			continue
		}
		canVeto := p.Grant.HasCapability(settings.PluginCapabilityPrePush)
		for _, cb := range cbs {
			vetoed, reason := r.callVeto(ctx, logCtx, p, cb, payload)
			if canVeto && vetoed && vetoErr == nil {
				if reason == "" {
					reason = "no reason given"
				}
				vetoErr = fmt.Errorf("push vetoed by plugin %q: %s", p.Manifest.Name, reason)
			}
		}
	}
	return vetoErr
}

// callString invokes a mutating callback expecting a single string return,
// bounded by the hook timeout with panic/error isolation.
func (r *Registry) callString(ctx, logCtx context.Context, p *LoadedPlugin, hook string, cb *lua.LFunction, payload map[string]any) (result string, ok bool) {
	cctx, cancel := context.WithTimeout(ctx, hookTimeout())
	defer cancel()
	p.dispatchCtx = cctx
	p.L.SetContext(cctx)
	defer p.L.SetContext(context.Background())
	defer func() {
		if rec := recover(); rec != nil {
			logging.Warn(logCtx, "mutating hook panicked",
				slog.String("plugin", p.Manifest.Name), slog.String("hook", hook))
		}
	}()

	arg := toLuaTable(p.L, payload)
	if err := p.L.CallByParam(lua.P{Fn: cb, NRet: 1, Protect: true}, arg); err != nil {
		logging.Warn(logCtx, "mutating hook callback failed",
			slog.String("plugin", p.Manifest.Name), slog.String("hook", hook), slog.String("error", err.Error()))
		return "", false
	}
	ret := p.L.Get(-1)
	p.L.Pop(1)
	if s, isStr := ret.(lua.LString); isStr {
		return string(s), true
	}
	return "", false
}

// callVeto invokes a pre_push callback, interpreting a boolean false first
// return as a veto and an optional second string as the reason.
func (r *Registry) callVeto(ctx, logCtx context.Context, p *LoadedPlugin, cb *lua.LFunction, payload map[string]any) (vetoed bool, reason string) {
	cctx, cancel := context.WithTimeout(ctx, hookTimeout())
	defer cancel()
	p.dispatchCtx = cctx
	p.L.SetContext(cctx)
	defer p.L.SetContext(context.Background())
	defer func() {
		if rec := recover(); rec != nil {
			logging.Warn(logCtx, "pre_push hook panicked",
				slog.String("plugin", p.Manifest.Name))
		}
	}()

	arg := toLuaTable(p.L, payload)
	if err := p.L.CallByParam(lua.P{Fn: cb, NRet: 2, Protect: true}, arg); err != nil {
		logging.Warn(logCtx, "pre_push hook callback failed",
			slog.String("plugin", p.Manifest.Name), slog.String("error", err.Error()))
		return false, ""
	}
	// Returns are pushed in order; with NRet:2 the stack top-1 is the first
	// return and top is the second.
	second := p.L.Get(-1)
	first := p.L.Get(-2)
	p.L.Pop(2)

	if b, isBool := first.(lua.LBool); isBool && !bool(b) {
		vetoed = true
	}
	if s, isStr := second.(lua.LString); isStr {
		reason = string(s)
	}
	return vetoed, reason
}
