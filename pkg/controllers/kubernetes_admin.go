package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	kubernetesindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/middleware"

	"github.com/gofiber/fiber/v2"
)

const kubernetesServiceContextKey = "versus.kubernetes.service"

// KubernetesAdminController adapts read-only HTTP requests to the shared service.
type KubernetesAdminController struct {
	resolve  func(string, string) (*kubernetes.Service, error)
	registry *kubernetes.ServiceRegistry
}

func NewKubernetesAdminController(service *kubernetes.Service) *KubernetesAdminController {
	controller := NewKubernetesAdminControllerWithRegistry(kubernetes.NewServiceRegistry(service))
	controller.resolve = func(orgID, clusterID string) (*kubernetes.Service, error) {
		if service == nil {
			return nil, kubernetes.ErrClusterUnavailable
		}
		if clusterID != "" && clusterID != service.Scope().ClusterID {
			return nil, kubernetes.ErrClusterNotFound
		}
		return service, nil
	}
	return controller
}

// NewKubernetesAdminControllerWithRegistry resolves an org-scoped service for every request.
func NewKubernetesAdminControllerWithRegistry(registry *kubernetes.ServiceRegistry) *KubernetesAdminController {
	return &KubernetesAdminController{resolve: registry.ResolveCluster, registry: registry}
}

func (controller *KubernetesAdminController) Register(router fiber.Router) {
	group := router.Group("/admin/kubernetes", adminGatewayGuard, controller.requirePermission)
	group.Get("/clusters", controller.clusters)
	group.Use(controller.requireInfrastructureView)
	group.Get("/overview", controller.overview)
	group.Get("/stream", controller.stream)
	group.Get("/changes", controller.changes)
	group.Get("/graph", controller.graph)
	group.Get("/graph/overview", controller.overviewGraph)
	group.Get("/graph/neighborhood", controller.neighborhood)
	group.Get("/issues", controller.issues)
	group.Get("/top", controller.top)
	group.Get("/gitops/apps", controller.gitOpsApps)
	group.Get("/rollouts", controller.rollouts)
	group.Get("/rollouts/:namespace/:name", controller.rollout)
	group.Get("/traffic", controller.traffic)
	group.Get("/releases", controller.releases)
	group.Get("/releases/:namespace/:name", controller.release)
	group.Get("/diagnose/:resourceId/:name", controller.diagnose)
	group.Get("/resources/discovery", controller.discovery)
	group.Get("/resources/search", controller.search)
	group.Get("/resources", controller.list)
	group.Get("/resources/:resourceId/:name", controller.get)
	group.Get("/resources/:resourceId/:name/describe", controller.describe)
	group.Get("/workloads", controller.workloads)
	group.Get("/workloads/:kind/:name", controller.workload)
	group.Get("/workloads/:kind/:name/logs", controller.workloadLogs)
	group.Get("/events", controller.events)
	group.Get("/pods/:namespace/:name/logs", controller.logs)
	group.Get("/pods/:namespace/:name/logs/stream", controller.podLogStream)
	group.Get("/usage", controller.usage)
}

func (controller *KubernetesAdminController) requireInfrastructureView(ctx *fiber.Ctx) error {
	var service *kubernetes.Service
	var err error
	clusterID := strings.Clone(ctx.Query("cluster"))
	if clusterID != "" && !core.CallerClusterAllowed(ctx.UserContext(), clusterID) {
		return writeKubernetes(ctx, nil, kubernetes.ErrClusterNotFound)
	}
	if controller != nil && controller.resolve != nil {
		service, err = controller.resolve(middleware.OrgFromContext(ctx), clusterID)
	}
	if err != nil {
		return writeKubernetes(ctx, nil, err)
	}
	if service == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Kubernetes connector is not configured"})
	}
	if !core.CallerClusterAllowed(ctx.UserContext(), service.Scope().ClusterID) {
		return writeKubernetes(ctx, nil, kubernetes.ErrClusterNotFound)
	}
	ctx.Locals(kubernetesServiceContextKey, service)
	ctx.Locals("versus.kubernetes.multiple", controller.registry.Multiple())
	return ctx.Next()
}

func (controller *KubernetesAdminController) requirePermission(ctx *fiber.Ctx) error {
	if controller == nil || controller.registry == nil || len(controller.registry.Clusters()) == 0 {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Kubernetes connector is not configured"})
	}
	if !controller.registry.Multiple() && controller.registry.ResolveOrg(middleware.OrgFromContext(ctx)) == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Kubernetes connector is not configured"})
	}
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureView))
	if !explicit || !allowed {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "infrastructure:view permission is required"})
	}
	authorization := core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}}
	if !core.CallerAuthorizationPresent(ctx.UserContext()) {
		ctx.SetUserContext(core.WithCallerAuthorization(ctx.UserContext(), authorization))
	}
	return ctx.Next()
}

func (controller *KubernetesAdminController) clusters(ctx *fiber.Ctx) error {
	return ctx.JSON(fiber.Map{"multiple": controller.registry.Multiple(), "clusters": controller.registry.Summaries(ctx.UserContext(), middleware.OrgFromContext(ctx), func(id string) bool { return core.CallerClusterAllowed(ctx.UserContext(), id) })})
}

func requestKubernetesService(ctx *fiber.Ctx) *kubernetes.Service {
	service, _ := ctx.Locals(kubernetesServiceContextKey).(*kubernetes.Service)
	return service
}

func (controller *KubernetesAdminController) overview(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Overview(ctx.UserContext())
	return writeKubernetes(ctx, value, err)
}

func (controller *KubernetesAdminController) changes(ctx *fiber.Ctx) error {
	since, err := parseKubernetesTime(ctx.Query("since"))
	if err != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	until, err := parseKubernetesTime(ctx.Query("until"))
	if err != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	value, err := requestKubernetesService(ctx).Changes(ctx.UserContext(), kubernetes.ChangeQuery{
		Since: since, Until: until, Namespace: ctx.Query("namespace"), Kind: ctx.Query("kind"), Name: ctx.Query("name"),
		Type: kubernetes.ChangeType(ctx.Query("type")), Limit: queryInt(ctx, "limit"), Cursor: ctx.Query("cursor"),
	})
	return writeKubernetes(ctx, value, err)
}

func (controller *KubernetesAdminController) graph(ctx *fiber.Ctx) error {
	queryArgs := ctx.Context().QueryArgs()
	completeValues := queryArgs.PeekMulti("complete")
	if len(completeValues) > 1 {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	complete := false
	if len(completeValues) == 1 {
		parsed, parseErr := strconv.ParseBool(string(completeValues[0]))
		if parseErr != nil {
			return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
		}
		complete = parsed
	}
	if complete {
		for _, name := range []string{"cursor", "limit", "max_nodes"} {
			if queryArgs.Has(name) {
				return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
			}
		}
	}
	value, err := requestKubernetesService(ctx).Graph(ctx.UserContext(), kubernetes.GraphQuery{
		Namespace: ctx.Query("namespace"), GroupBy: ctx.Query("group_by"), MaxNodes: queryInt(ctx, "max_nodes"),
		Limit: queryInt(ctx, "limit"), Cursor: ctx.Query("cursor"), ConnectedOnly: ctx.QueryBool("connected_only"),
		Complete: complete,
	})
	return writeKubernetes(ctx, value, err)
}

func (controller *KubernetesAdminController) overviewGraph(ctx *fiber.Ctx) error {
	invalid := false
	ctx.Context().QueryArgs().VisitAll(func(key, value []byte) {
		if string(key) != "cluster" {
			invalid = true
		}
	})
	if invalid {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	value, err := requestKubernetesService(ctx).OverviewGraph(ctx.UserContext())
	return writeKubernetes(ctx, value, err)
}

func (controller *KubernetesAdminController) neighborhood(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Neighborhood(ctx.UserContext(), kubernetes.NeighborhoodQuery{
		ResourceID: ctx.Query("resource_id"), Kind: ctx.Query("kind"), Namespace: ctx.Query("namespace"), Name: ctx.Query("name"),
		Hops: queryInt(ctx, "hops"), MaxNodes: queryInt(ctx, "max_nodes"),
	})
	return writeKubernetes(ctx, value, err)
}

func parseKubernetesTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, kubernetes.ErrInvalidArguments
	}
	return parsed, nil
}

func (controller *KubernetesAdminController) stream(ctx *fiber.Ctx) error {
	service := requestKubernetesService(ctx)
	streamContext := ctx.UserContext()
	namespace := strings.Clone(ctx.Query("namespace"))
	kinds := []string{"Node", "Pod", "Deployment"}
	if requested := strings.Split(strings.Clone(ctx.Query("kinds")), ","); len(requested) > 0 && requested[0] != "" {
		kinds = kinds[:0]
		for _, kind := range requested {
			kind = strings.TrimSpace(kind)
			if kind != "" && len(kinds) < 10 {
				kinds = append(kinds, strings.Clone(kind))
			}
		}
	}
	deltas, unsubscribe, status, err := service.SubscribeIndex(streamContext, 128, kinds...)
	if err != nil {
		return writeKubernetes(ctx, nil, err)
	}
	ctx.Set(fiber.HeaderContentType, "text/event-stream")
	ctx.Set(fiber.HeaderCacheControl, "no-cache")
	ctx.Set(fiber.HeaderConnection, "keep-alive")
	ctx.Set("X-Accel-Buffering", "no")
	ctx.Context().SetBodyStreamWriter(func(writer *bufio.Writer) {
		defer unsubscribe()
		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()
		poll := time.NewTicker(time.Second)
		defer poll.Stop()
		write := func(event string, value any) bool {
			encoded, marshalErr := json.Marshal(value)
			if marshalErr != nil || len(encoded) > 64<<10 {
				encoded = []byte(`{"resync":true}`)
				event = "resync"
			}
			if _, writeErr := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, encoded); writeErr != nil {
				return false
			}
			return writer.Flush() == nil
		}
		if !write("sync", status) {
			return
		}
		for {
			select {
			case <-streamContext.Done():
				return
			case delta, open := <-deltas:
				if !open {
					return
				}
				if !containsKubernetesKind(kinds, delta.Kind) {
					continue
				}
				if namespace != "" && !deltaMatchesNamespace(delta, namespace) {
					continue
				}
				if delta.Resync {
					if !write("resync", map[string]bool{"resync": true}) {
						return
					}
				} else if !write("records", delta) {
					return
				}
			case <-poll.C:
				_, refreshed, refreshErr := service.IndexSnapshot(streamContext, kinds...)
				if refreshErr == nil && !write("sync", refreshed) {
					return
				}
			case <-heartbeat.C:
				if _, writeErr := writer.WriteString(": heartbeat\n\n"); writeErr != nil || writer.Flush() != nil {
					return
				}
			}
		}
	})
	return nil
}

func containsKubernetesKind(kinds []string, target string) bool {
	for _, kind := range kinds {
		if kind == target {
			return true
		}
	}
	return false
}

func deltaMatchesNamespace(delta kubernetesindex.Delta, namespace string) bool {
	if delta.New != nil && delta.New.Namespace == namespace || delta.Old != nil && delta.Old.Namespace == namespace {
		return true
	}
	return false
}
func (controller *KubernetesAdminController) issues(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Issues(ctx.UserContext(), kubernetes.IssueOptions{
		Namespace: ctx.Query("namespace"), Severity: ctx.Query("severity"), Kind: ctx.Query("kind"), Limit: queryInt(ctx, "limit"), Cursor: ctx.Query("cursor"),
	})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) top(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Top(ctx.UserContext(), kubernetes.TopOptions{
		Kind: ctx.Query("kind"), Namespace: ctx.Query("namespace"), Sort: ctx.Query("sort"), Limit: queryInt(ctx, "limit"),
	})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) gitOpsApps(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).GitOpsApps(ctx.UserContext(), kubernetes.GitOpsAppOptions{
		Namespace: ctx.Query("namespace"), Tool: ctx.Query("tool"), Status: ctx.Query("status"),
		Limit: queryInt(ctx, "limit"), Cursor: ctx.Query("cursor"),
	})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) rollout(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Rollout(ctx.UserContext(), ctx.Params("namespace"), ctx.Params("name"))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) rollouts(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Rollouts(ctx.UserContext(), kubernetes.RolloutListOptions{
		Namespace: ctx.Query("namespace"), Limit: queryInt(ctx, "limit"), Cursor: ctx.Query("cursor"),
	})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) traffic(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Traffic(ctx.UserContext(), kubernetes.TrafficQuery{
		Namespace: ctx.Query("namespace"), Source: ctx.Query("source"), Window: ctx.Query("window"),
	})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) releases(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Releases(ctx.UserContext(), kubernetes.HelmReleaseOptions{Namespace: ctx.Query("namespace"), Status: ctx.Query("status"), Limit: queryInt(ctx, "limit"), Cursor: ctx.Query("cursor")})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) release(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Release(ctx.UserContext(), ctx.Params("namespace"), ctx.Params("name"))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) diagnose(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).DiagnoseWorkload(ctx.UserContext(), kubernetes.DiagnoseOptions{
		ResourceID: ctx.Params("resourceId"), Namespace: ctx.Query("namespace"), Name: ctx.Params("name"), LogTail: queryInt(ctx, "log_tail"),
		ChangeWindow: time.Duration(queryInt(ctx, "change_window_minutes")) * time.Minute,
	})
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
func (controller *KubernetesAdminController) workloads(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Workloads(ctx.UserContext(), kubernetes.WorkloadListOptions{
		Namespace: ctx.Query("namespace"), Kind: ctx.Query("kind"), Query: ctx.Query("q"),
		Limit: queryInt(ctx, "limit"), Cursor: ctx.Query("cursor"),
	})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) workload(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).GetWorkload(ctx.UserContext(), ctx.Query("namespace"), ctx.Params("kind"), ctx.Params("name"))
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) workloadLogs(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).WorkloadLogs(ctx.UserContext(), kubernetes.WorkloadLogOptions{
		Namespace: ctx.Query("namespace"), Kind: ctx.Params("kind"), Name: ctx.Params("name"), Pod: ctx.Query("pod"),
		Container: ctx.Query("container"), Previous: ctx.QueryBool("previous"), SinceSeconds: queryInt(ctx, "since_seconds"),
		TailLines: queryInt(ctx, "tail_lines"), Grep: ctx.Query("grep"), MaxPods: queryInt(ctx, "max_pods"),
	})
	return writeKubernetes(ctx, value, err)
}
func (controller *KubernetesAdminController) events(ctx *fiber.Ctx) error {
	typeFilter := ctx.Query("type")
	if typeFilter == "" {
		typeFilter = "Warning"
	}
	value, err := requestKubernetesService(ctx).ListEvents(ctx.UserContext(), kubernetes.EventOptions{Namespace: ctx.Query("namespace"), Type: typeFilter, Kind: ctx.Query("kind"), Name: ctx.Query("name"), UID: ctx.Query("uid"), Continue: ctx.Query("continue"), Cursor: ctx.Query("cursor"), Limit: queryInt(ctx, "limit")})
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
	multiple, _ := ctx.Locals("versus.kubernetes.multiple").(bool)
	clusterID := ""
	if service := requestKubernetesService(ctx); service != nil {
		clusterID = service.Scope().ClusterID
	}
	detail := kubernetes.DiagnoseClusterError(err, multiple, clusterID)
	log.Printf("kubernetes admin failure: code=%s retryable=%t", detail.Code, detail.Retryable)
	status := fiber.StatusBadGateway
	switch {
	case errors.Is(err, kubernetes.ErrInvalidArguments), errors.Is(err, kubernetes.ErrInvalidEndpoint), errors.Is(err, kubernetes.ErrClusterRequired):
		status = fiber.StatusBadRequest
	case errors.Is(err, kubernetes.ErrCompleteGraphLimit):
		status = fiber.StatusRequestEntityTooLarge
	case errors.Is(err, kubernetes.ErrIndexStreamBusy):
		status = fiber.StatusTooManyRequests
	case errors.Is(err, kubernetes.ErrClusterUnavailable):
		status = fiber.StatusServiceUnavailable
	case errors.Is(err, kubernetes.ErrGraphIncomplete):
		status = fiber.StatusServiceUnavailable
	case errors.Is(err, kubernetes.ErrForbidden):
		status = fiber.StatusForbidden
	case errors.Is(err, kubernetes.ErrNotFound), errors.Is(err, kubernetes.ErrClusterNotFound):
		status = fiber.StatusNotFound
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, kubernetes.ErrOperationBudget):
		status = fiber.StatusGatewayTimeout
	}
	response := fiber.Map{
		"error":     detail.Message,
		"code":      detail.Code,
		"action":    detail.Action,
		"retryable": detail.Retryable,
	}
	if errors.Is(err, kubernetes.ErrForbidden) {
		response["partial"] = true
	}
	return ctx.Status(status).JSON(response)
}
