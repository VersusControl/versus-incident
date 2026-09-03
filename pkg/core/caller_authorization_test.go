package core

import (
	"context"
	"testing"
)

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
