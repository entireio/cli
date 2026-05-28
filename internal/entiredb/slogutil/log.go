package slogutil

import (
	"context"
	"log/slog"
	"runtime"
	"time"
)

// Info logs at Info level with correct caller source location.
func Info(ctx context.Context, msg string, attrs ...slog.Attr) {
	log(ctx, slog.LevelInfo, msg, attrs)
}

// Warn logs at Warn level with correct caller source location.
func Warn(ctx context.Context, msg string, attrs ...slog.Attr) {
	log(ctx, slog.LevelWarn, msg, attrs)
}

// Error logs at Error level with correct caller source location.
func Error(ctx context.Context, msg string, attrs ...slog.Attr) {
	log(ctx, slog.LevelError, msg, attrs)
}

// Debug logs at Debug level with correct caller source location.
func Debug(ctx context.Context, msg string, attrs ...slog.Attr) {
	log(ctx, slog.LevelDebug, msg, attrs)
}

func log(ctx context.Context, level slog.Level, msg string, attrs []slog.Attr) {
	handler := slog.Default().Handler()
	if !handler.Enabled(ctx, level) {
		return
	}

	var pcs [1]uintptr
	runtime.Callers(3, pcs[:]) // skip Callers, log, Info/Warn/Error/Debug

	r := slog.NewRecord(time.Now(), level, msg, pcs[0])
	r.AddAttrs(attrs...)
	handler.Handle(ctx, r) //nolint:errcheck // logging must not fail callers
}
