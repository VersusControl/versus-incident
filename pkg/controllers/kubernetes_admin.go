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
	kuberneteschanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
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
	// The health report is the one Kubernetes answer that has to survive a dead
	// credential: it carries the cluster's durable first ready moment, which is
	// local state recorded before the fault. It is registered ahead of the strict
	// chain below, which keeps its exact behaviour for every other route.
	router.Get("/admin/kubernetes/sync", auditKubernetesSettings, adminGatewayGuard, controller.requireReportPermission, controller.requireSyncHealthView, controller.syncHealth)
	group := router.Group("/admin/kubernetes", auditKubernetesSettings, adminGatewayGuard, controller.requirePermission)
	group.Get("/clusters", controller.clusters)
	group.Use(controller.requireInfrastructureView)
	group.Get("/overview", controller.overview)
	group.Get("/namespaces", controller.namespaces)
	group.Get("/stream", controller.stream)
	group.Get("/changes", controller.changes)
	group.Get("/timeline", controller.timeline)
	group.Get("/crashes", controller.crashes)
	group.Get("/break-logs", controller.breakLogs)
	group.Get("/agent-settings", controller.agentSettings)
	group.Get("/issue-alerts", controller.issueAlerts)
	group.Put("/issue-alerts", controller.requireSettingsWrite, controller.saveIssueAlerts)
	group.Put("/agent-settings", controller.requireSettingsWrite, controller.saveAgentSettings)
	group.Post("/agent-settings/test-link", controller.requireSettingsWrite, controller.testLogLink)
	group.Get("/graph", controller.graph)
	group.Get("/graph/overview", controller.overviewGraph)
	group.Get("/graph/neighborhood", controller.neighborhood)
	group.Get("/graph/related", controller.related)
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
	group.Get("/usage-history", controller.usageHistory)
	group.Get("/workloads/:kind/:name/logs/stream", controller.workloadLogStream)
}

func (controller *KubernetesAdminController) requireInfrastructureView(ctx *fiber.Ctx) error {
	return controller.requireClusterView(ctx, false)
}

// requireSyncHealthView installs the cluster a health report is about. It answers
// the one question that survives a dead credential, so a credential fault falls
// back to the cluster's configured service instead of failing the request.
func (controller *KubernetesAdminController) requireSyncHealthView(ctx *fiber.Ctx) error {
	return controller.requireClusterView(ctx, true)
}

// requireClusterView installs the resolved cluster for a route. A report route
// (reportable) is answered even when the credential is dead: resolution is strict
// first, and only a credential fault falls back to the cluster's configured
// service, which the report uses for local state alone — the published generation
// and the inventory record — and never for a cluster read. Every other fault, and
// every non-report route, keeps its existing answer.
func (controller *KubernetesAdminController) requireClusterView(ctx *fiber.Ctx, reportable bool) error {
	clusterID := strings.Clone(ctx.Query("cluster"))
	if clusterID != "" && !core.CallerClusterAllowed(ctx.UserContext(), clusterID) {
		return writeKubernetes(ctx, nil, kubernetes.ErrClusterNotFound)
	}
	var service *kubernetes.Service
	var err error
	if controller != nil && controller.resolve != nil {
		service, err = controller.resolve(middleware.OrgFromContext(ctx), clusterID)
	}
	if err != nil {
		credentialFault := errors.Is(err, kubernetes.ErrCredentialUnavailable) || errors.Is(err, kubernetes.ErrUnauthorized)
		if !reportable || !credentialFault {
			return writeKubernetes(ctx, nil, connectorResolutionError(err))
		}
		service, err = controller.registry.LocalReportService(middleware.OrgFromContext(ctx), clusterID)
		if err != nil {
			return writeKubernetes(ctx, nil, connectorResolutionError(err))
		}
	}
	if service == nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrConnectorNotConfigured)
	}
	if !core.CallerClusterAllowed(ctx.UserContext(), service.Scope().ClusterID) {
		return writeKubernetes(ctx, nil, kubernetes.ErrClusterNotFound)
	}
	ctx.Locals(kubernetesServiceContextKey, service)
	ctx.Locals("versus.kubernetes.multiple", controller.registry.Multiple())
	return ctx.Next()
}

func (controller *KubernetesAdminController) requirePermission(ctx *fiber.Ctx) error {
	// The permission is answered before anything about the connector is looked
	// up. Whether a connector exists and whether its credential still works are
	// facts about the deployment, so reading them first would tell a caller
	// without the permission which of them it is looking at.
	if !callerCanViewInfrastructure(ctx) {
		return refuseCallerInfrastructureView(ctx)
	}
	if controller == nil || controller.registry == nil || len(controller.registry.Clusters()) == 0 {
		return writeKubernetes(ctx, nil, kubernetes.ErrConnectorNotConfigured)
	}
	if !controller.registry.Multiple() {
		// Resolving the org is where the credential is validated, so it is also
		// where a configured cluster with a dead credential fails. That answer has
		// to name the fault: "not configured" would send a surface to the setup
		// card over a cluster that has data.
		if _, err := controller.registry.ResolveCluster(middleware.OrgFromContext(ctx), ""); err != nil {
			return writeKubernetes(ctx, nil, connectorResolutionError(err))
		}
	}
	return ctx.Next()
}

// requireReportPermission is the permission gate for a route whose answer is a
// report about the cluster rather than a read of it, so it asks only that a
// connector is configured. Resolving an org validates the credential, and the
// report has to be answerable exactly when that validation fails.
func (controller *KubernetesAdminController) requireReportPermission(ctx *fiber.Ctx) error {
	// Same order as every other route in the group: the permission comes first,
	// so a caller without it learns nothing about whether a connector exists, and
	// a caller with it still gets the report across a dead credential.
	if !callerCanViewInfrastructure(ctx) {
		return refuseCallerInfrastructureView(ctx)
	}
	if controller == nil || controller.registry == nil || len(controller.registry.Clusters()) == 0 {
		return writeKubernetes(ctx, nil, kubernetes.ErrConnectorNotConfigured)
	}
	return ctx.Next()
}

// connectorResolutionError names a failed resolution of a connector this
// deployment has configured. Only a credential fault is renamed: it becomes the
// pair of "not configured" — the cluster is known and its stored data is still
// there, only the identity that reads it is gone — while every other resolution
// failure keeps the code its own fault already has.
func connectorResolutionError(err error) error {
	if !errors.Is(err, kubernetes.ErrCredentialUnavailable) && !errors.Is(err, kubernetes.ErrUnauthorized) {
		return err
	}
	// The cause stays attached for classification and logging; the answer a client
	// gets is rendered from the closed vocabulary, never from this text.
	return fmt.Errorf("%w: %w", kubernetes.ErrConnectorCredentialUnresolved, err)
}

// callerCanViewInfrastructure answers the permission itself and records the
// caller's authorization, but does not write a response or advance the chain. A
// gate that still has to look at the deployment asks this first, so the answer it
// gives an unauthorized caller cannot depend on what it would have found.
func callerCanViewInfrastructure(ctx *fiber.Ctx) bool {
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureView))
	if !explicit || !allowed {
		return false
	}
	authorization := core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}}
	if !core.CallerAuthorizationPresent(ctx.UserContext()) {
		ctx.SetUserContext(core.WithCallerAuthorization(ctx.UserContext(), authorization))
	}
	return true
}

// refuseCallerInfrastructureView is the one refusal every gated route gives a
// caller without the permission: it names the permission, not the deployment.
func refuseCallerInfrastructureView(ctx *fiber.Ctx) error {
	return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "infrastructure:view permission is required"})
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

func (controller *KubernetesAdminController) timeline(ctx *fiber.Ctx) error {
	since, err := parseKubernetesTime(ctx.Query("since"))
	if err != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	until, err := parseKubernetesTime(ctx.Query("until"))
	if err != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	limit := 0
	if raw := ctx.Query("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
		}
	}
	breaksOnly := false
	switch ctx.Query("breaks") {
	case "", "0", "false":
	case "1", "true":
		breaksOnly = true
	default:
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	if len(ctx.Context().QueryArgs().PeekMulti("breaks")) > 1 {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	value, err := requestKubernetesService(ctx).Timeline(ctx.UserContext(), kubernetes.TimelineQuery{
		Since: since, Until: until, Namespace: ctx.Query("namespace"), Kind: ctx.Query("kind"), RootUID: ctx.Query("root_uid"), PodUID: ctx.Query("pod_uid"), ResourceUID: ctx.Query("resource_uid"), Limit: limit, Cursor: ctx.Query("cursor"), BreaksOnly: breaksOnly,
	})
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
		RootUID: ctx.Query("root_uid"),
		Type:    kubernetes.ChangeType(ctx.Query("type")), Limit: queryInt(ctx, "limit"), Cursor: ctx.Query("cursor"),
	})
	return writeKubernetes(ctx, value, err)
}

func (controller *KubernetesAdminController) crashes(ctx *fiber.Ctx) error {
	since, err := parseKubernetesTime(ctx.Query("since"))
	if err != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	until, err := parseKubernetesTime(ctx.Query("until"))
	if err != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	limit := 0
	if value := ctx.Query("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
		}
	}
	value, err := requestKubernetesService(ctx).Crashes(ctx.UserContext(), kuberneteschanges.CrashQuery{Since: since, Until: until, Namespace: strings.Clone(ctx.Query("namespace")), RootUID: strings.Clone(ctx.Query("root_uid")), PodUID: strings.Clone(ctx.Query("pod_uid")), Limit: limit, Cursor: strings.Clone(ctx.Query("cursor"))})
	if errors.Is(err, kuberneteschanges.ErrInvalidQuery) {
		err = kubernetes.ErrInvalidArguments
	}
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

func (controller *KubernetesAdminController) namespaces(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Namespaces(ctx.UserContext(), ctx.Query("namespace"))
	return writeKubernetes(ctx, value, err)
}

func (controller *KubernetesAdminController) related(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).Related(ctx.UserContext(), kubernetes.NeighborhoodQuery{
		ResourceID: ctx.Query("resource_id"), Namespace: ctx.Query("namespace"), Name: ctx.Query("name"),
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
				_, refreshed, refreshErr := service.CachedIndexSnapshot(streamContext, kinds...)
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
	args := ctx.Context().QueryArgs()
	for _, key := range []string{"broken", "include", "root_uid", "context", "occurrence_id", "center"} {
		if len(args.PeekMulti(key)) > 1 {
			return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
		}
	}
	include := strings.Clone(ctx.Query("include"))
	if args.Has("include") && include != "changes" {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	broken := false
	if args.Has("broken") {
		parsed, err := strconv.ParseBool(ctx.Query("broken"))
		if err != nil {
			return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
		}
		broken = parsed
	}
	root := strings.Clone(ctx.Query("root_uid"))
	contextMode := strings.Clone(ctx.Query("context"))
	if args.Has("context") && contextMode != "persisted" {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	occurrenceID := strings.Clone(ctx.Query("occurrence_id"))
	center, centerErr := parseKubernetesTime(ctx.Query("center"))
	if centerErr != nil || (args.Has("occurrence_id") || args.Has("center")) && contextMode != "persisted" || args.Has("occurrence_id") && occurrenceID == "" || args.Has("center") && center.IsZero() {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	if args.Has("root_uid") && (root == "" || include != "changes" && contextMode != "persisted") {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	since, timeErr := parseKubernetesTime(ctx.Query("since"))
	if timeErr != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	until, timeErr := parseKubernetesTime(ctx.Query("until"))
	if timeErr != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	value, err := requestKubernetesService(ctx).Issues(ctx.UserContext(), kubernetes.IssueOptions{
		Since: since, Until: until, Namespace: ctx.Query("namespace"), Severity: ctx.Query("severity"), Kind: ctx.Query("kind"), Limit: queryInt(ctx, "limit"), Cursor: ctx.Query("cursor"),
		Broken: broken, IncludeChanges: include == "changes", RootUID: root, Context: contextMode, OccurrenceID: occurrenceID, Center: center,
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
	value, err := requestKubernetesService(ctx).CachedResources(ctx.UserContext(), listOptions(ctx))
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

// syncHealth reports the complete inventory health of the cluster: every known
// kind, not only the kinds one view asked for, so a gate can rely on it.
func (controller *KubernetesAdminController) syncHealth(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).SyncHealth(ctx.UserContext())
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
	value, err := requestKubernetesService(ctx).UsagePage(ctx.UserContext(), ctx.Query("namespace"), queryInt(ctx, "limit"), ctx.Query("cursor"))
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
	// A connector fault can still have a report to give. The cluster's durable
	// first ready moment is local state, so it survives the credential that wrote
	// it, and a surface needs it exactly when every cluster read is failing. The
	// route asked what this cluster's inventory state is and the report answers
	// error, so it is served as that answer: a failure status makes the one field
	// that says whether the cluster ever worked unreachable to a client that
	// discards error bodies.
	if report, ok := kubernetes.SyncHealthReport(err); ok {
		return ctx.JSON(report)
	}
	status := fiber.StatusBadGateway
	var throttled *kubernetes.ErrThrottled
	switch {
	case errors.As(err, &throttled):
		status = fiber.StatusTooManyRequests
		ctx.Set(fiber.HeaderRetryAfter, strconv.Itoa(max(1, int(throttled.RetryAfter.Seconds()))))
	case errors.Is(err, kubernetes.ErrInvalidArguments), errors.Is(err, kubernetes.ErrInvalidEndpoint), errors.Is(err, kubernetes.ErrClusterRequired):
		status = fiber.StatusBadRequest
	case errors.Is(err, kubernetes.ErrRequestTooLarge):
		// Explicit and bounded: the request is refused before anything is parsed,
		// so an intermediary's header limit can never answer in silence instead.
		status = fiber.StatusBadRequest
	case errors.Is(err, kubernetes.ErrCompleteGraphLimit):
		status = fiber.StatusRequestEntityTooLarge
	case errors.Is(err, kubernetes.ErrIndexStreamBusy):
		status = fiber.StatusTooManyRequests
	case errors.Is(err, kubernetes.ErrLogStreamBusy):
		status = fiber.StatusTooManyRequests
		// A bounded backoff hint, not a guarantee that another viewer has closed.
		ctx.Set(fiber.HeaderRetryAfter, "2")
	case errors.Is(err, kubernetes.ErrClusterUnavailable):
		status = fiber.StatusServiceUnavailable
	case errors.Is(err, kubernetes.ErrConnectorNotConfigured):
		status = fiber.StatusServiceUnavailable
	case errors.Is(err, kubernetes.ErrConnectorCredentialUnresolved):
		// A configured connector whose credential cannot be resolved is a state of
		// this server, not of the upstream API: 503 says the request can be retried
		// once the identity is restored, where 502 would blame a gateway.
		status = fiber.StatusServiceUnavailable
	case errors.Is(err, kubernetes.ErrTimelineContention):
		status = fiber.StatusServiceUnavailable
		ctx.Set(fiber.HeaderRetryAfter, "1")
	case errors.Is(err, kubernetes.ErrInventoryNotReady):
		// A stale or incomplete index is a retryable state of Versus, so it is
		// never reported as a permission problem.
		status = fiber.StatusServiceUnavailable
		ctx.Set(fiber.HeaderRetryAfter, strconv.Itoa(max(1, int(kubernetes.InventoryRetryAfter(err).Seconds()))))
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
