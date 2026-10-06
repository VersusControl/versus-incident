package controllers

import (
	"strings"
	"sync"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

type ChatKubernetesServiceResolver func(orgID string) *kubernetes.Service

var (
	chatKubernetesResolverMu sync.RWMutex
	chatKubernetesResolver   ChatKubernetesServiceResolver
)

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
	if resource.Provider != "kubernetes" || resource.ResourceID == "" || resource.Name == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid resource attachment"})
	}
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureView))
	if !explicit || !allowed {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "infrastructure:view permission is required"})
	}
	chatKubernetesResolverMu.RLock()
	resolver := chatKubernetesResolver
	chatKubernetesResolverMu.RUnlock()
	if resolver == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Kubernetes connector is not configured"})
	}
	orgID := tenancy.NormalizeOrgID(middleware.OrgFromContext(ctx))
	service := resolver(orgID)
	if service == nil {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Kubernetes cluster is not available to this organization"})
	}
	scope := service.Scope()
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
	return nil
}
