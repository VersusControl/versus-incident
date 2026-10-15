package controllers

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

type ChatKubernetesServiceResolver func(orgID, clusterID string) *kubernetes.Service

var (
	chatKubernetesResolverMu sync.RWMutex
	chatKubernetesResolver   ChatKubernetesServiceResolver
	chatKubernetesRegistry   *kubernetes.ServiceRegistry
)

func SetChatKubernetesRegistry(registry *kubernetes.ServiceRegistry) {
	chatKubernetesResolverMu.Lock()
	chatKubernetesRegistry = registry
	chatKubernetesResolverMu.Unlock()
}

// SetChatKubernetesServiceResolver installs the scoped service resolver used to validate resource attachments.
func SetChatKubernetesServiceResolver(resolver ChatKubernetesServiceResolver) {
	chatKubernetesResolverMu.Lock()
	chatKubernetesResolver = resolver
	chatKubernetesResolverMu.Unlock()
}

func validateChatKubernetesAttachment(ctx *fiber.Ctx, attachment *core.ChatAttachment) error {
	if attachment == nil || attachment.Resource == nil {
		return nil
	}
	resource := attachment.Resource
	resource.Break = nil
	if resource.Provider != "kubernetes" || resource.ResourceID == "" || resource.Name == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid resource attachment"})
	}
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureView))
	if !explicit || !allowed {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "infrastructure:view permission is required"})
	}
	chatKubernetesResolverMu.RLock()
	resolver := chatKubernetesResolver
	registry := chatKubernetesRegistry
	chatKubernetesResolverMu.RUnlock()
	if resolver == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Kubernetes connector is not configured"})
	}
	orgID := tenancy.NormalizeOrgID(middleware.OrgFromContext(ctx))
	if registry != nil && registry.Multiple() && resource.Cluster == "" {
		return writeKubernetes(ctx, nil, kubernetes.ErrClusterRequired)
	}
	if resource.Cluster != "" && core.CallerAuthorizationPresent(ctx.UserContext()) && !core.CallerClusterAllowed(ctx.UserContext(), resource.Cluster) {
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Kubernetes cluster is unavailable"})
	}
	service := resolver(orgID, resource.Cluster)
	if service == nil {
		if registry != nil && registry.Multiple() {
			return writeKubernetes(ctx, nil, kubernetes.ErrClusterNotFound)
		}
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Kubernetes cluster is not available to this organization"})
	}
	scope := service.Scope()
	if core.CallerAuthorizationPresent(ctx.UserContext()) && !core.CallerClusterAllowed(ctx.UserContext(), scope.ClusterID) {
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Kubernetes cluster is unavailable"})
	}
	if tenancy.NormalizeOrgID(scope.OrgID) != orgID {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Kubernetes cluster is not available to this organization"})
	}
	if resource.Cluster == "" {
		resource.Cluster = scope.ClusterID
	}
	if resource.Cluster != scope.ClusterID {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Kubernetes cluster is not available to this organization"})
	}
	if resource.Namespace != "" && (strings.TrimSpace(resource.Namespace) != resource.Namespace || strings.ContainsAny(resource.Namespace, "/\\\x00\r\n")) || strings.TrimSpace(resource.Name) != resource.Name || strings.ContainsAny(resource.Name, "/\\\x00\r\n") {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid resource attachment"})
	}
	discovery, err := service.Discover(ctx.UserContext())
	if err != nil {
		return ctx.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "Kubernetes resource discovery unavailable"})
	}
	var definition *kubernetes.ResourceDefinition
	for index := range discovery.Resources {
		if discovery.Resources[index].ID == resource.ResourceID && discovery.Resources[index].Available {
			definition = &discovery.Resources[index]
			break
		}
	}
	if definition == nil || definition.Namespaced && resource.Namespace == "" || !definition.Namespaced && resource.Namespace != "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "resource attachment does not match a readable Kubernetes resource"})
	}
	if resource.IncludeChanges {
		if resource.UID == "" {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "resource UID is required for related changes"})
		}
		issues, err := service.Issues(ctx.UserContext(), kubernetes.IssueOptions{RootUID: resource.UID, Namespace: resource.Namespace, IncludeChanges: true})
		if err != nil {
			return writeKubernetes(ctx, nil, err)
		}
		if len(issues.Items) != 1 || issues.Items[0].Root.Kind != definition.Kind || issues.Items[0].Root.Name != resource.Name || issues.Items[0].Root.Namespace != resource.Namespace {
			return writeKubernetes(ctx, nil, kubernetes.ErrNotFound)
		}
		resource.Break = chatBreakContext(issues.Items[0])
	}
	return nil
}

func chatBreakContext(issue kubernetes.Issue) *core.ChatResourceBreak {
	context := &core.ChatResourceBreak{Rule: issue.Rule, Severity: issue.Severity, BrokeAt: issue.BrokeAt, Count: issue.Count, Of: issue.Of, MoreChanges: issue.MoreChanges, History: issue.History, Rebuilt: append([]string(nil), issue.Rebuilt...), Untracked: append([]string(nil), issue.Untracked...), Partial: issue.Partial, Changes: []core.ChatResourceChange{}}
	context.BrokeAtSource = issue.BrokeAtSource
	for _, linked := range issue.Changes[:min(3, len(issue.Changes))] {
		change := core.ChatResourceChange{Kind: linked.Change.Kind, Namespace: linked.Change.Namespace, Name: linked.Change.Name, Type: string(linked.Change.Type), At: linked.Change.At, Relation: linked.Relation, LeadSeconds: linked.LeadSec}
		for _, field := range linked.Change.Fields {
			change.Fields = append(change.Fields, core.ChatResourceChangeField{Path: field.Path, From: field.From, To: field.To})
		}
		context.Changes = append(context.Changes, change)
	}
	if issue.Evidence != nil {
		context.Evidence = &core.ChatResourceEvidence{Warnings: issue.Evidence.Warnings, From: issue.Evidence.From, To: issue.Evidence.To}
		for _, item := range issue.Evidence.Terminations {
			context.Evidence.Terminations = append(context.Evidence.Terminations, core.ChatResourceTermination{Reason: item.Reason, ExitCode: item.ExitCode, Count: item.Count, Last: item.Last})
		}
		for _, item := range issue.Evidence.Waiting {
			context.Evidence.Waiting = append(context.Evidence.Waiting, core.ChatResourceWaiting{Reason: item.Reason, Count: item.Count})
		}
	}
	for {
		encoded, err := json.Marshal(context)
		if err == nil && len(encoded) <= 6<<10 {
			return context
		}
		context.Partial = true
		if len(context.Changes) > 0 {
			context.Changes = context.Changes[:len(context.Changes)-1]
			context.MoreChanges++
			continue
		}
		context.Evidence = nil
		return context
	}
}
