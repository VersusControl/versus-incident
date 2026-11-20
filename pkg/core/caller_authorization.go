package core

import (
	"context"
	"strings"
)

// Permission is an application capability evaluated for the current caller.
type Permission string

const (
	PermissionInfrastructureView          Permission = "infrastructure:view"
	PermissionInfrastructureSettingsWrite Permission = "infrastructure:settings:write"
	PermissionServiceHealthSettingsWrite  Permission = "service-health:settings:write"
	PermissionAgentApprove                Permission = "agent:approve"
	// PermissionFinOpsView reads cost data without the wider infrastructure view.
	PermissionFinOpsView Permission = "finops:view"
)

type callerAuthorizationKey struct{}

// CallerAuthorization is a request-scoped, role-neutral permission decision.
type CallerAuthorization struct {
	Authenticated bool
	Actor         string
	Permissions   map[Permission]bool
	Clusters      *ClusterScope
	// Accounts limits the cloud accounts whose cost the caller may read.
	// Nil means every account; an empty list means none.
	Accounts *AccountScope
	// ToolGroups limits the agent tools the caller may use to those in the
	// listed groups. Nil means no restriction; an empty list allows no tool.
	ToolGroups []string
}

type ClusterScope struct{ IDs []string }

// AccountScope lists the cloud account IDs a caller may read.
type AccountScope struct{ IDs []string }

// CallerAccountScope returns a copy of the caller's account scope, or nil when
// the caller may read every account (or no caller decision is present).
func CallerAccountScope(ctx context.Context) *AccountScope {
	authorization, ok := ctx.Value(callerAuthorizationKey{}).(CallerAuthorization)
	if !ok || authorization.Accounts == nil {
		return nil
	}
	return &AccountScope{IDs: append([]string{}, authorization.Accounts.IDs...)}
}

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
	if authorization.Accounts != nil {
		scope := &AccountScope{IDs: make([]string, len(authorization.Accounts.IDs))}
		for index, id := range authorization.Accounts.IDs {
			scope.IDs[index] = strings.Clone(id)
		}
		authorization.Accounts = scope
	}
	if authorization.ToolGroups != nil {
		groups := make([]string, len(authorization.ToolGroups))
		for index, group := range authorization.ToolGroups {
			groups[index] = strings.Clone(group)
		}
		authorization.ToolGroups = groups
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

// CallerToolGroups returns a copy of the tool groups the caller is limited to,
// or nil when the caller's tools are not restricted (or no caller decision is
// present).
func CallerToolGroups(ctx context.Context) []string {
	authorization, ok := ctx.Value(callerAuthorizationKey{}).(CallerAuthorization)
	if !ok || authorization.ToolGroups == nil {
		return nil
	}
	return append([]string{}, authorization.ToolGroups...)
}

// CallerToolGroupAllowed reports whether the caller may use a tool in group. A
// caller without a tool restriction may use every group; a restricted caller
// may use only the listed groups, and never a tool with no group.
func CallerToolGroupAllowed(ctx context.Context, group string) bool {
	authorization, ok := ctx.Value(callerAuthorizationKey{}).(CallerAuthorization)
	if !ok || authorization.ToolGroups == nil {
		return true
	}
	if group == "" {
		return false
	}
	for _, allowed := range authorization.ToolGroups {
		if allowed == group {
			return true
		}
	}
	return false
}
