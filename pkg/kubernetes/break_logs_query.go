package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

type BreakLogWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}
type BreakLogPod struct {
	Name  string `json:"name"`
	UID   string `json:"uid"`
	State string `json:"state"`
	Short string `json:"short"`
}
type BreakLogLine struct {
	Pod       string    `json:"pod,omitempty"`
	At        time.Time `json:"at"`
	PodUID    string    `json:"pod_uid"`
	Container string    `json:"container"`
	Init      bool      `json:"init"`
	Slot      string    `json:"slot"`
	Level     string    `json:"level"`
	Text      string    `json:"text"`
	Source    string    `json:"source"`
}
type BreakLogMarker struct {
	Type  string    `json:"type"`
	At    time.Time `json:"at"`
	Label string    `json:"label"`
}
type BreakLogCounts struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	All      int `json:"all"`
}
type BreakLogRate struct {
	Minute time.Time `json:"minute"`
	Total  int       `json:"total"`
	Errors int       `json:"errors"`
}
type BreakLogPattern struct {
	Template string `json:"template"`
	Level    string `json:"level"`
	Count    int    `json:"count"`
}
type BreakLogSource struct {
	Kind string    `json:"kind"`
	Name string    `json:"name"`
	Type string    `json:"type,omitempty"`
	At   time.Time `json:"at"`
}
type BreakLogPage struct {
	OccurrenceID string             `json:"occurrence_id,omitempty"`
	Window       BreakLogWindow     `json:"window"`
	Pods         []BreakLogPod      `json:"pods"`
	Lines        []BreakLogLine     `json:"lines"`
	Markers      []BreakLogMarker   `json:"markers"`
	Counts       BreakLogCounts     `json:"counts"`
	Rate         []BreakLogRate     `json:"rate"`
	Patterns     []BreakLogPattern  `json:"patterns"`
	Sources      []BreakLogSource   `json:"sources"`
	Coverage     breaklogs.Coverage `json:"coverage"`
	Next         string             `json:"next,omitempty"`
	// Container is the applied filter: a container name, or "*" for all.
	Container     string              `json:"container"`
	MainContainer string              `json:"main_container,omitempty"`
	Containers    []BreakLogContainer `json:"containers"`
	PatternInput  []BreakLogLine      `json:"-"`
}

// BreakLogContainer lists one container of the break's pods so a viewer can
// choose which container's logs to read.
type BreakLogContainer struct {
	Name string `json:"name"`
	Init bool   `json:"init,omitempty"`
	Main bool   `json:"main,omitempty"`
}

// breakLogContainers lists the break's containers in spec order (app
// containers before init containers) and picks the main one: the pod's
// declared default container, else the first app container. Captured
// snapshots fill in containers of pods that no longer exist.
func breakLogContainers(pods []kubeindex.Record, snapshots []breaklogs.Snapshot) ([]BreakLogContainer, string) {
	seen := map[string]int{}
	listed := []BreakLogContainer{}
	add := func(name string, init bool) {
		if name == "" {
			return
		}
		if position, exists := seen[name]; exists {
			listed[position].Init = listed[position].Init && init
			return
		}
		seen[name] = len(listed)
		listed = append(listed, BreakLogContainer{Name: name, Init: init})
	}
	main := ""
	for _, pod := range pods {
		containers := pod.LogContainers
		if len(containers) == 0 {
			containers = pod.Containers
		}
		for _, container := range containers {
			add(container.Name, container.Init)
		}
		for _, container := range pod.Containers {
			add(container.Name, container.Init)
		}
		if main == "" && pod.MainContainer != "" {
			main = pod.MainContainer
		}
	}
	// Captures carry no spec order, so containers known only from them are
	// listed by name to keep the default stable across requests.
	captured := append([]breaklogs.Snapshot(nil), snapshots...)
	sort.SliceStable(captured, func(left, right int) bool { return captured[left].Container < captured[right].Container })
	for _, item := range captured {
		add(item.Container, item.Init)
	}
	if position, exists := seen[main]; !exists || listed[position].Init {
		main = ""
	}
	sort.SliceStable(listed, func(left, right int) bool { return !listed[left].Init && listed[right].Init })
	for position := range listed {
		if main == "" && !listed[position].Init {
			main = listed[position].Name
		}
		listed[position].Main = listed[position].Name == main && main != ""
	}
	return listed, main
}

func (service *Service) BreakLogs(ctx context.Context, query breaklogs.Query) (BreakLogPage, error) {
	if err := query.Validate(); err != nil {
		return BreakLogPage{}, ErrInvalidArguments
	}
	if service.scrubber == nil {
		return BreakLogPage{}, ErrClusterUnavailable
	}
	if err := service.validateCredential(ctx); err != nil {
		return BreakLogPage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, credentialEpochContextKey{}, service)
	now := service.observationNow()
	store := service.changeStore()
	if store == nil {
		return BreakLogPage{}, ErrClusterUnavailable
	}

	// A future-only root window exposes no inventory or retained evidence.
	if query.From.After(now) && query.OccurrenceID == "" && query.Center.IsZero() && query.Cursor == "" {
		return BreakLogPage{Window: BreakLogWindow{query.From, query.To}, Pods: []BreakLogPod{}, Lines: []BreakLogLine{}, Markers: []BreakLogMarker{}, Rate: []BreakLogRate{}, Patterns: []BreakLogPattern{}, Sources: []BreakLogSource{}, Containers: []BreakLogContainer{}, Container: breaklogs.AllContainers, Coverage: breaklogs.Coverage{State: "pending", Reason: "pending", Gaps: []breaklogs.Gap{{From: query.From, To: query.To, Reason: "pending"}}}}, nil
	}
	snapshot, _, refreshErr := service.IndexSnapshot(ctx, "Pod", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob")
	ctx = captureReadContext(ctx, "")
	state, err := store.BreakLogs(now)
	if err != nil {
		return BreakLogPage{}, err
	}
	// Every response can carry protected identities, markers or retained gaps.
	if err := service.authorizeEvidenceKind(ctx, query.Namespace, "core~v1~pods"); err != nil {
		clearLinkedCache()
		return BreakLogPage{}, err
	}
	query, err = service.resolveBreakLogSelection(ctx, query, state)
	if err != nil {
		return BreakLogPage{}, err
	}
	page := BreakLogPage{Window: BreakLogWindow{query.From, query.To}, Pods: []BreakLogPod{}, Lines: []BreakLogLine{}, Markers: []BreakLogMarker{}, Rate: []BreakLogRate{}, Patterns: []BreakLogPattern{}, Sources: []BreakLogSource{}}
	page.OccurrenceID = query.OccurrenceID
	linkedPods := manifestQueryPods(state, query)
	selectedManifestIDs := map[string]bool{}
	for _, manifest := range state.Manifests {
		if !matchesBreakManifest(manifest, query) || manifest.To.Before(query.From) || manifest.From.After(query.To) {
			continue
		}
		selectedManifestIDs[manifest.ID] = true
		if page.OccurrenceID == "" && !query.Center.IsZero() {
			page.OccurrenceID = manifest.OccurrenceID
			if page.OccurrenceID == "" {
				page.OccurrenceID = manifest.ID
			}
		}
	}
	pods := breakLogPods(snapshot.Records, query.Namespace, query.RootUID)
	identityPods := append([]kubeindex.Record(nil), pods...)
	rootSnapshots := []breaklogs.Snapshot{}
	for _, item := range state.Snapshots {
		if item.Namespace == query.Namespace && item.RootUID == query.RootUID {
			rootSnapshots = append(rootSnapshots, item)
		}
	}
	for _, pod := range linkedPods {
		for _, container := range pod.Containers {
			rootSnapshots = append(rootSnapshots, breaklogs.Snapshot{Container: container.Name, Init: container.Init})
		}
	}
	page.Containers, page.MainContainer = breakLogContainers(pods, rootSnapshots)
	switch query.Container {
	case "":
		query.Container = page.MainContainer
		if query.Container == "" {
			query.Container = breaklogs.AllContainers
		}
	case breaklogs.AllContainers:
	default:
		known := false
		for _, container := range page.Containers {
			known = known || container.Name == query.Container
		}
		if !known {
			return BreakLogPage{}, ErrInvalidArguments
		}
	}
	page.Container = query.Container
	selectedContainer := func(name string) bool {
		return query.Container == breaklogs.AllContainers || name == query.Container
	}
	selected := map[string]bool{}
	for _, pod := range query.Pods {
		if !safeSegment(pod) {
			return BreakLogPage{}, ErrInvalidArguments
		}
		selected[pod] = true
	}
	selectedPod := func(name, uid string) bool { return len(selected) == 0 || selected[name] || selected[uid] }
	intervals := []breaklogs.Gap{}
	failures := []breaklogs.Gap{}
	for _, manifest := range state.Manifests {
		if selectedManifestIDs[manifest.ID] && manifest.Truncated {
			failures = append(failures, breaklogs.Gap{From: query.From, To: query.To, Reason: "budget"})
		}
	}
	for _, gap := range state.Gaps {
		if gap.Namespace == query.Namespace && (gap.RootUID == "" || gap.RootUID == query.RootUID) {
			failures = append(failures, gap)
		}
	}
	seenPods := map[string]bool{}
	seenLines := map[string]bool{}
	addPod := func(name, uid, status string) {
		key := uid
		if key == "" {
			key = "name:" + name
		}
		if seenPods[key] {
			return
		}
		seenPods[key] = true
		short := name
		if len(short) > 5 {
			short = short[len(short)-5:]
		}
		page.Pods = append(page.Pods, BreakLogPod{Name: name, UID: uid, State: status, Short: short})
	}
	addSnapshot := func(item breaklogs.Snapshot, source string) {
		if item.Source == "linked" {
			source = "linked"
		}
		if query.Mode != "" && query.Mode != "merged" && query.Mode != item.Slot || !selectedContainer(item.Container) {
			return
		}
		for _, line := range item.Lines {
			if line.At.Before(query.From) || line.At.After(query.To) {
				continue
			}
			text := service.scrubber.Scrub(line.Text)
			identity, _ := json.Marshal([]any{item.PodUID, item.Container, item.Init, item.Slot, line.At, text})
			if seenLines[string(identity)] {
				continue
			}
			seenLines[string(identity)] = true
			page.Lines = append(page.Lines, BreakLogLine{At: line.At, Pod: item.Pod, PodUID: item.PodUID, Container: item.Container, Init: item.Init, Slot: item.Slot, Level: breaklogs.Level(text), Text: text, Source: source})
		}
		page.Sources = append(page.Sources, BreakLogSource{Kind: source, Name: service.scope.ClusterID, At: item.CapturedAt})
	}
	matching := []breaklogs.Snapshot{}
	// podSnapshots are the captures of the selected pods in the window, before
	// the container filter; pod identity facts come from any container.
	podSnapshots := []breaklogs.Snapshot{}
	observations := []breaklogs.Snapshot{}
	for _, item := range state.Snapshots {
		if item.Source == "linked" && (query.OccurrenceID != "" || !query.Center.IsZero()) && !selectedManifestIDs[item.OccurrenceID] {
			continue
		}
		if item.Namespace != query.Namespace || item.RootUID != query.RootUID || !selectedPod(item.Pod, item.PodUID) || item.To.Before(query.From) || item.From.After(query.To) {
			continue
		}
		addPod(item.Pod, item.PodUID, "crashed")
		podSnapshots = append(podSnapshots, item)
		if !selectedContainer(item.Container) {
			continue
		}
		matching = append(matching, item)
		observations = append(observations, item)
		addSnapshot(item, "captured")
		if item.Source == "linked" && item.Truncated {
			failures = append(failures, breaklogs.Gap{SnapshotID: item.ID, From: item.From, To: item.To, Namespace: item.Namespace, RootUID: item.RootUID, PodUID: item.PodUID, Container: item.Container, Reason: "budget"})
		}
	}
	if len(pods) > 5 {
		failures = append(failures, breaklogs.Gap{From: query.From, To: query.To, Reason: "budget"})
		pods = pods[:5]
	}
	for _, pod := range pods {
		if !selectedPod(pod.Name, pod.UID) {
			continue
		}
		status := "running"
		if pod.ReadyStatus != "True" {
			status = "not_ready"
		}
		for _, container := range pod.Containers {
			if container.RestartCount > 0 {
				status = "crashed"
			}
		}
		addPod(pod.Name, pod.UID, status)
		for position := range page.Pods {
			if page.Pods[position].UID == pod.UID {
				page.Pods[position].State = status
			}
		}
		containers := pod.Containers
		if query.Container != breaklogs.AllContainers {
			containers = nil
			for _, candidates := range [][]kubeindex.ContainerState{pod.Containers, pod.LogContainers} {
				for _, container := range candidates {
					if len(containers) == 0 && container.Name == query.Container {
						containers = append(containers, container)
					}
				}
			}
		}
		if len(containers) > 3 {
			failures = append(failures, breaklogs.Gap{From: query.From, To: query.To, Reason: "budget"})
			containers = containers[:3]
		}
		for _, container := range containers {
			slots := []string{"current"}
			if !container.TerminatedAt.IsZero() || container.RestartCount > 0 {
				slots = append([]string{"previous"}, slots...)
			}
			for _, slot := range slots {
				if query.Mode != "" && query.Mode != "merged" && query.Mode != slot {
					continue
				}
				expectedFrom, expectedTo := query.From, minTime(query.To, now)
				if !container.TerminatedAt.IsZero() {
					if slot == "previous" {
						expectedTo = minTime(expectedTo, container.TerminatedAt)
					} else {
						expectedFrom = maxTime(expectedFrom, container.TerminatedAt)
					}
				}
				if !expectedTo.After(expectedFrom) {
					continue
				}
				covered := []breaklogs.Gap{}
				samples := []breaklogs.Snapshot{}
				for _, item := range matching {
					if item.PodUID == pod.UID && item.Container == container.Name && item.Init == container.Init && item.Slot == slot {
						covered = append(covered, breaklogs.Gap{From: item.From, To: item.To})
						samples = append(samples, item)
					}
				}
				coverage := breaklogs.Cover(expectedFrom, expectedTo, covered, nil, false)
				if coverage.State != "complete" && refreshErr == nil && !query.From.Before(now.Add(-24*time.Hour+time.Second)) {
					item, gap := service.readBreakContainer(ctx, pod, query.RootUID, container, slot, expectedFrom, expectedTo)
					if gap != nil {
						failures = append(failures, *gap)
					} else {
						addSnapshot(item, "kubernetes")
						observations = append(observations, item)
						covered = append(covered, breaklogs.Gap{From: item.From, To: item.To})
						samples = append(samples, item)
					}
				}
				coverage = breaklogs.Assess(expectedFrom, expectedTo, samples, breaklogs.ContainerObservation{PodUID: pod.UID, Container: container.Name, Init: container.Init, Slot: slot, Restarts: container.RestartCount})
				failures = append(failures, coverage.Gaps...)
				intervals = append(intervals, covered...)
			}
		}
	}
	if len(pods) == 0 {
		groups := map[string][]breaklogs.Gap{}
		contentful := 0
		for _, item := range matching {
			// A capture with no lines proves nothing about the window.
			if len(item.Lines) == 0 {
				continue
			}
			contentful++
			if query.Mode != "" && query.Mode != "merged" && query.Mode != item.Slot {
				continue
			}
			group := item.PodUID + "\x00" + item.Container
			interval := breaklogs.Gap{From: item.From, To: item.To, PodUID: item.PodUID, Container: item.Container}
			groups[group] = append(groups[group], interval)
			intervals = append(intervals, interval)
		}
		for _, covered := range groups {
			for _, gap := range breaklogs.Cover(query.From, minTime(query.To, now), covered, nil, false).Gaps {
				gap.PodUID = covered[0].PodUID
				gap.Container = covered[0].Container
				failures = append(failures, gap)
			}
		}
		if contentful == 0 {
			reason := "pod_deleted"
			if refreshErr != nil || snapshot.Kinds["Pod"].State != "ready" {
				reason = "capture_late"
			}
			failures = append(failures, breaklogs.Gap{From: query.From, To: query.To, Reason: reason})
		}
	}
	failures = breaklogs.ResolveBudgetGaps(failures, observations)
	page.Coverage = breaklogs.CoverObserved(query.From, query.To, now, intervals, failures)
	if page.Coverage.Tip && query.From.Before(now) {
		provider, key := service.logSettingsStore()
		settings, settingsErr := breaklogs.LoadSettings(provider, key)
		if settingsErr != nil {
			return BreakLogPage{}, settingsErr
		}
		for _, source := range service.configuredLogSources() {
			if source.Name != settings.Settings.BreakLogs.LinkedSource {
				continue
			}
			linkedPods = supplementLinkedPods(linkedPods, podSnapshots, identityPods, snapshot.Kinds["Pod"].ObservedAt)
			if len(linkedPods) > breaklogs.MaxManifestPods {
				linkedPods = linkedPods[:breaklogs.MaxManifestPods]
				page.Coverage.Gaps = append(page.Coverage.Gaps, breaklogs.Gap{From: query.From, To: query.To, Reason: "budget"})
			}
			if len(linkedPods) == 0 {
				break
			}
			for _, pod := range linkedPods {
				if selectedPod(pod.Name, pod.UID) {
					addPod(pod.Name, pod.UID, "crashed")
				}
			}
			// Persisted progress fences cache reuse after another process settles.
			selectedProgress := []breaklogs.Manifest{}
			for _, manifest := range state.Manifests {
				if selectedManifestIDs[manifest.ID] {
					selectedProgress = append(selectedProgress, manifest)
				}
			}
			rawState, _ := json.Marshal([]any{selectedProgress, rootSnapshots})
			stateDigest := sha256.Sum256(rawState)
			accepted, result, readErr := service.readLinkedBreak(ctx, query, linkedPods, settings.Settings.BreakLogs, source, true, hex.EncodeToString(stateDigest[:]))
			ready := query.To.Add(settings.Settings.BreakLogs.IngestionDelay(source.Type))
			if readErr != nil {
				if errors.Is(readErr, ErrForbidden) || errors.Is(readErr, ErrUnauthorized) || errors.Is(readErr, ErrCredentialUnavailable) {
					return BreakLogPage{}, readErr
				}
				reason := "source_error"
				switch readErr.Error() {
				case "timeout", "unauthorized", "not_found", "invalid_mapping", "unavailable":
					reason = readErr.Error()
				}
				page.Sources = append(page.Sources, BreakLogSource{Kind: "linked", Name: source.Name, Type: source.Type, At: now})
				page.Coverage.Gaps = append(page.Coverage.Gaps, breaklogs.Gap{From: query.From, To: query.To, Reason: reason})
				page.Coverage.Reason = reason
				if now.Before(ready) {
					page.Coverage.Gaps = append(page.Coverage.Gaps, breaklogs.Gap{From: query.From, To: query.To, Reason: "linked_pending", ReadyAt: ready})
					page.Coverage.Reason = "linked_pending"
				}
				break
			}
			for _, line := range accepted {
				identity, _ := json.Marshal([]any{line.PodUID, line.Container, line.Init, line.Slot, line.At, line.Text})
				if !seenLines[string(identity)] {
					seenLines[string(identity)] = true
					page.Lines = append(page.Lines, line)
				}
			}
			page.Sources = append(page.Sources, BreakLogSource{Kind: "linked", Name: source.Name, Type: source.Type, At: time.Now().UTC()})
			for _, complete := range result.Complete {
				// Older ingested rows cannot prove that the selected tail is ready.
				if now.Before(ready) {
					continue
				}
				leafLines := []BreakLogLine{}
				for _, line := range accepted {
					if !line.At.Before(complete.From) && !line.At.After(complete.To) {
						leafLines = append(leafLines, line)
					}
				}
				intervals = page.closeLinkedGaps(intervals, leafLines, complete.To, complete.From)
			}
			retainedFailures := []breaklogs.Gap{}
			for i := range page.Coverage.Gaps {
				gap := &page.Coverage.Gaps[i]
				original := *gap
				if gap.Reason == "pending" {
					continue
				}
				switch {
				case result.Truncated || gap.Reason == "budget":
					gap.Reason = "budget"
				case now.Before(ready):
					gap.Reason, gap.ReadyAt = "linked_pending", ready
				case len(result.Lines) == 0 && !result.Rejected:
					gap.Reason = "no_logs"
				}
				if original.Reason != "" && original.Reason != gap.Reason {
					retainedFailures = append(retainedFailures, original)
				}
			}
			if result.Truncated && now.Before(ready) && len(page.Coverage.Gaps) > 0 {
				page.Coverage.Gaps = append(page.Coverage.Gaps, breaklogs.Gap{From: query.From, To: query.To, Reason: "linked_pending", ReadyAt: ready})
			}
			for _, gap := range page.Coverage.Gaps {
				if gap.Reason != "pending" {
					page.Coverage.Reason = gap.Reason
					break
				}
			}
			page.Coverage.Gaps = append(page.Coverage.Gaps, retainedFailures...)
			break
		}
	}
	// Covered historical time needs no linked read, but a selected future tail
	// still carries ingestion readiness so the open panel can schedule itself.
	if page.Coverage.State == "pending" && !page.Coverage.Tip && query.From.Before(now) && len(page.Pods) > 0 {
		provider, key := service.logSettingsStore()
		settings, settingsErr := breaklogs.LoadSettings(provider, key)
		if settingsErr == nil {
			for _, source := range service.configuredLogSources() {
				if source.Name != settings.Settings.BreakLogs.LinkedSource {
					continue
				}
				ready := query.To.Add(settings.Settings.BreakLogs.IngestionDelay(source.Type))
				for i := range page.Coverage.Gaps {
					if page.Coverage.Gaps[i].Reason == "pending" {
						page.Coverage.Gaps[i].Reason, page.Coverage.Gaps[i].ReadyAt = "linked_pending", ready
					}
				}
				page.Coverage.Reason = "linked_pending"
				page.Sources = append(page.Sources, BreakLogSource{Kind: "linked", Name: source.Name, Type: source.Type, At: now})
				break
			}
		}
	}
	for _, root := range kubebreaks.Classify(snapshot.Records, now) {
		if root.Root.UID == query.RootUID && root.Root.Namespace == query.Namespace && !root.BrokeAt.IsZero() && query.Center.IsZero() {
			page.Markers = append(page.Markers, BreakLogMarker{Type: "break", At: root.BrokeAt, Label: "Break"})
		}
	}
	crashes, crashErr := store.QueryCrashes(kubechanges.CrashQuery{Since: now.Add(-24 * time.Hour), Until: now, Namespace: query.Namespace, RootUID: query.RootUID, Limit: 100}, now)
	if crashErr != nil {
		return BreakLogPage{}, crashErr
	}
	crashMarkers := map[string]bool{}
	for _, crash := range crashes.Items {
		if crash.Type == "termination" && selectedContainer(crash.Container) && !crash.At.Before(query.From) && !crash.At.After(query.To) {
			page.Markers = append(page.Markers, BreakLogMarker{Type: "crash", At: crash.At, Label: crash.Container + " " + crash.Reason + " exit " + strconv.FormatInt(int64(crash.ExitCode), 10)})
			crashMarkers[crash.PodUID+"\x00"+crash.Container+"\x00"+crash.At.Format(time.RFC3339Nano)] = true
		}
	}
	hasBreak := false
	if !query.Center.IsZero() {
		page.Markers = append(page.Markers, BreakLogMarker{Type: "break", At: query.Center, Label: "Break"})
		hasBreak = true
	}
	for _, marker := range page.Markers {
		if marker.Type == "break" {
			hasBreak = true
		}
	}
	// The break marker belongs to the whole break, so any container's capture
	// may place it; termination markers follow the container filter.
	addTermination := func(uid, container string, at time.Time, reason string, exitCode int32) {
		key := uid + "\x00" + container + "\x00" + at.Format(time.RFC3339Nano)
		if !selectedContainer(container) || crashMarkers[key] || at.IsZero() || at.Before(query.From) || at.After(query.To) {
			return
		}
		label := container + " previous termination"
		if reason = safeBreakReason(service.scrubber.Scrub(safeBreakReason(reason))); reason != "" {
			label = container + " " + reason + " exit " + strconv.FormatInt(int64(exitCode), 10)
		}
		page.Markers = append(page.Markers, BreakLogMarker{Type: "crash", At: at, Label: label})
		crashMarkers[key] = true
	}
	// The immutable manifest outlives both kubelet slots and crash-history
	// availability. Never infer its cause from a later live lastState.
	for _, manifest := range state.Manifests {
		if !selectedManifestIDs[manifest.ID] {
			continue
		}
		for _, pod := range manifest.Pods {
			if !selectedPod(pod.Name, pod.UID) {
				continue
			}
			for _, container := range pod.Containers {
				if container.TerminationReason != "" {
					addTermination(pod.UID, container.Name, container.TerminationAt, container.TerminationReason, container.TerminationExitCode)
				}
			}
		}
	}
	// Prefer cause-bearing snapshots before legacy generic markers.
	for _, item := range podSnapshots {
		if item.TerminationReason != "" {
			addTermination(item.PodUID, item.Container, item.TerminationAt, item.TerminationReason, item.TerminationExitCode)
		}
	}
	for _, item := range podSnapshots {
		if !hasBreak && !item.BreakAt.IsZero() && !item.BreakAt.Before(query.From) && !item.BreakAt.After(query.To) {
			page.Markers = append(page.Markers, BreakLogMarker{Type: "break", At: item.BreakAt, Label: "Break"})
			hasBreak = true
		}
		addTermination(item.PodUID, item.Container, item.TerminationAt, item.TerminationReason, item.TerminationExitCode)
	}
	sort.Slice(page.Markers, func(left, right int) bool { return page.Markers[left].At.Before(page.Markers[right].At) })
	sort.SliceStable(page.Lines, func(left, right int) bool {
		first, second := page.Lines[left], page.Lines[right]
		if !first.At.Equal(second.At) {
			return first.At.Before(second.At)
		}
		if first.PodUID != second.PodUID {
			return first.PodUID < second.PodUID
		}
		if first.Container != second.Container {
			return first.Container < second.Container
		}
		return first.Text < second.Text
	})
	rates := map[time.Time]*BreakLogRate{}
	filtered := []BreakLogLine{}
	for _, line := range page.Lines {
		page.Counts.All++
		if line.Level == "error" {
			page.Counts.Errors++
		}
		if line.Level == "warning" {
			page.Counts.Warnings++
		}
		minute := line.At.Truncate(time.Minute)
		if rates[minute] == nil {
			rates[minute] = &BreakLogRate{Minute: minute}
		}
		rates[minute].Total++
		if line.Level == "error" {
			rates[minute].Errors++
		}
		if query.Level != "" && line.Level != query.Level || query.Search != "" && !strings.Contains(strings.ToLower(line.Text), strings.ToLower(query.Search)) {
			continue
		}
		filtered = append(filtered, line)
	}
	for _, rate := range rates {
		page.Rate = append(page.Rate, *rate)
	}
	sort.Slice(page.Rate, func(left, right int) bool { return page.Rate[left].Minute.Before(page.Rate[right].Minute) })
	sourceSet := map[string]BreakLogSource{}
	for _, source := range page.Sources {
		key := source.Kind + "\x00" + source.Name + "\x00" + source.Type
		if previous, ok := sourceSet[key]; !ok || source.At.After(previous.At) {
			sourceSet[key] = source
		}
	}
	page.Sources = []BreakLogSource{}
	for _, source := range sourceSet {
		page.Sources = append(page.Sources, source)
	}
	sort.Slice(page.Sources, func(left, right int) bool {
		rank := map[string]int{"captured": 0, "kubernetes": 1, "linked": 2}
		if page.Sources[left].Kind != page.Sources[right].Kind {
			return rank[page.Sources[left].Kind] < rank[page.Sources[right].Kind]
		}
		return page.Sources[left].Name < page.Sources[right].Name
	})
	encoded, _ := json.Marshal(filtered)
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	page.PatternInput = filtered
	scope := service.scope.OrgID + "\x00" + service.scope.ClusterID + "\x00" + service.scope.CredentialID
	offset, observed, cursorErr := breaklogs.CursorOffset(scope, query, digest, now)
	if cursorErr != nil || offset > len(filtered) {
		return BreakLogPage{}, ErrInvalidArguments
	}
	limit := query.Limit
	if limit == 0 {
		limit = 500
	}
	stop := min(offset+limit, len(filtered))
	page.Lines = filtered[offset:stop]
	if stop < len(filtered) {
		page.Next = breaklogs.NextCursor(scope, query, digest, stop, observed)
	}
	if err := service.validateCredential(ctx); err != nil {
		return BreakLogPage{}, err
	}
	if err := service.authorizeEvidenceKind(ctx, query.Namespace, "core~v1~pods"); err != nil {
		clearLinkedCache()
		return BreakLogPage{}, err
	}
	return page, nil
}

func (page *BreakLogPage) closeLinkedGaps(intervals []breaklogs.Gap, lines []BreakLogLine, until time.Time, starts ...time.Time) []breaklogs.Gap {
	if len(lines) == 0 {
		return intervals
	}
	start := page.Window.From
	if len(starts) > 0 {
		start = starts[0]
	}
	remaining := []breaklogs.Gap{}
	for _, gap := range page.Coverage.Gaps {
		if gap.Reason == "pending" {
			continue
		}
		if gap.Reason == "budget" && (gap.PodUID == "" || gap.Container == "") {
			remaining = append(remaining, gap)
			continue
		}
		matched := func(uid, name string) bool {
			required := []string{gap.Container}
			if gap.Container == "" && page.Container != "" {
				required = nil
				for _, container := range page.Containers {
					if page.Container == breaklogs.AllContainers || page.Container == container.Name {
						required = append(required, container.Name)
					}
				}
			}
			if len(required) == 0 {
				required = []string{gap.Container}
			}
			for _, requiredContainer := range required {
				found := false
				for _, line := range lines {
					if line.At.Before(gap.From) || line.At.After(gap.To) || line.At.After(until) || requiredContainer != "" && requiredContainer != line.Container {
						continue
					}
					if uid != "" && line.PodUID == uid || uid == "" && line.Pod == name {
						found = true
						break
					}
				}
				if !found {
					return false
				}
			}
			return true
		}
		covered := false
		if gap.PodUID != "" {
			covered = matched(gap.PodUID, "")
		} else if len(page.Pods) > 0 {
			covered = true
			for _, pod := range page.Pods {
				covered = covered && matched(pod.UID, pod.Name)
			}
		}
		if !covered || !until.After(gap.From) || !gap.To.After(start) {
			remaining = append(remaining, gap)
			continue
		}
		if gap.From.Before(start) {
			before := gap
			before.To = start
			remaining = append(remaining, before)
		}
		intervals = append(intervals, breaklogs.Gap{From: maxTime(gap.From, start), To: minTime(gap.To, until)})
		if gap.To.After(until) {
			gap.From = until
			remaining = append(remaining, gap)
		}
	}
	failures := []breaklogs.Gap{}
	for _, gap := range remaining {
		if gap.Reason != "" || gap.PodUID != "" || gap.Container != "" {
			failures = append(failures, gap)
		}
	}
	page.Coverage = breaklogs.CoverObserved(page.Window.From, page.Window.To, until, intervals, failures)
	if len(starts) > 0 {
		page.Coverage = breaklogs.CoverObserved(page.Window.From, page.Window.To, time.Now().UTC(), intervals, failures)
	}
	return intervals
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
