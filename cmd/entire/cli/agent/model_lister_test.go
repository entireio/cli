package agent

import (
	"context"
	"testing"
)

type modelListerStub struct {
	Agent

	models []ModelInfo
	err    error
}

func (modelListerStub) DeclaredCapabilities() DeclaredCaps { return DeclaredCaps{} }

func (s modelListerStub) ListModels(context.Context) ([]ModelInfo, error) {
	return s.models, s.err
}

func TestAsModelLister(t *testing.T) {
	t.Parallel()

	t.Run("nil agent", func(t *testing.T) {
		t.Parallel()
		lister, ok := AsModelLister(nil)
		if ok || lister != nil {
			t.Errorf("AsModelLister(nil) = (%v, %v), want (nil, false)", lister, ok)
		}
	})

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		lister, ok := AsModelLister(&mockBaseAgent{})
		if ok || lister != nil {
			t.Errorf("AsModelLister(base agent) = (%v, %v), want (nil, false)", lister, ok)
		}
	})

	t.Run("ignores capability declarations", func(t *testing.T) {
		t.Parallel()
		lister, ok := AsModelLister(modelListerStub{models: []ModelInfo{{ID: "test-model"}}})
		if !ok || lister == nil {
			t.Fatalf("AsModelLister(declarer) = (%v, %v), want non-nil/true", lister, ok)
		}
		models, err := lister.ListModels(context.Background())
		if err != nil {
			t.Fatalf("ListModels: %v", err)
		}
		if len(models) != 1 || models[0].ID != "test-model" {
			t.Fatalf("ListModels() = %+v, want test-model", models)
		}
	})
}
