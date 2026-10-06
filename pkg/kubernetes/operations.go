package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultSearchPerKind = 20
	defaultSearchTotal   = 100
	maxSearchKinds       = 64
	maxSearchRequests    = 128
	maxSearchKindPages   = 16
	maxCollectedItems    = 10000
	listAllPageSize      = 100
)

// SearchOptions bounds a true cross-kind name search.
type SearchOptions struct {
	Query        string
	Namespace    string
	Category     string
	Labels       string
	Fields       string
	PerKindLimit int
	TotalLimit   int
}

// SearchResult contains ranked projected matches and explicit incomplete evidence.
type SearchResult struct {
	Items         []ProjectedResource `json:"items"`
	Continuations map[string]string   `json:"continuations,omitempty"`
	Requests      int                 `json:"requests"`
	RequestBudget int                 `json:"request_budget"`
	Truncated     bool                `json:"truncated"`
	Omitted       []string            `json:"omitted_categories,omitempty"`
	Partial       []PartialFailure    `json:"partial_failures,omitempty"`
}

// WorkloadPage aggregates the supported workload kinds.
type WorkloadPage struct {
	Items     []ProjectedResource `json:"items"`
	Counts    map[string]int      `json:"counts,omitempty"`
	Next      string              `json:"next,omitempty"`
	Truncated bool                `json:"truncated"`
	Omitted   []string            `json:"omitted_categories,omitempty"`
	Partial   []PartialFailure    `json:"partial_failures,omitempty"`
}

type WorkloadListOptions struct {
	Namespace string
	Kind      string
	Query     string
	Limit     int
	Cursor    string
}

// WorkloadContainer is the bounded operational view of one workload container.
type WorkloadContainer struct {
	Name     string            `json:"name"`
	Image    string            `json:"image,omitempty"`
	Probes   []string          `json:"probes,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// WorkloadPod captures status and placement for one related pod.
type WorkloadPod struct {
	Name         string    `json:"name"`
	Phase        string    `json:"phase,omitempty"`
	Node         string    `json:"node,omitempty"`
	RestartCount int64     `json:"restart_count"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
}

// WorkloadDetail is a dedicated bounded SRE view, not a generic resource projection.
type WorkloadDetail struct {
	ResourceID         string              `json:"resource_id"`
	Kind               string              `json:"kind"`
	Namespace          string              `json:"namespace,omitempty"`
	Name               string              `json:"name"`
	Desired            *int64              `json:"desired,omitempty"`
	Current            *int64              `json:"current,omitempty"`
	Ready              *int64              `json:"ready,omitempty"`
	Available          *int64              `json:"available,omitempty"`
	Unavailable        *int64              `json:"unavailable,omitempty"`
	Succeeded          *int64              `json:"succeeded,omitempty"`
	Failed             *int64              `json:"failed,omitempty"`
	Active             *int64              `json:"active,omitempty"`
	Generation         *int64              `json:"generation,omitempty"`
	ObservedGeneration *int64              `json:"observed_generation,omitempty"`
	UpdateStrategy     string              `json:"update_strategy,omitempty"`
	Conditions         []Condition         `json:"conditions,omitempty"`
	Containers         []WorkloadContainer `json:"containers,omitempty"`
	Pods               []WorkloadPod       `json:"pods,omitempty"`
	Nodes              []string            `json:"nodes,omitempty"`
	NodeSelector       map[string]string   `json:"node_selector,omitempty"`
	Affinity           []string            `json:"affinity,omitempty"`
	TopologySpread     []string            `json:"topology_spread,omitempty"`
	TerminationGrace   *int64              `json:"termination_grace_seconds,omitempty"`
	HPAs               []ObjectRef         `json:"hpas,omitempty"`
	PDBs               []ObjectRef         `json:"pdbs,omitempty"`
	Usage              []ResourceUsage     `json:"usage,omitempty"`
	Truncated          bool                `json:"truncated"`
	Omitted            []string            `json:"omitted_categories,omitempty"`
	Partial            []PartialFailure    `json:"partial_failures,omitempty"`
}

// ResourceDescription adds deterministic diagnostic evidence to one resource.
type ResourceDescription struct {
	Resource    ProjectedResource   `json:"resource"`
	Related     []ObjectRef         `json:"related_resources,omitempty"`
	Events      []ProjectedResource `json:"events,omitempty"`
	Usage       *ResourceUsage      `json:"usage,omitempty"`
	UsageStatus string              `json:"usage_status"`
	Truncated   bool                `json:"truncated"`
	Omitted     []string            `json:"omitted_categories,omitempty"`
	Partial     []PartialFailure    `json:"partial_failures,omitempty"`
}

type WorkloadLogOptions struct {
	Namespace    string
	Kind         string
	Name         string
	Pod          string
	Container    string
	Previous     bool
	SinceSeconds int
	TailLines    int
	Grep         string
	MaxPods      int
}

type WorkloadLogLine struct {
	Pod       string    `json:"pod"`
	Container string    `json:"container,omitempty"`
	At        time.Time `json:"at,omitempty"`
	Text      string    `json:"text"`
}

type WorkloadLogs struct {
	Pods        []string          `json:"pods"`
	Lines       []WorkloadLogLine `json:"lines"`
	Truncated   bool              `json:"truncated"`
	OmittedPods []string          `json:"omitted_pods"`
	Partial     []PartialFailure  `json:"partial_failures,omitempty"`
}

// EventOptions is the server-owned object event filter contract.
type EventOptions struct {
	Namespace string
	Type      string
	Kind      string
	Name      string
	UID       string
	Continue  string
	Cursor    string
	Limit     int
}

type EventPage struct {
	Items            []ProjectedResource `json:"items"`
	Next             string              `json:"next,omitempty"`
	Truncated        bool                `json:"truncated"`
	Partial          []PartialFailure    `json:"partial_failures,omitempty"`
	EncodedTruncated bool                `json:"-"`
	Omitted          []string            `json:"-"`
}

var workloadResources = []struct {
	Kind       string
	ResourceID string
}{
	{"Deployment", "apps~v1~deployments"},
	{"StatefulSet", "apps~v1~statefulsets"},
	{"DaemonSet", "apps~v1~daemonsets"},
	{"Job", "batch~v1~jobs"},
	{"CronJob", "batch~v1~cronjobs"},
	{"Pod", "core~v1~pods"},
}

func projectResourceReferences(kind string, metadata, spec, summary map[string]any) {
	addNames := func(key string, values ...string) {
		seen := map[string]bool{}
		var names []string
		for _, value := range values {
			value = boundString(value)
			if value != "" && !seen[value] {
				seen[value] = true
				names = append(names, value)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			if len(names) > maxProjectionItems {
				names = names[:maxProjectionItems]
			}
			summary[key] = names
		}
	}
	var configMaps, secrets, claims []string
	collectPodSpecReferences(spec, &configMaps, &secrets, &claims)
	if template, ok := spec["template"].(map[string]any); ok {
		if podSpec, ok := template["spec"].(map[string]any); ok {
			collectPodSpecReferences(podSpec, &configMaps, &secrets, &claims)
		}
	}
	addNames("config_maps", configMaps...)
	addNames("secrets", secrets...)
	addNames("persistent_volume_claims", claims...)
	switch kind {
	case "EndpointSlice":
		labels := stringMap(metadata["labels"])
		addNames("services", labels["kubernetes.io/service-name"])
		var pods []string
		for _, endpoint := range mapSlice(spec["endpoints"]) {
			if target, ok := endpoint["targetRef"].(map[string]any); ok && stringValue(target["kind"]) == "Pod" {
				pods = append(pods, stringValue(target["name"]))
			}
		}
		addNames("pods", pods...)
	case "Ingress":
		addNames("services", ingressServices(spec)...)
	case "HTTPRoute":
		addNames("services", routeServices(spec)...)
		addNames("gateways", routeGateways(spec)...)
	case "Gateway":
		addNames("services", routeServices(spec)...)
	case "HorizontalPodAutoscaler":
		if target, ok := spec["scaleTargetRef"].(map[string]any); ok {
			summary["target"] = ObjectRef{Kind: boundString(stringValue(target["kind"])), Name: boundString(stringValue(target["name"]))}
		}
	case "PersistentVolumeClaim":
		addNames("persistent_volumes", stringValue(spec["volumeName"]))
		addNames("storage_classes", stringValue(spec["storageClassName"]))
	case "PersistentVolume":
		addNames("storage_classes", stringValue(spec["storageClassName"]))
	}
}

func collectPodSpecReferences(spec map[string]any, configMaps, secrets, claims *[]string) {
	for _, containerKey := range []string{"containers", "initContainers"} {
		for _, container := range mapSlice(spec[containerKey]) {
			for _, source := range mapSlice(container["envFrom"]) {
				if ref, ok := source["configMapRef"].(map[string]any); ok {
					*configMaps = append(*configMaps, stringValue(ref["name"]))
				}
				if ref, ok := source["secretRef"].(map[string]any); ok {
					*secrets = append(*secrets, stringValue(ref["name"]))
				}
			}
			for _, env := range mapSlice(container["env"]) {
				valueFrom, _ := env["valueFrom"].(map[string]any)
				if ref, ok := valueFrom["configMapKeyRef"].(map[string]any); ok {
					*configMaps = append(*configMaps, stringValue(ref["name"]))
				}
				if ref, ok := valueFrom["secretKeyRef"].(map[string]any); ok {
					*secrets = append(*secrets, stringValue(ref["name"]))
				}
			}
		}
	}
	for _, volume := range mapSlice(spec["volumes"]) {
		if ref, ok := volume["configMap"].(map[string]any); ok {
			*configMaps = append(*configMaps, stringValue(ref["name"]))
		}
		if ref, ok := volume["secret"].(map[string]any); ok {
			*secrets = append(*secrets, stringValue(ref["secretName"]))
		}
		if ref, ok := volume["persistentVolumeClaim"].(map[string]any); ok {
			*claims = append(*claims, stringValue(ref["claimName"]))
		}
	}
}

func ingressServices(spec map[string]any) []string {
	var result []string
	if fallback, ok := spec["defaultBackend"].(map[string]any); ok {
		result = append(result, backendServiceName(fallback))
	}
	for _, rule := range mapSlice(spec["rules"]) {
		http, _ := rule["http"].(map[string]any)
		for _, path := range mapSlice(http["paths"]) {
			if backend, ok := path["backend"].(map[string]any); ok {
				result = append(result, backendServiceName(backend))
			}
		}
	}
	return result
}

func backendServiceName(backend map[string]any) string {
	service, _ := backend["service"].(map[string]any)
	return stringValue(service["name"])
}
func routeServices(spec map[string]any) []string {
	var result []string
	for _, rule := range mapSlice(spec["rules"]) {
		for _, ref := range mapSlice(rule["backendRefs"]) {
			if kind := stringValue(ref["kind"]); kind == "" || kind == "Service" {
				result = append(result, stringValue(ref["name"]))
			}
		}
	}
	return result
}

func routeGateways(spec map[string]any) []string {
	var result []string
	for _, ref := range mapSlice(spec["parentRefs"]) {
		if kind := stringValue(ref["kind"]); kind == "" || kind == "Gateway" {
			result = append(result, stringValue(ref["name"]))
		}
	}
	return result
}

func labelsMatch(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func (service *Service) listAll(ctx context.Context, options ListOptions) (ResourcePage, error) {
	result := ResourcePage{}
	var err error
	ctx, _, err = service.withDiscovery(ctx)
	if err != nil {
		return result, err
	}
	pageLimit := listAllPageSize
	for len(result.Items) < maxCollectedItems {
		options.Limit = pageLimit
		page, effectiveLimit, err := service.listPage(ctx, options)
		if err != nil {
			if errors.Is(err, ErrOperationBudget) {
				result.Truncated = true
				result.Omitted = appendUnique(result.Omitted, options.ResourceID)
				result.Omitted = appendUnique(result.Omitted, "budget_exhausted")
				result.Partial = append(result.Partial, PartialFailure{ResourceID: options.ResourceID, Scope: "operation", Class: "budget_exhausted"})
				return result, nil
			}
			return result, err
		}
		pageLimit = effectiveLimit
		result.Truncated = result.Truncated || page.Truncated && page.Continue == ""
		result.EncodedTruncated = result.EncodedTruncated || page.EncodedTruncated
		for _, omitted := range page.Omitted {
			result.Omitted = appendUnique(result.Omitted, omitted)
		}
		result.Partial = append(result.Partial, page.Partial...)
		remaining := maxCollectedItems - len(result.Items)
		if len(page.Items) > remaining {
			result.Items = append(result.Items, page.Items[:remaining]...)
			result.Truncated = true
			return result, nil
		}
		result.Items = append(result.Items, page.Items...)
		if page.Continue == "" {
			return result, nil
		}
		if len(page.Items) == 0 {
			result.Truncated = true
			result.Omitted = appendUnique(result.Omitted, "no_progress")
			return result, nil
		}
		options.Continue = page.Continue
	}
	result.Truncated = true
	return result, nil
}

// Search searches names across discovered readable kinds with per-kind and total budgets.
func (service *Service) Search(ctx context.Context, options SearchOptions) (SearchResult, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	query := strings.ToLower(strings.TrimSpace(options.Query))
	if query == "" || len(query) > 253 {
		return SearchResult{}, ErrInvalidArguments
	}
	if options.Namespace != "" && !safeSegment(options.Namespace) {
		return SearchResult{}, ErrInvalidArguments
	}
	if !validResourceCategory(options.Category) || !safeSelector(options.Labels) || !safeSelector(options.Fields) {
		return SearchResult{}, ErrInvalidArguments
	}
	perKind := options.PerKindLimit
	if perKind <= 0 || perKind > 100 {
		perKind = defaultSearchPerKind
	}
	total := options.TotalLimit
	if total <= 0 || total > 500 {
		total = defaultSearchTotal
	}
	ctx, discovery, err := service.withDiscovery(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	result := SearchResult{RequestBudget: maxSearchRequests, Partial: append([]PartialFailure(nil), discovery.Partial...)}
	resources := make([]ResourceDefinition, 0, len(discovery.Resources))
	for _, definition := range discovery.Resources {
		category := resourceCategory(definition.Kind)
		if (options.Category != "" && category != options.Category) || (options.Namespace != "" && !definition.Namespaced) {
			continue
		}
		resources = append(resources, definition)
	}
	if len(resources) > maxSearchKinds {
		skippedCategories := make(map[string]bool)
		for _, definition := range resources[maxSearchKinds:] {
			skippedCategories[resourceCategory(definition.Kind)] = true
		}
		skipped := len(resources) - maxSearchKinds
		resources = resources[:maxSearchKinds]
		result.Truncated = true
		result.Omitted = appendUnique(result.Omitted, fmt.Sprintf("resource_kinds=%d", skipped))
		categories := make([]string, 0, len(skippedCategories))
		for category := range skippedCategories {
			categories = append(categories, category)
		}
		sort.Strings(categories)
		for _, category := range categories {
			result.Omitted = appendUnique(result.Omitted, "category="+category)
		}
	}
	for resourceIndex, definition := range resources {
		if result.Requests == result.RequestBudget {
			result.Truncated = true
			result.Omitted = appendUnique(result.Omitted, "request_budget")
			for _, skipped := range resources[resourceIndex:] {
				appendSearchOmission(&result, skipped)
			}
			break
		}
		namespace := options.Namespace
		if !definition.Namespaced {
			namespace = ""
		}
		matched := 0
		continuation := ""
		for pageNumber := 0; pageNumber < maxSearchKindPages && result.Requests < result.RequestBudget; pageNumber++ {
			page, listErr := service.List(ctx, ListOptions{ResourceID: definition.ID, Namespace: namespace, Labels: options.Labels, Fields: options.Fields, Continue: continuation, Limit: maxPageSize})
			result.Requests++
			if listErr != nil {
				result.Partial = append(result.Partial, PartialFailure{ResourceID: definition.ID, Class: errorClass(listErr)})
				appendSearchOmission(&result, definition)
				break
			}
			result.Truncated = result.Truncated || (page.Truncated && page.Continue == "")
			for _, omitted := range page.Omitted {
				result.Omitted = appendUnique(result.Omitted, omitted)
			}
			if page.EncodedTruncated {
				appendSearchOmission(&result, definition)
			}
			result.Partial = append(result.Partial, page.Partial...)
			for _, item := range page.Items {
				if nameMatchRank(item.Name, query) < 0 {
					continue
				}
				if matched >= perKind {
					result.Truncated = true
					appendSearchOmission(&result, definition)
					continue
				}
				matched++
				result.Items = append(result.Items, item)
			}
			continuation = page.Continue
			if continuation == "" {
				break
			}
			if matched >= perKind {
				result.Truncated = true
				break
			}
		}
		if continuation != "" {
			result.Truncated = true
			appendSearchOmission(&result, definition)
			if result.Continuations == nil {
				result.Continuations = make(map[string]string)
			}
			result.Continuations[definition.ID] = continuation
		}
	}
	sort.SliceStable(result.Items, func(left, right int) bool {
		leftRank, rightRank := nameMatchRank(result.Items[left].Name, query), nameMatchRank(result.Items[right].Name, query)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if result.Items[left].Kind != result.Items[right].Kind {
			return result.Items[left].Kind < result.Items[right].Kind
		}
		return result.Items[left].Name < result.Items[right].Name
	})
	if len(result.Items) > total {
		result.Items = result.Items[:total]
		result.Truncated = true
		result.Omitted = appendUnique(result.Omitted, "result_limit")
	}
	var sizeTruncated bool
	result.Items, result.Omitted, sizeTruncated = trimAggregateItems(result.Items, result.Omitted)
	result.Truncated = result.Truncated || sizeTruncated
	return result, nil
}

func appendSearchOmission(result *SearchResult, definition ResourceDefinition) {
	result.Omitted = appendUnique(result.Omitted, definition.ID)
	result.Omitted = appendUnique(result.Omitted, "category="+resourceCategory(definition.Kind))
}

func nameMatchRank(name, query string) int {
	name = strings.ToLower(name)
	if name == query {
		return 0
	}
	if strings.HasPrefix(name, query) {
		return 1
	}
	if strings.Contains(name, query) {
		return 2
	}
	return -1
}

func validResourceCategory(category string) bool {
	switch category {
	case "", "workload", "pod", "node", "network", "storage", "configuration", "access", "event", "other":
		return true
	}
	return false
}

func resourceCategory(kind string) string {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "ReplicaSet":
		return "workload"
	case "Pod":
		return "pod"
	case "Node", "Namespace":
		return "node"
	case "Service", "EndpointSlice", "Ingress", "Gateway", "HTTPRoute":
		return "network"
	case "PersistentVolume", "PersistentVolumeClaim", "StorageClass":
		return "storage"
	case "ConfigMap", "Secret":
		return "configuration"
	case "Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding", "ServiceAccount":
		return "access"
	case "Event":
		return "event"
	default:
		return "other"
	}
}

// ListWorkloads aggregates every supported workload kind or one validated kind.
func (service *Service) ListWorkloads(ctx context.Context, namespace, kind string, limit int) (WorkloadPage, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	if namespace != "" && !safeSegment(namespace) {
		return WorkloadPage{}, ErrInvalidArguments
	}
	selected := workloadResources
	if kind != "" {
		selected = nil
		for _, candidate := range workloadResources {
			if candidate.Kind == kind {
				selected = append(selected, candidate)
				break
			}
		}
		if len(selected) == 0 {
			return WorkloadPage{}, ErrInvalidArguments
		}
	}
	if limit <= 0 || limit > maxPageSize {
		limit = maxPageSize
	}
	result := WorkloadPage{}
	var err error
	ctx, _, err = service.withDiscovery(ctx)
	if err != nil {
		return result, err
	}
	for _, candidate := range selected {
		kindItems := 0
		kindPageLimit := maxPageSize
		continuation := ""
		for kindItems < limit {
			pageLimit := min(limit-kindItems, kindPageLimit)
			page, effectiveLimit, err := service.listPage(ctx, ListOptions{ResourceID: candidate.ResourceID, Namespace: namespace, Continue: continuation, Limit: pageLimit})
			if err != nil {
				class := errorClass(err)
				if errors.Is(err, ErrNotFound) {
					class = "unsupported"
				}
				result.Partial = append(result.Partial, PartialFailure{ResourceID: candidate.ResourceID, Class: class})
				if errors.Is(err, ErrOperationBudget) {
					result.Truncated = true
					result.Omitted = appendUnique(result.Omitted, candidate.ResourceID)
					result.Omitted = appendUnique(result.Omitted, "budget_exhausted")
				}
				break
			}
			kindPageLimit = effectiveLimit
			result.Truncated = result.Truncated || (page.Truncated && page.Continue == "")
			for _, omitted := range page.Omitted {
				result.Omitted = appendUnique(result.Omitted, omitted)
			}
			result.Partial = append(result.Partial, page.Partial...)
			remaining := limit - kindItems
			if len(page.Items) > remaining {
				page.Items = page.Items[:remaining]
				result.Truncated = true
			}
			result.Items = append(result.Items, page.Items...)
			kindItems += len(page.Items)
			continuation = page.Continue
			if continuation == "" {
				break
			}
			if len(page.Items) == 0 {
				result.Truncated = true
				result.Omitted = appendUnique(result.Omitted, candidate.ResourceID)
				break
			}
		}
		if continuation != "" {
			result.Truncated = true
		}
	}
	sort.SliceStable(result.Items, func(i, j int) bool {
		if result.Items[i].Kind != result.Items[j].Kind {
			return result.Items[i].Kind < result.Items[j].Kind
		}
		if result.Items[i].Namespace != result.Items[j].Namespace {
			return result.Items[i].Namespace < result.Items[j].Namespace
		}
		return result.Items[i].Name < result.Items[j].Name
	})
	if len(result.Items) > limit {
		result.Items = result.Items[:limit]
		result.Truncated = true
	}
	var sizeTruncated bool
	result.Items, result.Omitted, sizeTruncated = trimAggregateItems(result.Items, result.Omitted)
	result.Truncated = result.Truncated || sizeTruncated
	return result, nil
}

// Workloads lists one stable page of supported workload projections.
func (service *Service) Workloads(ctx context.Context, options WorkloadListOptions) (WorkloadPage, error) {
	if options.Namespace != "" && !safeSegment(options.Namespace) || options.Query != "" && len(options.Query) > 253 {
		return WorkloadPage{}, ErrInvalidArguments
	}
	if options.Kind != "" && !isWorkloadKind(options.Kind) {
		return WorkloadPage{}, ErrInvalidArguments
	}
	limit := normalizePageLimit(options.Limit)
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	result := WorkloadPage{Items: []ProjectedResource{}, Counts: make(map[string]int, len(workloadResources))}
	for _, candidate := range workloadResources {
		result.Counts[candidate.Kind] = 0
	}
	var resources []ProjectedResource
	indexKinds := make([]string, 0, len(workloadResources))
	for _, candidate := range workloadResources {
		indexKinds = append(indexKinds, candidate.Kind)
	}
	view, status, indexErr := service.IndexSnapshot(ctx, indexKinds...)
	indexReady := indexErr == nil
	if indexReady {
		for _, candidate := range workloadResources {
			if status.Kinds[candidate.Kind].State != "ready" {
				indexReady = false
				break
			}
		}
	}
	if indexReady {
		for _, record := range view.Records {
			if !isWorkloadKind(record.Kind) {
				continue
			}
			resources = append(resources, projectedWorkloadIndexRecord(record))
		}
	} else {
		for _, candidate := range workloadResources {
			page, listErr := service.listAll(ctx, ListOptions{ResourceID: candidate.ResourceID, Namespace: options.Namespace})
			if listErr != nil {
				class := errorClass(listErr)
				if errors.Is(listErr, ErrNotFound) {
					class = "unsupported"
				}
				result.Partial = append(result.Partial, PartialFailure{ResourceID: candidate.ResourceID, Class: class})
				result.Truncated = true
				continue
			}
			resources = append(resources, page.Items...)
			result.Partial = append(result.Partial, page.Partial...)
			result.Truncated = result.Truncated || page.Truncated
			result.Omitted = append(result.Omitted, page.Omitted...)
		}
	}
	query := strings.ToLower(strings.TrimSpace(options.Query))
	filtered := make([]ProjectedResource, 0, len(resources))
	for _, resource := range resources {
		if options.Namespace != "" && resource.Namespace != options.Namespace || query != "" && !strings.Contains(strings.ToLower(resource.Name), query) && !strings.Contains(strings.ToLower(resource.Namespace), query) {
			continue
		}
		result.Counts[resource.Kind]++
		if options.Kind == "" || resource.Kind == options.Kind {
			filtered = append(filtered, resource)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Kind != filtered[j].Kind {
			return filtered[i].Kind < filtered[j].Kind
		}
		if filtered[i].Namespace != filtered[j].Namespace {
			return filtered[i].Namespace < filtered[j].Namespace
		}
		return filtered[i].Name < filtered[j].Name
	})
	start, end, next, err := pageWindow(options.Cursor, limit, len(filtered))
	if err != nil {
		return WorkloadPage{}, err
	}
	result.Items = append(result.Items, filtered[start:end]...)
	result.Next = next
	result.Truncated = result.Truncated || next != ""
	var sizeTruncated bool
	result.Items, result.Omitted, sizeTruncated = trimAggregateItems(result.Items, result.Omitted)
	result.Truncated = result.Truncated || sizeTruncated
	if sizeTruncated {
		start, _ := strconv.Atoi(options.Cursor)
		result.Next = strconv.Itoa(start + len(result.Items))
	}
	return result, nil
}

func normalizePageLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return min(limit, 200)
}

func pageWindow(cursor string, limit, total int) (int, int, string, error) {
	offset := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil || parsed < 0 || parsed > total {
			return 0, 0, "", ErrInvalidArguments
		}
		offset = parsed
	}
	end := min(offset+limit, total)
	next := ""
	if end < total {
		next = strconv.Itoa(end)
	}
	return offset, end, next, nil
}

func trimAggregateItems(items []ProjectedResource, omitted []string) ([]ProjectedResource, []string, bool) {
	bounded, truncated := enforceResultSize(items)
	if !truncated {
		return bounded, omitted, false
	}
	omitted = appendUnique(omitted, "encoded_result_size")
	for _, item := range items[len(bounded):] {
		omitted = appendUnique(omitted, item.ResourceID)
		omitted = appendUnique(omitted, "category="+resourceCategory(item.Kind))
	}
	return bounded, omitted, true
}

func enforceResultSize(items []ProjectedResource) ([]ProjectedResource, bool) {
	encodedBytes := 0
	for index, item := range items {
		encoded, _ := json.Marshal(item)
		if encodedBytes+len(encoded) > maxResultBytes {
			return items[:index], true
		}
		encodedBytes += len(encoded)
	}
	return items, false
}

// GetWorkload returns bounded workload state plus deterministic related resources.
func (service *Service) GetWorkload(ctx context.Context, namespace, kind, name string) (WorkloadDetail, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	resourceID, ok := workloadResourceID(kind)
	if !ok || !safeSegment(namespace) || !safeSegment(name) {
		return WorkloadDetail{}, ErrInvalidArguments
	}
	resource, err := service.Get(ctx, resourceID, namespace, name)
	if err != nil {
		return WorkloadDetail{}, err
	}
	result := WorkloadDetail{
		ResourceID: resource.ResourceID, Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name,
		Desired:   summaryInt(resource.Summary, "desired_replicas", "replicas", "desired_number_scheduled", "completions"),
		Current:   summaryInt(resource.Summary, "current_replicas", "current_number_scheduled"),
		Ready:     summaryInt(resource.Summary, "ready_replicas", "number_ready"),
		Available: summaryInt(resource.Summary, "available_replicas"), Unavailable: summaryInt(resource.Summary, "unavailable_replicas"),
		Succeeded: summaryInt(resource.Summary, "succeeded"), Failed: summaryInt(resource.Summary, "failed"), Active: summaryInt(resource.Summary, "active"),
		Generation: summaryInt(resource.Summary, "generation"), ObservedGeneration: summaryInt(resource.Summary, "observed_generation"),
		UpdateStrategy: summaryString(resource.Summary, "update_strategy", "rollout_strategy"), Conditions: append([]Condition(nil), resource.Conditions...),
		NodeSelector: summaryStringMap(resource.Summary, "node_selector"), Affinity: summaryStrings(resource.Summary, "affinity"),
		TopologySpread: summaryStrings(resource.Summary, "topology_spread"), TerminationGrace: summaryInt(resource.Summary, "termination_grace_seconds"),
	}
	result.Containers = summaryContainers(resource.Summary)
	selector := summaryStringMap(resource.Summary, "selector")
	pods, podPartial, podOmitted, podTruncated := service.relatedPods(ctx, namespace, kind, name, selector)
	result.Partial = append(result.Partial, podPartial...)
	for _, omitted := range podOmitted {
		result.Omitted = appendUnique(result.Omitted, omitted)
	}
	result.Truncated = podTruncated
	for _, pod := range pods {
		result.Pods = append(result.Pods, WorkloadPod{Name: pod.Name, Phase: summaryString(pod.Summary, "phase"), Node: summaryString(pod.Summary, "node"), RestartCount: summaryIntValue(pod.Summary, "restart_count")})
		result.Pods[len(result.Pods)-1].CreatedAt = resourceTime(pod.Summary)
		if node := summaryString(pod.Summary, "node"); node != "" {
			result.Nodes = append(result.Nodes, node)
		}
	}
	result.Nodes = uniqueSorted(result.Nodes)
	usage, usageErr := service.Usage(ctx, namespace, maxProjectionItems)
	if usageErr != nil {
		result.Partial = append(result.Partial, PartialFailure{ResourceID: "metrics.k8s.io", Class: errorClass(usageErr)})
	} else if usage.Availability == "unavailable" {
		result.Omitted = appendUnique(result.Omitted, "usage")
	} else {
		podNames := make(map[string]bool, len(result.Pods))
		for _, pod := range result.Pods {
			podNames[pod.Name] = true
		}
		for _, metric := range usage.Pods {
			if podNames[metric.Name] {
				result.Usage = append(result.Usage, metric)
			}
		}
		result.Partial = append(result.Partial, usage.Partial...)
	}
	for _, related := range []struct {
		resourceID string
		kind       string
	}{
		{"autoscaling~v2~horizontalpodautoscalers", "HorizontalPodAutoscaler"},
		{"policy~v1~poddisruptionbudgets", "PodDisruptionBudget"},
	} {
		items, partial, omitted, truncated := service.relatedWorkloadResources(ctx, related.resourceID, namespace)
		result.Partial = append(result.Partial, partial...)
		for _, item := range omitted {
			result.Omitted = appendUnique(result.Omitted, item)
		}
		result.Truncated = result.Truncated || truncated
		for _, item := range items {
			matches := false
			if related.kind == "HorizontalPodAutoscaler" {
				target, _ := item.Summary["target"].(ObjectRef)
				matches = target.Kind == kind && target.Name == name
			} else {
				matches = labelsMatch(selector, summaryStringMap(item.Summary, "selector"))
			}
			if matches {
				ref := ObjectRef{APIVersion: item.APIVersion, Kind: item.Kind, Namespace: item.Namespace, Name: item.Name, UID: item.UID}
				if related.kind == "HorizontalPodAutoscaler" {
					result.HPAs = append(result.HPAs, ref)
				} else {
					result.PDBs = append(result.PDBs, ref)
				}
			}
		}
	}
	sort.SliceStable(result.HPAs, func(i, j int) bool { return result.HPAs[i].Name < result.HPAs[j].Name })
	sort.SliceStable(result.PDBs, func(i, j int) bool { return result.PDBs[i].Name < result.PDBs[j].Name })
	return result, nil
}

func (service *Service) relatedPods(ctx context.Context, namespace, kind, name string, selector map[string]string) ([]ProjectedResource, []PartialFailure, []string, bool) {
	page, err := service.listAll(ctx, ListOptions{ResourceID: "core~v1~pods", Namespace: namespace})
	if err != nil {
		return nil, []PartialFailure{{ResourceID: "core~v1~pods", Class: errorClass(err)}}, nil, false
	}
	var result []ProjectedResource
	for _, pod := range page.Items {
		matches := kind == "Pod" && pod.Name == name
		if !matches && len(selector) > 0 {
			matches = labelsMatch(pod.Labels, selector)
		}
		if !matches {
			for _, owner := range pod.Owners {
				if owner.Kind == kind && owner.Name == name {
					matches = true
					break
				}
			}
		}
		if matches {
			result = append(result, pod)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	truncated := page.Truncated
	if len(result) > maxProjectionItems {
		result = result[:maxProjectionItems]
		truncated = true
	}
	return result, page.Partial, page.Omitted, truncated
}

func (service *Service) WorkloadLogs(ctx context.Context, options WorkloadLogOptions) (WorkloadLogs, error) {
	if options.Namespace == "" || options.Name == "" || options.Grep != "" && len(options.Grep) > 256 || options.Pod != "" && !safeSegment(options.Pod) {
		return WorkloadLogs{}, ErrInvalidArguments
	}
	if options.SinceSeconds < 0 || options.SinceSeconds > 86400 || options.TailLines < 0 || options.TailLines > 1000 {
		return WorkloadLogs{}, ErrInvalidArguments
	}
	if options.MaxPods <= 0 || options.MaxPods > 10 {
		options.MaxPods = 10
	}
	if options.TailLines == 0 {
		options.TailLines = 200
	}
	if options.Kind != "Pod" {
		if _, ok := workloadResourceID(options.Kind); !ok {
			return WorkloadLogs{}, ErrInvalidArguments
		}
	}

	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	workload, err := service.GetWorkload(ctx, options.Namespace, options.Kind, options.Name)
	if err != nil {
		return WorkloadLogs{}, err
	}
	pods := append([]WorkloadPod(nil), workload.Pods...)
	if options.Pod != "" {
		pods = filterWorkloadPods(pods, options.Pod)
		if len(pods) == 0 {
			return WorkloadLogs{}, ErrNotFound
		}
	}
	prioritizeWorkloadLogPods(pods)
	result := WorkloadLogs{Pods: []string{}, Lines: []WorkloadLogLine{}, OmittedPods: []string{}, Partial: append([]PartialFailure(nil), workload.Partial...)}
	if len(pods) > options.MaxPods {
		result.Truncated = true
		for _, pod := range pods[options.MaxPods:] {
			result.OmittedPods = append(result.OmittedPods, pod.Name)
		}
		pods = pods[:options.MaxPods]
	}
	result.Pods = make([]string, len(pods))
	for index, pod := range pods {
		result.Pods[index] = pod.Name
	}

	type podResult struct {
		pod  WorkloadPod
		logs PodLogs
		err  error
	}
	results := make(chan podResult, len(pods))
	semaphore := make(chan struct{}, 3)
	var workers sync.WaitGroup
	for _, pod := range pods {
		workers.Add(1)
		go func(pod WorkloadPod) {
			defer workers.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				results <- podResult{pod: pod, err: ctx.Err()}
				return
			}
			defer func() { <-semaphore }()
			logs, readErr := service.podLogs(ctx, options.Namespace, pod.Name, options.Container, options.Previous, options.SinceSeconds, options.TailLines, true)
			results <- podResult{pod: pod, logs: logs, err: readErr}
		}(pod)
	}
	workers.Wait()
	close(results)
	const maxWorkloadLogBytes = 512 << 10
	var encodedBytes int
	for item := range results {
		if item.err != nil {
			result.Truncated = true
			result.OmittedPods = appendUnique(result.OmittedPods, item.pod.Name)
			result.Partial = append(result.Partial, PartialFailure{ResourceID: "core~v1~pods", Scope: "logs", Class: errorClass(item.err)})
			continue
		}
		result.Truncated = result.Truncated || item.logs.Truncated
		for _, line := range filterWorkloadLogLines(item.logs.Text, options.Grep) {
			at, text := parseTimestampedLogLine(line)
			entry := WorkloadLogLine{Pod: item.pod.Name, Container: item.logs.Container, At: at, Text: text}
			encoded, _ := json.Marshal(entry)
			if encodedBytes+len(encoded) > maxWorkloadLogBytes {
				result.Truncated = true
				result.OmittedPods = appendUnique(result.OmittedPods, item.pod.Name)
				break
			}
			encodedBytes += len(encoded)
			result.Lines = append(result.Lines, entry)
		}
	}
	sort.SliceStable(result.Lines, func(i, j int) bool {
		if result.Lines[i].At.IsZero() != result.Lines[j].At.IsZero() {
			return !result.Lines[i].At.IsZero()
		}
		if !result.Lines[i].At.Equal(result.Lines[j].At) {
			return result.Lines[i].At.Before(result.Lines[j].At)
		}
		if result.Lines[i].Pod != result.Lines[j].Pod {
			return result.Lines[i].Pod < result.Lines[j].Pod
		}
		return result.Lines[i].Text < result.Lines[j].Text
	})
	return result, nil
}

func filterWorkloadPods(pods []WorkloadPod, name string) []WorkloadPod {
	for _, pod := range pods {
		if pod.Name == name {
			return []WorkloadPod{pod}
		}
	}
	return nil
}

func prioritizeWorkloadLogPods(pods []WorkloadPod) {
	sort.SliceStable(pods, func(i, j int) bool {
		left, right := podLogPriority(pods[i]), podLogPriority(pods[j])
		if left != right {
			return left > right
		}
		if !pods[i].CreatedAt.Equal(pods[j].CreatedAt) {
			return pods[i].CreatedAt.After(pods[j].CreatedAt)
		}
		if pods[i].RestartCount != pods[j].RestartCount {
			return pods[i].RestartCount > pods[j].RestartCount
		}
		return pods[i].Name < pods[j].Name
	})
}

func filterWorkloadLogLines(text, grep string) []string {
	lines := strings.Split(text, "\n")
	if grep == "" {
		return lines
	}
	needle := strings.ToLower(grep)
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), needle) {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

func podLogPriority(pod WorkloadPod) int {
	switch pod.Phase {
	case "Failed":
		return 3
	case "Pending":
		return 2
	case "Running":
		return 1
	default:
		return 0
	}
}

func parseTimestampedLogLine(line string) (time.Time, string) {
	separator := strings.IndexByte(line, ' ')
	if separator <= 0 {
		return time.Time{}, line
	}
	at, err := time.Parse(time.RFC3339Nano, line[:separator])
	if err != nil {
		return time.Time{}, line
	}
	return at, line[separator+1:]
}

func (service *Service) relatedWorkloadResources(ctx context.Context, resourceID, namespace string) ([]ProjectedResource, []PartialFailure, []string, bool) {
	if _, err := service.resolve(ctx, resourceID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, nil, false
		}
		return nil, []PartialFailure{{ResourceID: resourceID, Class: errorClass(err)}}, nil, false
	}
	page, err := service.listAll(ctx, ListOptions{ResourceID: resourceID, Namespace: namespace})
	if err != nil {
		return nil, []PartialFailure{{ResourceID: resourceID, Class: errorClass(err)}}, nil, false
	}
	if len(page.Items) > maxProjectionItems {
		page.Items = page.Items[:maxProjectionItems]
		page.Truncated = true
	}
	return page.Items, page.Partial, page.Omitted, page.Truncated
}

func summaryInt(summary map[string]any, keys ...string) *int64 {
	for _, key := range keys {
		if value, ok := summary[key].(float64); ok {
			result := int64(value)
			return &result
		}
	}
	return nil
}
func summaryIntValue(summary map[string]any, key string) int64 {
	value := summaryInt(summary, key)
	if value == nil {
		return 0
	}
	return *value
}
func summaryString(summary map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := summary[key].(string); ok {
			return value
		}
	}
	return ""
}
func summaryStringMap(summary map[string]any, key string) map[string]string {
	value, _ := summary[key].(map[string]string)
	return value
}
func summaryStrings(summary map[string]any, key string) []string {
	value, _ := summary[key].([]string)
	return append([]string(nil), value...)
}
func summaryContainers(summary map[string]any) []WorkloadContainer {
	raw, _ := summary["containers"].([]map[string]any)
	result := make([]WorkloadContainer, 0, len(raw))
	for _, item := range raw {
		result = append(result, WorkloadContainer{Name: stringValue(item["name"]), Image: stringValue(item["image"]), Probes: summaryStrings(item, "probes"), Requests: summaryStringMap(item, "requests"), Limits: summaryStringMap(item, "limits")})
	}
	return result
}
func uniqueSorted(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func workloadResourceID(kind string) (string, bool) {
	for _, candidate := range workloadResources {
		if candidate.Kind == kind {
			return candidate.ResourceID, true
		}
	}
	return "", false
}

// Describe returns one resource with deterministic owners and object-scoped events.
func (service *Service) Describe(ctx context.Context, resourceID, namespace, name string) (ResourceDescription, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	resource, err := service.Get(ctx, resourceID, namespace, name)
	if err != nil {
		return ResourceDescription{}, err
	}
	result := ResourceDescription{Resource: resource, UsageStatus: "unavailable"}
	result.Related = append(result.Related, resource.Owners...)
	for _, relation := range []struct {
		key  string
		kind string
	}{
		{"config_maps", "ConfigMap"}, {"secrets", "Secret"}, {"persistent_volume_claims", "PersistentVolumeClaim"},
		{"persistent_volumes", "PersistentVolume"}, {"storage_classes", "StorageClass"}, {"services", "Service"}, {"pods", "Pod"},
	} {
		for _, relatedName := range summaryStrings(resource.Summary, relation.key) {
			result.Related = append(result.Related, ObjectRef{Kind: relation.kind, Namespace: resource.Namespace, Name: relatedName})
		}
	}
	if target, ok := resource.Summary["target"].(ObjectRef); ok {
		target.Namespace = resource.Namespace
		result.Related = append(result.Related, target)
	}
	if resource.Kind == "Pod" || resource.Kind == "Node" {
		usage, usageErr := service.Usage(ctx, resource.Namespace, maxProjectionItems)
		if usageErr != nil {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: "metrics.k8s.io", Class: errorClass(usageErr)})
		} else {
			result.UsageStatus = usage.Availability
			result.Truncated = result.Truncated || usage.Truncated
			for _, omitted := range usage.Omitted {
				result.Omitted = appendUnique(result.Omitted, omitted)
			}
			metrics := usage.Pods
			if resource.Kind == "Node" {
				metrics = usage.Nodes
			}
			for _, metric := range metrics {
				if metric.Name == resource.Name {
					matched := metric
					result.Usage = &matched
					break
				}
			}
			result.Partial = append(result.Partial, usage.Partial...)
		}
	}
	result.Related = uniqueObjectRefs(result.Related)
	if len(result.Related) > maxProjectionItems {
		result.Related = result.Related[:maxProjectionItems]
		result.Truncated = true
		result.Omitted = appendUnique(result.Omitted, "relationships")
	}
	sort.SliceStable(result.Related, func(i, j int) bool {
		if result.Related[i].Kind != result.Related[j].Kind {
			return result.Related[i].Kind < result.Related[j].Kind
		}
		return result.Related[i].Name < result.Related[j].Name
	})
	events, eventErr := service.listAllEvents(ctx, EventOptions{Namespace: namespace, Kind: resource.Kind, Name: resource.Name, UID: resource.UID})
	if eventErr != nil {
		result.Partial = append(result.Partial, PartialFailure{ResourceID: "core~v1~events", Class: errorClass(eventErr)})
	} else {
		result.Events = events.Items
		result.Partial = append(result.Partial, events.Partial...)
		if events.Truncated {
			result.Truncated = true
			result.Omitted = appendUnique(result.Omitted, "events")
		}
	}
	enforceDescriptionSize(&result)
	return result, nil
}

func enforceDescriptionSize(result *ResourceDescription) {
	encoded, _ := json.Marshal(result)
	if len(encoded) <= maxResultBytes {
		return
	}
	result.Truncated = true
	result.Omitted = appendUnique(result.Omitted, "encoded_result_size")
	result.Partial = append(result.Partial, PartialFailure{ResourceID: result.Resource.ResourceID, Scope: "describe", Class: "encoded_result_size"})
	related, events := result.Related, result.Events
	result.Related, result.Events = nil, nil
	base, _ := json.Marshal(result)
	encodedBytes := len(base)
	if len(related) > 0 {
		encodedBytes += len(`,"related_resources":[]`)
		for _, item := range related {
			itemBytes, _ := json.Marshal(item)
			separator := 0
			if len(result.Related) > 0 {
				separator = 1
			}
			if encodedBytes+separator+len(itemBytes) > maxResultBytes {
				break
			}
			result.Related = append(result.Related, item)
			encodedBytes += separator + len(itemBytes)
		}
	}
	if len(events) > 0 {
		encodedBytes += len(`,"events":[]`)
		for _, item := range events {
			itemBytes, _ := json.Marshal(item)
			separator := 0
			if len(result.Events) > 0 {
				separator = 1
			}
			if encodedBytes+separator+len(itemBytes) > maxResultBytes {
				break
			}
			result.Events = append(result.Events, item)
			encodedBytes += separator + len(itemBytes)
		}
	}
}

// ListEvents lists events using server-authored field selectors only.
func (service *Service) ListEvents(ctx context.Context, options EventOptions) (EventPage, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	if options.Namespace != "" && !safeSegment(options.Namespace) {
		return EventPage{}, ErrInvalidArguments
	}
	if options.Kind != "" || options.Name != "" || options.UID != "" {
		if options.Kind == "" || options.Name == "" || !safeEventField(options.Kind) || !safeEventField(options.Name) || (options.UID != "" && !safeEventField(options.UID)) {
			return EventPage{}, ErrInvalidArguments
		}
	}
	if options.Type != "" && options.Type != "Warning" && options.Type != "Normal" {
		return EventPage{}, ErrInvalidArguments
	}
	fields := ""
	if options.Type != "" {
		fields = "type=" + options.Type
	}
	if options.Kind != "" {
		if fields != "" {
			fields += ","
		}
		fields += "involvedObject.kind=" + options.Kind + ",involvedObject.name=" + options.Name
		if options.UID != "" {
			fields += ",involvedObject.uid=" + options.UID
		}
	}
	continuation := options.Cursor
	if continuation == "" {
		continuation = options.Continue
	}
	page, err := service.List(ctx, ListOptions{ResourceID: "core~v1~events", Namespace: options.Namespace, Fields: fields, Continue: continuation, Limit: normalizePageLimit(options.Limit)})
	if err != nil {
		return EventPage{}, err
	}
	if page.Items == nil {
		page.Items = []ProjectedResource{}
	}
	return EventPage{Items: page.Items, Next: page.Continue, Truncated: page.Truncated, Partial: page.Partial, EncodedTruncated: page.EncodedTruncated, Omitted: page.Omitted}, nil
}

func (service *Service) listAllEvents(ctx context.Context, options EventOptions) (ResourcePage, error) {
	result := ResourcePage{}
	for len(result.Items) < maxProjectionItems {
		options.Limit = min(maxProjectionItems-len(result.Items), maxPageSize)
		page, err := service.ListEvents(ctx, options)
		if err != nil {
			return result, err
		}
		result.Truncated = result.Truncated || page.Truncated && page.Next == ""
		result.EncodedTruncated = result.EncodedTruncated || page.EncodedTruncated
		for _, omitted := range page.Omitted {
			result.Omitted = appendUnique(result.Omitted, omitted)
		}
		result.Partial = append(result.Partial, page.Partial...)
		result.Items = append(result.Items, page.Items...)
		options.Continue = page.Next
		if options.Continue == "" {
			return result, nil
		}
		if len(page.Items) == 0 {
			result.Truncated = true
			result.Omitted = appendUnique(result.Omitted, "no_progress")
			return result, nil
		}
	}
	result.Truncated = result.Truncated || options.Continue != ""
	return result, nil
}

func safeEventField(value string) bool {
	return safeSegment(value) && !strings.ContainsAny(value, ",=")
}

func uniqueObjectRefs(values []ObjectRef) []ObjectRef {
	sort.SliceStable(values, func(i, j int) bool {
		left := values[i].Kind + "\x00" + values[i].Namespace + "\x00" + values[i].Name
		right := values[j].Kind + "\x00" + values[j].Namespace + "\x00" + values[j].Name
		return left < right
	})
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1].Kind != value.Kind || result[len(result)-1].Namespace != value.Namespace || result[len(result)-1].Name != value.Name {
			result = append(result, value)
		}
	}
	return result
}
