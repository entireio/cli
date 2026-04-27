package dispatch

import (
	_ "embed"
	"strings"
)

//go:embed voices/neutral.md
var voiceNeutral string

//go:embed voices/marvin.md
var voiceMarvin string

type Voice struct {
	Name string
	Text string
}

var presets = map[string]string{
	"neutral": voiceNeutral,
	"marvin":  voiceMarvin,
}

func ResolveVoice(value string) Voice {
	if value == "" {
		return Voice{Name: "neutral", Text: voiceNeutral}
	}

	name := strings.ToLower(strings.TrimSpace(value))
	if text, ok := presets[name]; ok {
		return Voice{Name: name, Text: text}
	}

	return Voice{Text: value}
}
