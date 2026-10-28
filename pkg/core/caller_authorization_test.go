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
