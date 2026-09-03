package core

import "context"

// Permission is an application capability evaluated for the current caller.
type Permission string

const PermissionInfrastructureView Permission = "infrastructure:view"

type callerAuthorizationKey struct{}

// CallerAuthorization is a request-scoped, role-neutral permission decision.
type CallerAuthorization struct {
	Authenticated bool
	Permissions   map[Permission]bool
}

// WithCallerAuthorization carries one caller decision into HTTP and model tools.
func WithCallerAuthorization(ctx context.Context, authorization CallerAuthorization) context.Context {
	copyPermissions := make(map[Permission]bool, len(authorization.Permissions))
	for permission, allowed := range authorization.Permissions {
		copyPermissions[permission] = allowed
	}
	authorization.Permissions = copyPermissions
	return context.WithValue(ctx, callerAuthorizationKey{}, authorization)
}

// CallerAuthorized returns false for absent/background callers and explicit denials.
func CallerAuthorized(ctx context.Context, permission Permission) bool {
	authorization, ok := ctx.Value(callerAuthorizationKey{}).(CallerAuthorization)
	return ok && authorization.Authenticated && authorization.Permissions[permission]
}
