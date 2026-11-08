package kubernetes

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type GitOpsAppOptions struct {
	Namespace string
	Tool      string
	Status    string
	Limit     int
	Cursor    string
}

type GitOpsApp struct {
	Tool       string    `json:"tool"`
	Kind       string    `json:"kind"`
	Namespace  string    `json:"namespace"`
	Name       string    `json:"name"`
	Sync       string    `json:"sync"`
	Health     string    `json:"health"`
	Revision   string    `json:"revision,omitempty"`
	LastSyncAt time.Time `json:"last_sync_at,omitempty"`
	Message    string    `json:"message,omitempty"`
	Source     string    `json:"source,omitempty"`
	Suspended  bool      `json:"suspended"`
}

type GitOpsAppPage struct {
	Items     []GitOpsApp      `json:"items"`
	Available bool             `json:"available"`
	Reason    string           `json:"reason,omitempty"`
	Next      string           `json:"next,omitempty"`
	Partial   []PartialFailure `json:"partial_failures,omitempty"`
	Truncated bool             `json:"truncated"`
	// Total and Limit size the pager: Total counts every item the filters
	// match, and a non-zero Limit marks Total as present.
	Total int `json:"total,omitempty"`
	Limit int `json:"limit,omitempty"`
	// resume mints cursors inside the page for callers that show fewer rows.
	resume snapshotResume
}

// ResumeAfter returns the cursor that continues this list after the first
// shown rows of this page, for a caller that showed fewer rows than the page
// holds. It is empty when the page cannot be resumed that way.
func (page GitOpsAppPage) ResumeAfter(shown int) string { return page.resume.after(shown) }

type Rollout struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Phase      string `json:"phase"`
	Strategy   string `json:"strategy,omitempty"`
	Step       int    `json:"step"`
	TotalSteps int    `json:"total_steps"`
	StableRS   string `json:"stable_rs,omitempty"`
	CanaryRS   string `json:"canary_rs,omitempty"`
	Weight     int    `json:"weight,omitempty"`
	Message    string `json:"message,omitempty"`
}

type RolloutListOptions struct {
	Namespace string
	Limit     int
	Cursor    string
}

type RolloutPage struct {
	Items     []Rollout        `json:"items"`
	Available bool             `json:"available"`
	Reason    string           `json:"reason,omitempty"`
	Next      string           `json:"next,omitempty"`
	Truncated bool             `json:"truncated"`
	Partial   []PartialFailure `json:"partial_failures,omitempty"`
}

// GitOpsApps lists only discovered GitOps APIs and safely projected status.
func (service *Service) GitOpsApps(ctx context.Context, options GitOpsAppOptions) (GitOpsAppPage, error) {
	if options.Namespace != "" && !safeSegment(options.Namespace) || options.Tool != "" && options.Tool != "argocd" && options.Tool != "flux" || len(options.Status) > 64 {
		return GitOpsAppPage{}, ErrInvalidArguments
	}
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	ctx, discovery, err := service.withDiscovery(ctx)
	if err != nil {
		return GitOpsAppPage{}, err
	}
	result := GitOpsAppPage{Items: []GitOpsApp{}}
	for _, definition := range discovery.Resources {
		tool := gitOpsTool(definition)
		if tool == "" || options.Tool != "" && options.Tool != tool {
			continue
		}
		result.Available = true
		if !definition.Available {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: definition.ID, Class: "forbidden"})
			continue
		}
		page, listErr := service.listAll(ctx, ListOptions{ResourceID: definition.ID, Namespace: options.Namespace})
		if listErr != nil {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: definition.ID, Class: errorClass(listErr)})
			continue
		}
		result.Partial = append(result.Partial, page.Partial...)
		result.Truncated = result.Truncated || page.Truncated
		for _, resource := range page.Items {
			app := gitOpsAppFromResource(tool, resource)
			if options.Status != "" && !strings.EqualFold(options.Status, app.Sync) && !strings.EqualFold(options.Status, app.Health) {
				continue
			}
			result.Items = append(result.Items, app)
		}
	}
	if !result.Available {
		result.Reason = "GitOps APIs are not discovered"
	}
	sort.Slice(result.Items, func(i, j int) bool {
		left, right := gitOpsStatusRank(result.Items[i]), gitOpsStatusRank(result.Items[j])
		if left != right {
			return left < right
		}
		if result.Items[i].Namespace != result.Items[j].Namespace {
			return result.Items[i].Namespace < result.Items[j].Namespace
		}
		if result.Items[i].Name != result.Items[j].Name {
			return result.Items[i].Name < result.Items[j].Name
		}
		// An Argo CD Application and a Flux Kustomization can share a name.
		if result.Items[i].Tool != result.Items[j].Tool {
			return result.Items[i].Tool < result.Items[j].Tool
		}
		return result.Items[i].Kind < result.Items[j].Kind
	})
	rows := make([]string, len(result.Items))
	for position, app := range result.Items {
		rows[position] = app.Tool + "\x00" + app.Kind + "\x00" + app.Namespace + "\x00" + app.Name
	}
	cursor, err := bindSnapshotCursor("gitops_apps", service.indexScope(), struct{ Namespace, Tool, Status string }{options.Namespace, options.Tool, options.Status}, rows)
	if err != nil {
		return GitOpsAppPage{}, err
	}
	start, end, next, err := cursor.window(options.Cursor, normalizePageLimit(options.Limit), len(result.Items))
	if err != nil {
		return GitOpsAppPage{}, err
	}
	result.Total, result.Limit = len(result.Items), normalizePageLimit(options.Limit)
	result.Items = result.Items[start:end]
	result.Next = next
	result.resume = cursor.resumeFrom(start)
	result.Truncated = result.Truncated || next != ""
	return result, nil
}

// Rollout returns one safely projected Argo Rollouts status record.
func (service *Service) Rollout(ctx context.Context, namespace, name string) (Rollout, error) {
	if !safeSegment(namespace) || !safeSegment(name) {
		return Rollout{}, ErrInvalidArguments
	}
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	ctx, discovery, err := service.withDiscovery(ctx)
	if err != nil {
		return Rollout{}, err
	}
	for _, definition := range discovery.Resources {
		if definition.Kind != "Rollout" || definition.Group != "argoproj.io" {
			continue
		}
		if !definition.Available {
			return Rollout{}, ErrForbidden
		}
		resource, getErr := service.Get(ctx, definition.ID, namespace, name)
		if getErr != nil {
			return Rollout{}, getErr
		}
		return rolloutFromResource(resource), nil
	}
	return Rollout{}, ErrNotFound
}

// Rollouts lists discovered Argo Rollouts using the same projection as Rollout.
func (service *Service) Rollouts(ctx context.Context, options RolloutListOptions) (RolloutPage, error) {
	if options.Namespace != "" && !safeSegment(options.Namespace) {
		return RolloutPage{}, ErrInvalidArguments
	}
	options.Limit = normalizePageLimit(options.Limit)
	if len(options.Cursor) > 4096 {
		return RolloutPage{}, ErrInvalidArguments
	}
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	ctx, discovery, err := service.withDiscovery(ctx)
	if err != nil {
		return RolloutPage{}, err
	}
	result := RolloutPage{Items: []Rollout{}}
	for _, definition := range discovery.Resources {
		if definition.Kind != "Rollout" || definition.Group != "argoproj.io" {
			continue
		}
		result.Available = true
		if !definition.Available {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: definition.ID, Class: "forbidden"})
			return result, nil
		}
		page, listErr := service.List(ctx, ListOptions{ResourceID: definition.ID, Namespace: options.Namespace, Limit: options.Limit, Continue: options.Cursor})
		if listErr != nil {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: definition.ID, Class: errorClass(listErr)})
			result.Truncated = true
			return result, nil
		}
		for _, resource := range page.Items {
			result.Items = append(result.Items, rolloutFromResource(resource))
		}
		result.Next = page.Continue
		result.Truncated = page.Truncated
		result.Partial = append(result.Partial, page.Partial...)
		return result, nil
	}
	result.Reason = "Argo Rollouts CRD is not discovered"
	return result, nil
}

func gitOpsTool(definition ResourceDefinition) string {
	switch {
	case definition.Group == "argoproj.io" && definition.Kind == "Application":
		return "argocd"
	case strings.Contains(definition.Group, "toolkit.fluxcd.io") && (definition.Kind == "Kustomization" || definition.Kind == "HelmRelease"):
		return "flux"
	default:
		return ""
	}
}

func gitOpsAppFromResource(tool string, resource ProjectedResource) GitOpsApp {
	return GitOpsApp{
		Tool: tool, Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name,
		Sync: summaryString(resource.Summary, "gitops_sync"), Health: summaryString(resource.Summary, "gitops_health"),
		Revision: summaryString(resource.Summary, "gitops_revision"), LastSyncAt: parseGitOpsTime(summaryString(resource.Summary, "gitops_last_sync_at")),
		Message: summaryString(resource.Summary, "gitops_message"), Source: summaryString(resource.Summary, "gitops_source"),
		Suspended: resource.Summary["gitops_suspended"] == true,
	}
}

func rolloutFromResource(resource ProjectedResource) Rollout {
	return Rollout{
		Namespace: resource.Namespace, Name: resource.Name,
		Phase:      summaryString(resource.Summary, "rollout_phase"),
		Strategy:   summaryString(resource.Summary, "rollout_strategy"),
		Step:       int(triageSummaryIntValue(resource.Summary, "rollout_step")),
		TotalSteps: int(triageSummaryIntValue(resource.Summary, "rollout_total_steps")),
		StableRS:   summaryString(resource.Summary, "rollout_stable_rs"),
		CanaryRS:   summaryString(resource.Summary, "rollout_canary_rs"),
		Weight:     int(triageSummaryIntValue(resource.Summary, "rollout_weight")),
		Message:    summaryString(resource.Summary, "rollout_message"),
	}
}

func projectGitOpsSummary(kind string, raw map[string]any, summary map[string]any, scrubber Scrubber) {
	spec, _ := raw["spec"].(map[string]any)
	status, _ := raw["status"].(map[string]any)
	if kind == "Application" || kind == "Kustomization" || kind == "HelmRelease" {
		syncStatus, healthStatus := "Unknown", "Unknown"
		if sync, ok := status["sync"].(map[string]any); ok {
			if value := stringValue(sync["status"]); value != "" {
				syncStatus = value
			}
			if value := stringValue(sync["revision"]); value != "" {
				summary["gitops_revision"] = boundString(value)
			}
		}
		if health, ok := status["health"].(map[string]any); ok {
			if value := stringValue(health["status"]); value != "" {
				healthStatus = value
			}
		}
		for _, condition := range mapSlice(status["conditions"]) {
			if stringValue(condition["type"]) == "Ready" {
				value := stringValue(condition["status"])
				if value != "" {
					healthStatus = value
					if value == "True" {
						syncStatus = "Synced"
					} else {
						syncStatus = "OutOfSync"
					}
				}
				if message := stringValue(condition["message"]); message != "" {
					summary["gitops_message"] = scrubbedBounded(message, scrubber, 256)
				}
			}
		}
		summary["gitops_sync"], summary["gitops_health"] = syncStatus, healthStatus
		if value := stringValue(status["lastAppliedRevision"]); value != "" && summary["gitops_revision"] == nil {
			summary["gitops_revision"] = boundString(value)
		}
		if source, ok := spec["source"].(map[string]any); ok {
			summary["gitops_source"] = sanitizeGitOpsSource(stringValue(source["repoURL"]))
		}
		if source := stringValue(spec["url"]); source != "" {
			summary["gitops_source"] = sanitizeGitOpsSource(source)
		}
		summary["gitops_suspended"] = spec["suspend"] == true
		if operation, ok := status["operationState"].(map[string]any); ok {
			if message := stringValue(operation["message"]); message != "" {
				summary["gitops_message"] = scrubbedBounded(message, scrubber, 256)
			}
			if finished := stringValue(operation["finishedAt"]); finished != "" {
				summary["gitops_last_sync_at"] = boundString(finished)
			}
		}
		for _, key := range []string{"lastHandledReconcileAt", "lastAppliedTime", "lastTransitionTime"} {
			if value := stringValue(status[key]); value != "" && summary["gitops_last_sync_at"] == nil {
				summary["gitops_last_sync_at"] = boundString(value)
			}
		}
		if message := stringValue(status["message"]); message != "" && summary["gitops_message"] == nil {
			summary["gitops_message"] = scrubbedBounded(message, scrubber, 256)
		}
	}
	if kind != "Rollout" {
		return
	}
	strategy, _ := spec["strategy"].(map[string]any)
	rolloutStrategy := ""
	var steps []any
	if canary, ok := strategy["canary"].(map[string]any); ok {
		rolloutStrategy = "canary"
		steps, _ = canary["steps"].([]any)
	}
	if _, ok := strategy["blueGreen"].(map[string]any); ok {
		rolloutStrategy = "blueGreen"
	}
	if rolloutStrategy != "" {
		summary["rollout_strategy"] = rolloutStrategy
	}
	if phase := stringValue(status["phase"]); phase != "" {
		summary["rollout_phase"] = boundString(phase)
	}
	if step, ok := status["currentStepIndex"]; ok {
		summary["rollout_step"] = safeScalar(step)
	}
	summary["rollout_total_steps"] = len(steps)
	for rawKey, summaryKey := range map[string]string{"stableRS": "rollout_stable_rs", "canaryRS": "rollout_canary_rs"} {
		if value := stringValue(status[rawKey]); value != "" {
			summary[summaryKey] = boundString(value)
		}
	}
	stepIndex := int(triageSummaryIntValue(summary, "rollout_step"))
	if stepIndex >= 0 && stepIndex < len(steps) {
		if step, ok := steps[stepIndex].(map[string]any); ok {
			if weight, exists := step["setWeight"]; exists {
				summary["rollout_weight"] = safeScalar(weight)
			}
		}
	}
	if message := stringValue(status["message"]); message != "" {
		summary["rollout_message"] = scrubbedBounded(message, scrubber, 256)
	}
}

func scrubbedBounded(message string, scrubber Scrubber, maxBytes int) string {
	message = strings.ToValidUTF8(message, "")
	if scrubber != nil {
		message = scrubber.Scrub(message)
	}
	if len(message) > maxBytes {
		for maxBytes > 0 && !utf8.RuneStart(message[maxBytes]) {
			maxBytes--
		}
		message = message[:maxBytes]
	}
	return message
}

func sanitizeGitOpsSource(source string) string {
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func gitOpsStatusRank(app GitOpsApp) int {
	if strings.EqualFold(app.Health, "Degraded") || strings.EqualFold(app.Sync, "OutOfSync") {
		return 0
	}
	if strings.EqualFold(app.Health, "Progressing") || strings.EqualFold(app.Sync, "Unknown") {
		return 1
	}
	return 2
}

func parseGitOpsTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}
