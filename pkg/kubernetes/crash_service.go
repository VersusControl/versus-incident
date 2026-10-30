package kubernetes

import (
	"context"
	"encoding/json"
	"time"

	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

type crashCaptureContextKey struct{}

type crashCaptureBatch struct {
	events    []kubechanges.CrashEvent
	pods      map[string]kubeindex.Record
	bytes     int
	read      bool
	attempted bool
	omitted   bool
}

func (batch *crashCaptureBatch) add(cluster string, pod kubeindex.Record, status map[string]any, observed time.Time) {
	batch.read = true
	if batch.pods == nil {
		batch.pods = map[string]kubeindex.Record{}
	}
	for _, field := range []string{"containerStatuses", "initContainerStatuses"} {
		for _, raw := range mapSlice(status[field]) {
			states := projectCrashContainers(map[string]any{field: []any{raw}})
			if len(states) == 0 {
				batch.omitted = true
				continue
			}
			state := states[0]
			base := kubechanges.CrashEvent{Cluster: cluster, Namespace: pod.Namespace, Pod: pod.Name, PodUID: pod.UID, Container: state.Name, Init: state.Init, RestartCount: state.RestartCount, At: observed, ObservedAt: observed, TimeSource: "observed_at", Type: "status", WaitingReason: state.WaitingReason}
			for _, source := range []string{"state", "lastState"} {
				containerState, _ := raw[source].(map[string]any)
				if _, exists := containerState["terminated"].(map[string]any); !exists {
					continue
				}
				termination := projectCrashContainers(map[string]any{field: []any{map[string]any{"name": state.Name, "state": containerState}}})[0]
				if (termination.TerminatedReason == "Completed" || termination.TerminatedReason == "") && termination.ExitCode == 0 && termination.Signal == 0 {
					continue
				}
				event := base
				event.Type, event.WaitingReason = "termination", ""
				event.TerminationSlot = "current"
				if source == "lastState" {
					event.TerminationSlot = "previous"
				}
				event.Reason, event.ExitCode, event.Signal = termination.TerminatedReason, termination.ExitCode, termination.Signal
				if !termination.TerminatedAt.IsZero() && !termination.TerminatedAt.After(observed) {
					event.At, event.TimeSource = termination.TerminatedAt, "finished_at"
				}
				batch.append(event, pod)
			}
			if state.WaitingReason != "" || state.RestartCount > 0 {
				batch.append(base, pod)
			}
		}
	}
}

func (batch *crashCaptureBatch) append(event kubechanges.CrashEvent, pod kubeindex.Record) {
	encoded, _ := json.Marshal(event)
	if event.Type == "termination" {
		for position := len(batch.events) - 1; position >= 0 && (len(batch.events) >= 5000 || batch.bytes+len(encoded) > 1<<20); position-- {
			if batch.events[position].Type != "status" {
				continue
			}
			dropped := batch.events[position]
			removed, _ := json.Marshal(dropped)
			batch.bytes -= len(removed)
			copy(batch.events[position:], batch.events[position+1:])
			batch.events[len(batch.events)-1] = kubechanges.CrashEvent{}
			batch.events = batch.events[:len(batch.events)-1]
			retained := false
			for _, remaining := range batch.events {
				if remaining.PodUID == dropped.PodUID {
					retained = true
					break
				}
			}
			if !retained {
				delete(batch.pods, dropped.PodUID)
			}
			batch.omitted = true
		}
	}
	if len(batch.events) >= 5000 || batch.bytes+len(encoded) > 1<<20 {
		batch.omitted = true
		return
	}
	batch.bytes += len(encoded)
	batch.events = append(batch.events, event)
	batch.pods[pod.UID] = kubeindex.Record{UID: pod.UID, Kind: pod.Kind, Namespace: pod.Namespace, Name: pod.Name, Owners: pod.Owners, Summary: map[string]string{"references_partial": pod.Summary["references_partial"]}}
}

func (batch *crashCaptureBatch) resolve(snapshot kubeindex.Snapshot, now time.Time) ([]kubechanges.CrashEvent, []kubechanges.Gap, bool) {
	if batch == nil {
		return nil, nil, false
	}
	state := snapshot.Kinds["Pod"]
	podRead := batch.read
	byUID := make(map[string]kubeindex.Record, len(snapshot.Records))
	for _, record := range snapshot.Records {
		byUID[record.UID] = record
	}
	retained := batch.events[:0]
	for _, event := range batch.events {
		pod := batch.pods[event.PodUID]
		root := kubebreaks.Root(pod, byUID)
		event.RootUID, event.RootKind, event.RootName = root.UID, root.Kind, root.Name
		event.AncestryPartial = crashAncestryPartial(pod, byUID)
		event.ID = kubechanges.CrashID(event)
		if !event.Valid() || event.ObservedAt.After(now) {
			batch.omitted = true
			continue
		}
		retained = append(retained, event)
	}
	clear(batch.events[len(retained):])
	batch.events = retained
	var gaps []kubechanges.Gap
	if batch.omitted || (batch.read || batch.attempted) && (state.State != "ready" || state.Partial) {
		gaps = append(gaps, kubechanges.Gap{From: now.Add(-24 * time.Hour), To: now})
	}
	return batch.events, gaps, podRead
}

func crashAncestryPartial(record kubeindex.Record, byUID map[string]kubeindex.Record) bool {
	seen := map[string]bool{}
	for depth := 0; depth < 16; depth++ {
		if seen[record.UID] || record.Summary["references_partial"] == "true" {
			return true
		}
		seen[record.UID] = true
		if len(record.Owners) == 0 {
			return false
		}
		var selected *kubeindex.OwnerRef
		for position := range record.Owners {
			owner := &record.Owners[position]
			if owner.ControllerKnown && owner.Controller {
				selected = owner
				break
			}
		}
		if selected == nil {
			return true
		}
		next, exists := byUID[selected.UID]
		if !exists || next.Kind != selected.Kind || next.Namespace != record.Namespace || selected.Namespace != "" && selected.Namespace != record.Namespace {
			return true
		}
		record = next
	}
	return true
}

type CrashPage struct {
	kubechanges.CrashPage
	Capture CaptureStatus `json:"capture"`
	Sync    SyncStatus    `json:"sync"`
}

func (service *Service) Crashes(ctx context.Context, query kubechanges.CrashQuery) (CrashPage, error) {
	if service.evidenceStore() != nil {
		return service.evidenceCrashes(ctx, query)
	}
	if service == nil {
		return CrashPage{}, ErrInvalidArguments
	}
	if err := service.validateCredential(ctx); err != nil {
		return CrashPage{}, err
	}
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return CrashPage{}, err
	}
	store := service.changeStore()
	if store == nil {
		return CrashPage{}, kubechanges.ErrStoreLimit
	}
	if len(service.readOrgs) > 1 {
		service.changes.mu.Lock()
		provider := service.changes.provider
		service.changes.mu.Unlock()
		var err error
		store, err = kubechanges.NewView(provider, service.indexScope(), service.readOrgs[1:])
		if err != nil {
			return CrashPage{}, err
		}
	}
	syncStatus := SyncStatus{State: "unavailable", Partial: true}
	if service.client != nil {
		_, status, err := service.CachedIndexSnapshot(ctx, "Pod", "ReplicaSet", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob")
		syncStatus = SyncStatus{State: status.State, AgeSeconds: status.AgeSeconds, Partial: status.Partial || err != nil, Progress: status.Progress, Pending: status.Pending, PendingKinds: status.PendingKinds}
		if err != nil && ctx.Err() != nil {
			return CrashPage{}, ctx.Err()
		}
	}
	page, err := store.QueryCrashes(query, time.Now().UTC())
	if err != nil {
		return CrashPage{}, err
	}
	if err := service.validateCredential(ctx); err != nil {
		return CrashPage{}, err
	}
	service.changes.mu.Lock()
	journalFailed := service.changes.journalFailed[service.indexScope()]
	service.changes.mu.Unlock()
	capture := service.CaptureStatus()
	if journalFailed {
		syncStatus.Partial = true
		capture.Behind = true
		page.Limitations = append(page.Limitations, "journal_commit_failed")
	}
	return CrashPage{CrashPage: page, Capture: capture, Sync: syncStatus}, nil
}
