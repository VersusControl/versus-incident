package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

const (
	MaxWorkloadLogPods     = 20
	MaxWorkloadLogSources  = 40
	maxWorkloadCursorBytes = 512 << 10
	maxWorkloadListedPods  = 200
)

var errWorkloadByteLimit = errors.New("kubernetes: aggregate log byte limit")
var errWorkloadSourceGone = errors.New("kubernetes: log source ended or replaced")
var workloadStreamProcess = logIdentity(strconv.FormatInt(time.Now().UnixNano(), 10))
var logUIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$`)

type WorkloadLogStreamOptions struct {
	Kind, Name, Namespace, UID string
	PodUIDs                    []string
	Container                  string
	SinceSeconds, TailLines    int
	Previous, Timestamps       bool
	Cursor                     string
}

type WorkloadLogSource struct {
	SourceID    string `json:"source_id"`
	PodUID      string `json:"pod_uid"`
	Pod         string `json:"pod"`
	Namespace   string `json:"namespace"`
	Container   string `json:"container"`
	Incarnation string `json:"incarnation"`
	Admitted    bool   `json:"admitted"`
	Reason      string `json:"reason,omitempty"`
}

type WorkloadLogPod struct {
	UID             string   `json:"uid"`
	Name            string   `json:"name"`
	Containers      []string `json:"containers"`
	InitContainers  []string `json:"init_containers"`
	MainContainer   string   `json:"main_container,omitempty"`
	Admitted        bool     `json:"admitted"`
	Truncated       bool     `json:"truncated,omitempty"`
	Reason          string   `json:"reason,omitempty"`
	AllPodsAdmitted bool     `json:"all_pods_admitted"`
	AllPodsReason   string   `json:"all_pods_reason,omitempty"`
}

type WorkloadLogStreamEvent struct {
	PodLogStreamEvent
	WorkloadUID string              `json:"workload_uid"`
	PodUID      string              `json:"pod_uid,omitempty"`
	Pod         string              `json:"pod,omitempty"`
	Namespace   string              `json:"namespace,omitempty"`
	Incarnation string              `json:"incarnation,omitempty"`
	SourceID    string              `json:"source_id,omitempty"`
	Sources     []WorkloadLogSource `json:"sources,omitempty"`
	Pods        []WorkloadLogPod    `json:"pods,omitempty"`
	Truncated   bool                `json:"truncated,omitempty"`
}

type workloadLogCursor struct {
	Target       string            `json:"target"`
	Process      string            `json:"process"`
	Sources      map[string]string `json:"sources"`
	Reference    string            `json:"reference,omitempty"`
	Uncertain    bool              `json:"uncertain,omitempty"`
	SinceSeconds int               `json:"since_seconds,omitempty"`
	TailLines    int               `json:"tail_lines,omitempty"`
	Owner        string            `json:"-"`
}

func logIdentity(value string) string {
	hash := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func (service *Service) workloadLogTarget(options WorkloadLogStreamOptions) string {
	pods := append([]string(nil), options.PodUIDs...)
	sort.Strings(pods)
	return logIdentity(service.cacheKey() + "\x00" + options.Kind + "\x00" + options.Namespace + "\x00" + options.Name + "\x00" + options.UID + "\x00" + options.Container + "\x00" + strconv.FormatBool(options.Previous) + "\x00" + strings.Join(pods, "\x00"))
}

func (service *Service) workloadCursor(options WorkloadLogStreamOptions) (workloadLogCursor, error) {
	result := workloadLogCursor{Target: service.workloadLogTarget(options), Process: workloadStreamProcess, Sources: map[string]string{}}
	if options.Cursor == "" {
		return result, nil
	}
	if len(options.Cursor) > maxWorkloadCursorBytes || options.SinceSeconds != 0 || options.TailLines != 0 {
		return result, ErrInvalidArguments
	}
	raw, err := base64.RawURLEncoding.DecodeString(options.Cursor)
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Target != service.workloadLogTarget(options) || len(result.Sources) > MaxWorkloadLogSources || len(result.Process) != 43 || result.SinceSeconds < 0 || result.SinceSeconds > 86400 || result.TailLines < 0 || result.TailLines > 5000 {
		return result, ErrInvalidArguments
	}
	if result.Reference != "" {
		if len(result.Reference) != 24 || len(result.Sources) != 0 {
			return result, ErrInvalidArguments
		}
		retained, exists := restoreWorkloadCheckpoint(result.Reference)
		if exists {
			if retained.Target != result.Target || retained.Process != result.Process {
				return result, ErrInvalidArguments
			}
			result = retained
		} else {
			result.Sources = map[string]string{}
			result.Uncertain = true
		}
	}
	for source, cursor := range result.Sources {
		if len(source) != 43 || len(cursor) > 16<<10 || cursor == "" {
			return result, ErrInvalidArguments
		}
		// Validate the complete Pod checkpoint before it can influence a read.
		var checkpoint logResumeCursor
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
		if decodeErr != nil || json.Unmarshal(decoded, &checkpoint) != nil {
			return result, ErrInvalidArguments
		}
		// Pod targets are validated against actual source identities on admission.
		if checkpoint.Timestamp.IsZero() || !checkpoint.Quiet && checkpoint.Ordinal == 0 || checkpoint.Ordinal > 1<<53 || len(checkpoint.Target) != 43 || len(checkpoint.Occurrences) > maxLogCursorTimestamps {
			return result, ErrInvalidArguments
		}
	}
	if result.Sources == nil {
		result.Sources = map[string]string{}
	}
	return result, nil
}

func (service *Service) validateWorkloadLogOptions(ctx context.Context, options WorkloadLogStreamOptions) error {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return ErrForbidden
	}
	if service == nil || service.client == nil || service.client.streamHTTP == nil || service.scrubber == nil {
		return ErrInvalidEndpoint
	}
	if options.Kind != "Deployment" && options.Kind != "StatefulSet" || !validPodLogName(options.Name, 253, true) || !validPodLogName(options.Namespace, 63, false) || !logUIDPattern.MatchString(options.UID) || len(options.PodUIDs) > MaxWorkloadLogPods || options.Container != "" && options.Container != "*" && !validPodLogName(options.Container, 63, false) || options.SinceSeconds < 0 || options.SinceSeconds > 86400 || options.TailLines < 0 || options.TailLines > 5000 {
		return ErrInvalidArguments
	}
	seen := make(map[string]bool)
	for _, uid := range options.PodUIDs {
		if !logUIDPattern.MatchString(uid) || seen[uid] {
			return ErrInvalidArguments
		}
		seen[uid] = true
	}
	_, err := service.workloadCursor(options)
	return err
}

func workloadOwnedBy(record kubeindex.Record, kind, uid string) bool {
	for _, owner := range record.Owners {
		if owner.Kind == kind && owner.UID == uid && owner.Namespace == record.Namespace && owner.ControllerKnown && owner.Controller {
			return true
		}
	}
	return false
}

// workloadLogPods admits only fresh, UID-verified controller chains.
func workloadLogPods(snapshot kubeindex.Snapshot, options WorkloadLogStreamOptions, initial bool) ([]kubeindex.Record, error) {
	kinds := []string{options.Kind, "Pod"}
	if options.Kind == "Deployment" {
		kinds = append(kinds, "ReplicaSet")
	}
	for _, kind := range kinds {
		status, exists := snapshot.Kinds[kind]
		if !exists || status.State != "ready" || status.Partial || time.Since(status.ObservedAt) > 2*time.Minute {
			return nil, ErrForbidden
		}
	}
	found := false
	for _, record := range snapshot.Records {
		if record.Kind == options.Kind && record.Namespace == options.Namespace && record.Name == options.Name {
			if record.UID != options.UID {
				return nil, ErrNotFound
			}
			found = true
		}
	}
	if !found {
		return nil, ErrNotFound
	}
	replicaSets := make(map[string]bool)
	for _, record := range snapshot.Records {
		if record.Kind == "ReplicaSet" && record.Namespace == options.Namespace && workloadOwnedBy(record, "Deployment", options.UID) {
			replicaSets[record.UID] = true
		}
	}
	result := []kubeindex.Record{}
	owned := make(map[string]bool)
	for _, record := range snapshot.Records {
		if record.Kind != "Pod" || record.Namespace != options.Namespace {
			continue
		}
		valid := options.Kind == "StatefulSet" && workloadOwnedBy(record, "StatefulSet", options.UID)
		if options.Kind == "Deployment" {
			for _, owner := range record.Owners {
				if replicaSets[owner.UID] && owner.Kind == "ReplicaSet" && workloadOwnedBy(record, "ReplicaSet", owner.UID) {
					valid = true
				}
			}
		}
		if valid {
			owned[record.UID] = true
			result = append(result, record)
		}
	}
	if initial {
		for _, uid := range options.PodUIDs {
			if !owned[uid] {
				return nil, ErrInvalidArguments
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].UID < result[j].UID
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func (service *Service) workloadLogMembership(ctx context.Context, options WorkloadLogStreamOptions, initial bool) ([]kubeindex.Record, error) {
	kinds := []string{options.Kind, "Pod"}
	if options.Kind == "Deployment" {
		kinds = append(kinds, "ReplicaSet")
	}
	snapshot, _, err := service.IndexSnapshot(ctx, kinds...)
	if err != nil {
		return nil, err
	}
	return workloadLogPods(snapshot, options, initial)
}

func (service *Service) ValidateWorkloadLogStream(ctx context.Context, options WorkloadLogStreamOptions) error {
	if err := service.validateWorkloadLogOptions(ctx, options); err != nil {
		return err
	}
	_, err := service.workloadLogMembership(ctx, options, true)
	return err
}

type workloadSourceWorker struct {
	source               WorkloadLogSource
	options              PodLogStreamOptions
	cancel               context.CancelFunc
	release              func()
	done                 chan struct{}
	started              bool
	finished             bool
	terminal             bool
	suspended            bool
	retiredForCompletion bool
	observation          string
	expected             kubeindex.Record
}

type workloadSourceMessage struct {
	id       string
	event    PodLogStreamEvent
	err      error
	finished bool
}

// WorkloadLogSession owns one viewer admission and every source reservation.
// Close is idempotent; Stream must have a single caller.
type WorkloadLogSession struct {
	service     *Service
	options     WorkloadLogStreamOptions
	ctx         context.Context
	cancel      context.CancelFunc
	release     func()
	once        sync.Once
	mu          sync.Mutex
	streaming   bool
	closed      bool
	workers     map[string]*workloadSourceWorker
	cursor      workloadLogCursor
	uncertain   bool
	queue       chan workloadSourceMessage
	pods        []WorkloadLogPod
	truncated   bool
	statuses    map[string]bool
	lastSources string
	failures    map[string]string
}

func (service *Service) OpenWorkloadLogStream(ctx context.Context, options WorkloadLogStreamOptions) (*WorkloadLogSession, error) {
	if err := service.validateWorkloadLogOptions(ctx, options); err != nil {
		return nil, err
	}
	ctx, stopAuthorization, err := service.logStreamAccessContext(ctx)
	if err != nil {
		return nil, err
	}
	pods, err := service.workloadLogMembership(ctx, options, true)
	if err != nil {
		stopAuthorization()
		return nil, err
	}
	release, err := acquireLogStream(service.scope.OrgID)
	if err != nil {
		stopAuthorization()
		return nil, err
	}
	parent, stopCredential := service.credentialContext(ctx)
	parent, cancel := context.WithCancel(parent)
	stopService := func() bool { return false }
	if service.logStreamContext != nil {
		stopService = context.AfterFunc(service.logStreamContext, cancel)
	}
	cursor, _ := service.workloadCursor(options)
	if cursor.SinceSeconds > 0 {
		options.SinceSeconds, options.TailLines = cursor.SinceSeconds, cursor.TailLines
	}
	options.SinceSeconds, options.TailLines = logBounds(options.SinceSeconds, options.TailLines)
	cursor.SinceSeconds, cursor.TailLines = options.SinceSeconds, options.TailLines
	cursor.Owner = registerWorkloadCheckpointOwner()
	session := &WorkloadLogSession{service: service, options: options, ctx: parent, cancel: func() { stopService(); cancel(); stopCredential(); stopAuthorization() }, release: release, workers: map[string]*workloadSourceWorker{}, cursor: cursor, uncertain: cursor.Uncertain || cursor.Process != workloadStreamProcess, queue: make(chan workloadSourceMessage, 64), statuses: make(map[string]bool)}
	cursor.Process = workloadStreamProcess
	if err := session.reconcile(pods); err != nil {
		session.Close()
		return nil, err
	}
	if len(session.workers) == 0 {
		for _, pod := range session.pods {
			if pod.Reason == "source_limit" {
				session.Close()
				return nil, ErrLogStreamBusy
			}
		}
	}
	return session, nil
}

func (session *WorkloadLogSession) Close() {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.closed = true
	session.once.Do(func() {
		session.cancel()
		releaseWorkloadCheckpointOwner(session.cursor.Owner)
		if !session.streaming {
			for _, worker := range session.workers {
				worker.release()
			}
			session.release()
		}
	})
}

func (service *Service) StreamWorkloadLogs(ctx context.Context, options WorkloadLogStreamOptions, emit func(WorkloadLogStreamEvent) error) error {
	session, err := service.OpenWorkloadLogStream(ctx, options)
	if err != nil {
		return err
	}
	defer session.Close()
	return session.Stream(emit)
}

func workloadAllPodsCapacity(records []kubeindex.Record, selection string, maxFollows int) map[string]string {
	reasons := make(map[string]string, len(records))
	pods, follows := 0, 0
	for _, record := range records {
		containers := record.LogContainers
		if len(containers) == 0 {
			containers = record.Containers
		}
		main := record.MainContainer
		if main == "" {
			for _, container := range containers {
				if !container.Init {
					main = container.Name
					break
				}
			}
		}
		count := 0
		for _, container := range containers {
			if selection == "*" || selection == "" && container.Name == main || selection != "" && selection == container.Name {
				count++
			}
		}
		if count == 0 {
			reasons[record.UID] = "container_unavailable"
			continue
		}
		if pods >= MaxWorkloadLogPods || follows+count > maxFollows {
			reasons[record.UID] = "source_limit"
		}
		if pods < MaxWorkloadLogPods && follows < maxFollows {
			pods++
			follows += min(count, maxFollows-follows)
		}
	}
	return reasons
}

func workloadContainerIncarnation(container kubeindex.ContainerState, previous bool) string {
	incarnation := strconv.Itoa(int(container.RestartCount)) + ":" + container.StartedAt.Format(time.RFC3339Nano)
	if previous {
		incarnation += ":previous:" + container.TerminatedAt.Format(time.RFC3339Nano)
	} else if container.Init && container.CurrentTermination {
		incarnation += ":init_terminated:" + container.TerminatedAt.Format(time.RFC3339Nano) + ":" + strconv.Itoa(int(container.ExitCode)) + ":" + strconv.Itoa(int(container.Signal))
	}
	return incarnation
}

func (session *WorkloadLogSession) finiteSelection() bool {
	if session.options.Previous {
		return true
	}
	if len(session.workers) == 0 {
		return false
	}
	for _, worker := range session.workers {
		if !worker.options.finite {
			return false
		}
	}
	return true
}

func workloadSourceObservation(record kubeindex.Record, name string) string {
	var state kubeindex.ContainerState
	for _, containers := range [][]kubeindex.ContainerState{record.LogContainers, record.Containers} {
		for _, container := range containers {
			if container.Name == name {
				state = container
				break
			}
		}
		if state.Name != "" {
			break
		}
	}
	encoded, _ := json.Marshal(struct {
		Phase, Node, Ready string
		Owners             []kubeindex.OwnerRef
		Container          kubeindex.ContainerState
	}{record.Phase, record.NodeName, record.ReadyStatus, record.Owners, state})
	return logIdentity(string(encoded))
}

// An init's completion changes its finite-read identity, but not its run.
// Transfer only a validated checkpoint for the same scoped Pod/container and
// restart count, with known running start and compatible completion times.
func (session *WorkloadLogSession) initCompletionCheckpoint(record kubeindex.Record, current kubeindex.ContainerState, options PodLogStreamOptions) (string, *workloadSourceWorker, string, error) {
	if options.Previous || !current.Init || !current.StateKnown || !current.CurrentTermination || current.TerminatedAt.IsZero() {
		return "", nil, "", nil
	}
	for id, previous := range session.workers {
		if previous.options.Previous || previous.source.PodUID != record.UID || previous.source.Pod != record.Name || previous.source.Namespace != record.Namespace || previous.source.Container != current.Name {
			continue
		}
		for _, containers := range [][]kubeindex.ContainerState{previous.expected.LogContainers, previous.expected.Containers} {
			for _, old := range containers {
				if old.Name != current.Name || !old.Init || !old.StateKnown || !old.Running || old.CurrentTermination || old.StartedAt.IsZero() || old.RestartCount != current.RestartCount || current.TerminatedAt.Before(old.StartedAt) || !current.StartedAt.IsZero() && !current.StartedAt.Equal(old.StartedAt) {
					continue
				}
				checkpoint := session.cursor.Sources[id]
				if checkpoint == "" {
					continue
				}
				priorOptions := previous.options
				priorOptions.Cursor, priorOptions.SinceSeconds, priorOptions.TailLines = checkpoint, 0, 0
				cursor, err := session.service.logCursor(priorOptions)
				if err != nil {
					return "", nil, "", err
				}
				if !cursor.Quiet && cursor.Timestamp.After(current.TerminatedAt.Add(time.Minute)) {
					continue
				}
				cursor.Target = session.service.logTarget(options)
				encoded, err := json.Marshal(cursor)
				if err != nil {
					return "", nil, "", err
				}
				return id, previous, base64.RawURLEncoding.EncodeToString(encoded), nil
			}
		}
	}
	return "", nil, "", nil
}

func retireInitForCompletion(worker *workloadSourceWorker) {
	if worker.retiredForCompletion {
		return
	}
	if worker.started {
		worker.cancel()
		<-worker.done
	} else {
		worker.release()
	}
	worker.retiredForCompletion, worker.finished, worker.terminal = true, true, true
	worker.source.Admitted, worker.source.Reason = false, "source_limit"
}

func (session *WorkloadLogSession) reconcile(records []kubeindex.Record) error {
	wanted := make(map[string]*workloadSourceWorker)
	success := false
	defer func() {
		if !success {
			for id, worker := range wanted {
				if session.workers[id] != worker {
					worker.release()
				}
			}
		}
	}()
	pods := []WorkloadLogPod{}
	focus := make(map[string]bool)
	for _, uid := range session.options.PodUIDs {
		focus[uid] = true
	}
	podCount := 0
	logReadAdmissions.Lock()
	available := MaxProcessLogReads - logReadAdmissions.total
	logReadAdmissions.Unlock()
	for _, worker := range session.workers {
		if !worker.finished {
			available++
		}
	}
	capacity := workloadAllPodsCapacity(records, session.options.Container, min(MaxWorkloadLogSources, max(0, available)))
	for _, record := range records {
		containers := record.LogContainers
		if len(containers) == 0 {
			containers = record.Containers
		}
		pod := WorkloadLogPod{UID: record.UID, Name: record.Name, Containers: []string{}, InitContainers: []string{}, MainContainer: record.MainContainer, Truncated: record.LogContainersPartial}
		pod.AllPodsReason = capacity[record.UID]
		pod.AllPodsAdmitted = pod.AllPodsReason == ""
		for _, container := range containers {
			if container.Init {
				pod.InitContainers = append(pod.InitContainers, container.Name)
			} else {
				pod.Containers = append(pod.Containers, container.Name)
				if pod.MainContainer == "" {
					pod.MainContainer = container.Name
				}
			}
		}
		selected := len(focus) == 0 || focus[record.UID]
		if !selected {
			pod.Reason = "not_selected"
		}
		if selected {
			pod.Reason = "source_limit"
		}
		if selected && podCount < MaxWorkloadLogPods {
			pod.Reason = "container_unavailable"
			for _, container := range containers {
				if session.options.Container == "" && container.Name != pod.MainContainer || session.options.Container != "" && session.options.Container != "*" && container.Name != session.options.Container {
					continue
				}
				if len(wanted) >= MaxWorkloadLogSources {
					pod.Reason = "source_limit"
					break
				}
				incarnation := workloadContainerIncarnation(container, session.options.Previous)
				id := logIdentity(session.service.workloadLogTarget(session.options) + "\x00" + record.UID + "\x00" + container.Name + "\x00" + incarnation)
				worker := session.workers[id]
				observation := workloadSourceObservation(record, container.Name)
				if worker != nil && worker.finished && !worker.terminal && !session.options.Previous && (!worker.suspended || worker.observation != observation) {
					worker.cancel()
					<-worker.done
					worker = nil
				}
				if worker == nil {
					options := PodLogStreamOptions{Namespace: record.Namespace, Pod: record.Name, Container: container.Name, SinceSeconds: session.options.SinceSeconds, TailLines: session.options.TailLines, Previous: session.options.Previous, Timestamps: session.options.Timestamps, sourceIdentity: id, checkpointEveryLine: true}
					options.finite = !options.Previous && container.Init && container.CurrentTermination
					migratedID, prior, migrated, err := session.initCompletionCheckpoint(record, container, options)
					if err != nil {
						return err
					}
					for oldID, previous := range session.workers {
						if oldID != id && oldID != migratedID && previous.source.PodUID == record.UID && previous.source.Container == container.Name {
							session.uncertain = true
						}
					}
					if prior != nil {
						retireInitForCompletion(prior)
					}
					release, err := acquireLogRead()
					if err != nil {
						// Keep the stopped source/checkpoint as a bounded anchor if
						// another reader won the just-released process slot.
						if prior != nil {
							wanted[migratedID] = prior
						}
						pod.Reason = "source_limit"
						continue
					}
					checkpoint := session.cursor.Sources[id]
					if checkpoint == "" {
						checkpoint = migrated
					}
					if checkpoint != "" {
						options.Cursor, options.SinceSeconds, options.TailLines = checkpoint, 0, 0
						if _, err := session.service.logCursor(options); err != nil {
							release()
							return err
						}
						session.cursor.Sources[id] = checkpoint
					}
					if session.cursor.Sources[id] == "" {
						session.cursor.Sources[id] = session.service.quietLogCursor(options, time.Now().UTC())
					}
					worker = &workloadSourceWorker{source: WorkloadLogSource{SourceID: id, PodUID: record.UID, Pod: record.Name, Namespace: record.Namespace, Container: container.Name, Incarnation: incarnation, Admitted: true}, options: options, release: release, done: make(chan struct{}), expected: record, observation: observation}
				}
				wanted[id] = worker
				pod.Admitted = true
				pod.Reason = ""
				if session.options.Cursor != "" && !worker.started && (!container.StateKnown || container.Running && container.StartedAt.IsZero()) {
					session.uncertain = true
				}
			}
			if pod.Admitted {
				podCount++
			}
		}
		if len(pods) < maxWorkloadListedPods {
			pods = append(pods, pod)
		}
	}
	for id, worker := range session.workers {
		if wanted[id] == nil {
			if worker.started {
				worker.cancel()
				<-worker.done
			} else {
				worker.release()
			}
			delete(session.cursor.Sources, id)
		}
	}
	session.workers, session.pods, session.truncated = wanted, pods, len(records) > len(pods)
	for id := range session.cursor.Sources {
		if wanted[id] == nil {
			delete(session.cursor.Sources, id)
			session.uncertain = true
		}
		for id := range session.failures {
			if wanted[id] == nil {
				delete(session.failures, id)
			}
		}
	}
	success = true
	return nil
}

func (service *Service) validateWorkloadSource(ctx context.Context, worker *workloadSourceWorker) error {
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw map[string]any
	path := "/api/v1/namespaces/" + url.PathEscape(worker.source.Namespace) + "/pods/" + url.PathEscape(worker.source.Pod)
	if err := service.client.GetJSON(check, path, &raw); err != nil {
		if errors.Is(err, ErrNotFound) {
			return errWorkloadSourceGone
		}
		return err
	}
	actual := indexRecord(service.projectResource(ResourceDefinition{ID: "core~v1~pods", Kind: "Pod"}, raw))
	if actual.UID != worker.source.PodUID || actual.Namespace != worker.source.Namespace || actual.Name != worker.source.Pod {
		return errWorkloadSourceGone
	}
	owned := false
	for _, owner := range worker.expected.Owners {
		if owner.Controller && owner.ControllerKnown && workloadOwnedBy(actual, owner.Kind, owner.UID) {
			owned = true
		}
	}
	if !owned {
		return errWorkloadSourceGone
	}
	for _, container := range actual.LogContainers {
		if container.Name != worker.source.Container {
			continue
		}
		incarnation := workloadContainerIncarnation(container, worker.options.Previous)
		if incarnation != worker.source.Incarnation {
			return errWorkloadSourceGone
		}
		if !worker.options.Previous && container.CurrentTermination && !container.Init {
			return errWorkloadSourceGone
		}
		return nil
	}
	return errWorkloadSourceGone
}

func (session *WorkloadLogSession) startWorkers() {
	for id, worker := range session.workers {
		if worker.started || worker.retiredForCompletion {
			continue
		}
		worker.started = true
		ctx, cancel := context.WithCancel(session.ctx)
		worker.cancel = cancel
		go func(id string, worker *workloadSourceWorker) {
			defer close(worker.done)
			defer worker.release()
			err := session.service.validateWorkloadSource(ctx, worker)
			if err == nil {
				err = session.service.readPodLogs(ctx, worker.options, func(event PodLogStreamEvent) error {
					select {
					case session.queue <- workloadSourceMessage{id: id, event: event}:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}, MaxLogStreamDuration, LogStreamIdleTimeout, LogStreamHeartbeat, MaxLogStreamBytes)
			}
			if errors.Is(err, ErrNotFound) {
				err = errWorkloadSourceGone
			}
			select {
			case session.queue <- workloadSourceMessage{id: id, err: err, finished: true}:
			case <-ctx.Done():
			}
		}(id, worker)
	}
}

func (session *WorkloadLogSession) checkpoint() string {
	session.cursor.Process = workloadStreamProcess
	session.cursor.Reference = ""
	session.cursor.Uncertain = false
	if session.cursor.Owner == "" {
		session.cursor.Owner = registerWorkloadCheckpointOwner()
	}
	reference, err := retainWorkloadCheckpoint(session.cursor)
	if err != nil {
		session.uncertain = true
	}
	token := workloadLogCursor{Target: session.cursor.Target, Process: session.cursor.Process, Reference: reference, Uncertain: err != nil, SinceSeconds: session.cursor.SinceSeconds, TailLines: session.cursor.TailLines}
	encoded, _ := json.Marshal(token)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func (session *WorkloadLogSession) membershipChange() (WorkloadLogStreamEvent, bool) {
	list := []WorkloadLogSource{}
	for _, worker := range session.workers {
		list = append(list, worker.source)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SourceID < list[j].SourceID })
	event := WorkloadLogStreamEvent{PodLogStreamEvent: PodLogStreamEvent{Event: "sources"}, Sources: list, Pods: session.pods, Truncated: session.truncated}
	encoded, _ := json.Marshal(event)
	if string(encoded) == session.lastSources {
		return event, false
	}
	session.lastSources = string(encoded)
	return event, true
}

func (session *WorkloadLogSession) Stream(emit func(WorkloadLogStreamEvent) error) (result error) {
	if emit == nil {
		return ErrInvalidArguments
	}
	session.mu.Lock()
	if session.streaming || session.closed {
		session.mu.Unlock()
		return ErrInvalidArguments
	}
	session.streaming = true
	session.mu.Unlock()
	defer func() {
		session.cancel()
		for _, worker := range session.workers {
			if worker.started {
				worker.cancel()
				<-worker.done
			} else {
				worker.release()
			}
		}
		session.release()
		session.Close()
	}()
	start := time.NewTimer(MaxLogStreamDuration)
	defer start.Stop()
	reconcile := time.NewTicker(5 * time.Second)
	defer reconcile.Stop()
	heartbeat := time.NewTicker(LogStreamHeartbeat)
	defer heartbeat.Stop()
	var bytes int
	var sequence uint64
	send := func(event WorkloadLogStreamEvent) error {
		event.WorkloadUID = session.options.UID
		if event.Event != "source_status" {
			event.ReplayUncertain = event.ReplayUncertain || session.uncertain
		}
		raw, _ := json.Marshal(event)
		if len(raw) > 1<<20 {
			return ErrInvalidArguments
		}
		if event.Event != "limit" && event.Event != "end" && bytes+len(raw)+2*maxWorkloadCursorBytes+4096 > MaxLogStreamBytes {
			return errWorkloadByteLimit
		}
		bytes += len(raw) + len(event.Event) + 16
		err := emit(event)
		if err == nil && event.Event != "source_status" {
			session.uncertain = false
		}
		return err
	}
	finish := func(reason string, limited bool) error {
		event := WorkloadLogStreamEvent{PodLogStreamEvent: PodLogStreamEvent{Event: "end", Reason: reason, Cursor: session.checkpoint()}}
		if limited {
			event.Event = "limit"
			if err := send(event); err != nil {
				return err
			}
			event.Event = "end"
		}
		return send(event)
	}
	defer func() {
		if errors.Is(result, errWorkloadByteLimit) {
			result = finish("bytes", true)
		}
	}()
	sources := func() error {
		event, changed := session.membershipChange()
		if changed {
			if err := send(event); err != nil {
				return err
			}
		}
		current := make(map[string]bool)
		for _, pod := range session.pods {
			if pod.Reason != "container_unavailable" {
				continue
			}
			current[pod.UID] = true
			if !session.statuses[pod.UID] {
				if err := send(WorkloadLogStreamEvent{PodLogStreamEvent: PodLogStreamEvent{Event: "source_status", Code: "container_unavailable", Message: "Logs not available: container has not started or is not present."}, PodUID: pod.UID, Pod: pod.Name, Namespace: session.options.Namespace}); err != nil {
					return err
				}
			}
		}
		session.statuses = current
		return nil
	}
	if err := sources(); err != nil {
		return err
	}
	if session.options.Previous && len(session.workers) == 0 {
		return finish("complete", false)
	}
	session.startWorkers()
	for {
		select {
		case <-session.ctx.Done():
			return session.ctx.Err()
		case <-start.C:
			return finish("duration", true)
		case <-heartbeat.C:
			if err := send(WorkloadLogStreamEvent{PodLogStreamEvent: PodLogStreamEvent{Event: "heartbeat", Cursor: session.checkpoint()}}); err != nil {
				return err
			}
		case <-reconcile.C:
			pods, err := session.service.workloadLogMembership(session.ctx, session.options, false)
			if err != nil {
				// Stale or inaccessible ownership never authorizes reopening a source.
				for _, worker := range session.workers {
					if sendErr := send(WorkloadLogStreamEvent{PodLogStreamEvent: PodLogStreamEvent{Event: "source_status", Code: "membership_unavailable", Message: "Logs not available: workload ownership could not be verified."}, PodUID: worker.source.PodUID, Pod: worker.source.Pod, Namespace: worker.source.Namespace, SourceID: worker.source.SourceID}); sendErr != nil {
						return sendErr
					}
				}
				return err
			}
			if err := session.reconcile(pods); err != nil {
				return err
			}
			if err := sources(); err != nil {
				return err
			}
			session.startWorkers()
		case message := <-session.queue:
			worker := session.workers[message.id]
			if worker == nil || worker.retiredForCompletion {
				continue
			}
			event := WorkloadLogStreamEvent{PodLogStreamEvent: message.event, PodUID: worker.source.PodUID, Pod: worker.source.Pod, Namespace: worker.source.Namespace, Incarnation: worker.source.Incarnation, SourceID: message.id}
			event.Container = worker.source.Container
			if message.finished {
				worker.finished = true
				if worker.options.finite {
					worker.terminal = true
				}
				if errors.Is(message.err, errWorkloadSourceGone) {
					worker.terminal = true
				}
				if message.err != nil && !errors.Is(message.err, context.Canceled) && !errors.Is(message.err, errWorkloadSourceGone) {
					detail := DiagnoseError(message.err)
					worker.suspended = !detail.Retryable
					event.Event, event.Code, event.Message = "source_status", detail.Code, detail.Message
					if session.failures == nil {
						session.failures = make(map[string]string)
					}
					if session.failures[message.id] != event.Code {
						if err := send(event); err != nil {
							return err
						}
						session.failures[message.id] = event.Code
					}
				}
				complete := true
				for _, candidate := range session.workers {
					complete = complete && candidate.finished
				}
				if session.finiteSelection() && complete {
					return finish("complete", false)
				}
				continue
			}
			oldCursor := session.cursor.Sources[message.id]
			if event.Cursor != "" {
				session.cursor.Sources[message.id] = event.Cursor
			}
			if event.Event == "end" || event.Event == "heartbeat" {
				if event.ReplayUncertain {
					session.uncertain = true
				}
				continue
			}
			if event.Event == "limit" {
				session.uncertain = session.uncertain || event.ReplayUncertain
				return finish(event.Reason, true)
			}
			if event.Event == "line" {
				delete(session.failures, message.id)
				sequence++
				event.Sequence = sequence
				event.Cursor = ""
				if sequence == 1 || sequence%logCursorCheckpoint == 0 {
					event.Cursor = session.checkpoint()
				}
			}
			if err := send(event); err != nil {
				if oldCursor == "" {
					delete(session.cursor.Sources, message.id)
				} else {
					session.cursor.Sources[message.id] = oldCursor
				}
				return err
			}
		}
	}
}
