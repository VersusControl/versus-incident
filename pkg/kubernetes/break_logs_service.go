package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func breakLogPods(records []kubeindex.Record, namespace, rootUID string) []kubeindex.Record {
	byUID := map[string]kubeindex.Record{}
	for _, record := range records {
		byUID[record.UID] = record
	}
	pods := []kubeindex.Record{}
	for _, record := range records {
		if record.Kind == "Pod" && record.Namespace == namespace && kubebreaks.Root(record, byUID).UID == rootUID {
			pods = append(pods, record)
		}
	}
	sort.Slice(pods, func(left, right int) bool {
		rank := func(pod kubeindex.Record) int {
			for _, container := range pod.Containers {
				if container.RestartCount > 0 || !container.TerminatedAt.IsZero() {
					return 0
				}
			}
			if pod.ReadyStatus != "True" {
				return 1
			}
			return 2
		}
		if rank(pods[left]) != rank(pods[right]) {
			return rank(pods[left]) < rank(pods[right])
		}
		if !pods[left].CreatedAt.Equal(pods[right].CreatedAt) {
			return pods[left].CreatedAt.After(pods[right].CreatedAt)
		}
		return pods[left].UID < pods[right].UID
	})
	return pods
}

func logJobID(root kubebreaks.Break, pods []kubeindex.Record) string {
	identities := []any{root.Root.UID}
	identities = append(identities, root.BrokeAt)
	for _, pod := range pods {
		for _, container := range pod.Containers {
			identities = append(identities, []any{pod.UID, container.Name, container.Init, container.RestartCount, container.TerminatedAt})
		}
	}
	encoded, _ := json.Marshal(identities)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (service *Service) enqueueBreakLogs(snapshot kubeindex.Snapshot, owner string, parent context.Context) {
	if service.scrubber == nil || service.changeStore() == nil {
		return
	}
	scope := service.indexScope()
	service.changes.mu.Lock()
	if service.changes.stopped[scope] || service.changes.logActive[scope] {
		service.changes.mu.Unlock()
		return
	}
	service.changes.logActive[scope] = true
	if parent == nil {
		parent = service.changes.lifecycle
	}
	if parent == nil {
		parent = service.identity.ctx
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	done := make(chan struct{})
	service.changes.logDone[scope] = done
	service.changes.logCancel[scope] = cancel
	service.changes.mu.Unlock()
	go func() {
		defer func() {
			service.changes.mu.Lock()
			delete(service.changes.logActive, scope)
			delete(service.changes.logDone, scope)
			delete(service.changes.logCancel, scope)
			close(done)
			service.changes.mu.Unlock()
		}()
		defer cancel()
		stop := context.AfterFunc(service.identity.ctx, cancel)
		defer stop()
		ctx = captureReadContext(ctx, owner)
		ctx = context.WithValue(ctx, credentialEpochContextKey{}, service)
		service.captureBreakLogs(ctx, snapshot, owner)
	}()
}

func (service *Service) captureBreakLogs(ctx context.Context, snapshot kubeindex.Snapshot, owner string) {
	store := service.changeStore()
	now := time.Now().UTC()
	state, err := store.BreakLogs(now)
	if err != nil {
		return
	}
	if err := service.commitCredential(ctx, func() error { return store.CheckCaptureLease(owner) }); err != nil {
		return
	}
	roots, _ := classifyReadyBreaks(snapshot, now)
	for _, root := range roots {
		if root.Source == "first_seen" {
			service.changes.mu.Lock()
			first := service.changes.firstSeen[service.indexScope()][root.Root.Namespace+"\x00"+root.Root.UID]
			service.changes.mu.Unlock()
			if !first.IsZero() {
				root.BrokeAt = first
			}
		}
		pods := breakLogPods(snapshot.Records, root.Root.Namespace, root.Root.UID)
		job := logJobID(root, pods)
		if _, exists := state.Jobs[job]; exists {
			continue
		}
		from := root.BrokeAt.Add(-5 * time.Minute)
		if root.Source == "first_seen" {
			service.changes.mu.Lock()
			first := service.changes.firstSeen[service.indexScope()][root.Root.Namespace+"\x00"+root.Root.UID]
			service.changes.mu.Unlock()
			if !first.IsZero() {
				from = first.Add(-5 * time.Minute)
			}
		}
		if root.BrokeAt.IsZero() {
			from = now.Add(-5 * time.Minute)
		}
		from = maxTime(from, now.Add(-24*time.Hour+time.Second))
		items := []breaklogs.Snapshot{}
		gaps := []breaklogs.Gap{}
		retry := false
		if len(pods) > 5 {
			gaps = append(gaps, breaklogs.Gap{From: from, To: now, Namespace: root.Root.Namespace, RootUID: root.Root.UID, Reason: "budget"})
			pods = pods[:5]
		}
		if len(pods) == 0 {
			gaps = append(gaps, breaklogs.Gap{From: from, To: now, Namespace: root.Root.Namespace, RootUID: root.Root.UID, Reason: "pod_deleted"})
		}
		for _, pod := range pods {
			containers := append([]kubeindex.ContainerState(nil), pod.Containers...)
			sort.SliceStable(containers, func(left, right int) bool {
				return containerEvidenceRank(containers[left]) < containerEvidenceRank(containers[right])
			})
			if len(containers) > 3 {
				gaps = append(gaps, breaklogs.Gap{From: from, To: now, Namespace: pod.Namespace, RootUID: root.Root.UID, PodUID: pod.UID, Reason: "budget"})
				containers = containers[:3]
			}
			for _, container := range containers {
				previous := !container.TerminatedAt.IsZero() && !container.TerminatedAt.Before(from) && !container.TerminatedAt.After(now)
				slots := []string{}
				if previous {
					slots = append(slots, "previous")
				}
				if !container.Ready && (!container.StateKnown && pod.ReadyStatus != "True" || container.Running) && container.WaitingReason == "" && !container.CurrentTermination {
					slots = append(slots, "current")
				}
				for _, slot := range slots {
					item, gap := service.readBreakContainer(ctx, pod, root.Root.UID, container, slot, from, now)
					if gap != nil {
						gaps = append(gaps, *gap)
						retry = retry || gap.Reason == "budget"
					} else {
						item.BreakAt = root.BrokeAt
						items = append(items, item)
					}
				}
			}
		}
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				commitContext, stop := context.WithTimeout(service.identity.ctx, time.Second)
				commitErr := service.commitCredential(commitContext, func() error { return store.CommitBreakLogs(items, gaps, "", owner, time.Now().UTC()) })
				stop()
				if commitErr != nil {
					return
				}
			}
			return
		}
		completedJob := job
		if retry {
			completedJob = ""
		}
		err = service.commitCredential(ctx, func() error { return store.CommitBreakLogs(items, gaps, completedJob, owner, time.Now().UTC()) })
		if err != nil {
			return
		}
	}
}

func (service *Service) readBreakContainer(ctx context.Context, pod kubeindex.Record, rootUID string, container kubeindex.ContainerState, slot string, from, to time.Time) (breaklogs.Snapshot, *breaklogs.Gap) {
	item := breaklogs.Snapshot{Cluster: service.scope.ClusterID, Namespace: pod.Namespace, RootUID: rootUID, Pod: pod.Name, PodUID: pod.UID, Container: container.Name, Init: container.Init, Slot: slot, TerminationAt: container.TerminatedAt, Restarts: container.RestartCount, CapturedAt: time.Now().UTC(), From: from, To: to, Lines: []breaklogs.Line{}}
	item.PodCreatedAt = pod.CreatedAt
	item.ID = breaklogs.Identity(item)
	if slot == "previous" && !container.TerminatedAt.IsZero() {
		item.To = minTime(item.To, container.TerminatedAt)
	}
	bounds := podLogBounds{LimitBytes: 64 << 10}
	if slot == "current" {
		bounds.SinceTime = from
	}
	logs, err := service.podLogs(ctx, pod.Namespace, pod.Name, container.Name, slot == "previous", 0, 500, true, bounds)
	if err != nil {
		reason := "capture_late"
		var throttled *ErrThrottled
		switch {
		case errors.Is(err, ErrForbidden):
			reason = "forbidden"
		case errors.Is(err, ErrNotFound):
			reason = "pod_deleted"
		case errors.As(err, &throttled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrOperationBudget):
			reason = "budget"
		}
		return item, &breaklogs.Gap{SnapshotID: item.ID, ObservedAt: time.Now().UTC(), From: item.From, To: item.To, Namespace: pod.Namespace, RootUID: rootUID, PodUID: pod.UID, Container: container.Name, Reason: reason}
	}
	item.CapturedAt = time.Now().UTC()
	item.Truncated = logs.Truncated
	usedBytes := 0
	for _, text := range strings.Split(logs.Text, "\n") {
		timestamp, message, ok := strings.Cut(text, " ")
		at, parseErr := time.Parse(time.RFC3339Nano, timestamp)
		if !ok || parseErr != nil || at.Before(item.From) || at.After(item.To) {
			continue
		}
		message = service.scrubber.Scrub(message)
		if len(message) > 4096 {
			message = strings.ToValidUTF8(message[:4096], "")
			item.Truncated = true
		}
		if len(item.Lines) == 500 {
			item.Truncated = true
			break
		}
		usedBytes += len(message)
		if usedBytes > 64<<10 {
			item.Truncated = true
			break
		}
		item.Lines = append(item.Lines, breaklogs.Line{At: at, Text: message})
	}
	if slot == "previous" && !container.TerminatedAt.IsZero() && container.TerminatedAt.Before(item.To) {
		item.To = container.TerminatedAt
	}
	if len(item.Lines) == 500 {
		item.Truncated = true
	}
	if item.Truncated && len(item.Lines) > 0 && item.Lines[0].At.After(item.From) {
		item.From = item.Lines[0].At
	}
	if item.Truncated && len(item.Lines) > 0 {
		item.To = minTime(item.To, item.Lines[len(item.Lines)-1].At)
	}
	if item.Truncated && len(item.Lines) == 0 {
		item.From = item.To
	}
	return item, nil
}
