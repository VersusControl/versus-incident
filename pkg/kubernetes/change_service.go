package kubernetes

import (
	"context"
	"sync"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type Change = kubechanges.Change
type ChangeFieldChange = kubechanges.FieldChange
type ChangeType = kubechanges.Type
type ChangeQuery = kubechanges.Query

type ChangePage struct {
	Items     []Change          `json:"items"`
	Next      string            `json:"next,omitempty"`
	Truncated bool              `json:"truncated"`
	Partial   []PartialFailure  `json:"partial_failures,omitempty"`
	Gaps      []kubechanges.Gap `json:"gaps"`
	Sync      SyncStatus        `json:"sync"`
}

type changeStoreState struct {
	timelines         map[string]*timelineSnapshot
	timelineManifests map[string]timelineManifest
	inventories       map[kubeindex.Scope]*inventoryCache
	issueAlerts       map[kubeindex.Scope]*issueAlertWindow
	mu                sync.Mutex
	provider          storage.Provider
	evidenceEnabled   bool
	stores            map[kubeindex.Scope]*kubechanges.Store
	captureLocks      map[kubeindex.Scope]*sync.Mutex
	refreshes         map[indexRefreshKey]*indexRefreshCall
	fallbacks         map[kubeindex.Scope]map[*indexRefreshCall]struct{}
	stopped           map[kubeindex.Scope]bool
	captures          map[kubeindex.Scope]*captureLoop
	lifecycle         context.Context
	firstSeen         map[kubeindex.Scope]map[string]time.Time
	journalFailed     map[kubeindex.Scope]bool
	logActive         map[kubeindex.Scope]bool
	logDone           map[kubeindex.Scope]chan struct{}
	logCancel         map[kubeindex.Scope]context.CancelFunc
}

type indexRefreshKey struct {
	scope      kubeindex.Scope
	background bool
	cached     bool
}

type indexRefreshCall struct {
	done      chan struct{}
	requested map[string]struct{}
	ctx       context.Context
	cancel    context.CancelFunc
}

func newChangeStoreState() *changeStoreState {
	return &changeStoreState{
		timelines:         make(map[string]*timelineSnapshot),
		timelineManifests: make(map[string]timelineManifest),
		inventories:       make(map[kubeindex.Scope]*inventoryCache),
		stores:            make(map[kubeindex.Scope]*kubechanges.Store),
		captureLocks:      make(map[kubeindex.Scope]*sync.Mutex),
		refreshes:         make(map[indexRefreshKey]*indexRefreshCall),
		fallbacks:         make(map[kubeindex.Scope]map[*indexRefreshCall]struct{}),
		stopped:           make(map[kubeindex.Scope]bool),
		captures:          make(map[kubeindex.Scope]*captureLoop),
		firstSeen:         map[kubeindex.Scope]map[string]time.Time{},
		journalFailed:     map[kubeindex.Scope]bool{},
		logActive:         map[kubeindex.Scope]bool{},
		logDone:           map[kubeindex.Scope]chan struct{}{},
		logCancel:         map[kubeindex.Scope]context.CancelFunc{},
	}
}

// SetChangeStorage installs the shared persistence provider for scoped timelines.
func (service *Service) SetChangeStorage(provider storage.Provider) {
	if service == nil || service.changes == nil {
		return
	}
	service.changes.mu.Lock()
	if service.changes.provider == provider {
		service.changes.mu.Unlock()
		return
	}
	service.changes.provider = provider
	if service.inventoryRestore != nil {
		service.inventoryRestore.validated.Store(false)
	}
	service.changes.stores = make(map[kubeindex.Scope]*kubechanges.Store)
	service.changes.captureLocks = make(map[kubeindex.Scope]*sync.Mutex)
	service.changes.journalFailed = make(map[kubeindex.Scope]bool)
	service.changes.mu.Unlock()
}

func (state *changeStoreState) captureLock(scope kubeindex.Scope) *sync.Mutex {
	state.mu.Lock()
	defer state.mu.Unlock()
	lock := state.captureLocks[scope]
	if lock == nil {
		lock = &sync.Mutex{}
		state.captureLocks[scope] = lock
	}
	return lock
}

func (service *Service) changeStore() *kubechanges.Store {
	if service == nil || service.changes == nil {
		return nil
	}
	scope := kubeindex.Scope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, CredentialID: service.scope.CredentialID}
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	if existing := service.changes.stores[scope]; existing != nil {
		return existing
	}
	created := kubechanges.NewStore(service.changes.provider, scope)
	if service.changes.evidenceEnabled {
		if provider, ok := service.changes.provider.(storage.EvidenceStore); ok && provider.EvidencePolicy().Mode != "unsupported" {
			// A restarted process must find the same retained excerpts after
			// same-principal recovery. Live indexes and cancellation still
			// use the runtime epoch; storage retains full credential binding.
			persistentScope := scope
			if service.client != nil {
				persistentScope.CredentialID = service.persistentCredentialID
			}
			created = kubechanges.NewCaptureState(service.changes.provider, persistentScope)
		}
	}
	if created != nil {
		service.changes.stores[scope] = created
	}
	return created
}

// Changes returns the bounded timeline for the service's org/cluster scope.
func (service *Service) Changes(ctx context.Context, query ChangeQuery) (ChangePage, error) {
	if service.evidenceStore() != nil {
		return service.evidenceChanges(ctx, query)
	}
	if err := service.validateCredential(ctx); err != nil {
		return ChangePage{}, err
	}
	if service == nil {
		return ChangePage{}, ErrInvalidArguments
	}
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return ChangePage{}, err
	}
	query.Limit = normalizePageLimit(query.Limit)
	if query.RootUID != "" {
		if !safeSegment(query.RootUID) || len(query.RootUID) > 256 {
			return ChangePage{}, ErrInvalidArguments
		}
		snapshot, _, err := service.CachedIndexSnapshot(ctx, graphIndexKinds...)
		if err != nil {
			return ChangePage{}, err
		}
		query.UIDs = map[string]bool{query.RootUID: true}
		for _, record := range snapshot.Records {
			if record.UID == query.RootUID {
				related := directedRelated(snapshot, record.Kind, record.Namespace, record.Name)
				for _, node := range related.Nodes {
					query.UIDs[node.ID] = true
				}
				break
			}
		}
	}
	store := service.changeStore()
	if store == nil {
		return ChangePage{Items: []Change{}, Gaps: []kubechanges.Gap{}, Sync: service.withSyncProgress(SyncStatus{State: "syncing"})}, nil
	}
	if len(service.readOrgs) > 1 {
		var err error
		service.changes.mu.Lock()
		provider := service.changes.provider
		service.changes.mu.Unlock()
		store, err = kubechanges.NewView(provider, kubeindex.Scope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, CredentialID: service.scope.CredentialID}, service.readOrgs[1:])
		if err != nil {
			return ChangePage{}, err
		}
	}
	var refreshed kubeindex.Status
	var refreshErr error
	if service.client == nil {
		refreshed = kubeindex.Status{State: "unavailable", Partial: true}
	} else {
		_, refreshed, refreshErr = service.CachedIndexSnapshot(ctx, "Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob")
	}
	page, err := store.Query(query, time.Now().UTC())
	if err != nil {
		return ChangePage{}, err
	}
	if err := service.validateCredential(ctx); err != nil {
		return ChangePage{}, err
	}
	if refreshed.State == "" {
		refreshed.State = "unavailable"
		refreshed.Partial = true
	}
	if page.Items == nil {
		page.Items = []Change{}
	}
	if page.Gaps == nil {
		page.Gaps = []kubechanges.Gap{}
	}
	result := ChangePage{Items: page.Items, Next: page.Next, Gaps: page.Gaps, Sync: service.withSyncProgress(SyncStatus{State: refreshed.State, AgeSeconds: refreshed.AgeSeconds, Partial: refreshed.Partial || refreshErr != nil, Pending: refreshed.Pending, PendingKinds: refreshed.PendingKinds})}
	if refreshErr != nil {
		result.Partial = append(result.Partial, PartialFailure{ResourceID: "kubernetes.index", Class: errorClass(refreshErr)})
	}
	return result, nil
}
