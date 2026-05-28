package slogutil

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// MsgInterpolationHandler optionally appends attribute key-value pairs to
// the log message. When enabled (via ENTIRE_LOG_ATTRS_IN_MSG=true at
// construction time), it rewrites:
//
//	"Detected cloud environment" + {provider: "aws", region: "us-east-1"}
//
// to:
//
//	"Detected cloud environment [provider: 'aws', region: 'us-east-1']"
type MsgInterpolationHandler struct {
	slog.Handler

	enabled bool
}

// NewMsgInterpolationHandler wraps inner and reads ENTIRE_LOG_ATTRS_IN_MSG
// once at construction time.
func NewMsgInterpolationHandler(inner slog.Handler) *MsgInterpolationHandler {
	return &MsgInterpolationHandler{
		Handler: inner,
		enabled: os.Getenv("ENTIRE_LOG_ATTRS_IN_MSG") == "true",
	}
}

func (h *MsgInterpolationHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.enabled {
		r = interpolateMsg(r)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *MsgInterpolationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &MsgInterpolationHandler{Handler: h.Handler.WithAttrs(attrs), enabled: h.enabled}
}

func (h *MsgInterpolationHandler) WithGroup(name string) slog.Handler {
	return &MsgInterpolationHandler{Handler: h.Handler.WithGroup(name), enabled: h.enabled}
}

// interpolateMsg appends attrs to the message as [key: 'value', ...].
func interpolateMsg(r slog.Record) slog.Record {
	var pairs []string
	r.Attrs(func(a slog.Attr) bool {
		pairs = append(pairs, fmt.Sprintf("%s: '%s'", a.Key, a.Value.String()))
		return true
	})
	if len(pairs) > 0 {
		r = r.Clone()
		r.Message = r.Message + " [" + strings.Join(pairs, ", ") + "]"
	}
	return r
}
