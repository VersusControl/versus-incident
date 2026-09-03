package controllers

import (
	"context"
	"errors"
	"strconv"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"

	"github.com/gofiber/fiber/v2"
)

const kubernetesServiceContextKey = "versus.kubernetes.service"

// KubernetesAdminController adapts read-only HTTP requests to the shared service.
type KubernetesAdminController struct {
	resolve func(string) *kubernetes.Service
}

func NewKubernetesAdminController(service *kubernetes.Service) *KubernetesAdminController {
	return &KubernetesAdminController{resolve: func(string) *kubernetes.Service { return service }}
}

// NewKubernetesAdminControllerWithRegistry resolves an org-scoped service for every request.
func NewKubernetesAdminControllerWithRegistry(registry *kubernetes.ServiceRegistry) *KubernetesAdminController {
	return &KubernetesAdminController{resolve: func(orgID string) *kubernetes.Service {
		if registry == nil {
			return nil
		}
		return registry.ResolveOrg(orgID)
	}}
}

func (controller *KubernetesAdminController) Register(router fiber.Router) {
	group := router.Group("/admin/kubernetes", adminGatewayGuard, controller.requireInfrastructureView)
	group.Get("/overview", controller.overview)
	group.Get("/resources/discovery", controller.discovery)
	group.Get("/resources/search", controller.search)
	group.Get("/resources", controller.list)
	group.Get("/resources/:resourceId/:name", controller.get)
	group.Get("/resources/:resourceId/:name/describe", controller.describe)
	group.Get("/workloads", controller.workloads)
	group.Get("/workloads/:kind/:name", controller.workload)
	group.Get("/topology", controller.topology)
	group.Get("/events", controller.events)
	group.Get("/pods/:namespace/:name/logs", controller.logs)
	group.Get("/usage", controller.usage)
}

func (controller *KubernetesAdminController) requireInfrastructureView(ctx *fiber.Ctx) error {
	var service *kubernetes.Service
	if controller != nil && controller.resolve != nil {
		service = controller.resolve(middleware.OrgFromContext(ctx))
	}
	if service == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Kubernetes connector is not configured"})
	}
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureView))
	if !explicit || !allowed {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "infrastructure:view permission is required"})
	}
	authorization := core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}}
	ctx.SetUserContext(core.WithCallerAuthorization(ctx.UserContext(), authorization))
	ctx.Locals(kubernetesServiceContextKey, service)
	return ctx.Next()
}

func requestKubernetesService(ctx *fiber.Ctx) *kubernetes.Service {
	service, _ := ctx.Locals(kubernetesServiceContextKey).(*kubernetes.Service)
	return service
}

func (controller *KubernetesAdminController) overview(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Overview(ctx.UserContext())
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) discovery(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Discover(ctx.UserContext())
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) list(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).List(ctx.UserContext(), listOptions(ctx))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) search(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Search(ctx.UserContext(), kubernetes.SearchOptions{Query: ctx.Query("q"), Namespace: ctx.Query("namespace"), Category: ctx.Query("category"), Labels: ctx.Query("labels"), Fields: ctx.Query("fields"), PerKindLimit: queryInt(ctx, "per_kind_limit"), TotalLimit: queryInt(ctx, "limit")})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) get(ctx *fiber.Ctx) error {
	if ctx.QueryBool("diagnostic") {
		return controller.describe(ctx)
	}
	value, err := requestKubernetesService(ctx).Get(ctx.UserContext(), ctx.Params("resourceId"), ctx.Query("namespace"), ctx.Params("name"))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) describe(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Describe(ctx.UserContext(), ctx.Params("resourceId"), ctx.Query("namespace"), ctx.Params("name"))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) topology(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Topology(ctx.UserContext(), ctx.Query("namespace"), queryInt(ctx, "node_cap"), queryInt(ctx, "edge_cap"))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) workloads(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).ListWorkloads(ctx.UserContext(), ctx.Query("namespace"), ctx.Query("kind"), queryInt(ctx, "limit"))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) workload(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).GetWorkload(ctx.UserContext(), ctx.Query("namespace"), ctx.Params("kind"), ctx.Params("name"))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) events(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).ListEvents(ctx.UserContext(), kubernetes.EventOptions{Namespace: ctx.Query("namespace"), Type: ctx.Query("type"), Kind: ctx.Query("kind"), Name: ctx.Query("name"), UID: ctx.Query("uid"), Continue: ctx.Query("continue"), Limit: queryInt(ctx, "limit")})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) logs(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).PodLogs(ctx.UserContext(), ctx.Params("namespace"), ctx.Params("name"), ctx.Query("container"), ctx.QueryBool("previous"), queryInt(ctx, "since_seconds"), queryInt(ctx, "tail_lines"))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) usage(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Usage(ctx.UserContext(), ctx.Query("namespace"), queryInt(ctx, "limit"))
	return writeKubernetes(ctx, value, err)
}

func listOptions(ctx *fiber.Ctx) kubernetes.ListOptions {
	return kubernetes.ListOptions{ResourceID: ctx.Query("resource_id"), Namespace: ctx.Query("namespace"), Labels: ctx.Query("labels"), Fields: ctx.Query("fields"), Continue: ctx.Query("continue"), Limit: queryInt(ctx, "limit")}
}
func queryInt(ctx *fiber.Ctx, name string) int {
	value, _ := strconv.Atoi(ctx.Query(name))
	return value
}
func writeKubernetes(ctx *fiber.Ctx, value any, err error) error {
	if err == nil {
		return ctx.JSON(value)
	}
	switch {
	case errors.Is(err, kubernetes.ErrInvalidArguments), errors.Is(err, kubernetes.ErrInvalidEndpoint):
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid arguments"})
	case errors.Is(err, kubernetes.ErrForbidden):
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden", "partial": true})
	case errors.Is(err, kubernetes.ErrUnauthorized):
		return ctx.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "cluster authentication failed"})
	case errors.Is(err, kubernetes.ErrNotFound):
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "resource unavailable"})
	case errors.Is(err, context.DeadlineExceeded):
		return ctx.Status(fiber.StatusGatewayTimeout).JSON(fiber.Map{"error": "Kubernetes read timed out"})
	default:
		return ctx.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "Kubernetes read failed"})
	}
}
