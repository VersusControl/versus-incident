package kubernetes

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/kref"
)

func objectRef(ref kref.Ref) ObjectRef {
	return ObjectRef{APIVersion: ref.APIVersion, Kind: ref.Kind, Name: ref.Name, Namespace: ref.Namespace, UID: ref.UID}
}

func (service *Service) issuesSnapshot(ctx context.Context, options IssueOptions) (IssuePage, error) {
	if service == nil || options.Namespace != "" && !safeSegment(options.Namespace) || options.Severity != "" && options.Severity != "critical" && options.Severity != "warning" && options.Severity != "info" || options.Kind != "" && !safeSegment(options.Kind) || options.RootUID != "" && (!safeSegment(options.RootUID) || len(options.RootUID) > 256 || !options.IncludeChanges) {
		return IssuePage{}, ErrInvalidArguments
	}
	if err := service.validateCredential(ctx); err != nil {
		return IssuePage{}, err
	}
	ctx, cancelIdentity := service.credentialContext(ctx)
	defer cancelIdentity()
	ctx, cancel := ensureOperationBudgetRequests(ctx, overviewOperationRequests)
	defer cancel()
	kinds := []string{"Pod", "ReplicaSet", "Deployment", "StatefulSet", "DaemonSet", "Job", "Node", "Event"}
	if options.IncludeChanges {
		kinds = append(kinds, "HorizontalPodAutoscaler", "ConfigMap", "Secret", "PersistentVolumeClaim", "ServiceAccount", "ControllerRevision")
	}
	snapshot, status, err := service.CachedNamespaceSnapshot(ctx, options.Namespace, kinds...)
	if err != nil {
		return IssuePage{}, err
	}
	page := IssuePage{Items: []Issue{}, Totals: map[string]int{}, Broken: BrokenTotals{BySeverity: map[string]int{}}, Sync: SyncStatus{State: status.State, AgeSeconds: status.AgeSeconds, Progress: status.Progress, Pending: status.Pending, PendingKinds: status.PendingKinds}}
	// failed tracks real coverage failures; kinds still loading keep the page
	// partial (so nothing is resolved or claimed complete) but are not failures.
	failed := false
	page.Sync.Partial = len(status.Pending) > 0
	for _, kind := range kinds {
		state, exists := status.Kinds[kind]
		if !exists && status.State == "syncing" || state.State == "pending" {
			// A kind still loading after the core generation is reported in
			// sync.pending, never as a failure.
			continue
		}
		if !exists || state.State != "ready" || state.Partial {
			if (kind == "Secret" || kind == "ServiceAccount" || kind == "ConfigMap" || kind == "PersistentVolumeClaim") && state.Error == "forbidden" || kind == "ControllerRevision" && (state.Error == "forbidden" || state.Error == "not_found") {
				continue
			}
			class := state.Error
			if class == "" {
				class = "incomplete"
			}
			page.Partial = append(page.Partial, PartialFailure{ResourceID: indexedResourceIDs[kind], Class: class})
			page.Sync.Partial = true
			failed = true
		}
	}
	now := time.Now().UTC()
	records := make([]kubeindex.Record, 0, len(snapshot.Records))
	requestedKinds := map[string]bool{}
	for _, kind := range kinds {
		requestedKinds[kind] = true
	}
	byUID := map[string]kubeindex.Record{}
	for _, record := range snapshot.Records {
		if !requestedKinds[record.Kind] {
			continue
		}
		if options.Namespace != "" && record.Namespace != options.Namespace && record.Namespace != "" {
			continue
		}
		if state := snapshot.Kinds[record.Kind]; state.State == "error" && (state.Error == "forbidden" || state.Error == "unauthorized") {
			continue
		}
		records = append(records, record)
		byUID[record.UID] = record
	}
	partialRoots := map[string]bool{}
	for _, record := range records {
		if record.Summary["break_evidence_partial"] == "true" || record.Summary["revision_history_partial"] == "true" || record.Summary["references_partial"] == "true" {
			partialRoots[kubebreaks.Root(record, byUID).UID] = true
		}
	}
	if target := options.targetObject; target != nil {
		for _, record := range records {
			if record.Kind == target.Kind && record.Namespace == target.Namespace && record.Name == target.Name && (target.UID == "" || target.UID == record.UID) {
				options.RootUID = kubebreaks.Root(record, byUID).UID
				break
			}
		}
		if options.RootUID == "" {
			if status.State == "syncing" {
				return page, nil
			}
			return IssuePage{}, ErrNotFound
		}
	}
	if options.RootUID != "" {
		root, exists := byUID[options.RootUID]
		if !exists || options.Namespace != "" && root.Namespace != options.Namespace {
			if status.State == "syncing" {
				return page, nil
			}
			return IssuePage{}, ErrNotFound
		}
	}
	broken := kubebreaks.Classify(records, now)
	service.rememberBreaks(broken, options.Namespace, now, page.Sync.Partial)
	brokenByRoot := map[string]kubebreaks.Break{}
	matches := func(root ObjectRef, severity string) bool {
		return (options.RootUID == "" || root.UID == options.RootUID) && (options.Kind == "" || root.Kind == options.Kind) && (options.Severity == "" || severity == options.Severity) && (options.Namespace == "" || root.Namespace == options.Namespace)
	}
	for _, item := range broken {
		brokenByRoot[item.Root.UID] = item
		if matches(objectRef(item.Root), item.Severity) {
			page.Broken.Total++
			page.Broken.BySeverity[item.Severity]++
		}
	}
	if options.Broken || options.RootUID != "" && brokenByRoot[options.RootUID].Root.UID != "" {
		for _, item := range broken {
			issue := breakIssue(item, now)
			if matches(issue.Root, issue.Severity) {
				page.Items = append(page.Items, issue)
			}
		}
	} else {
		groups := map[string]*Issue{}
		for _, record := range records {
			findings := classifyIssue(recordProjection(record))
			if rule := kubebreaks.Rule(record, now); rule == "pod.crash_loop" || rule == "pod.image_pull" || rule == "pod.config_error" || rule == "pod.evicted" || rule == "pod.unschedulable" || rule == "workload.rollout_stalled" {
				findings = append(findings, issueFinding{rule: rule, count: 1, severity: "warning", example: objectRef(kubebreaks.Ref(record))})
			}
			for _, finding := range findings {
				rootRecord := record
				if record.Kind == "Event" && record.Target != nil {
					if target, exists := byUID[record.Target.UID]; exists {
						rootRecord = target
					}
				}
				root := objectRef(kubebreaks.Root(rootRecord, byUID))
				if record.Kind == "Event" && record.Target != nil && rootRecord.Kind == "Event" {
					root = ObjectRef{Kind: record.Target.Kind, Name: record.Target.Name, Namespace: record.Target.Namespace, UID: record.Target.UID}
				}
				key := root.UID + "\x00" + root.Kind + "\x00" + root.Namespace + "\x00" + root.Name + "\x00" + finding.rule
				issue := groups[key]
				if issue == nil {
					issue = &Issue{Root: root, Rule: finding.rule, Severity: finding.severity}
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
					if finding.at.After(issue.LastSeen) {
						issue.LastSeen = finding.at
					}
				}
				if kubebreaks.Priority(finding.rule) < 99 {
					if item, exists := brokenByRoot[root.UID]; exists {
						enriched := breakIssue(item, now)
						issue.Broken, issue.Of, issue.BrokeAt, issue.BrokeAtSource, issue.Evidence = true, enriched.Of, enriched.BrokeAt, enriched.BrokeAtSource, enriched.Evidence
						issue.Severity = enriched.Severity
					}
				}
			}
		}
		for _, issue := range groups {
			if matches(issue.Root, issue.Severity) {
				page.Items = append(page.Items, *issue)
			}
		}
	}
	for _, issue := range page.Items {
		page.Totals[issue.Severity]++
	}
	for position := range page.Items {
		page.Items[position].Partial = partialRoots[page.Items[position].Root.UID]
	}
	sort.Slice(page.Items, func(left, right int) bool {
		first, second := page.Items[left], page.Items[right]
		if severityRank(first.Severity) != severityRank(second.Severity) {
			return severityRank(first.Severity) < severityRank(second.Severity)
		}
		if first.Broken != second.Broken {
			return first.Broken
		}
		if first.BrokeAt != nil && second.BrokeAt != nil && !first.BrokeAt.Equal(*second.BrokeAt) {
			return first.BrokeAt.Before(*second.BrokeAt)
		}
		if first.Count != second.Count {
			return first.Count > second.Count
		}
		if first.Root.Namespace != second.Root.Namespace {
			return first.Root.Namespace < second.Root.Namespace
		}
		if first.Root.Name != second.Root.Name {
			return first.Root.Name < second.Root.Name
		}
		return first.Rule < second.Rule
	})
	start, end, next := 0, len(page.Items), ""
	if !options.unpaged {
		var err error
		start, end, next, err = pageWindow(options.Cursor, normalizePageLimit(options.Limit), len(page.Items))
		if err != nil {
			return IssuePage{}, err
		}
	}
	page.Items, page.Next = page.Items[start:end], next
	if options.RootUID != "" && len(page.Items) > 1 {
		page.Items = page.Items[:1]
		page.Next = ""
	}
	page.Truncated = false
	if !page.Sync.Partial && page.Sync.State == "error" {
		page.Sync.State = "ready"
	}
	if options.IncludeChanges && len(page.Items) > 0 {
		edges, complete := kubegraph.BuildComplete(kubeindex.Snapshot{Records: records}, 0)
		earliest := now
		for _, issue := range page.Items[:min(5, len(page.Items))] {
			if issue.BrokeAt != nil && issue.BrokeAt.Before(earliest) {
				earliest = *issue.BrokeAt
			}
		}
		history := kubechanges.Page{}
		if err := service.validateCredential(ctx); err != nil {
			return IssuePage{}, err
		}
		if store := service.changeStore(); store != nil {
			if err := service.commitCredential(ctx, func() error {
				return store.Append(kubechanges.Reconstruct(service.scope.ClusterID, records, now), now)
			}); err != nil && !errors.Is(err, kubechanges.ErrLeaseLost) {
				page.Partial = append(page.Partial, PartialFailure{ResourceID: "kubernetes.history", Class: errorClass(err)})
				page.Sync.Partial = true
				failed = true
			}
			// A continuous owner fences demand reconstruction writes, not
			// history reads. Its stored coverage still governs completeness.
			history, err = store.ReadWindow(earliest.Add(-30*time.Minute), now.Add(2*time.Minute), now)
			if err != nil {
				page.Partial = append(page.Partial, PartialFailure{ResourceID: "kubernetes.history", Class: errorClass(err)})
				page.Sync.Partial = true
				failed = true
			}
		}
		for position := range page.Items[:min(5, len(page.Items))] {
			issue := &page.Items[position]
			item, exists := brokenByRoot[issue.Root.UID]
			if !exists {
				item = kubebreaks.Break{Root: kref.Ref{Kind: issue.Root.Kind, Name: issue.Root.Name, Namespace: issue.Root.Namespace, UID: issue.Root.UID}, BrokeAt: now}
			}
			denied := map[string]bool{}
			for _, kind := range []string{"Secret", "ConfigMap", "ServiceAccount", "PersistentVolumeClaim"} {
				denied[kind] = snapshot.Kinds[kind].Error == "forbidden"
			}
			if len(page.Sync.Pending) > 0 {
				// Evidence from kinds that are still loading is a loading gap,
				// not a coverage failure; real partial causes (failed reads,
				// truncated change fields, partial projections) still count.
				loading := map[string]bool{}
				for kind, id := range indexedResourceIDs {
					loading[kind] = containsString(page.Sync.Pending, id)
				}
				linked := kubebreaks.CorrelateLoading(item, records, edges, history, issue.Partial || failed || !complete, denied, loading)
				issue.Changes, issue.MoreChanges, issue.History, issue.Gaps, issue.Rebuilt, issue.Untracked = linked.Changes, linked.MoreChanges, linked.History, linked.Gaps, linked.Rebuilt, linked.Untracked
				issue.Pending = page.Sync.Pending
				issue.Partial = issue.Partial || failed || !complete || linked.Partial
			} else {
				linked := kubebreaks.Correlate(item, records, edges, history, issue.Partial || page.Sync.Partial || !complete, denied)
				issue.Changes, issue.MoreChanges, issue.History, issue.Gaps, issue.Rebuilt, issue.Untracked = linked.Changes, linked.MoreChanges, linked.History, linked.Gaps, linked.Rebuilt, linked.Untracked
				issue.Partial = issue.Partial || page.Sync.Partial || !complete || linked.Partial
			}
		}
	}
	if err := service.validateCredential(ctx); err != nil {
		return IssuePage{}, err
	}
	return page, nil
}

func breakIssue(item kubebreaks.Break, observed time.Time) Issue {
	evidence := item.Evidence
	at := item.BrokeAt
	issue := Issue{Root: objectRef(item.Root), Rule: item.Rule, Severity: item.Severity, Count: item.Count, Of: item.Of, FirstSeen: observed, LastSeen: observed, Broken: true, BrokeAt: &at, BrokeAtSource: item.Source, Evidence: &evidence}
	if !item.FirstSeen.IsZero() {
		issue.FirstSeen = item.FirstSeen
	}
	for _, ref := range item.Examples {
		issue.Examples = append(issue.Examples, objectRef(ref))
	}
	return issue
}

// classifyReadyBreaks defers unresolved indexed roots until a complete owner
// LIST started after a child's first observation of that owner completed. A cached ready
// state or overlapping LIST cannot prove absence. Forbidden reads and roots
// already present in the index (including standalone Pods) remain usable.
func classifyReadyBreaks(snapshot kubeindex.Snapshot, observed time.Time) ([]kubebreaks.Break, []kubebreaks.Break) {
	byUID := make(map[string]kubeindex.Record, len(snapshot.Records))
	for _, record := range snapshot.Records {
		byUID[record.UID] = record
	}
	childBoundary := map[string]time.Time{}
	for _, record := range snapshot.Records {
		root := kubebreaks.Root(record, byUID)
		if _, exists := byUID[root.UID]; exists {
			continue
		}
		// Only the direct reference to the missing root proves when that
		// owner became known. Earlier Pods cannot date a newly indexed
		// ReplicaSet's reference to a still-missing Deployment.
		for _, owner := range record.Owners {
			if owner.UID != root.UID || owner.Kind != root.Kind || owner.ControllerKnown && !owner.Controller {
				continue
			}
			boundary := record.OwnerObservedAt
			if boundary.IsZero() {
				continue
			}
			if first := childBoundary[root.UID]; first.IsZero() || boundary.Before(first) {
				childBoundary[root.UID] = boundary
			}
		}
	}
	items := kubebreaks.Classify(snapshot.Records, observed)
	ready := items[:0]
	var deferred []kubebreaks.Break
	for _, item := range items {
		if _, exists := byUID[item.Root.UID]; !exists {
			if _, indexed := indexedResourceIDs[item.Root.Kind]; indexed {
				status := snapshot.Kinds[item.Root.Kind]
				boundary := childBoundary[item.Root.UID]
				current := status.State == "ready" && !status.Partial && !boundary.IsZero() && status.ObservedAt.After(boundary)
				if !current && !(status.State == "error" && status.Error == "forbidden") {
					deferred = append(deferred, item)
					continue
				}
			}
		}
		ready = append(ready, item)
	}
	return ready, deferred
}

func (service *Service) rememberBreaks(items []kubebreaks.Break, namespace string, observed time.Time, partial bool, deferred ...kubebreaks.Break) {
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	scope := service.indexScope()
	seen := service.changes.firstSeen[scope]
	if seen == nil {
		seen = map[string]time.Time{}
		service.changes.firstSeen[scope] = seen
	}
	active := map[string]bool{}
	for _, item := range deferred {
		active[item.Root.Namespace+"\x00"+item.Root.UID] = true
	}
	for position := range items {
		item := &items[position]
		key := item.Root.Namespace + "\x00" + item.Root.UID
		active[key] = true
		first, exists := seen[key]
		if !exists {
			first = observed
			seen[key] = first
		}
		item.FirstSeen = first
		if item.Source == "first_seen" {
			item.BrokeAt = first
		}
	}
	if !partial {
		for key := range seen {
			if !active[key] && (namespace == "" || strings.HasPrefix(key, namespace+"\x00")) {
				delete(seen, key)
			}
		}
	}
}
