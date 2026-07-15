package settings

import (
	"strings"
	"testing"
)

func TestLoadFromBytes_PluginsAllowlist(t *testing.T) {
	t.Parallel()
	data := []byte(`{
		"plugins": {
			"notify": {"enabled": true, "capabilities": ["http", "fs"]},
			"linter": {"enabled": false}
		}
	}`)
	s, err := LoadFromBytes(data)
	if err != nil {
		t.Fatalf("LoadFromBytes() error = %v", err)
	}
	if !s.IsPluginEnabled("notify") {
		t.Error("notify should be enabled")
	}
	if s.IsPluginEnabled("linter") {
		t.Error("linter should be disabled")
	}
	if s.IsPluginEnabled("absent") {
		t.Error("absent plugin should not be enabled")
	}

	grant, ok := s.PluginGrant("notify")
	if !ok {
		t.Fatal("expected notify grant present")
	}
	if !grant.HasCapability(PluginCapabilityHTTP) || !grant.HasCapability(PluginCapabilityFS) {
		t.Errorf("notify missing capabilities: %+v", grant.Capabilities)
	}
	if grant.HasCapability(PluginCapabilityExec) {
		t.Error("notify should not have exec capability")
	}
}

func TestLoadFromBytes_RejectsUnknownCapability(t *testing.T) {
	t.Parallel()
	data := []byte(`{"plugins": {"bad": {"enabled": true, "capabilities": ["telepathy"]}}}`)
	_, err := LoadFromBytes(data)
	if err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Fatalf("expected unknown capability error, got %v", err)
	}
}

func TestLoadFromBytes_RejectsUnknownPluginField(t *testing.T) {
	t.Parallel()
	// DisallowUnknownFields must reject a typo'd PluginSettings field.
	data := []byte(`{"plugins": {"p": {"enable": true}}}`)
	_, err := LoadFromBytes(data)
	if err == nil {
		t.Fatal("expected error for unknown plugin field 'enable'")
	}
}

func TestPluginGrant_NilSafe(t *testing.T) {
	t.Parallel()
	var s *EntireSettings
	if _, ok := s.PluginGrant("x"); ok {
		t.Error("nil settings should report no grant")
	}
	if s.IsPluginEnabled("x") {
		t.Error("nil settings should report plugin disabled")
	}
}
