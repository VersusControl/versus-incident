package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"github.com/gofiber/fiber/v2"
)

// stallHangGuard is how long the test waits for an answer before it calls the
// wait a hang. A passing run never comes near it: the longest wait a request
// makes is the registry's own resolve bound.
const stallHangGuard = 2 * time.Minute

// requestDeadline is a request's deadline that passes on an event instead of a
// timer: when the stalled database holds a call made for the request, or when
// the request is in flight on a replica whose database connection holds a call
// that has no context (a commit holding the credential lock the request needs).
type requestDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

type requestDeadlineKey struct{}

func (deadline *requestDeadline) Done() <-chan struct{} { return deadline.done }

func (deadline *requestDeadline) Err() error {
	select {
	case <-deadline.done:
		return context.DeadlineExceeded
	default:
		return deadline.Context.Err()
	}
}

func (deadline *requestDeadline) Value(key any) any {
	if key == (requestDeadlineKey{}) {
		return deadline
	}
	return deadline.Context.Value(key)
}

func (deadline *requestDeadline) pass() { deadline.once.Do(func() { close(deadline.done) }) }

// databaseStall is the shared history database: whether it is stalled, and
// the moment it comes back.
type databaseStall struct {
	stalled atomic.Bool
	thaw    chan struct{}
	// backgroundHeld receives when the stalled database holds a call made for
	// no request: a collection's own commit, such as a lease claim.
	backgroundHeld chan struct{}
}

// stallingStore is one replica's connection to the shared database. While the
// database is stalled, a call with a context waits until that context ends and
// fails with its error, as the Postgres provider does; a call without one waits
// for the database to come back. It tracks the replica's requests in flight, so
// a request's deadline passes on the replica's own events, and it reports when
// the replica reads the inventory record, so a test knows a replica has seen
// the lease another one holds.
type stallingStore struct {
	storage.Provider
	cas           storage.BlobCAS
	evidence      storage.EvidenceStore
	stall         *databaseStall
	inventoryRead chan struct{}

	mu        sync.Mutex
	inFlight  map[*requestDeadline]bool
	heldNoCtx int
	held      chan struct{}
}

func (store *stallingStore) begin(parent context.Context) *requestDeadline {
	deadline := &requestDeadline{Context: parent, done: make(chan struct{})}
	store.mu.Lock()
	store.inFlight[deadline] = true
	if store.heldNoCtx > 0 {
		deadline.pass()
	}
	store.mu.Unlock()
	return deadline
}

func (store *stallingStore) end(deadline *requestDeadline) {
	store.mu.Lock()
	delete(store.inFlight, deadline)
	store.mu.Unlock()
}

// holdWithoutContext blocks a call that has no context until the database
// comes back. Every request in flight on this replica runs out of time
// meanwhile: it may need a lock the call's caller holds.
func (store *stallingStore) holdWithoutContext() {
	if !store.stall.stalled.Load() {
		return
	}
	store.mu.Lock()
	store.heldNoCtx++
	for deadline := range store.inFlight {
		deadline.pass()
	}
	store.mu.Unlock()
	select {
	case store.held <- struct{}{}:
	default:
	}
	<-store.stall.thaw
	store.mu.Lock()
	store.heldNoCtx--
	store.mu.Unlock()
}

// hold blocks a call with a context until that context ends, and fails it
// with the context's error as the Postgres provider does. A call made for a
// request passes that request's deadline.
func (store *stallingStore) hold(ctx context.Context, operation string) error {
	if !store.stall.stalled.Load() {
		return nil
	}
	if deadline, ok := ctx.Value(requestDeadlineKey{}).(*requestDeadline); ok {
		deadline.pass()
	} else {
		select {
		case store.stall.backgroundHeld <- struct{}{}:
		default:
		}
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("storage: %s: %w", operation, ctx.Err())
	case <-store.stall.thaw:
		return nil
	}
}

func (store *stallingStore) noteRead(name string) {
	if strings.HasPrefix(name, "kubernetes/inventory/") {
		select {
		case store.inventoryRead <- struct{}{}:
		default:
		}
	}
}

func (store *stallingStore) ReadBlob(name string) ([]byte, error) {
	store.holdWithoutContext()
	store.noteRead(name)
	return store.Provider.ReadBlob(name)
}

func (store *stallingStore) WriteBlob(name string, data []byte) error {
	store.holdWithoutContext()
	return store.Provider.WriteBlob(name, data)
}

func (store *stallingStore) ListBlobs(prefix string) ([]storage.Blob, error) {
	store.holdWithoutContext()
	return store.Provider.ListBlobs(prefix)
}

func (store *stallingStore) ReadBlobContext(ctx context.Context, name string) ([]byte, error) {
	if err := store.hold(ctx, "read blob"); err != nil {
		return nil, err
	}
	store.noteRead(name)
	return store.Provider.ReadBlob(name)
}

func (store *stallingStore) WriteBlobContext(ctx context.Context, name string, data []byte) error {
	if err := store.hold(ctx, "write blob"); err != nil {
		return err
	}
	return store.Provider.WriteBlob(name, data)
}

func (store *stallingStore) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	store.holdWithoutContext()
	return store.cas.CompareAndSwapBlob(name, expected, replacement)
}

func (store *stallingStore) CompareAndSwapBlobContext(ctx context.Context, name string, expected, replacement []byte) (bool, error) {
	if err := store.hold(ctx, "swap blob"); err != nil {
		return false, err
	}
	return store.cas.CompareAndSwapBlob(name, expected, replacement)
}

func (store *stallingStore) SetEvidenceAuthority(ctx context.Context, scope storage.EvidenceScope, principal string, authority map[string]storage.EvidenceAuthority) error {
	if err := store.hold(ctx, "set evidence authority"); err != nil {
		return err
	}
	return store.evidence.SetEvidenceAuthority(ctx, scope, principal, authority)
}

func (store *stallingStore) EvidencePolicy() storage.EvidencePolicy {
	return store.evidence.EvidencePolicy()
}

func (store *stallingStore) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	if err := store.hold(ctx, "commit evidence"); err != nil {
		return err
	}
	return store.evidence.CommitEvidence(ctx, scope, batch)
}

func (store *stallingStore) QueryEvidence(ctx context.Context, scope storage.EvidenceScope, query storage.EvidenceQuery) (storage.EvidencePage, error) {
	if err := store.hold(ctx, "query evidence"); err != nil {
		return storage.EvidencePage{}, err
	}
	return store.evidence.QueryEvidence(ctx, scope, query)
}

func (store *stallingStore) EvidenceCheckpoint(ctx context.Context, scope storage.EvidenceScope) (storage.EvidenceCheckpoint, error) {
	if err := store.hold(ctx, "read evidence checkpoint"); err != nil {
		return storage.EvidenceCheckpoint{}, err
	}
	return store.evidence.EvidenceCheckpoint(ctx, scope)
}

func (store *stallingStore) DeleteEvidence(ctx context.Context, scope storage.EvidenceScope) error {
	if err := store.hold(ctx, "delete evidence"); err != nil {
		return err
	}
	return store.evidence.DeleteEvidence(ctx, scope)
}

// stalledReplica is one Versus replica on the shared database and cluster.
type stalledReplica struct {
	name  string
	app   *fiber.App
	store *stallingStore
}

// stallAnswer is what one live view answered during the stall.
type stallAnswer struct {
	replica, path string
	status        int
	code, action  string
	retryAfter    string
	err           error
}

// During a database stall no live view blames the Kubernetes API server, on
// any replica role: the inventory lease owner, a peer that follows it, and a
// replica that collects nothing. Each first inventory is still pending, so no
// cache can answer. A view answers from memory, or 503 with Retry-After: named
// for the history store when its deadline ran out in its own store call, and
// the neutral server_timeout when it ran out anywhere else. A collection
// commit held in the stalled database holds no lock, so the views answer the
// same while it waits. No view answers 504 request_timeout, sends the
// operator to connectors.kubernetes.timeout, or blames the credential.
func TestLiveViewsDuringADatabaseStallNeverBlameTheCluster(t *testing.T) {
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
	if err != nil {
		t.Fatal(err)
	}
	// Every inventory list stays pending, so the first generation is never
	// published and every cache stays cold. Identity probes still answer.
	releaseLists := make(chan struct{})
	listing := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		limit := request.URL.Query().Get("limit")
		if request.Method == http.MethodGet && limit != "" && limit != "1" {
			select {
			case listing <- struct{}{}:
			default:
			}
			select {
			case <-releaseLists:
			case <-request.Context().Done():
				return
			}
		}
		fixture.ServeHTTP(writer, request)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(releaseLists) })

	memory := storage.NewMemory()
	t.Cleanup(func() { _ = memory.Close() })
	stall := &databaseStall{thaw: make(chan struct{}), backgroundHeld: make(chan struct{}, 1)}
	// Cleanups run last-in first-out: the database comes back before any
	// replica is closed, so no replica's shutdown waits on it.
	var thaw sync.Once
	thawDatabase := func() { thaw.Do(func() { close(stall.thaw) }) }

	newReplica := func(name string, worker bool) *stalledReplica {
		client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true, Credentials: &flippedCredential{}})
		if err != nil {
			t.Fatal(err)
		}
		store := &stallingStore{Provider: memory, cas: memory.(storage.BlobCAS), evidence: memory.(storage.EvidenceStore), stall: stall, inventoryRead: make(chan struct{}, 1), inFlight: map[*requestDeadline]bool{}, held: make(chan struct{}, 1)}
		service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: "stalled"}, time.Hour)
		service.SetChangeStorage(store)
		registry := kubernetes.NewServiceRegistry(service)
		t.Cleanup(registry.Close)
		if worker {
			registry.StartCapture(t.Context())
		}
		app := fiber.New(fiber.Config{Immutable: true})
		app.Use(func(ctx *fiber.Ctx) error {
			middleware.MarkAuthorized(ctx)
			middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
			deadline := store.begin(ctx.UserContext())
			defer store.end(deadline)
			ctx.SetUserContext(deadline)
			return ctx.Next()
		})
		NewKubernetesAdminControllerWithRegistry(registry).Register(app.Group("/api"))
		return &stalledReplica{name: name, app: app, store: store}
	}
	waitEvent := func(ch <-chan struct{}, what string) {
		t.Helper()
		timer := time.NewTimer(stallHangGuard)
		defer timer.Stop()
		select {
		case <-ch:
		case <-timer.C:
			t.Fatalf("hung waiting for %s", what)
		}
	}

	owner := newReplica("lease owner", true)
	// The owner lists only after it has claimed the inventory lease.
	waitEvent(listing, "the lease owner's first inventory list")
	// The peer follows: its first read of the inventory record already names
	// the owner's lease.
	peer := newReplica("peer", true)
	waitEvent(peer.store.inventoryRead, "the peer's read of the inventory lease")
	idle := newReplica("no worker", false)
	t.Cleanup(thawDatabase)

	stall.stalled.Store(true)

	paths := []string{"/api/admin/kubernetes/workloads", "/api/admin/kubernetes/usage", "/api/admin/kubernetes/sync"}
	replicas := []*stalledReplica{owner, peer, idle}
	storeNamed := 0
	ask := func(phase string) {
		t.Helper()
		answers := make(chan stallAnswer)
		for _, replica := range replicas {
			for _, path := range paths {
				go func() {
					answer := stallAnswer{replica: replica.name, path: path}
					response, err := replica.app.Test(httptest.NewRequest(http.MethodGet, path, nil), -1)
					if err != nil {
						answer.err = err
						answers <- answer
						return
					}
					defer response.Body.Close()
					var body struct {
						Code   string `json:"code"`
						Action string `json:"action"`
					}
					answer.status, answer.retryAfter = response.StatusCode, response.Header.Get(fiber.HeaderRetryAfter)
					answer.err = json.NewDecoder(response.Body).Decode(&body)
					answer.code, answer.action = body.Code, body.Action
					answers <- answer
				}()
			}
		}
		timer := time.NewTimer(stallHangGuard)
		defer timer.Stop()
		for range len(replicas) * len(paths) {
			var answer stallAnswer
			select {
			case answer = <-answers:
			case <-timer.C:
				t.Fatalf("%s: a live view hung during the database stall instead of answering", phase)
			}
			if answer.err != nil {
				t.Errorf("%s: %s %s: %v", phase, answer.replica, answer.path, answer.err)
				continue
			}
			t.Logf("%s: %s %s: %d %s retry-after=%q", phase, answer.replica, answer.path, answer.status, answer.code, answer.retryAfter)
			if answer.code == "request_timeout" || answer.status == fiber.StatusGatewayTimeout || strings.Contains(answer.action, "connectors.kubernetes") {
				t.Errorf("%s: %s %s: the stalled database was blamed on the Kubernetes API server: %d %s %q", phase, answer.replica, answer.path, answer.status, answer.code, answer.action)
				continue
			}
			if answer.code == "credential_unavailable" || answer.code == "cluster_authentication_failed" {
				t.Errorf("%s: %s %s: the stalled database was blamed on the credential: %d %s", phase, answer.replica, answer.path, answer.status, answer.code)
				continue
			}
			switch answer.status {
			case fiber.StatusOK:
			case fiber.StatusServiceUnavailable:
				switch {
				case answer.retryAfter == "":
					t.Errorf("%s: %s %s: 503 %s without Retry-After", phase, answer.replica, answer.path, answer.code)
				case answer.code == "history_store_timeout" || answer.code == "history_store_unavailable":
					storeNamed++
				case answer.code == "server_timeout":
					// The deadline passed outside any call of the view's own
					// (a lock, the resolve gate): unknown is shown as unknown.
				default:
					t.Errorf("%s: %s %s: 503 %s, want history_store_timeout, history_store_unavailable or server_timeout", phase, answer.replica, answer.path, answer.code)
				}
			default:
				t.Errorf("%s: %s %s: status %d %s, want 200 from memory or 503 with Retry-After", phase, answer.replica, answer.path, answer.status, answer.code)
			}
		}
	}
	// First the deadline passes inside each view's own store call.
	ask("own store call")
	// Then a collection's own commit is held in a store call by the stalled
	// database. The commit holds no lock while it waits, so every view still
	// answers, and none blames the cluster or the credential.
	waitEvent(stall.backgroundHeld, "a collection commit held in the stalled database")
	ask("while a commit is held in the store")
	if storeNamed == 0 {
		t.Errorf("no live view was named for the stalled database; the stall was not exercised")
	}
}
