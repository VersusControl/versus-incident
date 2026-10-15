package kubernetes

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/google/uuid"
)

type CaptureStatus struct {
	Mode         string                       `json:"mode"`
	Reason       string                       `json:"reason,omitempty"`
	LastCycleAt  *time.Time                   `json:"last_cycle_at,omitempty"`
	Behind       bool                         `json:"behind"`
	Verification *projectedVerificationStatus `json:"verification,omitempty"`
}

type captureLoop struct {
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	owner         string
	owned         bool
	ownedContext  context.Context
	status        CaptureStatus
	next          map[string]time.Time
	success       map[string]time.Time
	completed     map[string]time.Time
	backoff       time.Time
	gapFrom       map[string]time.Time
	leaseTTL      time.Duration
	renewInterval time.Duration
}

var captureCadences = map[string]time.Duration{
	"Pod": 30 * time.Second, "Deployment": 30 * time.Second, "ReplicaSet": 30 * time.Second, "StatefulSet": 30 * time.Second, "DaemonSet": 30 * time.Second, "Event": 30 * time.Second,
	"ControllerRevision": 30 * time.Second,
	"Job":                time.Minute, "CronJob": time.Minute, "Node": time.Minute,
	"ConfigMap": 5 * time.Minute, "Secret": 5 * time.Minute, "HorizontalPodAutoscaler": 5 * time.Minute,
}

func (service *Service) indexScope() kubeindex.Scope {
	return kubeindex.Scope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, CredentialID: service.scope.CredentialID}
}

func (service *Service) SetCaptureMode(mode string) error {
	if service == nil {
		return ErrInvalidArguments
	}
	if mode == "" {
		mode = "continuous"
	}
	if mode != "continuous" && mode != "demand" {
		return ErrInvalidArguments
	}
	service.captureMode = mode
	return nil
}

func (service *Service) CaptureStatus() CaptureStatus {
	service.changes.mu.Lock()
	loop := service.changes.captures[service.indexScope()]
	service.changes.mu.Unlock()
	if loop == nil {
		return service.effectiveCaptureStatus()
	}
	loop.mu.Lock()
	defer loop.mu.Unlock()
	status := loop.status
	if status.LastCycleAt != nil {
		at := *status.LastCycleAt
		status.LastCycleAt = &at
	}
	return status
}

func (service *Service) StartCapture(parent context.Context) {
	if service == nil || service.client == nil || service.changes == nil || parent == nil {
		return
	}
	validation, stopValidation := context.WithTimeout(parent, 10*time.Second)
	err := service.validateCredential(validation)
	stopValidation()
	if err != nil {
		return
	}
	effective := service.effectiveCaptureStatus()
	service.changes.mu.Lock()
	if service.changes.stopped[service.indexScope()] {
		service.changes.mu.Unlock()
		return
	}
	if service.changes.lifecycle == nil {
		service.changes.lifecycle = parent
	}
	scope := service.indexScope()
	if service.changes.captures[scope] != nil {
		service.changes.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	if service.identity != nil {
		cancelScope := cancel
		stopIdentity := context.AfterFunc(service.identity.ctx, cancelScope)
		cancel = func() { stopIdentity(); cancelScope() }
	}
	loop := &captureLoop{ctx: ctx, cancel: cancel, done: make(chan struct{}), owner: uuid.NewString(), next: map[string]time.Time{}, success: map[string]time.Time{}, completed: map[string]time.Time{}, gapFrom: map[string]time.Time{}, status: effective}
	service.changes.captures[scope] = loop
	service.changes.mu.Unlock()
	if effective.Mode == "demand" {
		close(loop.done)
		if effective.Reason != "" {
			log.Printf("kubernetes: capture using demand mode: %s", effective.Reason)
		}
		return
	}
	go service.capture(loop)
}

func (service *Service) effectiveCaptureStatus() CaptureStatus {
	if service.captureMode == "demand" {
		return CaptureStatus{Mode: "demand"}
	}
	rotating := false
	if service.client != nil {
		switch service.client.credentials.(type) {
		case *expiringBearerSource, *tokenFileSource:
			rotating = true
		}
	}
	service.identity.mu.Lock()
	known := service.identity.known
	service.identity.mu.Unlock()
	if rotating && !known {
		status := CaptureStatus{Mode: "demand", Reason: "stable_principal_unverified"}
		if source, ok := service.client.credentials.(*tokenFileSource); ok && source.verifier != nil {
			verification := source.verifier.verificationStatus()
			status.Verification = &verification
		}
		return status
	}
	service.changes.mu.Lock()
	_, atomicStorage := service.changes.provider.(storage.BlobCAS)
	service.changes.mu.Unlock()
	if !atomicStorage {
		return CaptureStatus{Mode: "demand", Reason: "atomic_storage_unavailable"}
	}
	return CaptureStatus{Mode: "continuous", Behind: true}
}

func (service *Service) StopCapture() {
	if service == nil || service.changes == nil {
		return
	}
	service.changes.mu.Lock()
	scope := service.indexScope()
	service.changes.stopped[scope] = true
	loop := service.changes.captures[scope]
	refreshes := []*indexRefreshCall{}
	for key, call := range service.changes.refreshes {
		if key.scope == scope {
			call.cancel()
			refreshes = append(refreshes, call)
		}
	}
	for call := range service.changes.fallbacks[scope] {
		call.cancel()
		refreshes = append(refreshes, call)
	}
	service.changes.mu.Unlock()
	if loop != nil {
		loop.cancel()
		<-loop.done
	}
	for _, refresh := range refreshes {
		<-refresh.done
	}
}

func (registry *ServiceRegistry) StartCapture(ctx context.Context) {
	if registry == nil || ctx == nil {
		return
	}
	registry.mu.Lock()
	if registry.closed || registry.captureContext != nil {
		registry.mu.Unlock()
		return
	}
	registry.captureContext = ctx
	context.AfterFunc(ctx, registry.Close)
	services := make([]*Service, 0, len(registry.services))
	for _, service := range registry.services {
		services = append(services, service)
	}
	registry.mu.Unlock()
	for _, service := range services {
		service.StartCapture(ctx)
	}
}

func (registry *ServiceRegistry) Close() {
	_ = registry.CloseContext(context.Background())
}

func (registry *ServiceRegistry) CloseContext(ctx context.Context) error {
	if registry == nil {
		return nil
	}
	registry.mu.Lock()
	if registry.closeDone == nil {
		registry.closed = true
		registry.closeDone = make(chan struct{})
		services := make([]*Service, 0, len(registry.services))
		for _, service := range registry.services {
			services = append(services, service)
		}
		go func() {
			var workers sync.WaitGroup
			for _, service := range services {
				workers.Add(1)
				go func(service *Service) {
					defer workers.Done()
					service.StopCapture()
				}(service)
			}
			workers.Wait()
			close(registry.closeDone)
		}()
	}
	done := registry.closeDone
	registry.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (registry *ServiceRegistry) DeregisterCluster(clusterID string) {
	if registry == nil {
		return
	}
	registry.mu.Lock()
	services := []*Service{}
	for key, service := range registry.services {
		if service.scope.ClusterID == clusterID {
			services = append(services, service)
			delete(registry.services, key)
		}
	}
	entries := registry.entries[:0]
	for _, entry := range registry.entries {
		if entry.Info.ID != clusterID {
			entries = append(entries, entry)
		}
	}
	registry.entries = entries
	registry.mu.Unlock()
	for _, service := range services {
		service.StopCapture()
	}
}

func (service *Service) capture(loop *captureLoop) {
	defer close(loop.done)
	store := service.changeStore()
	defer func() {
		if err := store.ReleaseLease(loop.owner); err != nil {
			log.Printf("kubernetes: capture lease release unavailable")
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	leaseTTL, renewInterval := loop.leaseTTL, loop.renewInterval
	if leaseTTL <= 0 {
		leaseTTL = 90 * time.Second
	}
	if renewInterval <= 0 {
		renewInterval = 20 * time.Second
	}
	for loop.ctx.Err() == nil {
		var leaseExpires time.Time
		if err := service.commitCredential(loop.ctx, func() error {
			now := time.Now().UTC()
			leaseExpires = now.Add(leaseTTL)
			return store.ClaimLease(loop.owner, now, leaseTTL)
		}); err == nil {
			service.captureOwned(loop, store, leaseTTL, renewInterval, leaseExpires, ticker.C)
			if err := store.ReleaseLease(loop.owner); err != nil {
				log.Printf("kubernetes: capture lease release unavailable")
			}
			loop.mu.Lock()
			loop.owner = uuid.NewString()
			loop.next = map[string]time.Time{}
			loop.success = map[string]time.Time{}
			loop.completed = map[string]time.Time{}
			loop.mu.Unlock()
		}
		select {
		case <-loop.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (service *Service) captureOwned(loop *captureLoop, store *kubechanges.Store, leaseTTL, renewInterval time.Duration, leaseExpires time.Time, ticks <-chan time.Time) {
	ownedContext, cancelOwned := context.WithCancel(loop.ctx)
	leaseTimer := time.AfterFunc(time.Until(leaseExpires), cancelOwned)
	defer leaseTimer.Stop()
	ownedContext = captureReadContext(ownedContext, loop.owner)
	loop.mu.Lock()
	loop.owned = true
	loop.ownedContext = ownedContext
	loop.mu.Unlock()
	now := time.Now().UTC()
	if previous, err := store.ReadWindow(now.Add(-24*time.Hour), now, now); err == nil {
		loop.mu.Lock()
		for kind, interval := range previous.Coverage {
			loop.gapFrom[kind] = interval.To
		}
		loop.mu.Unlock()
	}
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		renew := time.NewTicker(renewInterval)
		defer renew.Stop()
		for {
			select {
			case <-ownedContext.Done():
				return
			case <-renew.C:
				attemptedRenewal := false
				if err := service.commitCredential(ownedContext, func() error {
					attemptedRenewal = true
					now := time.Now().UTC()
					leaseExpires = now.Add(leaseTTL)
					return store.RenewLease(loop.owner, now, leaseTTL)
				}); err != nil {
					loop.mu.Lock()
					loop.status.Behind = true
					loop.mu.Unlock()
					if attemptedRenewal || ownedContext.Err() != nil || errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, kubechanges.ErrLeaseLost) {
						cancelOwned()
						return
					}
					continue
				}
				leaseTimer.Reset(time.Until(leaseExpires))
			}
		}
	}()
	defer func() {
		cancelOwned()
		<-renewDone
		loop.mu.Lock()
		loop.owned = false
		loop.ownedContext = nil
		loop.mu.Unlock()
	}()
	for {
		now := time.Now().UTC()
		loop.mu.Lock()
		due := []string{}
		if !now.Before(loop.backoff) {
			for kind := range captureCadences {
				if !now.Before(loop.next[kind]) {
					due = append(due, kind)
				}
			}
		}
		loop.mu.Unlock()
		if len(due) > 0 {
			_, _, err := service.IndexSnapshot(ownedContext, due...)
			if err != nil && !errors.Is(err, context.Canceled) {
				loop.mu.Lock()
				loop.status.Behind = true
				loop.mu.Unlock()
			}
		}
		select {
		case <-ownedContext.Done():
			return
		case <-ticks:
		}
	}
}

func (service *Service) captureState() *captureLoop {
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	return service.changes.captures[service.indexScope()]
}

func (service *Service) captureCycle(store *kubechanges.Store, deltas <-chan kubeindex.Delta, snapshot kubeindex.Snapshot, kinds []string, started time.Time, expectedOwner ...string) error {
	now := time.Now().UTC()
	loop := service.captureState()
	continuous := loop != nil && loop.status.Mode == "continuous"
	if continuous {
		loop.mu.Lock()
		owned := loop.owned && loop.ctx.Err() == nil && (loop.ownedContext == nil || loop.ownedContext.Err() == nil) && (len(expectedOwner) == 0 || expectedOwner[0] != "" && expectedOwner[0] == loop.owner)
		loop.mu.Unlock()
		if !owned {
			return kubechanges.ErrLeaseLost
		}
	}
	collected, gaps := drainIndexChanges(service.scope.ClusterID, deltas, started, now, continuous)
	collected = append(collected, kubechanges.Reconstruct(service.scope.ClusterID, snapshot.Records, now)...)
	partial := false
	for kind, state := range snapshot.Kinds {
		if state.Partial && !(kind == "Secret" || kind == "ControllerRevision") {
			partial = true
		}
	}
	if loop == nil || loop.status.Mode == "demand" {
		if err := store.Commit(collected, gaps, nil, "", now); err != nil {
			return err
		}
		service.rememberBreaks(kubebreaks.Classify(snapshot.Records, now), "", now, partial)
		return nil
	}
	loop.mu.Lock()
	defer loop.mu.Unlock()
	if !loop.owned || loop.ctx.Err() != nil || loop.ownedContext != nil && loop.ownedContext.Err() != nil || len(expectedOwner) > 0 && (expectedOwner[0] == "" || expectedOwner[0] != loop.owner) {
		return kubechanges.ErrLeaseLost
	}
	coverage := map[string]kubechanges.Gap{}
	success := map[string]time.Time{}
	behind := false
	for _, kind := range kinds {
		cadence, tracked := captureCadences[kind]
		if !tracked {
			continue
		}
		state := snapshot.Kinds[kind]
		pages := max(1, (state.Records+kubernetesIndexPageSize-1)/kubernetesIndexPageSize)
		stretched := time.Duration(float64(pages) / 2 * 1.5 * float64(time.Second))
		stretched = max(stretched, time.Duration(float64(now.Sub(started))*1.5))
		cadence = max(cadence, stretched)
		if state.State != "ready" || state.Partial {
			if !(kind == "Secret" && state.Error == "forbidden" || kind == "ControllerRevision" && (state.Error == "forbidden" || state.Error == "not_found")) {
				behind = true
			}
			loop.next[kind] = now.Add(cadence)
			continue
		}
		observed := state.ObservedAt
		if observed.IsZero() {
			observed = now
		}
		previous := loop.success[kind]
		if !previous.IsZero() && !observed.After(previous) {
			continue
		}
		loop.next[kind] = now.Add(cadence)
		if previous.IsZero() {
			coverage[kind] = kubechanges.Gap{From: observed, To: observed}
			if old := loop.gapFrom[kind]; !old.IsZero() {
				gaps = append(gaps, kubechanges.Gap{From: old, To: observed})
				coverage[kind] = kubechanges.Gap{From: old, To: observed}
			}
		} else {
			if observed.Sub(previous) > 2*cadence {
				gaps = append(gaps, kubechanges.Gap{From: previous, To: observed})
			}
			coverage[kind] = kubechanges.Gap{From: previous, To: observed}
		}
		success[kind] = observed
	}
	if err := store.Commit(collected, gaps, coverage, loop.owner, now); err != nil {
		loop.status.Behind = true
		return err
	}
	service.rememberBreaks(kubebreaks.Classify(snapshot.Records, now), "", now, partial)
	for kind, at := range success {
		loop.success[kind] = at
	}
	if loop.completed == nil {
		loop.completed = map[string]time.Time{}
	}
	for kind := range success {
		loop.completed[kind] = now
	}
	for kind := range success {
		delete(loop.gapFrom, kind)
	}
	if len(success) > 0 {
		loop.status.LastCycleAt = &now
	}
	loop.status.Behind = behind
	return nil
}

func drainIndexChanges(cluster string, deltas <-chan kubeindex.Delta, started, now time.Time, continuous ...bool) ([]kubechanges.Change, []kubechanges.Gap) {
	result := []kubechanges.Change{}
	gaps := []kubechanges.Gap{}
	for deltas != nil {
		select {
		case delta, open := <-deltas:
			if !open {
				deltas = nil
				continue
			}
			if delta.Resync {
				gaps = append(gaps, kubechanges.Gap{From: started, To: now})
				continue
			}
			if len(continuous) > 0 && continuous[0] {
				result = append(result, kubechanges.DetectContinuous(cluster, delta, now)...)
			} else {
				result = append(result, kubechanges.DetectAll(cluster, delta, now)...)
			}
		default:
			deltas = nil
		}
	}
	return result, gaps
}
