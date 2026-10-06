package kubernetes

import (
	"context"
	"math/big"
	"sort"
	"strings"
	"time"
)

type SyncStatus struct {
	State      string  `json:"state"`
	AgeSeconds float64 `json:"age_s"`
	Partial    bool    `json:"partial"`
}

type IssueOptions struct {
	Namespace string
	Severity  string
	Kind      string
	Limit     int
	Cursor    string
}

type Issue struct {
	Root      ObjectRef   `json:"root"`
	Rule      string      `json:"rule"`
	Severity  string      `json:"severity"`
	Count     int         `json:"count"`
	Examples  []ObjectRef `json:"examples,omitempty"`
	FirstSeen time.Time   `json:"first_seen,omitempty"`
	LastSeen  time.Time   `json:"last_seen,omitempty"`
}

type IssuePage struct {
	Items     []Issue          `json:"items"`
	Totals    map[string]int   `json:"totals"`
	Truncated bool             `json:"truncated"`
	Next      string           `json:"next,omitempty"`
	Partial   []PartialFailure `json:"partial_failures,omitempty"`
	Sync      SyncStatus       `json:"sync"`
}

type TopOptions struct {
	Kind      string
	Namespace string
	Sort      string
	Limit     int
}

type TopItem struct {
	ResourceUsage
	RequestCPU    string     `json:"request_cpu,omitempty"`
	RequestMemory string     `json:"request_memory,omitempty"`
	Owner         *ObjectRef `json:"owner,omitempty"`
}

type TopPage struct {
	Items        []TopItem        `json:"items"`
	Total        int              `json:"total"`
	Truncated    bool             `json:"truncated"`
	Availability string           `json:"availability"`
	Fresh        bool             `json:"fresh"`
	Partial      []PartialFailure `json:"partial_failures,omitempty"`
	Sync         SyncStatus       `json:"sync"`
}

var issueResourceIDs = []string{
	"core~v1~pods", "apps~v1~deployments", "apps~v1~statefulsets",
	"apps~v1~daemonsets", "batch~v1~jobs", "core~v1~nodes", "core~v1~events",
}

// Issues returns bounded findings from projected cluster resources.
func (service *Service) Issues(ctx context.Context, options IssueOptions) (IssuePage, error) {
	if options.Namespace != "" && !safeSegment(options.Namespace) || options.Severity != "" && options.Severity != "critical" && options.Severity != "warning" && options.Severity != "info" || options.Kind != "" && !safeSegment(options.Kind) {
		return IssuePage{}, ErrInvalidArguments
	}
	options.Limit = normalizePageLimit(options.Limit)
	ctx, cancel := ensureOperationBudgetRequests(ctx, overviewOperationRequests)
	defer cancel()
	page := IssuePage{Items: []Issue{}, Totals: map[string]int{}, Sync: SyncStatus{State: "direct"}}
	groups := make(map[string]*Issue)
	for _, resourceID := range issueResourceIDs {
		namespace := options.Namespace
		if resourceID == "core~v1~nodes" {
			namespace = ""
		}
		resources, err := service.listAll(ctx, ListOptions{ResourceID: resourceID, Namespace: namespace})
		if err != nil {
			page.Truncated = true
			page.Sync.Partial = true
			page.Partial = append(page.Partial, PartialFailure{ResourceID: resourceID, Class: errorClass(err)})
			continue
		}
		page.Partial = append(page.Partial, resources.Partial...)
		page.Truncated = page.Truncated || resources.Truncated
		page.Sync.Partial = page.Sync.Partial || resources.Truncated || len(resources.Partial) > 0
		for _, resource := range resources.Items {
			for _, finding := range classifyIssue(resource) {
				if options.Kind != "" && finding.root.Kind != options.Kind || options.Severity != "" && finding.severity != options.Severity {
					continue
				}
				key := finding.root.UID + "\x00" + finding.root.Kind + "\x00" + finding.root.Namespace + "\x00" + finding.root.Name + "\x00" + finding.rule
				issue := groups[key]
				if issue == nil {
					issue = &Issue{Root: finding.root, Rule: finding.rule, Severity: finding.severity, Examples: []ObjectRef{}}
					groups[key] = issue
				}
				issue.Count += finding.count
				if len(issue.Examples) < 5 {
					issue.Examples = append(issue.Examples, finding.example)
				}
				if !finding.at.IsZero() {
					if issue.FirstSeen.IsZero() || finding.at.Before(issue.FirstSeen) {
						issue.FirstSeen = finding.at
					}
					if issue.LastSeen.IsZero() || finding.at.After(issue.LastSeen) {
						issue.LastSeen = finding.at
					}
				}
			}
		}
	}
	for _, issue := range groups {
		page.Totals[issue.Severity]++
		page.Items = append(page.Items, *issue)
	}
	sort.Slice(page.Items, func(i, j int) bool {
		left, right := severityRank(page.Items[i].Severity), severityRank(page.Items[j].Severity)
		if left != right {
			return left < right
		}
		if page.Items[i].Count != page.Items[j].Count {
			return page.Items[i].Count > page.Items[j].Count
		}
		if page.Items[i].Root.Namespace != page.Items[j].Root.Namespace {
			return page.Items[i].Root.Namespace < page.Items[j].Root.Namespace
		}
		if page.Items[i].Root.Name != page.Items[j].Root.Name {
			return page.Items[i].Root.Name < page.Items[j].Root.Name
		}
		return page.Items[i].Rule < page.Items[j].Rule
	})
	start, end, next, err := pageWindow(options.Cursor, options.Limit, len(page.Items))
	if err != nil {
		return IssuePage{}, err
	}
	page.Next = next
	page.Truncated = page.Truncated || next != ""
	page.Items = page.Items[start:end]
	return page, nil
}

// Top returns the highest-usage pods or nodes from the Metrics API.
func (service *Service) Top(ctx context.Context, options TopOptions) (TopPage, error) {
	if options.Sort == "" {
		options.Sort = "cpu"
	}
	if options.Kind != "pod" && options.Kind != "node" || options.Namespace != "" && !safeSegment(options.Namespace) || options.Sort != "cpu" && options.Sort != "memory" {
		return TopPage{}, ErrInvalidArguments
	}
	if options.Kind == "node" && options.Namespace != "" {
		return TopPage{}, ErrInvalidArguments
	}
	if options.Limit <= 0 || options.Limit > 50 {
		options.Limit = 20
	}
	usage, err := service.Usage(ctx, options.Namespace, maxPageSize)
	if err != nil {
		return TopPage{}, err
	}
	items := usage.Pods
	status := usage.PodMetrics
	if options.Kind == "node" {
		items = usage.Nodes
		status = usage.NodeMetrics
	}
	result := TopPage{Availability: status.Availability, Fresh: status.Fresh, Partial: append([]PartialFailure(nil), usage.Partial...), Sync: SyncStatus{State: "direct", Partial: !status.Fresh || len(usage.Partial) > 0}}
	result.Total = len(items)
	for _, item := range items {
		result.Items = append(result.Items, TopItem{ResourceUsage: item})
	}
	if options.Kind == "pod" && len(result.Items) > 0 {
		pods, listErr := service.listAll(ctx, ListOptions{ResourceID: "core~v1~pods", Namespace: options.Namespace})
		if listErr != nil {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: "core~v1~pods", Class: errorClass(listErr)})
			result.Sync.Partial = true
		} else {
			lookup := make(map[string]ProjectedResource, len(pods.Items))
			for _, pod := range pods.Items {
				lookup[pod.Namespace+"/"+pod.Name] = pod
			}
			for index := range result.Items {
				pod, exists := lookup[result.Items[index].Namespace+"/"+result.Items[index].Name]
				if !exists {
					continue
				}
				result.Items[index].RequestCPU = summaryString(pod.Summary, "requested_cpu")
				result.Items[index].RequestMemory = summaryString(pod.Summary, "requested_memory")
				if len(pod.Owners) > 0 {
					owner := pod.Owners[0]
					owner.Namespace = pod.Namespace
					result.Items[index].Owner = &owner
				}
			}
			result.Partial = append(result.Partial, pods.Partial...)
			result.Sync.Partial = result.Sync.Partial || pods.Truncated || len(pods.Partial) > 0
		}
	}
	sort.SliceStable(result.Items, func(i, j int) bool {
		left := triageQuantity(result.Items[i].CPU)
		right := triageQuantity(result.Items[j].CPU)
		if options.Sort == "memory" {
			left = triageQuantity(result.Items[i].Memory)
			right = triageQuantity(result.Items[j].Memory)
		}
		if left == nil {
			left = new(big.Rat)
		}
		if right == nil {
			right = new(big.Rat)
		}
		return left.Cmp(right) > 0
	})
	if len(result.Items) > options.Limit {
		result.Items = result.Items[:options.Limit]
		result.Truncated = true
	}
	result.Truncated = result.Truncated || usage.Truncated
	return result, nil
}

func triageQuantity(value string) *big.Rat {
	if quantity, ok := new(big.Rat).SetString(value); ok {
		return quantity
	}
	quantity, err := ParseQuantity(value)
	if err != nil {
		return nil
	}
	return quantity
}

type issueFinding struct {
	root     ObjectRef
	example  ObjectRef
	rule     string
	severity string
	count    int
	at       time.Time
}

func classifyIssue(resource ProjectedResource) []issueFinding {
	ref := ObjectRef{APIVersion: resource.APIVersion, Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name, UID: resource.UID}
	root := ref
	if len(resource.Owners) > 0 {
		root = resource.Owners[0]
		if root.Namespace == "" {
			root.Namespace = resource.Namespace
		}
	}
	var findings []issueFinding
	add := func(rule, severity string, count int, at time.Time) {
		findings = append(findings, issueFinding{root: root, example: ref, rule: rule, severity: severity, count: count, at: at})
	}
	switch resource.Kind {
	case "Pod":
		phase := summaryString(resource.Summary, "phase")
		switch phase {
		case "Failed":
			add("pod.failed", "critical", 1, resourceTime(resource.Summary))
		case "Pending":
			add("pod.pending", "warning", 1, resourceTime(resource.Summary))
		}
	case "Deployment", "StatefulSet", "DaemonSet":
		desiredKeys := []string{"desired_replicas", "replicas"}
		if resource.Kind == "DaemonSet" {
			desiredKeys = []string{"desiredNumberScheduled", "desired_number_scheduled"}
		}
		desired, desiredKnown := triageSummaryInt(resource.Summary, desiredKeys...)
		available, availableKnown := triageSummaryInt(resource.Summary, "availableReplicas", "available_replicas", "numberReady", "number_ready", "readyReplicas", "ready_replicas")
		if desiredKnown && availableKnown && available < desired {
			add("workload.unavailable", "warning", int(desired-available), resourceTime(resource.Summary))
		}
	case "Job":
		if triageSummaryIntValue(resource.Summary, "failed") > 0 {
			add("job.failed", "critical", int(triageSummaryIntValue(resource.Summary, "failed")), resourceTime(resource.Summary))
		}
	case "Node":
		ready := false
		for _, condition := range resource.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
			if strings.HasSuffix(condition.Type, "Pressure") && condition.Status == "True" {
				add("node.pressure", "warning", 1, time.Time{})
			}
		}
		if !ready {
			add("node.not_ready", "critical", 1, time.Time{})
		}
	case "Event":
		if summaryString(resource.Summary, "type") == "Warning" {
			if involved, ok := resource.Summary["involved_object"].(ObjectRef); ok && involved.Name != "" {
				root = involved
				if root.Namespace == "" {
					root.Namespace = resource.Namespace
				}
			}
			count := int(triageSummaryIntValue(resource.Summary, "count"))
			if count <= 0 {
				count = 1
			}
			add("event.warning", "warning", count, resourceTime(resource.Summary))
		}
	}
	for index := range findings {
		findings[index].root = root
	}
	return findings
}

func triageSummaryIntValue(summary map[string]any, keys ...string) int64 {
	value, _ := triageSummaryInt(summary, keys...)
	return value
}

func triageSummaryInt(summary map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch value := summary[key].(type) {
		case float64:
			return int64(value), true
		case int64:
			return value, true
		case int:
			return int64(value), true
		}
	}
	return 0, false
}

func resourceTime(summary map[string]any) time.Time {
	for _, key := range []string{"lastTimestamp", "created_at", "firstTimestamp"} {
		if value := summaryString(summary, key); value != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return parsed
			}
		}
	}
	return time.Time{}
}

func severityRank(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}
