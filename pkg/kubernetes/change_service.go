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
	mu           sync.Mutex
	provider     storage.Provider
	stores       map[kubeindex.Scope]*kubechanges.Store
	captureLocks map[kubeindex.Scope]*sync.Mutex
	refreshes    map[kubeindex.Scope]*indexRefreshCall
}

type indexRefreshCall struct {
	done      chan struct{}
	requested map[string]struct{}
}

func newChangeStoreState() *changeStoreState {
	return &changeStoreState{
		stores:       make(map[kubeindex.Scope]*kubechanges.Store),
		captureLocks: make(map[kubeindex.Scope]*sync.Mutex),
		refreshes:    make(map[kubeindex.Scope]*indexRefreshCall),
	}
}

// SetChangeStorage installs the shared persistence provider for scoped timelines.
func (service *Service) SetChangeStorage(provider storage.Provider) {
	if service == nil || service.changes == nil {
		return
	}
	service.changes.mu.Lock()
	service.changes.provider = provider
	service.changes.stores = make(map[kubeindex.Scope]*kubechanges.Store)
	service.changes.captureLocks = make(map[kubeindex.Scope]*sync.Mutex)
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
	if created != nil {
		service.changes.stores[scope] = created
	}
	return created
}

// Changes returns the bounded timeline for the service's org/cluster scope.
func (service *Service) Changes(ctx context.Context, query ChangeQuery) (ChangePage, error) {
	if service == nil {
		return ChangePage{}, ErrInvalidArguments
	}
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return ChangePage{}, err
	}
	query.Limit = normalizePageLimit(query.Limit)
	store := service.changeStore()
	if store == nil {
		return ChangePage{Items: []Change{}, Gaps: []kubechanges.Gap{}, Sync: SyncStatus{State: "warming", Partial: true}}, nil
	}
	var refreshed kubeindex.Status
	var refreshErr error
	if service.client == nil {
		refreshed = kubeindex.Status{State: "unavailable", Partial: true}
	} else {
		_, refreshed, refreshErr = service.IndexSnapshot(ctx, "Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob")
	}
	page, err := store.Query(query, time.Now().UTC())
	if err != nil {
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
	result := ChangePage{Items: page.Items, Next: page.Next, Truncated: page.Next != "", Gaps: page.Gaps, Sync: SyncStatus{State: refreshed.State, AgeSeconds: refreshed.AgeSeconds, Partial: refreshed.Partial || refreshErr != nil}}
	if refreshErr != nil {
		result.Partial = append(result.Partial, PartialFailure{ResourceID: "kubernetes.index", Class: errorClass(refreshErr)})
	}
	if service.client != nil && service.indexes != nil {
		if index, indexErr := service.indexes.ForScope(kubeindex.Scope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, CredentialID: service.scope.CredentialID}); indexErr == nil {
			status := index.Status(time.Now().UTC())
			result.Sync = SyncStatus{State: status.State, AgeSeconds: status.AgeSeconds, Partial: status.Partial}
		}
	}
	return result, nil
}
