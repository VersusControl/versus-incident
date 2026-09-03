package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultPageSize    = 100
	maxPageSize        = 500
	maxStringBytes     = 2048
	maxPodLogBytes     = 128 << 10
	maxProjectionItems = 128
	maxProjectionBytes = 64 << 10
	maxResultBytes     = 1 << 20
)

// Scope isolates cached discovery and reads by organization, cluster, and credential.
type Scope struct {
	OrgID        string `json:"org_id,omitempty"`
	ClusterID    string `json:"cluster_id"`
	CredentialID string `json:"-"`
}

// ResourceDefinition is a server-owned canonical mapping to one API resource.
type ResourceDefinition struct {
	ID         string   `json:"resource_id"`
	Group      string   `json:"group,omitempty"`
	Version    string   `json:"version"`
	Resource   string   `json:"resource"`
	Kind       string   `json:"kind"`
	Namespaced bool     `json:"namespaced"`
	Aliases    []string `json:"aliases,omitempty"`
	Verbs      []string `json:"verbs,omitempty"`
	Available  bool     `json:"available"`
	Reason     string   `json:"reason,omitempty"`
}

// Discovery is a bounded cluster-driven API registry.
type Discovery struct {
	ObservedAt time.Time            `json:"observed_at"`
	Resources  []ResourceDefinition `json:"resources"`
	Partial    []PartialFailure     `json:"partial_failures,omitempty"`
}

// PartialFailure is a bounded failure class with no raw Kubernetes Status text.
type PartialFailure struct {
	ResourceID   string `json:"resource_id,omitempty"`
	Scope        string `json:"scope,omitempty"`
	GroupVersion string `json:"group_version,omitempty"`
	Class        string `json:"class"`
}

// ProjectedResource is a bounded safe view, never an arbitrary raw object.
type ProjectedResource struct {
	ResourceID          string            `json:"resource_id"`
	APIVersion          string            `json:"api_version"`
	Kind                string            `json:"kind"`
	Namespace           string            `json:"namespace,omitempty"`
	Name                string            `json:"name"`
	UID                 string            `json:"uid,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	Owners              []ObjectRef       `json:"owners,omitempty"`
	Summary             map[string]any    `json:"summary,omitempty"`
	Conditions          []Condition       `json:"conditions,omitempty"`
	ProjectionTruncated []string          `json:"projection_truncated,omitempty"`
}

type ObjectRef struct {
	APIVersion string `json:"api_version,omitempty"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
}

type Condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type ListOptions struct {
	ResourceID string
	Namespace  string
	Labels     string
	Fields     string
	Continue   string
	Limit      int
}

type ResourcePage struct {
	Items            []ProjectedResource `json:"items"`
	Continue         string              `json:"continue,omitempty"`
	Truncated        bool                `json:"truncated"`
	EncodedTruncated bool                `json:"encoded_truncated"`
	Omitted          []string            `json:"omitted_categories,omitempty"`
	Partial          []PartialFailure    `json:"partial_failures,omitempty"`
}

type Overview struct {
	Connector         string           `json:"connector"`
	ClusterID         string           `json:"cluster_id"`
	ObservedAt        time.Time        `json:"observed_at"`
	Nodes             int              `json:"nodes"`
	ReadyNodes        int              `json:"ready_nodes"`
	Pods              int              `json:"pods"`
	RunningPods       int              `json:"running_pods"`
	Namespaces        int              `json:"namespaces"`
	ActiveNamespaces  int              `json:"active_namespaces"`
	Workloads         int              `json:"workloads"`
	Warnings          int              `json:"warnings"`
	RequestedCPU      string           `json:"requested_cpu,omitempty"`
	LimitedCPU        string           `json:"limited_cpu,omitempty"`
	AllocatableCPU    string           `json:"allocatable_cpu,omitempty"`
	RequestedMemory   string           `json:"requested_memory,omitempty"`
	LimitedMemory     string           `json:"limited_memory,omitempty"`
	AllocatableMemory string           `json:"allocatable_memory,omitempty"`
	UsageCPU          string           `json:"usage_cpu,omitempty"`
	UsageMemory       string           `json:"usage_memory,omitempty"`
	UsageSource       string           `json:"usage_source"`
	MetricsStatus     string           `json:"metrics_status"`
	MetricsObservedAt *time.Time       `json:"metrics_observed_at,omitempty"`
	MetricsFresh      bool             `json:"metrics_fresh"`
	Truncated         bool             `json:"truncated"`
	Omitted           []string         `json:"omitted_categories,omitempty"`
	Partial           []PartialFailure `json:"partial_failures,omitempty"`
}

// ResourceUsage is one bounded exact pod or node Metrics API sample.
type ResourceUsage struct {
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace,omitempty"`
	Name      string    `json:"name"`
	Timestamp time.Time `json:"timestamp,omitempty"`
	Window    string    `json:"window,omitempty"`
	CPU       string    `json:"cpu,omitempty"`
	Memory    string    `json:"memory,omitempty"`
}

// MetricsSourceStatus summarizes one Metrics API source before display caps.
type MetricsSourceStatus struct {
	Availability string     `json:"availability"`
	Fresh        bool       `json:"fresh"`
	Total        int        `json:"total"`
	CPU          string     `json:"cpu,omitempty"`
	Memory       string     `json:"memory,omitempty"`
	ObservedAt   *time.Time `json:"observed_at,omitempty"`
}

// Usage reports Metrics API availability separately from an empty metric set.
type Usage struct {
	ObservedAt   time.Time           `json:"observed_at"`
	Availability string              `json:"availability"`
	Fresh        bool                `json:"fresh"`
	PodMetrics   MetricsSourceStatus `json:"pod_metrics"`
	NodeMetrics  MetricsSourceStatus `json:"node_metrics"`
	Pods         []ResourceUsage     `json:"pods,omitempty"`
	Nodes        []ResourceUsage     `json:"nodes,omitempty"`
	Truncated    bool                `json:"truncated"`
	Omitted      []string            `json:"omitted_categories,omitempty"`
	Partial      []PartialFailure    `json:"partial_failures,omitempty"`
}

type TopologyNode struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Status    string `json:"status,omitempty"`
}

type TopologyEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Type   string `json:"type"`
	Intent bool   `json:"intent,omitempty"`
}

type Topology struct {
	Nodes     []TopologyNode   `json:"nodes"`
	Edges     []TopologyEdge   `json:"edges"`
	NodeCap   int              `json:"node_cap"`
	EdgeCap   int              `json:"edge_cap"`
	Truncated bool             `json:"truncated"`
	Omitted   []string         `json:"omitted_categories,omitempty"`
	Partial   []PartialFailure `json:"partial_failures,omitempty"`
}

type PodLogs struct {
	ClusterID    string `json:"cluster_id"`
	Namespace    string `json:"namespace"`
	Pod          string `json:"pod"`
	Container    string `json:"container"`
	Previous     bool   `json:"previous"`
	SinceSeconds int    `json:"since_seconds"`
	TailLines    int    `json:"tail_lines"`
	Text         string `json:"text"`
	Truncated    bool   `json:"truncated"`
}

type discoveryCacheEntry struct {
	discovery Discovery
	expires   time.Time
}

type discoveryContextKey struct{}

const partialDiscoveryTTL = 5 * time.Second

// Scrubber removes sensitive substrings before text leaves the service.
type Scrubber interface{ Scrub(string) string }

// Service owns all Kubernetes path construction, decoding, projection, and relationships.
type Service struct {
	client   *Client
	scope    Scope
	ttl      time.Duration
	mu       *sync.Mutex
	cache    map[string]discoveryCacheEntry
	scrubber Scrubber
}

// SetScrubber installs the shared model/API text redactor.
func (service *Service) SetScrubber(scrubber Scrubber) {
	if service != nil {
		service.scrubber = scrubber
	}
}

// NewService constructs a scoped read-only Kubernetes application service.
func NewService(client *Client, scope Scope, discoveryTTL time.Duration) *Service {
	if discoveryTTL <= 0 || discoveryTTL > time.Hour {
		discoveryTTL = 5 * time.Minute
	}
	return &Service{client: client, scope: scope, ttl: discoveryTTL, mu: &sync.Mutex{}, cache: make(map[string]discoveryCacheEntry)}
}

// Scoped returns a service for another org/cluster/credential that shares the
// transport and keyed discovery cache with the original service.
func (service *Service) Scoped(scope Scope) *Service {
	if service == nil {
		return nil
	}
	return &Service{client: service.client, scope: scope, ttl: service.ttl, mu: service.mu, cache: service.cache, scrubber: service.scrubber}
}

// Scope returns the immutable cache and request identity of this service.
func (service *Service) Scope() Scope {
	if service == nil {
		return Scope{}
	}
	return service.scope
}

// ServiceRegistry resolves one shared service instance per org, cluster, and credential.
type ServiceRegistry struct {
	mu       sync.Mutex
	base     *Service
	services map[string]*Service
}

func NewServiceRegistry(base *Service) *ServiceRegistry {
	registry := &ServiceRegistry{base: base, services: make(map[string]*Service)}
	if base != nil {
		registry.services[base.cacheKey()] = base
	}
	return registry
}

func (registry *ServiceRegistry) Resolve(scope Scope) *Service {
	if registry == nil || registry.base == nil {
		return nil
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := scope.OrgID + "\x00" + scope.ClusterID + "\x00" + scope.CredentialID
	if service := registry.services[key]; service != nil {
		return service
	}
	service := registry.base.Scoped(scope)
	registry.services[key] = service
	return service
}

// ResolveOrg inherits the configured cluster and credential while changing org scope.
func (registry *ServiceRegistry) ResolveOrg(orgID string) *Service {
	if registry == nil || registry.base == nil {
		return nil
	}
	scope := registry.base.Scope()
	scope.OrgID = orgID
	return registry.Resolve(scope)
}

// InvalidateDiscovery expires only this service's scoped discovery generation.
func (service *Service) InvalidateDiscovery() {
	service.mu.Lock()
	delete(service.cache, service.cacheKey())
	service.mu.Unlock()
}

// Discover builds a preferred-version resource registry with partial failures.
func (service *Service) Discover(ctx context.Context) (Discovery, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	key := service.cacheKey()
	service.mu.Lock()
	if cached, ok := service.cache[key]; ok && time.Now().Before(cached.expires) {
		service.mu.Unlock()
		return cloneDiscovery(cached.discovery), nil
	}
	service.mu.Unlock()
	discovery := Discovery{ObservedAt: time.Now().UTC()}
	type apiVersions struct {
		Versions []string `json:"versions"`
	}
	var core apiVersions
	if err := service.client.GetJSON(ctx, "/api", &core); err != nil {
		return Discovery{}, err
	}
	if containsString(core.Versions, "v1") {
		service.discoverVersion(ctx, "", "v1", "/api/v1", &discovery)
	}
	type groupVersion struct {
		GroupVersion string `json:"groupVersion"`
		Version      string `json:"version"`
	}
	type apiGroup struct {
		Name             string         `json:"name"`
		PreferredVersion groupVersion   `json:"preferredVersion"`
		Versions         []groupVersion `json:"versions"`
	}
	var groups struct {
		Groups []apiGroup `json:"groups"`
	}
	if err := service.client.GetJSON(ctx, "/apis", &groups); err != nil {
		discovery.Partial = append(discovery.Partial, PartialFailure{Class: errorClass(err)})
	} else {
		for _, group := range groups.Groups {
			versions := append([]groupVersion{group.PreferredVersion}, group.Versions...)
			seen := make(map[string]bool)
			groupPartialBefore := len(discovery.Partial)
			for _, version := range versions {
				if version.Version == "" || seen[version.Version] {
					continue
				}
				seen[version.Version] = true
				before := len(discovery.Resources)
				service.discoverVersion(ctx, group.Name, version.Version, "/apis/"+url.PathEscape(group.Name)+"/"+url.PathEscape(version.Version), &discovery)
				if len(discovery.Resources) > before {
					discovery.Partial = discovery.Partial[:groupPartialBefore]
					break
				}
			}
		}
	}
	sort.SliceStable(discovery.Resources, func(i, j int) bool { return discovery.Resources[i].ID < discovery.Resources[j].ID })
	cacheTTL := service.ttl
	if len(discovery.Partial) > 0 && cacheTTL > partialDiscoveryTTL {
		cacheTTL = partialDiscoveryTTL
	}
	service.mu.Lock()
	service.cache[key] = discoveryCacheEntry{discovery: cloneDiscovery(discovery), expires: time.Now().Add(cacheTTL)}
	service.mu.Unlock()
	return discovery, nil
}

func (service *Service) discoverVersion(ctx context.Context, group, version, apiPath string, output *Discovery) {
	var list struct {
		Resources []struct {
			Name       string   `json:"name"`
			Kind       string   `json:"kind"`
			Namespaced bool     `json:"namespaced"`
			ShortNames []string `json:"shortNames"`
			Verbs      []string `json:"verbs"`
		} `json:"resources"`
	}
	if err := service.client.GetJSON(ctx, apiPath, &list); err != nil {
		output.Partial = append(output.Partial, PartialFailure{Scope: "discovery", GroupVersion: resourceID(group, version, ""), Class: errorClass(err)})
		return
	}
	for _, item := range list.Resources {
		if strings.Contains(item.Name, "/") || !containsString(item.Verbs, "get") || !containsString(item.Verbs, "list") {
			continue
		}
		output.Resources = append(output.Resources, ResourceDefinition{ID: resourceID(group, version, item.Name), Group: group, Version: version, Resource: item.Name, Kind: item.Kind, Namespaced: item.Namespaced, Aliases: append([]string(nil), item.ShortNames...), Verbs: append([]string(nil), item.Verbs...), Available: true})
	}
}

// List reads one discovered resource page and safely projects every item.
func (service *Service) List(ctx context.Context, options ListOptions) (ResourcePage, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	definition, err := service.resolve(ctx, options.ResourceID)
	if err != nil {
		return ResourcePage{}, err
	}
	apiPath, err := resourcePath(definition, options.Namespace, "")
	if err != nil {
		return ResourcePage{}, err
	}
	limit := options.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if options.Labels != "" {
		if !safeSelector(options.Labels) {
			return ResourcePage{}, ErrInvalidArguments
		}
		query.Set("labelSelector", options.Labels)
	}
	if options.Fields != "" {
		if !safeSelector(options.Fields) {
			return ResourcePage{}, ErrInvalidArguments
		}
		query.Set("fieldSelector", options.Fields)
	}
	if options.Continue != "" {
		query.Set("continue", boundString(options.Continue))
	}
	var raw struct {
		Items    []map[string]any `json:"items"`
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if err := service.client.GetJSON(ctx, apiPath+"?"+query.Encode(), &raw); err != nil {
		return ResourcePage{}, err
	}
	result := ResourcePage{Continue: boundString(raw.Metadata.Continue), Truncated: raw.Metadata.Continue != ""}
	itemLimit := operationBudgetFrom(ctx).takeItems(len(raw.Items))
	if itemLimit < len(raw.Items) {
		raw.Items = raw.Items[:itemLimit]
		result.Truncated = true
		result.Continue = ""
		result.Omitted = append(result.Omitted, definition.ID, "budget_exhausted")
		result.Partial = append(result.Partial, PartialFailure{ResourceID: definition.ID, Scope: "operation", Class: "budget_exhausted"})
	}
	encodedBytes := 0
	for index, item := range raw.Items {
		if index >= limit {
			result.Truncated = true
			result.Continue = ""
			result.Omitted = append(result.Omitted, "item_limit")
			result.Partial = append(result.Partial, PartialFailure{ResourceID: definition.ID, Scope: "projection", Class: "item_limit"})
			break
		}
		projected := service.projectResource(definition, item)
		encoded, _ := json.Marshal(projected)
		if encodedBytes+len(encoded) > maxResultBytes {
			result.Truncated = true
			result.EncodedTruncated = true
			result.Continue = ""
			result.Omitted = append(result.Omitted, "encoded_result_size")
			result.Partial = append(result.Partial, PartialFailure{ResourceID: definition.ID, Scope: "projection", Class: "encoded_result_size"})
			break
		}
		encodedBytes += len(encoded)
		result.Items = append(result.Items, projected)
	}
	return result, nil
}

// Get reads one discovered resource by canonical ID and name.
func (service *Service) Get(ctx context.Context, resourceID, namespace, name string) (ProjectedResource, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	definition, err := service.resolve(ctx, resourceID)
	if err != nil {
		return ProjectedResource{}, err
	}
	apiPath, err := resourcePath(definition, namespace, name)
	if err != nil {
		return ProjectedResource{}, err
	}
	var raw map[string]any
	if err := service.client.GetJSON(ctx, apiPath, &raw); err != nil {
		return ProjectedResource{}, err
	}
	return service.projectResource(definition, raw), nil
}

// Overview returns bounded health counts and explicit partial failures.
func (service *Service) Overview(ctx context.Context) (Overview, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	result := Overview{Connector: "kubernetes", ClusterID: service.scope.ClusterID, ObservedAt: time.Now().UTC(), UsageSource: "unavailable", MetricsStatus: "unavailable"}
	var err error
	ctx, _, err = service.withDiscovery(ctx)
	if err != nil {
		return result, err
	}
	requestedCPU, limitedCPU := new(big.Rat), new(big.Rat)
	requestedMemory, limitedMemory := new(big.Rat), new(big.Rat)
	allocatableCPU, allocatableMemory := new(big.Rat), new(big.Rat)
	for _, request := range []struct {
		id    string
		apply func(ProjectedResource)
	}{
		{"core~v1~nodes", func(item ProjectedResource) {
			result.Nodes++
			if conditionTrue(item.Conditions, "Ready") {
				result.ReadyNodes++
			}
			addSummaryQuantity(allocatableCPU, item.Summary, "allocatable_cpu")
			addSummaryQuantity(allocatableMemory, item.Summary, "allocatable_memory")
		}},
		{"core~v1~pods", func(item ProjectedResource) {
			result.Pods++
			if item.Summary["phase"] == "Running" {
				result.RunningPods++
			}
			addSummaryQuantity(requestedCPU, item.Summary, "requested_cpu")
			addSummaryQuantity(limitedCPU, item.Summary, "limited_cpu")
			addSummaryQuantity(requestedMemory, item.Summary, "requested_memory")
			addSummaryQuantity(limitedMemory, item.Summary, "limited_memory")
		}},
		{"core~v1~namespaces", func(item ProjectedResource) {
			result.Namespaces++
			if phase := summaryString(item.Summary, "phase"); phase == "" || phase == "Active" {
				result.ActiveNamespaces++
			}
		}},
		{"apps~v1~deployments", func(ProjectedResource) { result.Workloads++ }},
		{"apps~v1~statefulsets", func(ProjectedResource) { result.Workloads++ }},
		{"apps~v1~daemonsets", func(ProjectedResource) { result.Workloads++ }},
		{"batch~v1~jobs", func(ProjectedResource) { result.Workloads++ }},
		{"batch~v1~cronjobs", func(ProjectedResource) { result.Workloads++ }},
		{"core~v1~events", func(item ProjectedResource) {
			if item.Summary["type"] == "Warning" {
				result.Warnings++
			}
		}},
	} {
		page, err := service.listAll(ctx, ListOptions{ResourceID: request.id})
		if err != nil {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: request.id, Class: errorClass(err)})
			continue
		}
		for _, item := range page.Items {
			request.apply(item)
		}
		result.Partial = append(result.Partial, page.Partial...)
		if page.Truncated {
			result.Truncated = true
			result.Omitted = appendUnique(result.Omitted, request.id)
		}
	}
	result.RequestedCPU, result.LimitedCPU, result.AllocatableCPU = rationalString(requestedCPU), rationalString(limitedCPU), rationalString(allocatableCPU)
	result.RequestedMemory, result.LimitedMemory, result.AllocatableMemory = rationalString(requestedMemory), rationalString(limitedMemory), rationalString(allocatableMemory)
	usage, usageErr := service.Usage(ctx, "", maxPageSize)
	if usageErr != nil {
		result.Partial = append(result.Partial, PartialFailure{ResourceID: "metrics.k8s.io", Class: errorClass(usageErr)})
	} else {
		result.Partial = append(result.Partial, usage.Partial...)
		result.Truncated = result.Truncated || usage.Truncated
		for _, omitted := range usage.Omitted {
			result.Omitted = appendUnique(result.Omitted, omitted)
		}
		selected := usage.NodeMetrics
		if selected.Total > 0 {
			result.UsageSource = "node_metrics"
		} else if usage.PodMetrics.Total > 0 {
			selected = usage.PodMetrics
			result.UsageSource = "pod_metrics"
		}
		if selected.Total > 0 {
			result.MetricsStatus = selected.Availability
			result.MetricsFresh = selected.Fresh
			result.UsageCPU = selected.CPU
			result.UsageMemory = selected.Memory
			result.MetricsObservedAt = selected.ObservedAt
		}
	}
	return result, nil
}

// Usage returns bounded pod and node Metrics API samples with freshness metadata.
func (service *Service) Usage(ctx context.Context, namespace string, limit int) (Usage, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	if namespace != "" && !safeSegment(namespace) {
		return Usage{}, ErrInvalidArguments
	}
	if limit <= 0 || limit > maxPageSize {
		limit = maxPageSize
	}
	result := Usage{
		ObservedAt: time.Now().UTC(), Availability: "unavailable",
		PodMetrics:  MetricsSourceStatus{Availability: "unavailable"},
		NodeMetrics: MetricsSourceStatus{Availability: "unavailable"},
	}
	ctx, discovery, err := service.withDiscovery(ctx)
	if err != nil {
		return result, err
	}
	available := make(map[string]bool, len(discovery.Resources))
	for _, definition := range discovery.Resources {
		available[definition.ID] = true
	}
	for _, request := range []struct {
		id   string
		kind string
	}{
		{"metrics.k8s.io~v1beta1~pods", "Pod"},
		{"metrics.k8s.io~v1beta1~nodes", "Node"},
	} {
		if !available[request.id] {
			result.Omitted = appendUnique(result.Omitted, request.id)
			continue
		}
		metricNamespace := namespace
		if request.kind == "Node" {
			metricNamespace = ""
		}
		page, listErr := service.listAll(ctx, ListOptions{ResourceID: request.id, Namespace: metricNamespace})
		if listErr != nil {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: request.id, Class: errorClass(listErr)})
			continue
		}
		if page.Truncated {
			result.Truncated = true
			result.Omitted = appendUnique(result.Omitted, request.id)
		}
		result.Partial = append(result.Partial, page.Partial...)
		for _, item := range page.Items {
			metric := ResourceUsage{Kind: request.kind, Namespace: item.Namespace, Name: item.Name, Window: summaryString(item.Summary, "window"), CPU: summaryString(item.Summary, "usage_cpu"), Memory: summaryString(item.Summary, "usage_memory")}
			if timestamp := summaryString(item.Summary, "timestamp"); timestamp != "" {
				metric.Timestamp, _ = time.Parse(time.RFC3339Nano, timestamp)
			}
			if request.kind == "Pod" {
				result.Pods = append(result.Pods, metric)
			} else {
				result.Nodes = append(result.Nodes, metric)
			}
		}
		if request.kind == "Pod" {
			result.PodMetrics = summarizeMetricsSource(result.Pods, result.ObservedAt)
		} else {
			result.NodeMetrics = summarizeMetricsSource(result.Nodes, result.ObservedAt)
		}
	}
	if len(result.Pods)+len(result.Nodes) == 0 {
		return result, nil
	}
	result.Availability = "available"
	result.Fresh = true
	cutoff := result.ObservedAt.Add(-5 * time.Minute)
	for _, metric := range append(append([]ResourceUsage(nil), result.Pods...), result.Nodes...) {
		if metric.Timestamp.IsZero() || metric.Timestamp.Before(cutoff) {
			result.Fresh = false
			result.Availability = "stale"
			break
		}
	}
	if len(result.Pods) > limit {
		result.Pods = result.Pods[:limit]
		result.Truncated = true
		result.Omitted = appendUnique(result.Omitted, "metrics.k8s.io~v1beta1~pods")
		result.Partial = append(result.Partial, PartialFailure{ResourceID: "metrics.k8s.io~v1beta1~pods", Class: "item_limit"})
	}
	if len(result.Nodes) > limit {
		result.Nodes = result.Nodes[:limit]
		result.Truncated = true
		result.Omitted = appendUnique(result.Omitted, "metrics.k8s.io~v1beta1~nodes")
		result.Partial = append(result.Partial, PartialFailure{ResourceID: "metrics.k8s.io~v1beta1~nodes", Class: "item_limit"})
	}
	return result, nil
}

func summarizeMetricsSource(metrics []ResourceUsage, observedAt time.Time) MetricsSourceStatus {
	status := MetricsSourceStatus{Availability: "available", Fresh: true, Total: len(metrics)}
	cpu, memory := new(big.Rat), new(big.Rat)
	cutoff := observedAt.Add(-5 * time.Minute)
	for _, metric := range metrics {
		if metric.Timestamp.IsZero() || metric.Timestamp.Before(cutoff) {
			status.Availability = "stale"
			status.Fresh = false
		}
		if parsed, ok := new(big.Rat).SetString(metric.CPU); ok {
			cpu.Add(cpu, parsed)
		}
		if parsed, ok := new(big.Rat).SetString(metric.Memory); ok {
			memory.Add(memory, parsed)
		}
		if !metric.Timestamp.IsZero() && (status.ObservedAt == nil || metric.Timestamp.After(*status.ObservedAt)) {
			timestamp := metric.Timestamp
			status.ObservedAt = &timestamp
		}
	}
	status.CPU = rationalString(cpu)
	status.Memory = rationalString(memory)
	return status
}

// Topology derives bounded containment, ownership, and scheduling edges.
func (service *Service) Topology(ctx context.Context, namespace string, nodeCap, edgeCap int) (Topology, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	if nodeCap <= 0 || nodeCap > 500 {
		nodeCap = 200
	}
	if edgeCap <= 0 || edgeCap > 1000 {
		edgeCap = 400
	}
	result := Topology{NodeCap: nodeCap, EdgeCap: edgeCap}
	ctx, discovery, err := service.withDiscovery(ctx)
	if err != nil {
		return result, err
	}
	available := make(map[string]bool, len(discovery.Resources))
	definitions := make(map[string]ResourceDefinition, len(discovery.Resources))
	for _, definition := range discovery.Resources {
		available[definition.ID] = true
		definitions[definition.ID] = definition
	}
	result.Partial = append(result.Partial, discovery.Partial...)
	seen := make(map[string]bool)
	resources := make(map[string]ProjectedResource)
	resourceByNode := make(map[string]string)
	markOmitted := func(resourceID string) {
		result.Omitted = appendUnique(result.Omitted, resourceID)
		if definition, ok := definitions[resourceID]; ok {
			result.Omitted = appendUnique(result.Omitted, "category="+resourceCategory(definition.Kind))
		}
	}
	addEdge := func(edge TopologyEdge) {
		if !seen[edge.From] || !seen[edge.To] {
			return
		}
		for _, existing := range result.Edges {
			if existing == edge {
				return
			}
		}
		if len(result.Edges) < edgeCap {
			result.Edges = append(result.Edges, edge)
		} else {
			result.Truncated = true
		}
	}
	availableResourceIDs := make([]string, 0, len(topologyResourceIDs))
	for _, resourceID := range topologyResourceIDs {
		if !available[resourceID] {
			result.Omitted = appendUnique(result.Omitted, resourceID)
			continue
		}
		availableResourceIDs = append(availableResourceIDs, resourceID)
	}
	collectionBudget := min(maxCollectedItems, nodeCap+2*edgeCap)
	if collectionBudget < len(availableResourceIDs) {
		collectionBudget = len(availableResourceIDs)
	}
	resourceBudgets := topologyCollectionBudgets(availableResourceIDs, definitions, collectionBudget)
	pages := make(map[string][]ProjectedResource, len(availableResourceIDs))
	for _, resourceID := range availableResourceIDs {
		remaining := resourceBudgets[resourceID]
		continuation := ""
		for remaining > 0 {
			page, listErr := service.List(ctx, ListOptions{ResourceID: resourceID, Namespace: namespace, Continue: continuation, Limit: min(remaining, maxPageSize)})
			if listErr != nil {
				result.Partial = append(result.Partial, PartialFailure{ResourceID: resourceID, Class: errorClass(listErr)})
				markOmitted(resourceID)
				break
			}
			pages[resourceID] = append(pages[resourceID], page.Items...)
			result.Partial = append(result.Partial, page.Partial...)
			remaining -= len(page.Items)
			continuation = page.Continue
			if continuation == "" || len(page.Items) == 0 {
				if page.Truncated || len(page.Items) == 0 && continuation != "" {
					result.Truncated = true
					markOmitted(resourceID)
				}
				break
			}
		}
		if continuation != "" {
			result.Truncated = true
			markOmitted(resourceID)
		}
		sort.SliceStable(pages[resourceID], func(i, j int) bool {
			return objectID(pages[resourceID][i].Kind, pages[resourceID][i].Namespace, pages[resourceID][i].Name) < objectID(pages[resourceID][j].Kind, pages[resourceID][j].Namespace, pages[resourceID][j].Name)
		})
	}
	cappedResourceID := ""
	for index := 0; len(result.Nodes) < nodeCap; index++ {
		added := false
		for _, resourceID := range topologyResourceIDs {
			items := pages[resourceID]
			if index >= len(items) {
				continue
			}
			added = true
			item := items[index]
			id := objectID(item.Kind, item.Namespace, item.Name)
			if seen[id] {
				continue
			}
			status := summaryString(item.Summary, "phase")
			if status == "" && conditionTrue(item.Conditions, "Ready") {
				status = "Ready"
			}
			seen[id] = true
			resources[id] = item
			resourceByNode[id] = resourceID
			result.Nodes = append(result.Nodes, TopologyNode{ID: id, Kind: item.Kind, Namespace: item.Namespace, Name: item.Name, Status: status})
			if len(result.Nodes) == nodeCap {
				cappedResourceID = resourceID
				break
			}
		}
		if !added {
			break
		}
	}
	if cappedResourceID != "" {
		result.Truncated = true
		markOmitted(cappedResourceID)
	}
	for _, resourceID := range topologyResourceIDs {
		items := pages[resourceID]
		admitted := 0
		for _, item := range items {
			if seen[objectID(item.Kind, item.Namespace, item.Name)] {
				admitted++
			}
		}
		if admitted < len(items) {
			result.Truncated = true
			markOmitted(resourceID)
		}
	}
	deriveTopologyEdges(resources, func(edge TopologyEdge) {
		before := len(result.Edges)
		addEdge(edge)
		if len(result.Edges) == edgeCap && len(result.Edges) > before {
			result.Truncated = true
			markOmitted(resourceByNode[edge.From])
			markOmitted(resourceByNode[edge.To])
		}
		if len(result.Edges) == before && seen[edge.From] && seen[edge.To] && len(result.Edges) >= edgeCap {
			markOmitted(resourceByNode[edge.From])
			markOmitted(resourceByNode[edge.To])
		}
	})
	sort.SliceStable(result.Nodes, func(i, j int) bool { return result.Nodes[i].ID < result.Nodes[j].ID })
	sort.SliceStable(result.Edges, func(i, j int) bool {
		if result.Edges[i].From != result.Edges[j].From {
			return result.Edges[i].From < result.Edges[j].From
		}
		if result.Edges[i].To != result.Edges[j].To {
			return result.Edges[i].To < result.Edges[j].To
		}
		return result.Edges[i].Type < result.Edges[j].Type
	})
	return result, nil
}

func topologyCollectionBudgets(resourceIDs []string, definitions map[string]ResourceDefinition, total int) map[string]int {
	budgets := make(map[string]int, len(resourceIDs))
	if len(resourceIDs) == 0 || total <= 0 {
		return budgets
	}
	categories := make([]string, 0)
	byCategory := make(map[string][]string)
	for _, resourceID := range resourceIDs {
		category := resourceCategory(definitions[resourceID].Kind)
		if len(byCategory[category]) == 0 {
			categories = append(categories, category)
		}
		byCategory[category] = append(byCategory[category], resourceID)
		budgets[resourceID] = 1
	}
	remaining := total - len(resourceIDs)
	next := make(map[string]int, len(categories))
	for remaining > 0 {
		for _, category := range categories {
			resources := byCategory[category]
			resourceID := resources[next[category]%len(resources)]
			budgets[resourceID]++
			next[category]++
			remaining--
			if remaining == 0 {
				break
			}
		}
	}
	return budgets
}

// PodLogs reads one bounded pod log stream, optionally selecting a container.
func (service *Service) PodLogs(ctx context.Context, namespace, pod, container string, previous bool, sinceSeconds, tailLines int) (PodLogs, error) {
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	for _, value := range []string{namespace, pod} {
		if !safeSegment(value) {
			return PodLogs{}, ErrInvalidEndpoint
		}
	}
	if container != "" && !safeSegment(container) {
		return PodLogs{}, ErrInvalidEndpoint
	}
	if sinceSeconds < 0 || sinceSeconds > 86400 {
		return PodLogs{}, ErrInvalidArguments
	}
	if sinceSeconds == 0 {
		sinceSeconds = 3600
	}
	if tailLines < 0 || tailLines > 5000 {
		return PodLogs{}, ErrInvalidArguments
	}
	if tailLines == 0 {
		tailLines = 500
	}
	query := url.Values{"previous": {strconv.FormatBool(previous)}, "sinceSeconds": {strconv.Itoa(sinceSeconds)}, "tailLines": {strconv.Itoa(tailLines)}}
	if container != "" {
		query.Set("container", container)
	}
	payload, truncated, err := service.client.getBounded(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods/"+url.PathEscape(pod)+"/log?"+query.Encode(), "*/*", maxPodLogBytes)
	if err != nil {
		return PodLogs{}, err
	}
	text := strings.ToValidUTF8(string(payload), "")
	if service.scrubber != nil {
		text = service.scrubber.Scrub(text)
	}
	return PodLogs{ClusterID: service.scope.ClusterID, Namespace: namespace, Pod: pod, Container: container, Previous: previous, SinceSeconds: sinceSeconds, TailLines: tailLines, Text: text, Truncated: truncated}, nil
}

// ParseQuantity converts a Kubernetes quantity to an exact rational value.
func ParseQuantity(value string) (*big.Rat, error) {
	value = strings.TrimSpace(value)
	match := quantityPattern.FindStringSubmatch(value)
	if match == nil {
		return nil, errors.New("kubernetes: invalid quantity")
	}
	suffixes := []struct {
		suffix     string
		multiplier *big.Rat
	}{
		{"Ki", binaryMultiplier(10)}, {"Mi", binaryMultiplier(20)}, {"Gi", binaryMultiplier(30)},
		{"Ti", binaryMultiplier(40)}, {"Pi", binaryMultiplier(50)}, {"Ei", binaryMultiplier(60)},
		{"m", decimalMultiplier(-3)}, {"u", decimalMultiplier(-6)}, {"n", decimalMultiplier(-9)},
		{"k", decimalMultiplier(3)}, {"K", decimalMultiplier(3)}, {"M", decimalMultiplier(6)},
		{"G", decimalMultiplier(9)}, {"T", decimalMultiplier(12)}, {"P", decimalMultiplier(15)}, {"E", decimalMultiplier(18)},
	}
	multiplier := big.NewRat(1, 1)
	for _, candidate := range suffixes {
		if match[3] == candidate.suffix {
			multiplier = candidate.multiplier
			break
		}
	}
	if match[3] != "" && multiplier.Cmp(big.NewRat(1, 1)) == 0 {
		return nil, errors.New("kubernetes: invalid quantity")
	}
	number, ok := new(big.Rat).SetString(match[1])
	if !ok {
		return nil, errors.New("kubernetes: invalid quantity")
	}
	if match[2] != "" {
		exponent, err := strconv.Atoi(match[2][1:])
		if err != nil || exponent < -100 || exponent > 100 {
			return nil, errors.New("kubernetes: invalid quantity")
		}
		number.Mul(number, decimalMultiplier(exponent))
	}
	return number.Mul(number, multiplier), nil
}

var quantityPattern = regexp.MustCompile(`^([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))([eE][+-]?[0-9]+)?([a-zA-Z]*)$`)

func binaryMultiplier(shift uint) *big.Rat {
	return new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), shift))
}

func decimalMultiplier(exponent int) *big.Rat {
	power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(exponent))), nil)
	if exponent < 0 {
		return new(big.Rat).SetFrac(big.NewInt(1), power)
	}
	return new(big.Rat).SetInt(power)
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (service *Service) resolve(ctx context.Context, id string) (ResourceDefinition, error) {
	discovery, ok := ctx.Value(discoveryContextKey{}).(Discovery)
	if !ok {
		var err error
		discovery, err = service.Discover(ctx)
		if err != nil {
			return ResourceDefinition{}, err
		}
	}
	for _, resource := range discovery.Resources {
		if resource.ID == id {
			return resource, nil
		}
	}
	return ResourceDefinition{}, ErrNotFound
}

func (service *Service) withDiscovery(ctx context.Context) (context.Context, Discovery, error) {
	if discovery, ok := ctx.Value(discoveryContextKey{}).(Discovery); ok {
		return ctx, discovery, nil
	}
	discovery, err := service.Discover(ctx)
	if err != nil {
		return ctx, Discovery{}, err
	}
	return context.WithValue(ctx, discoveryContextKey{}, discovery), discovery, nil
}

func resourcePath(definition ResourceDefinition, namespace, name string) (string, error) {
	if definition.Namespaced && name != "" && !safeSegment(namespace) {
		return "", ErrInvalidArguments
	}
	if namespace != "" && !safeSegment(namespace) {
		return "", ErrInvalidEndpoint
	}
	if name != "" && !safeSegment(name) {
		return "", ErrInvalidEndpoint
	}
	base := "/api/" + url.PathEscape(definition.Version)
	if definition.Group != "" {
		base = "/apis/" + url.PathEscape(definition.Group) + "/" + url.PathEscape(definition.Version)
	}
	if definition.Namespaced && namespace != "" {
		base += "/namespaces/" + url.PathEscape(namespace)
	}
	base += "/" + url.PathEscape(definition.Resource)
	if name != "" {
		base += "/" + url.PathEscape(name)
	}
	return base, nil
}

func (service *Service) projectResource(definition ResourceDefinition, raw map[string]any) ProjectedResource {
	metadata, _ := raw["metadata"].(map[string]any)
	labels, labelsTruncated := boundedStringMap(metadata["labels"], maxProjectionItems)
	result := ProjectedResource{ResourceID: definition.ID, APIVersion: boundString(stringValue(raw["apiVersion"])), Kind: boundString(stringValue(raw["kind"])), Namespace: boundString(stringValue(metadata["namespace"])), Name: boundString(stringValue(metadata["name"])), UID: boundString(stringValue(metadata["uid"])), Labels: labels}
	if labelsTruncated {
		result.ProjectionTruncated = append(result.ProjectionTruncated, "labels")
	}
	owners := mapSlice(metadata["ownerReferences"])
	if len(owners) > maxProjectionItems {
		owners = owners[:maxProjectionItems]
		result.ProjectionTruncated = append(result.ProjectionTruncated, "owners")
	}
	for _, owner := range owners {
		result.Owners = append(result.Owners, ObjectRef{APIVersion: boundString(stringValue(owner["apiVersion"])), Kind: boundString(stringValue(owner["kind"])), Name: boundString(stringValue(owner["name"])), UID: boundString(stringValue(owner["uid"]))})
	}
	status, _ := raw["status"].(map[string]any)
	conditions := mapSlice(status["conditions"])
	if len(conditions) > maxProjectionItems {
		conditions = conditions[:maxProjectionItems]
		result.ProjectionTruncated = append(result.ProjectionTruncated, "conditions")
	}
	for _, condition := range conditions {
		result.Conditions = append(result.Conditions, Condition{Type: boundString(stringValue(condition["type"])), Status: boundString(stringValue(condition["status"])), Reason: boundString(stringValue(condition["reason"]))})
	}
	result.Summary = safeSummary(definition.Kind, raw, service.scrubber)
	if definition.Kind == "Secret" && mapLength(raw["data"]) > maxProjectionItems {
		result.ProjectionTruncated = appendUnique(result.ProjectionTruncated, "data_keys")
	}
	if definition.Kind == "ConfigMap" && mapLength(raw["data"])+mapLength(raw["binaryData"]) > maxProjectionItems {
		result.ProjectionTruncated = appendUnique(result.ProjectionTruncated, "data_keys")
	}
	spec, _ := raw["spec"].(map[string]any)
	podSpec := spec
	if template, ok := spec["template"].(map[string]any); ok {
		if nested, ok := template["spec"].(map[string]any); ok {
			podSpec = nested
		}
	}
	if len(mapSlice(podSpec["containers"])) > maxProjectionItems {
		result.ProjectionTruncated = appendUnique(result.ProjectionTruncated, "containers")
	}
	if len(mapSlice(spec["rules"])) > maxProjectionItems {
		result.ProjectionTruncated = appendUnique(result.ProjectionTruncated, "rbac_rules")
	}
	if len(mapSlice(raw["subjects"])) > maxProjectionItems {
		result.ProjectionTruncated = appendUnique(result.ProjectionTruncated, "rbac_subjects")
	}
	if result.Kind == "" {
		result.Kind = definition.Kind
	}
	return enforceProjectionSize(result)
}

func enforceProjectionSize(result ProjectedResource) ProjectedResource {
	encodedSize := func() int { encoded, _ := json.Marshal(result); return len(encoded) }
	if encodedSize() <= maxProjectionBytes {
		return result
	}
	keys := make([]string, 0, len(result.Summary))
	for key := range result.Summary {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, key := range keys {
		delete(result.Summary, key)
		result.ProjectionTruncated = appendUnique(result.ProjectionTruncated, "summary")
		if encodedSize() <= maxProjectionBytes {
			return result
		}
	}
	labelKeys := make([]string, 0, len(result.Labels))
	for key := range result.Labels {
		labelKeys = append(labelKeys, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(labelKeys)))
	for _, key := range labelKeys {
		delete(result.Labels, key)
		result.ProjectionTruncated = appendUnique(result.ProjectionTruncated, "labels")
		if encodedSize() <= maxProjectionBytes {
			return result
		}
	}
	for len(result.Conditions) > 0 && encodedSize() > maxProjectionBytes {
		result.Conditions = result.Conditions[:len(result.Conditions)-1]
		result.ProjectionTruncated = appendUnique(result.ProjectionTruncated, "conditions")
	}
	for len(result.Owners) > 0 && encodedSize() > maxProjectionBytes {
		result.Owners = result.Owners[:len(result.Owners)-1]
		result.ProjectionTruncated = appendUnique(result.ProjectionTruncated, "owners")
	}
	return result
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func safeSummary(kind string, raw map[string]any, scrubber Scrubber) map[string]any {
	summary := make(map[string]any)
	metadata, _ := raw["metadata"].(map[string]any)
	if kind == "Secret" {
		summary["type"] = boundString(stringValue(raw["type"]))
		summary["keys"] = sortedKeys(raw["data"])
		return summary
	}
	if kind == "ConfigMap" {
		summary["keys"] = append(sortedKeys(raw["data"]), sortedKeys(raw["binaryData"])...)
		sort.Strings(summary["keys"].([]string))
		return summary
	}
	status, _ := raw["status"].(map[string]any)
	spec, _ := raw["spec"].(map[string]any)
	if generation, ok := metadata["generation"]; ok {
		summary["generation"] = safeScalar(generation)
	}
	if observedGeneration, ok := status["observedGeneration"]; ok {
		summary["observed_generation"] = safeScalar(observedGeneration)
	}
	if kind == "Event" {
		for _, key := range []string{"type", "reason", "action", "count", "firstTimestamp", "lastTimestamp"} {
			if value, ok := raw[key]; ok {
				summary[key] = safeScalar(value)
			}
		}
		if message := boundString(stringValue(raw["message"])); message != "" {
			if scrubber != nil {
				message = scrubber.Scrub(message)
			}
			summary["message"] = boundString(message)
		}
		if involved, ok := raw["involvedObject"].(map[string]any); ok {
			summary["involved_object"] = ObjectRef{APIVersion: boundString(stringValue(involved["apiVersion"])), Kind: boundString(stringValue(involved["kind"])), Namespace: boundString(stringValue(involved["namespace"])), Name: boundString(stringValue(involved["name"])), UID: boundString(stringValue(involved["uid"]))}
		}
	}
	for _, key := range []string{"phase", "replicas", "readyReplicas", "availableReplicas", "unavailableReplicas", "updatedReplicas", "currentReplicas", "desiredNumberScheduled", "numberReady", "succeeded", "failed", "active"} {
		if value, ok := status[key]; ok {
			summary[key] = safeScalar(value)
		}
	}
	if node := stringValue(spec["nodeName"]); node != "" {
		summary["node"] = boundString(node)
	}
	if kind == "Pod" {
		resourceSummary(summary, mapSlice(spec["containers"]))
		var restarts int64
		for _, container := range mapSlice(status["containerStatuses"]) {
			if value, ok := container["restartCount"].(float64); ok {
				restarts += int64(value)
			}
		}
		summary["restart_count"] = float64(restarts)
	}
	if isWorkloadKind(kind) {
		projectWorkloadSummary(summary, spec, status)
	}
	if kind == "Node" {
		if allocatable, ok := status["allocatable"].(map[string]any); ok {
			copyQuantity(summary, "allocatable_cpu", allocatable["cpu"])
			copyQuantity(summary, "allocatable_memory", allocatable["memory"])
		}
	}
	if kind == "PodMetrics" {
		metricsSummary(summary, mapSlice(raw["containers"]))
	}
	if kind == "PodMetrics" || kind == "NodeMetrics" {
		for _, key := range []string{"timestamp", "window"} {
			if value := boundString(stringValue(raw[key])); value != "" {
				summary[key] = value
			}
		}
	}
	if kind == "Role" || kind == "ClusterRole" {
		rules := mapSlice(raw["rules"])
		projected := make([]map[string]any, 0, min(len(rules), maxProjectionItems))
		for _, rule := range rules[:min(len(rules), maxProjectionItems)] {
			projected = append(projected, map[string]any{
				"api_groups": boundedStrings(rule["apiGroups"]), "resources": boundedStrings(rule["resources"]),
				"resource_names": boundedStrings(rule["resourceNames"]), "verbs": boundedStrings(rule["verbs"]),
			})
		}
		if len(projected) > 0 {
			summary["rules"] = projected
		}
	}
	if kind == "RoleBinding" || kind == "ClusterRoleBinding" {
		subjects := mapSlice(raw["subjects"])
		projected := make([]ObjectRef, 0, min(len(subjects), maxProjectionItems))
		for _, subject := range subjects[:min(len(subjects), maxProjectionItems)] {
			projected = append(projected, ObjectRef{Kind: boundString(stringValue(subject["kind"])), Namespace: boundString(stringValue(subject["namespace"])), Name: boundString(stringValue(subject["name"]))})
		}
		if len(projected) > 0 {
			summary["subjects"] = projected
		}
		if roleRef, ok := raw["roleRef"].(map[string]any); ok {
			summary["role_ref"] = ObjectRef{Kind: boundString(stringValue(roleRef["kind"])), Name: boundString(stringValue(roleRef["name"]))}
		}
	}
	if kind == "NodeMetrics" {
		if usage, ok := raw["usage"].(map[string]any); ok {
			copyQuantity(summary, "usage_cpu", usage["cpu"])
			copyQuantity(summary, "usage_memory", usage["memory"])
		}
	}
	if selector, ok := spec["selector"].(map[string]any); ok {
		if matchLabels, nested := selector["matchLabels"].(map[string]any); nested {
			summary["selector"] = stringMap(matchLabels)
		} else {
			summary["selector"] = stringMap(selector)
		}
	}
	projectTopologyReferences(kind, metadata, spec, summary)
	if created := stringValue(metadata["creationTimestamp"]); created != "" {
		summary["created_at"] = boundString(created)
	}
	return summary
}

func isWorkloadKind(kind string) bool {
	switch kind {
	case "Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob":
		return true
	}
	return false
}

func projectWorkloadSummary(summary, spec, status map[string]any) {
	for _, key := range []string{"replicas", "parallelism", "completions", "minReadySeconds", "progressDeadlineSeconds", "terminationGracePeriodSeconds"} {
		if value, ok := spec[key]; ok {
			summary[camelToSnake(key)] = safeScalar(value)
		}
	}
	for _, key := range []string{"replicas", "readyReplicas", "currentReplicas", "updatedReplicas", "availableReplicas", "unavailableReplicas", "desiredNumberScheduled", "currentNumberScheduled", "numberReady", "succeeded", "failed", "active"} {
		if value, ok := status[key]; ok {
			summary[camelToSnake(key)] = safeScalar(value)
		}
	}
	if strategy, ok := spec["strategy"].(map[string]any); ok {
		summary["rollout_strategy"] = boundString(stringValue(strategy["type"]))
	}
	if strategy, ok := spec["updateStrategy"].(map[string]any); ok {
		summary["update_strategy"] = boundString(stringValue(strategy["type"]))
	}
	podSpec := spec
	if template, ok := spec["template"].(map[string]any); ok {
		if nested, ok := template["spec"].(map[string]any); ok {
			podSpec = nested
		}
	}
	if grace, ok := podSpec["terminationGracePeriodSeconds"]; ok {
		summary["termination_grace_seconds"] = safeScalar(grace)
	}
	containers := mapSlice(podSpec["containers"])
	projected := make([]map[string]any, 0, min(len(containers), maxProjectionItems))
	for _, container := range containers {
		if len(projected) == maxProjectionItems {
			break
		}
		item := map[string]any{"name": boundString(stringValue(container["name"])), "image": boundString(stringValue(container["image"]))}
		var probes []string
		for _, probe := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
			if value, ok := container[probe].(map[string]any); ok {
				probes = append(probes, probeType(value))
			}
		}
		if len(probes) > 0 {
			item["probes"] = probes
		}
		if resources, ok := container["resources"].(map[string]any); ok {
			for _, key := range []string{"requests", "limits"} {
				if values, ok := resources[key].(map[string]any); ok {
					safe, _ := boundedStringMap(values, maxProjectionItems)
					if len(safe) > 0 {
						item[key] = safe
					}
				}
			}
		}
		projected = append(projected, item)
	}
	if len(projected) > 0 {
		summary["containers"] = projected
	}
	if selector, ok := podSpec["nodeSelector"].(map[string]any); ok {
		summary["node_selector"] = stringMap(selector)
	}
	if affinity, ok := podSpec["affinity"].(map[string]any); ok {
		var kinds []string
		for _, key := range []string{"nodeAffinity", "podAffinity", "podAntiAffinity"} {
			if value, present := affinity[key].(map[string]any); present && len(value) > 0 {
				kinds = append(kinds, camelToSnake(key))
			}
		}
		if len(kinds) > 0 {
			summary["affinity"] = kinds
		}
	}
	if spreads := mapSlice(podSpec["topologySpreadConstraints"]); len(spreads) > 0 {
		var projected []string
		for _, spread := range spreads[:min(len(spreads), maxProjectionItems)] {
			projected = append(projected, boundString(stringValue(spread["topologyKey"]))+":"+boundString(stringValue(spread["whenUnsatisfiable"])))
		}
		summary["topology_spread"] = projected
	}
	if selector, ok := spec["selector"].(map[string]any); ok {
		if matchLabels, ok := selector["matchLabels"].(map[string]any); ok {
			summary["selector"] = stringMap(matchLabels)
		}
	}
}

func probeType(probe map[string]any) string {
	for _, kind := range []string{"httpGet", "tcpSocket", "grpc", "exec"} {
		if _, ok := probe[kind]; ok {
			return strings.TrimSuffix(kind, "Get")
		}
	}
	return "configured"
}

func camelToSnake(value string) string {
	var result strings.Builder
	for index, r := range value {
		if index > 0 && r >= 'A' && r <= 'Z' {
			result.WriteByte('_')
		}
		result.WriteRune([]rune(strings.ToLower(string(r)))[0])
	}
	return result.String()
}

func resourceSummary(summary map[string]any, containers []map[string]any) {
	totals := map[string]*big.Rat{"requested_cpu": new(big.Rat), "requested_memory": new(big.Rat), "limited_cpu": new(big.Rat), "limited_memory": new(big.Rat)}
	for _, container := range containers {
		resources, _ := container["resources"].(map[string]any)
		for source, prefix := range map[string]string{"requests": "requested_", "limits": "limited_"} {
			values, _ := resources[source].(map[string]any)
			for _, name := range []string{"cpu", "memory"} {
				if quantity, err := ParseQuantity(stringValue(values[name])); err == nil {
					totals[prefix+name].Add(totals[prefix+name], quantity)
				}
			}
		}
	}
	for key, value := range totals {
		if value.Sign() != 0 {
			summary[key] = value.RatString()
		}
	}
}

func metricsSummary(summary map[string]any, containers []map[string]any) {
	cpu, memory := new(big.Rat), new(big.Rat)
	for _, container := range containers {
		usage, _ := container["usage"].(map[string]any)
		if value, err := ParseQuantity(stringValue(usage["cpu"])); err == nil {
			cpu.Add(cpu, value)
		}
		if value, err := ParseQuantity(stringValue(usage["memory"])); err == nil {
			memory.Add(memory, value)
		}
	}
	if cpu.Sign() != 0 {
		summary["usage_cpu"] = cpu.RatString()
	}
	if memory.Sign() != 0 {
		summary["usage_memory"] = memory.RatString()
	}
}

func copyQuantity(summary map[string]any, key string, raw any) {
	if quantity, err := ParseQuantity(stringValue(raw)); err == nil {
		summary[key] = quantity.RatString()
	}
}

func addSummaryQuantity(total *big.Rat, summary map[string]any, key string) {
	if value, ok := summary[key].(string); ok {
		if quantity, parsed := new(big.Rat).SetString(value); parsed {
			total.Add(total, quantity)
		}
	}
}

func rationalString(value *big.Rat) string {
	if value.Sign() == 0 {
		return ""
	}
	return value.RatString()
}

func resourceID(group, version, resource string) string {
	if group == "" {
		group = "core"
	}
	if resource == "" {
		return group + "~" + version
	}
	return group + "~" + version + "~" + resource
}
func (service *Service) cacheKey() string {
	return service.scope.OrgID + "\x00" + service.scope.ClusterID + "\x00" + service.scope.CredentialID
}
func cloneDiscovery(value Discovery) Discovery {
	value.Resources = append([]ResourceDefinition(nil), value.Resources...)
	value.Partial = append([]PartialFailure(nil), value.Partial...)
	return value
}
func errorClass(err error) string {
	switch {
	case errors.Is(err, ErrOperationBudget):
		return "budget_exhausted"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrNotFound):
		return "unsupported"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline"):
		return "timeout"
	default:
		return "unavailable"
	}
}
func safeSegment(value string) bool {
	return value != "" && len(value) <= 253 && !strings.ContainsAny(value, "/\\\r\n?#") && value != "." && value != ".."
}
func safeSelector(value string) bool {
	return len(value) <= 1024 && !strings.ContainsAny(value, "\x00\r\n")
}
func boundString(value string) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) > maxStringBytes {
		return value[:maxStringBytes]
	}
	return value
}
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
func stringValue(value any) string { result, _ := value.(string); return result }
func mapSlice(value any) []map[string]any {
	raw, _ := value.([]any)
	result := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if mapped, ok := item.(map[string]any); ok {
			result = append(result, mapped)
		}
	}
	return result
}
func stringMap(value any) map[string]string {
	result, _ := boundedStringMap(value, maxProjectionItems)
	return result
}
func boundedStringMap(value any, limit int) (map[string]string, bool) {
	raw, _ := value.(map[string]any)
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	truncated := len(keys) > limit
	if truncated {
		keys = keys[:limit]
	}
	result := make(map[string]string)
	for _, key := range keys {
		item := raw[key]
		if text, ok := item.(string); ok {
			result[boundString(key)] = boundString(text)
		}
	}
	if len(result) == 0 {
		return nil, truncated
	}
	return result, truncated
}
func sortedKeys(value any) []string {
	raw, _ := value.(map[string]any)
	result := make([]string, 0, len(raw))
	for key := range raw {
		result = append(result, boundString(key))
	}
	sort.Strings(result)
	if len(result) > maxProjectionItems {
		result = result[:maxProjectionItems]
	}
	return result
}
func mapLength(value any) int {
	raw, _ := value.(map[string]any)
	return len(raw)
}
func boundedStrings(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, min(len(raw), maxProjectionItems))
	for _, item := range raw[:min(len(raw), maxProjectionItems)] {
		if text, ok := item.(string); ok {
			result = append(result, boundString(text))
		}
	}
	sort.Strings(result)
	return result
}
func safeScalar(value any) any {
	switch typed := value.(type) {
	case string:
		return boundString(typed)
	case float64, bool, nil:
		return typed
	default:
		return nil
	}
}
func conditionTrue(conditions []Condition, kind string) bool {
	for _, condition := range conditions {
		if condition.Type == kind && condition.Status == "True" {
			return true
		}
	}
	return false
}
func objectID(kind, namespace, name string) string {
	return strings.ToLower(kind) + ":" + namespace + ":" + name
}
