package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
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
	PatternInput []BreakLogLine     `json:"-"`
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
	now := time.Now().UTC()
	store := service.changeStore()
	if store == nil {
		return BreakLogPage{}, ErrClusterUnavailable
	}
	snapshot, _, refreshErr := service.IndexSnapshot(ctx, "Pod", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob")
	ctx = captureReadContext(ctx, "")
	state, err := store.BreakLogs(now)
	if err != nil {
		return BreakLogPage{}, err
	}
	page := BreakLogPage{Window: BreakLogWindow{query.From, query.To}, Pods: []BreakLogPod{}, Lines: []BreakLogLine{}, Markers: []BreakLogMarker{}, Rate: []BreakLogRate{}, Patterns: []BreakLogPattern{}, Sources: []BreakLogSource{}}
	pods := breakLogPods(snapshot.Records, query.Namespace, query.RootUID)
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
	for _, gap := range state.Gaps {
		if gap.Namespace == query.Namespace && gap.RootUID == query.RootUID {
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
		if query.Mode != "" && query.Mode != "merged" && query.Mode != item.Slot {
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
	observations := []breaklogs.Snapshot{}
	for _, item := range state.Snapshots {
		if item.Namespace != query.Namespace || item.RootUID != query.RootUID || !selectedPod(item.Pod, item.PodUID) || item.To.Before(query.From) || item.From.After(query.To) {
			continue
		}
		matching = append(matching, item)
		observations = append(observations, item)
		addPod(item.Pod, item.PodUID, "crashed")
		addSnapshot(item, "captured")
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
		for _, item := range matching {
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
		if len(matching) == 0 {
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
			reader, buildErr := source.Build(settings.Settings.BreakLogs.Fields[source.Type])
			if buildErr != nil {
				break
			}
			linkedQuery := breaklogs.LinkedQuery{Namespace: query.Namespace, From: query.From, To: minTime(query.To, now), Limit: 2000}
			for _, record := range snapshot.Records {
				if record.UID == query.RootUID && record.Namespace == query.Namespace {
					linkedQuery.PodPrefix = record.Name
					break
				}
			}
			byName := map[string]string{}
			createdAt := map[string]time.Time{}
			ambiguousNames := map[string]bool{}
			for _, item := range matching {
				if !item.PodCreatedAt.IsZero() {
					createdAt[item.PodUID] = item.PodCreatedAt
					if item.PodCreatedAt.After(linkedQuery.From) {
						ambiguousNames[item.Pod] = true
					}
				}
			}
			for _, pod := range pods {
				if !pod.CreatedAt.IsZero() {
					createdAt[pod.UID] = pod.CreatedAt
				}
			}
			for _, pod := range page.Pods {
				uid, seen := byName[pod.Name]
				if !seen {
					linkedQuery.Pods = append(linkedQuery.Pods, pod.Name)
				}
				if uid != "" && pod.UID != "" && uid != pod.UID {
					ambiguousNames[pod.Name] = true
				}
				if uid == "" {
					byName[pod.Name] = pod.UID
				}
			}
			for _, pod := range snapshot.Records {
				if pod.Kind == "Pod" && pod.Namespace == query.Namespace && pod.CreatedAt.After(linkedQuery.From) {
					ambiguousNames[pod.Name] = true
				}
			}
			if len(linkedQuery.Pods) == 0 && linkedQuery.PodPrefix == "" {
				break
			}
			started := time.Now()
			linkedCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			result, readErr := reader.Read(linkedCtx, linkedQuery)
			stop()
			if healthErr := service.recordLogHealth(ctx, source.Name, started, readErr); healthErr != nil {
				return BreakLogPage{}, healthErr
			}
			if readErr != nil {
				break
			}
			accepted := []BreakLogLine{}
			for _, line := range result.Lines {
				if line.At.Before(linkedQuery.From) || line.At.After(linkedQuery.To) || line.Container == "" {
					continue
				}
				inGap := false
				for _, gap := range page.Coverage.Gaps {
					if !line.At.Before(gap.From) && !line.At.After(gap.To) {
						inGap = true
						break
					}
				}
				if !inGap {
					continue
				}
				if byName[line.Pod] == "" || ambiguousNames[line.Pod] {
					continue
				}
				created := createdAt[byName[line.Pod]]
				if created.IsZero() || line.At.Before(created) {
					continue
				}
				text := service.scrubber.Scrub(line.Text)
				slot := "current"
				init := false
				for _, item := range matching {
					if item.PodUID == byName[line.Pod] && item.Container == line.Container {
						init = item.Init
						if !item.TerminationAt.IsZero() && !line.At.After(item.TerminationAt) {
							slot = "previous"
						}
					}
				}
				for _, pod := range pods {
					if pod.UID == byName[line.Pod] {
						for _, container := range pod.Containers {
							if container.Name == line.Container {
								init = container.Init
								if !container.TerminatedAt.IsZero() && !line.At.After(container.TerminatedAt) {
									slot = "previous"
								}
							}
						}
					}
				}
				if query.Mode != "" && query.Mode != "merged" && query.Mode != slot {
					continue
				}
				accepted = append(accepted, BreakLogLine{At: line.At, Pod: line.Pod, PodUID: byName[line.Pod], Container: line.Container, Init: init, Slot: slot, Level: breaklogs.Level(text), Text: text, Source: "linked"})
			}
			page.Lines = append(page.Lines, accepted...)
			page.Sources = append(page.Sources, BreakLogSource{Kind: "linked", Name: source.Name, Type: source.Type, At: time.Now().UTC()})
			if !result.Truncated {
				page.closeLinkedGaps(intervals, accepted, linkedQuery.To)
			}
			break
		}
	}
	for _, root := range kubebreaks.Classify(snapshot.Records, now) {
		if root.Root.UID == query.RootUID && root.Root.Namespace == query.Namespace && !root.BrokeAt.IsZero() {
			page.Markers = append(page.Markers, BreakLogMarker{Type: "break", At: root.BrokeAt, Label: "Break"})
		}
	}
	crashes, crashErr := store.QueryCrashes(kubechanges.CrashQuery{Since: now.Add(-24 * time.Hour), Until: now, Namespace: query.Namespace, RootUID: query.RootUID, Limit: 100}, now)
	if crashErr != nil {
		return BreakLogPage{}, crashErr
	}
	crashMarkers := map[string]bool{}
	for _, crash := range crashes.Items {
		if crash.Type == "termination" && !crash.At.Before(query.From) && !crash.At.After(query.To) {
			page.Markers = append(page.Markers, BreakLogMarker{Type: "crash", At: crash.At, Label: crash.Container + " " + crash.Reason + " exit " + strconv.FormatInt(int64(crash.ExitCode), 10)})
			crashMarkers[crash.PodUID+"\x00"+crash.Container+"\x00"+crash.At.Format(time.RFC3339Nano)] = true
		}
	}
	hasBreak := false
	for _, marker := range page.Markers {
		if marker.Type == "break" {
			hasBreak = true
		}
	}
	for _, item := range matching {
		if !hasBreak && !item.BreakAt.IsZero() && !item.BreakAt.Before(query.From) && !item.BreakAt.After(query.To) {
			page.Markers = append(page.Markers, BreakLogMarker{Type: "break", At: item.BreakAt, Label: "Break"})
			hasBreak = true
		}
		key := item.PodUID + "\x00" + item.Container + "\x00" + item.TerminationAt.Format(time.RFC3339Nano)
		if !crashMarkers[key] && !item.TerminationAt.IsZero() && !item.TerminationAt.Before(query.From) && !item.TerminationAt.After(query.To) {
			page.Markers = append(page.Markers, BreakLogMarker{Type: "crash", At: item.TerminationAt, Label: item.Container + " previous termination"})
			crashMarkers[key] = true
		}
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
	return page, nil
}

func (page *BreakLogPage) closeLinkedGaps(intervals []breaklogs.Gap, lines []BreakLogLine, until time.Time) {
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
			for _, line := range lines {
				if line.At.Before(gap.From) || line.At.After(gap.To) || line.At.After(until) || gap.Container != "" && gap.Container != line.Container {
					continue
				}
				if uid != "" && line.PodUID == uid || uid == "" && line.Pod == name {
					return true
				}
			}
			return false
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
		if !covered || !until.After(gap.From) {
			remaining = append(remaining, gap)
			continue
		}
		intervals = append(intervals, breaklogs.Gap{From: gap.From, To: minTime(gap.To, until)})
		if gap.To.After(until) {
			gap.From = until
			remaining = append(remaining, gap)
		}
	}
	page.Coverage = breaklogs.CoverObserved(page.Window.From, page.Window.To, until, intervals, remaining)
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
