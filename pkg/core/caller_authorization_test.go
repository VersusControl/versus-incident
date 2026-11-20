package core

import (
	"context"
	"testing"
)

func TestCallerClusterScope(t *testing.T) {
	if CallerClusterAllowed(context.Background(), "one") {
		t.Fatal("background authorized")
	}
	unrestricted := WithCallerAuthorization(context.Background(), CallerAuthorization{Authenticated: true})
	if !CallerClusterAllowed(unrestricted, "any") {
		t.Fatal("nil scope denied")
	}
	empty := WithCallerAuthorization(context.Background(), CallerAuthorization{Authenticated: true, Clusters: &ClusterScope{}})
	if CallerClusterAllowed(empty, "one") {
		t.Fatal("explicit empty scope authorized")
	}
	scope := &ClusterScope{IDs: []string{"one"}}
	ctx := WithCallerAuthorization(context.Background(), CallerAuthorization{Authenticated: true, Clusters: scope})
	scope.IDs[0] = "two"
	if !CallerClusterAllowed(ctx, "one") || CallerClusterAllowed(ctx, "two") {
		t.Fatal("scope not copied")
	}
}

func TestCallerAuthorizationFailsClosedAndCopiesPermissions(t *testing.T) {
	if CallerAuthorized(context.Background(), PermissionInfrastructureView) {
		t.Fatal("background caller was authorized")
	}
	permissions := map[Permission]bool{PermissionInfrastructureView: true}
	ctx := WithCallerAuthorization(context.Background(), CallerAuthorization{Authenticated: true, Permissions: permissions})
	permissions[PermissionInfrastructureView] = false
	if !CallerAuthorized(ctx, PermissionInfrastructureView) {
		t.Fatal("authorized caller was denied or permission map was not copied")
	}
	if CallerAuthorized(WithCallerAuthorization(context.Background(), CallerAuthorization{Permissions: map[Permission]bool{PermissionInfrastructureView: true}}), PermissionInfrastructureView) {
		t.Fatal("unauthenticated caller was authorized")
	}
}

func TestCallerAccountScope(t *testing.T) {
	if CallerAccountScope(context.Background()) != nil {
		t.Fatal("a background caller has an account scope")
	}
	if CallerAccountScope(WithCallerAuthorization(context.Background(), CallerAuthorization{Authenticated: true})) != nil {
		t.Fatal("a caller without a scope is limited")
	}
	empty := CallerAccountScope(WithCallerAuthorization(context.Background(), CallerAuthorization{Authenticated: true, Accounts: &AccountScope{}}))
	if empty == nil || len(empty.IDs) != 0 {
		t.Fatalf("an explicit empty scope reads every account: %+v", empty)
	}
	scope := &AccountScope{IDs: []string{"111111111111"}}
	ctx := WithCallerAuthorization(context.Background(), CallerAuthorization{Authenticated: true, Accounts: scope})
	scope.IDs[0] = "222222222222"
	got := CallerAccountScope(ctx)
	if len(got.IDs) != 1 || got.IDs[0] != "111111111111" {
		t.Fatalf("scope not copied into the context: %+v", got)
	}
	got.IDs[0] = "333333333333"
	if again := CallerAccountScope(ctx); again.IDs[0] != "111111111111" {
		t.Fatalf("scope not copied out of the context: %+v", again)
	}
}
