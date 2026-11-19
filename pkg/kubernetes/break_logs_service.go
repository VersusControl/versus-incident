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
	"github.com/VersusControl/versus-incident/pkg/logging"
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
	service.observeBreakInventory(ctx, snapshot, owner)
	state, err := store.BreakLogs(now)
	if err != nil {
		return
	}
	if err := service.commitCredential(ctx, func() error { return store.CheckCaptureLease(owner) }); err != nil {
		return
	}

	roots, _ := classifyReadyBreaks(snapshot, now)
	logRoots := roots[:0]
	for _, root := range roots {
		// Cluster-scoped breaks remain issues, but have no namespace Pod logs.
		// A manifest requires a namespace; one Node must not reject the batch.
		if root.Root.Namespace != "" {
			logRoots = append(logRoots, root)
		}
	}
	roots = logRoots
	scope := service.indexScope()
	service.changes.mu.Lock()
	previous := service.changes.settledBreaks[scope]
	service.changes.mu.Unlock()
	// settled is rebuilt on every run from the roots still broken, so a root
	// that recovered is forgotten and the record never outgrows the cluster.
	settled := map[string]settledBreak{}
	defer func() {
		service.changes.mu.Lock()
		service.changes.settledBreaks[scope] = settled
		service.changes.mu.Unlock()
	}()
	changed := []kubebreaks.Break{}
	fingerprints := []string{}
	for _, root := range roots {
		if root.Source == "first_seen" {
			service.changes.mu.Lock()
			first := service.changes.firstSeen[scope][root.Root.Namespace+"\x00"+root.Root.UID]
			service.changes.mu.Unlock()
			if !first.IsZero() {
				root.BrokeAt = first
			}
		}
		// A break whose manifest and logs are stored, and whose restarts,
		// terminations and pods are what they were then, has nothing new to
		// capture; resolving its occurrence again would only repeat a read.
		fingerprint := logJobID(root, breakLogPods(snapshot.Records, root.Root.Namespace, root.Root.UID))
		key := settledBreakKey(root)
		if prior, known := previous[key]; known && prior.evidence == fingerprint && prior.stored(state) {
			settled[key] = prior
			continue
		}
		changed = append(changed, root)
		fingerprints = append(fingerprints, fingerprint)
	}
	logging.Debugf("kubernetes: break logs cluster=%q roots=%d unchanged=%d", service.scope.ClusterID, len(roots), len(roots)-len(changed))
	roots = changed
	manifests := make([]breaklogs.Manifest, len(roots))
	// Admit every root before slow container reads can consume this cycle.
	for i := range roots {
		root := &roots[i]
		manifests[i] = service.newBreakManifest(ctx, *root, state, now)
		root.BrokeAt = manifests[i].BreakAt
	}
	if service.evidenceStore() != nil {
		// Without the persisted occurrence the center may still drift, so the
		// manifest would not resolve by occurrence later. Retry next cycle.
		kept := roots[:0]
		keptManifests := manifests[:0]
		keptFingerprints := fingerprints[:0]
		for i := range roots {
			if manifests[i].OccurrenceID != "" {
				kept = append(kept, roots[i])
				keptManifests = append(keptManifests, manifests[i])
				keptFingerprints = append(keptFingerprints, fingerprints[i])
			}
		}
		roots, manifests, fingerprints = kept, keptManifests, keptFingerprints
	}

	admissionContext := ctx
	stopAdmission := func() {}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// Optional enrichment cannot discard already-authorized inventory metadata.
		// Credential retirement and capture leases still fence the bounded commit.
		admissionContext, stopAdmission = context.WithTimeout(service.identity.ctx, time.Second)
	}
	defer stopAdmission()
	if err := service.commitCredential(admissionContext, func() error { return store.CommitBreakLogs(nil, nil, "", owner, time.Now().UTC(), manifests...) }); err != nil {

		return
	}
	for i, root := range roots {
		pods := breakLogPods(snapshot.Records, root.Root.Namespace, root.Root.UID)
		job := logJobID(root, pods)
		done := settledBreak{evidence: fingerprints[i], manifest: manifests[i].ID, job: job, pods: len(pods)}
		if _, exists := state.Jobs[job]; exists {
			settled[settledBreakKey(root)] = done
			continue
		}
		manifest := manifests[i]
		from := manifest.From
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
				commitErr := service.commitCredential(commitContext, func() error { return store.CommitBreakLogs(items, gaps, "", owner, time.Now().UTC(), manifest) })
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
		err = service.commitCredential(ctx, func() error {
			return store.CommitBreakLogs(items, gaps, completedJob, owner, time.Now().UTC(), manifest)
		})
		if err != nil {
			return
		}
		if completedJob != "" {
			settled[settledBreakKey(root)] = done
		}
	}
}

// settledBreak records a break whose manifest and logs are stored: the
// evidence they were captured from (the root's break time and, per pod and
// container, its restarts and latest termination), the manifest it was filed
// under and the completed log job.
type settledBreak struct {
	evidence, manifest, job string
	pods                    int
}

func settledBreakKey(root kubebreaks.Break) string {
	return root.Root.Namespace + "\x00" + root.Root.UID + "\x00" + root.Rule
}

// stored reports whether the capture state still holds the break's manifest
// and completed log job. A manifest filed before its pods were known may still
// be filled in, so it does not count as settled while pods exist.
func (settled settledBreak) stored(state breaklogs.State) bool {
	if _, done := state.Jobs[settled.job]; !done {
		return false
	}
	for _, manifest := range state.Manifests {
		if manifest.ID == settled.manifest {
			return len(manifest.Pods) > 0 || settled.pods == 0
		}
	}
	return false
}

func (service *Service) readBreakContainer(ctx context.Context, pod kubeindex.Record, rootUID string, container kubeindex.ContainerState, slot string, from, to time.Time) (breaklogs.Snapshot, *breaklogs.Gap) {
	item := breaklogs.Snapshot{Cluster: service.scope.ClusterID, Namespace: pod.Namespace, RootUID: rootUID, Pod: pod.Name, PodUID: pod.UID, Container: container.Name, Init: container.Init, Slot: slot, TerminationAt: container.TerminatedAt, Restarts: container.RestartCount, CapturedAt: time.Now().UTC(), From: from, To: to, Lines: []breaklogs.Line{}}
	item.TerminationReason = safeBreakReason(service.scrubber.Scrub(safeBreakReason(container.TerminatedReason)))
	item.TerminationExitCode = container.ExitCode
	item.PodCreatedAt = pod.CreatedAt
	item.ID = breaklogs.Identity(item)
	if slot == "previous" && !container.TerminatedAt.IsZero() {
		item.To = minTime(item.To, breaklogs.TerminationEnd(container.TerminatedAt))
	}
	bounds := podLogBounds{LimitBytes: 64 << 10}
	if slot == "current" || container.CurrentTermination {
		bounds.SinceTime = from
	}
	// A terminated current incarnation still occupies kubelet's current slot
	// until it restarts. The snapshot remains terminated-window evidence.
	previous := slot == "previous" && !container.CurrentTermination
	logs, err := service.podLogs(ctx, pod.Namespace, pod.Name, container.Name, previous, 0, 500, true, bounds)
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
	if len(item.Lines) == 0 {
		return item, &breaklogs.Gap{SnapshotID: item.ID, ObservedAt: item.CapturedAt, From: from, To: to, Namespace: pod.Namespace, RootUID: rootUID, PodUID: pod.UID, Container: container.Name, Reason: "capture_late"}
	}
	return item, nil
}
