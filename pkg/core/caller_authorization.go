package core

import (
	"context"
	"strings"
)

// Permission is an application capability evaluated for the current caller.
type Permission string

const (
	PermissionInfrastructureView         Permission = "infrastructure:view"
	PermissionServiceHealthSettingsWrite Permission = "service-health:settings:write"
	PermissionAgentApprove               Permission = "agent:approve"
)

type callerAuthorizationKey struct{}

// CallerAuthorization is a request-scoped, role-neutral permission decision.
type CallerAuthorization struct {
	Authenticated bool
	Actor         string
	Permissions   map[Permission]bool
	Clusters      *ClusterScope
}

type ClusterScope struct{ IDs []string }

func CallerAuthorizationPresent(ctx context.Context) bool {
	_, ok := ctx.Value(callerAuthorizationKey{}).(CallerAuthorization)
	return ok
}

func CallerClusterScope(ctx context.Context) *ClusterScope {
	authorization, ok := ctx.Value(callerAuthorizationKey{}).(CallerAuthorization)
	if !ok || authorization.Clusters == nil {
		return nil
	}
	return &ClusterScope{IDs: append([]string{}, authorization.Clusters.IDs...)}
}

func CallerClusterAllowed(ctx context.Context, clusterID string) bool {
	authorization, ok := ctx.Value(callerAuthorizationKey{}).(CallerAuthorization)
	if !ok || !authorization.Authenticated {
		return false
	}
	if authorization.Clusters == nil {
		return true
	}
	for _, id := range authorization.Clusters.IDs {
		if id == clusterID {
			return true
		}
	}
	return false
}

// WithCallerAuthorization carries one caller decision into HTTP and model tools.
func WithCallerAuthorization(ctx context.Context, authorization CallerAuthorization) context.Context {
	copyPermissions := make(map[Permission]bool, len(authorization.Permissions))
	for permission, allowed := range authorization.Permissions {
		copyPermissions[permission] = allowed
	}
	authorization.Actor = strings.Clone(strings.TrimSpace(authorization.Actor))
	authorization.Permissions = copyPermissions
	if authorization.Clusters != nil {
		scope := &ClusterScope{IDs: make([]string, len(authorization.Clusters.IDs))}
		for index, id := range authorization.Clusters.IDs {
			scope.IDs[index] = strings.Clone(id)
		}
		authorization.Clusters = scope
	}
	return context.WithValue(ctx, callerAuthorizationKey{}, authorization)
}

func CallerActor(ctx context.Context) string {
	authorization, ok := ctx.Value(callerAuthorizationKey{}).(CallerAuthorization)
	if !ok || !authorization.Authenticated {
		return ""
	}
	return authorization.Actor
}

// CallerAuthorized returns false for absent/background callers and explicit denials.
func CallerAuthorized(ctx context.Context, permission Permission) bool {
	authorization, ok := ctx.Value(callerAuthorizationKey{}).(CallerAuthorization)
	return ok && authorization.Authenticated && authorization.Permissions[permission]
}
