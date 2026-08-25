package repopolicy

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"testing"
)

func TestClassifyRepoPolicy_ReadOnlyDoesNotCreateRegistry(t *testing.T) {
	root, repository := newPolicyRepo(t)
	setPolicyGlobal(t, `{"global":{"enabled":true}}`)
	policy := policyAt(t, root)
	if !policy.Active || policy.Route.Layout != RuntimeGitCommon {
		t.Fatalf("policy = %+v, want active proposed git-common route", policy)
	}
	if _, err := os.Stat(registryDir(repository)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only classification created registry or returned unexpected error: %v", err)
	}
}

func TestRepoPolicyContext_RoundTripsSnapshot(t *testing.T) {
	t.Parallel()
	want := RepoPolicy{Active: true, ActivationSource: ActivationGlobal}
	ctx := WithRepoPolicy(t.Context(), want)
	got, ok := RepoPolicyFromContext(ctx)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("RepoPolicyFromContext = %+v, %v; want %+v", got, ok, want)
	}
}
