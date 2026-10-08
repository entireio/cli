package logging

import (
	"context"
	"testing"
)

// testComponent and testAgent are defined in logger_test.go

func TestAttrsFromContext(t *testing.T) {
	ctx := context.Background()
	ctx = WithComponent(ctx, testComponent)
	ctx = WithAgent(ctx, testAgent)

	attrs := attrsFromContext(ctx)

	if len(attrs) != 2 {
		t.Errorf("attrsFromContext() returned %d attrs, want 2", len(attrs))
	}

	attrMap := make(map[string]string)
	for _, attr := range attrs {
		attrMap[attr.Key] = attr.Value.String()
	}

	if attrMap["component"] != testComponent {
		t.Errorf("component = %q, want %q", attrMap["component"], testComponent)
	}
	if attrMap["agent"] != testAgent {
		t.Errorf("agent = %q, want %q", attrMap["agent"], testAgent)
	}
}

func TestAttrsFromContext_Empty(t *testing.T) {
	attrs := attrsFromContext(context.Background())
	if len(attrs) != 0 {
		t.Errorf("attrsFromContext() on empty context returned %d attrs, want 0", len(attrs))
	}
}
