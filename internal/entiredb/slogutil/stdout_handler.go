package slogutil

import (
	"context"
	"log/slog"
	"os"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
)

// NewStdoutHandler returns a slog.Handler writing to stdout. When stdout is a
// terminal, log lines are rendered in a human-friendly coloured format via
// tint with a time-only timestamp and the service name rendered immediately
// after the level (e.g. `12:03:53.495 INF entire-core starting`). Otherwise
// JSON is emitted for ingestion by log pipelines, with service carried as a
// structured attribute.
func NewStdoutHandler(service string, level slog.Level) slog.Handler {
	if isatty.IsTerminal(os.Stdout.Fd()) {
		h := tint.NewHandler(os.Stdout, &tint.Options{
			Level:      level,
			TimeFormat: "15:04:05.000",
		})
		if service == "" {
			return h
		}
		return servicePrefixHandler{Handler: h, prefix: service}
	}
	h := slog.Handler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	if service != "" {
		h = h.WithAttrs([]slog.Attr{slog.String("service", service)})
	}
	return h
}

// CLILogLevel returns LevelDebug when ENTIRE_DEBUG is set to any non-empty
// value, otherwise LevelInfo. Mirrors the git-remote-entire convention so
// `ENTIRE_DEBUG=1 entire-repo …` flips on debug logs across the CLI fleet.
func CLILogLevel() slog.Level {
	if os.Getenv("ENTIRE_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// InstallStdoutDefault installs NewStdoutHandler as slog's default logger,
// wrapped in the standard interpolation and context handlers. Safe to call
// more than once — the last call wins.
func InstallStdoutDefault(service string, level slog.Level) {
	base := NewStdoutHandler(service, level)
	h := NewMsgInterpolationHandler(NewCtxHandler(base))
	slog.SetDefault(slog.New(h))
}

// servicePrefixHandler prepends a fixed service token to every record's
// message so tint renders it immediately after the level. JSON output keeps
// service as a structured attribute instead (see NewStdoutHandler).
type servicePrefixHandler struct {
	slog.Handler

	prefix string
}

func (h servicePrefixHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Message = h.prefix + " " + r.Message
	return h.Handler.Handle(ctx, r)
}

func (h servicePrefixHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return servicePrefixHandler{Handler: h.Handler.WithAttrs(attrs), prefix: h.prefix}
}

func (h servicePrefixHandler) WithGroup(name string) slog.Handler {
	return servicePrefixHandler{Handler: h.Handler.WithGroup(name), prefix: h.prefix}
}
