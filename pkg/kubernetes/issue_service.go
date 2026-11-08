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
	generation, err := service.readInventory(ctx)
	if err != nil {
		return IssuePage{}, err
	}
	ctx = pinInventory(ctx, generation)
	kinds := []string{"Pod", "ReplicaSet", "Deployment", "StatefulSet", "DaemonSet", "Job", "Node", "Event"}
	if options.IncludeChanges {
		kinds = append(kinds, "HorizontalPodAutoscaler", "ConfigMap", "Secret", "PersistentVolumeClaim", "ServiceAccount", "ControllerRevision")
	}
	snapshot, status, err := service.CachedNamespaceSnapshot(ctx, options.Namespace, kinds...)
	if err != nil {
		return IssuePage{}, err
	}
	if !options.captureOnly && len(snapshot.Records) > 0 {
		// Share the established scoped control-read lane with Workloads. It
		// distinguishes partial authority from total revocation without
		// requiring unsupported or still-loading correlation kinds. Positive
		// per-kind collection denials remain enforced below.
		if err := service.authorizeWorkloadRead(ctx, options.Namespace); err != nil {
			return IssuePage{}, err
		}
	}
	page := IssuePage{Items: []Issue{}, Totals: map[string]int{}, Broken: BrokenTotals{BySeverity: map[string]int{}}, Sync: SyncStatus{State: status.State, AgeSeconds: status.AgeSeconds, Progress: status.Progress, Pending: status.Pending, PendingKinds: status.PendingKinds}}
	// failed tracks real coverage failures; kinds still loading keep the page
	// partial (so nothing is resolved or claimed complete) but are not failures.
	failed := false
	if generation.EvidenceFailure != "" {
		page.Partial = append(page.Partial, PartialFailure{ResourceID: "kubernetes.history", Class: inventoryCycleClass(generation.EvidenceFailure)})
		page.Sync.Partial = true
	}
	page.Sync.Partial = page.Sync.Partial || len(status.Pending) > 0
	for _, kind := range kinds {
		state, exists := status.Kinds[kind]
		if !options.captureOnly {
			// Nodes only add node-level issues and context. An identity that may
			// not list them still reads every workload issue, and the denial is
			// named below as a partial failure rather than refusing the read.
			optional := kind == "Secret" || kind == "ServiceAccount" || kind == "ConfigMap" || kind == "PersistentVolumeClaim" || kind == "ControllerRevision" || kind == "Node"
			if !optional && state.Error == "forbidden" {
				return IssuePage{}, ErrForbidden
			}
			if !optional && state.Error == "unauthorized" {
				return IssuePage{}, ErrUnauthorized
			}
		}
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
	_, now, _ := issueObservationBounds(snapshot)
	if now.IsZero() {
		now = time.Now().UTC()
	}
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
	if !options.captureOnly {
		service.rememberBreaks(broken, options.Namespace, now, page.Sync.Partial)
	}
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
				if record.Kind == "Event" && record.Target != nil && record.Target.UID != "" && rootRecord.Kind == "Event" {
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
		// Newest break first within a severity; issues without a break time
		// follow those with one.
		if (first.BrokeAt == nil) != (second.BrokeAt == nil) {
			return first.BrokeAt != nil
		}
		if first.BrokeAt != nil && second.BrokeAt != nil && !first.BrokeAt.Equal(*second.BrokeAt) {
			return first.BrokeAt.After(*second.BrokeAt)
		}
		if first.Count != second.Count {
			return first.Count > second.Count
		}
		// The rest is a total order over issue identity, so issues that tie on
		// everything above (a Deployment and a StatefulSet both named "web")
		// keep one order on every read and offset pages never overlap.
		if first.Root.Kind != second.Root.Kind {
			return first.Root.Kind < second.Root.Kind
		}
		if first.Root.Namespace != second.Root.Namespace {
			return first.Root.Namespace < second.Root.Namespace
		}
		if first.Root.Name != second.Root.Name {
			return first.Root.Name < second.Root.Name
		}
		if first.Root.UID != second.Root.UID {
			return first.Root.UID < second.Root.UID
		}
		return first.Rule < second.Rule
	})
	start, end, next := 0, len(page.Items), ""
	page.Total = len(page.Items)
	if !options.unpaged {
		page.Limit = normalizePageLimit(options.Limit)
		rows := make([]string, len(page.Items))
		for position, issue := range page.Items {
			rows[position] = issue.Root.Kind + "\x00" + issue.Root.Namespace + "\x00" + issue.Root.Name + "\x00" + issue.Root.UID + "\x00" + issue.Rule + "\x00" + issue.Severity
		}
		filters := struct {
			Namespace, Severity, Kind, RootUID string
			Broken                             bool
		}{options.Namespace, options.Severity, options.Kind, options.RootUID, options.Broken}
		cursor, err := bindSnapshotCursor("issues", service.indexScope(), filters, rows)
		if err != nil {
			return IssuePage{}, err
		}
		start, end, next, err = cursor.window(options.Cursor, page.Limit, len(page.Items))
		if err != nil {
			return IssuePage{}, err
		}
		page.resume = cursor.resumeFrom(start)
	}
	page.Items, page.Next = page.Items[start:end], next
	if options.RootUID != "" && len(page.Items) > 1 {
		page.Items = page.Items[:1]
		page.Next = ""
		page.Total = 1
	}
	page.Truncated = false
	if !page.Sync.Partial && page.Sync.State == "error" {
		page.Sync.State = "ready"
	}
	if options.IncludeChanges && len(page.Items) > 0 {
		edges, complete := kubegraph.BuildComplete(kubeindex.Snapshot{Records: records}, 0)
		enriched := page.Items[:min(5, len(page.Items))]
		breaks := make([]kubebreaks.Break, len(enriched))
		for position, issue := range enriched {
			item, exists := brokenByRoot[issue.Root.UID]
			if !exists {
				item = kubebreaks.Break{Root: kref.Ref{Kind: issue.Root.Kind, Name: issue.Root.Name, Namespace: issue.Root.Namespace, UID: issue.Root.UID}, BrokeAt: now}
			}
			breaks[position] = item
		}
		// Each issue reads only the window its correlation uses, so the same
		// issue is enriched, and warned about, identically on every page and
		// under every list filter.
		windows, windowOf := issueChangeWindows(breaks)
		nodeWindowOf := issueNodeWindows(&windows, breaks, records, byUID)
		if err := service.validateCredential(ctx); err != nil {
			return IssuePage{}, err
		}
		store := service.changeStore()
		if store != nil && service.evidenceStore() == nil {
			if err := service.commitCredential(ctx, func() error {
				return store.Append(kubechanges.Reconstruct(service.scope.ClusterID, records, now), now)
			}); err != nil && !errors.Is(err, kubechanges.ErrLeaseLost) {
				page.Partial = append(page.Partial, PartialFailure{ResourceID: "kubernetes.history", Class: errorClass(err)})
				page.Sync.Partial = true
				failed = true
			}
		}
		// The local change store reads whole-cluster windows, so a namespaced
		// issue's own window already holds the Node changes an extra Node
		// window would add. Only a window some issue uses as its own is read
		// there; a cluster-scoped root's own window keeps only its own kind.
		wholeCluster := service.evidenceStore() == nil && store != nil
		ownWindow := make([]bool, len(windows))
		for _, index := range windowOf {
			ownWindow[index] = true
		}
		for index := range windows {
			window := &windows[index]
			var readErr error
			switch {
			case service.evidenceStore() != nil:
				window.page, readErr = service.evidenceChangeWindow(ctx, window.from, window.to, window.namespace, window.kind)
			case store != nil && !ownWindow[index]:
				continue
			case store != nil:
				// A continuous owner fences demand reconstruction writes, not
				// history reads. Its stored coverage still governs completeness.
				window.page, readErr = store.ReadWindow(window.from, window.to, now)
				if readErr == nil && window.kind != "" {
					window.page.Items = changesOfKind(window.page.Items, window.kind)
				}
			}
			if readErr != nil {
				page.Partial = append(page.Partial, PartialFailure{ResourceID: "kubernetes.history", Class: errorClass(readErr)})
				page.Sync.Partial = true
				failed = true
				continue
			}
			// A full window is a bounded read, not a failure: only the issue, or
			// the same-instant issues, that use this window are marked as
			// missing some changes.
			window.truncated = window.page.Next != ""
		}
		for position := range enriched {
			issue := &page.Items[position]
			item := breaks[position]
			window := windows[windowOf[position]]
			history := window.page
			truncated := window.truncated
			if index := nodeWindowOf[position]; index >= 0 && !wholeCluster {
				history = mergeIssueChangeWindows(history, windows[index].page)
				truncated = truncated || windows[index].truncated
			}
			if truncated {
				issue.HistoryTruncated = true
				issue.Partial = true
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

// issueChangeWindow is one bounded history read, shared only by issues that
// broke at the same instant in the same scope. A namespaced window reads one
// namespace; a cluster-scoped window (namespace empty) reads one cluster-scoped
// kind, so a busy namespace can never fill another issue's read.
type issueChangeWindow struct {
	from, to        time.Time
	namespace, kind string
	page            kubechanges.Page
	truncated       bool
}

// issueChangeWindows returns one correlation window per distinct break time
// and issue scope, and for each break the index of its window. Each break
// reads exactly [BrokeAt-30m, BrokeAt+2m], the range correlation reads, in its
// own root's namespace. A cluster-scoped root reads its own kind instead.
// Overlapping windows are never merged: a merged read that hits its cap would
// mark, and crowd out, every issue in it, so an issue's changes and warnings
// would depend on its neighbours on the page or on the list filter. Only
// identical windows share a read.
func issueChangeWindows(breaks []kubebreaks.Break) ([]issueChangeWindow, []int) {
	windows := []issueChangeWindow{}
	windowOf := make([]int, len(breaks))
	for position := range breaks {
		root := breaks[position].Root
		kind := ""
		if root.Namespace == "" {
			kind = root.Kind
		}
		windowOf[position] = addIssueChangeWindow(&windows, breaks[position].BrokeAt, root.Namespace, kind)
	}
	return windows, windowOf
}

// issueNodeWindows adds a cluster-scoped Node window for each namespaced break
// whose Pods run on an indexed Node, because correlation links that Node's
// changes to the issue. It returns, per break, the index of that window or -1.
// Same-instant breaks share the Node read, so a request reads at most one
// namespace window and one Node window per enriched issue.
func issueNodeWindows(windows *[]issueChangeWindow, breaks []kubebreaks.Break, records []kubeindex.Record, byUID map[string]kubeindex.Record) []int {
	nodeWindowOf := make([]int, len(breaks))
	nodes := map[string]bool{}
	for _, record := range records {
		if record.Kind == "Node" {
			nodes[record.Name] = true
		}
	}
	for position := range breaks {
		nodeWindowOf[position] = -1
		root := breaks[position].Root
		if root.Namespace == "" || len(nodes) == 0 {
			continue
		}
		for _, record := range records {
			if record.Kind == "Pod" && record.NodeName != "" && nodes[record.NodeName] && kubebreaks.Root(record, byUID).UID == root.UID {
				nodeWindowOf[position] = addIssueChangeWindow(windows, breaks[position].BrokeAt, "", "Node")
				break
			}
		}
	}
	return nodeWindowOf
}

func addIssueChangeWindow(windows *[]issueChangeWindow, brokeAt time.Time, namespace, kind string) int {
	from, to := brokeAt.Add(-30*time.Minute), brokeAt.Add(2*time.Minute)
	for index, window := range *windows {
		if window.from.Equal(from) && window.to.Equal(to) && window.namespace == namespace && window.kind == kind {
			return index
		}
	}
	*windows = append(*windows, issueChangeWindow{from: from, to: to, namespace: namespace, kind: kind})
	return len(*windows) - 1
}

// changesOfKind keeps the changes to objects of one kind.
func changesOfKind(changes []Change, kind string) []Change {
	kept := make([]Change, 0, len(changes))
	for _, change := range changes {
		if change.Kind == kind {
			kept = append(kept, change)
		}
	}
	return kept
}

// mergeIssueChangeWindows combines an issue's own window with the
// cluster-scoped window it also correlates against.
func mergeIssueChangeWindows(own, cluster kubechanges.Page) kubechanges.Page {
	merged := kubechanges.Page{Items: append(append([]Change{}, own.Items...), cluster.Items...), Gaps: append(append([]kubechanges.Gap{}, own.Gaps...), cluster.Gaps...), Coverage: map[string]kubechanges.Gap{}}
	for _, page := range []kubechanges.Page{own, cluster} {
		for kind, interval := range page.Coverage {
			merged.Coverage[kind] = interval
		}
	}
	merged.Next = own.Next
	if merged.Next == "" {
		merged.Next = cluster.Next
	}
	return merged
}
