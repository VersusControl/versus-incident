package kubernetes

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// heldStore holds every write and compare-and-swap inside the store until
// the call's context ends or the test releases it. held receives once per
// call that reached the store. With ignoreCancel the call waits for release
// alone and then writes, as a store that does not stop mid-write does.
type heldStore struct {
	storage.Provider
	held         chan struct{}
	release      chan struct{}
	ignoreCancel bool
	// disarmed lets calls through untouched, while a fixture is set up.
	disarmed atomic.Bool
}

func newHeldStore(base storage.Provider) *heldStore {
	return &heldStore{Provider: base, held: make(chan struct{}, 16), release: make(chan struct{})}
}

func (store *heldStore) hold(ctx context.Context) error {
	if store.disarmed.Load() {
		return nil
	}
	store.held <- struct{}{}
	if store.ignoreCancel {
		<-store.release
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.release:
		return nil
	}
}

func (store *heldStore) ReadBlobContext(ctx context.Context, name string) ([]byte, error) {
	return storage.ReadBlobContext(ctx, store.Provider, name)
}

func (store *heldStore) WriteBlobContext(ctx context.Context, name string, data []byte) error {
	if err := store.hold(ctx); err != nil {
		return err
	}
	if store.ignoreCancel {
		return store.Provider.WriteBlob(name, data)
	}
	return storage.WriteBlobContext(ctx, store.Provider, name, data)
}

func (store *heldStore) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return store.CompareAndSwapBlobContext(context.Background(), name, expected, replacement)
}

func (store *heldStore) CompareAndSwapBlobContext(ctx context.Context, name string, expected, replacement []byte) (bool, error) {
	if err := store.hold(ctx); err != nil {
		return false, err
	}
	cas, ok := store.Provider.(storage.BlobCAS)
	if !ok {
		return false, errors.New("store has no compare-and-swap")
	}
	if store.ignoreCancel {
		return cas.CompareAndSwapBlob(name, expected, replacement)
	}
	return storage.CompareAndSwapBlobContext(ctx, cas, name, expected, replacement)
}

// commitFenceStores are the stores a commit is fenced on: memory and file
// always, and Postgres when the harness provides an isolated database.
func commitFenceStores(t *testing.T) map[string]storage.Provider {
	t.Helper()
	stores := map[string]storage.Provider{"memory": storage.NewMemory()}
	file, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	stores["file"] = file
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		postgres, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		if closer, ok := postgres.(interface{ Close() error }); ok {
			t.Cleanup(func() { _ = closer.Close() })
		}
		stores["postgres"] = postgres
	}
	return stores
}

func staticCommitService(t *testing.T) *Service {
	t.Helper()
	credentials, err := staticTokenCredential("commit-fence-token")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{Endpoint: "http://127.0.0.1:12345", Credentials: credentials, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "configured"}, 0)
	if err := service.validateCredential(t.Context()); err != nil {
		t.Fatal(err)
	}
	return service
}

// retireAsync retires the service's epoch the way a rotation does and closes
// the returned channel once retirement returned.
func retireAsync(service *Service) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.identity.mu.Lock()
		service.retireLocked(false)
		service.identity.mu.Unlock()
	}()
	return done
}

// Retiring the epoch while a commit is held inside a store call cancels the
// commit rather than waiting for it: retirement returns while the store call
// is still held, the commit reports a conflict, and no write lands after the
// retirement. Run over 100 rounds on every store the fence covers.
func TestRetirementCancelsACommitHeldInTheStore(t *testing.T) {
	for name, base := range commitFenceStores(t) {
		t.Run(name, func(t *testing.T) {
			landed := 0
			for round := 0; round < 100; round++ {
				service := staticCommitService(t)
				store := newHeldStore(base)
				key := "kubernetes/commit-fence-test/" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(round)
				result := make(chan error, 1)
				go func() {
					result <- service.commitCredential(t.Context(), func(ctx context.Context) error {
						return storage.WriteBlobContext(ctx, store, key, []byte(`{"round":true}`))
					})
				}()
				waitFor(t, store.held, "the commit to reach the store")
				waitFor(t, retireAsync(service), "retirement while a commit is held in the store")
				err := waitFor(t, result, "the cancelled commit to return")
				var conflict commitConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("round %d: a commit cancelled by retirement answered %v, want a commit conflict", round, err)
				}
				if data, readErr := base.ReadBlob(key); readErr == nil && len(data) > 0 {
					landed++
				}
			}
			t.Logf("%s: %d of 100 writes landed after retirement", name, landed)
			if landed != 0 {
				t.Fatalf("%s: %d writes landed after retirement", name, landed)
			}
		})
	}
}

// A store that does not stop mid-write finishes its write after retirement;
// the commit still answers a conflict, so the caller reads again instead of
// taking the write as admitted under a live credential.
func TestCommitThatOutlivesItsEpochReportsAConflict(t *testing.T) {
	service := staticCommitService(t)
	store := newHeldStore(storage.NewMemory())
	store.ignoreCancel = true
	result := make(chan error, 1)
	go func() {
		result <- service.commitCredential(t.Context(), func(ctx context.Context) error {
			return storage.WriteBlobContext(ctx, store, "kubernetes/commit-fence-test/late", []byte(`{}`))
		})
	}()
	waitFor(t, store.held, "the commit to reach the store")
	waitFor(t, retireAsync(service), "retirement while a commit is held in the store")
	close(store.release)
	var conflict commitConflictError
	if err := waitFor(t, result, "the late commit to return"); !errors.As(err, &conflict) {
		t.Fatalf("a write that finished after retirement answered %v, want a commit conflict", err)
	}
}

// A lease renewal cancelled by retirement is a commit conflict, not a lost
// lease, and the release that follows never touches the lease another holder
// took meanwhile.
func TestCancelledLeaseRenewalLeavesAnotherHoldersLeaseAlone(t *testing.T) {
	service := staticCommitService(t)
	base := storage.NewMemory()
	store := newHeldStore(base)
	leases := kubechanges.NewStore(store, index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "configured"})
	now := time.Now().UTC()
	// The first holder claims its lease directly; only the renewal is held.
	direct := kubechanges.NewStore(base, index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "configured"})
	if err := direct.ClaimLease("first", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- service.commitCredential(t.Context(), func(ctx context.Context) error {
			return leases.RenewLeaseContext(ctx, "first", now.Add(time.Second), time.Minute)
		})
	}()
	waitFor(t, store.held, "the renewal to reach the store")
	waitFor(t, retireAsync(service), "retirement while a renewal is held in the store")
	err := waitFor(t, result, "the cancelled renewal to return")
	var conflict commitConflictError
	if !errors.As(err, &conflict) || errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatalf("a renewal cancelled by retirement answered %v, want a commit conflict and not a lost lease", err)
	}
	// The first lease lapses and another holder takes it; the first holder's
	// release afterwards must leave it in place.
	later := now.Add(3 * time.Minute)
	if err := direct.ClaimLease("second", later, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := direct.ReleaseLease("first"); err != nil {
		t.Fatal(err)
	}
	if err := direct.CheckCaptureLease("second"); err != nil && !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatal(err)
	}
	if err := direct.ClaimLease("third", later.Add(time.Second), time.Minute); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatalf("the second holder's lease was released or overwritten: a third holder claimed it (%v)", err)
	}
}

// Readers never wait on a commit's store I/O: while a commit is stalled
// inside the store, request admission and the credential re-check a live log
// stream runs both answer, and the re-check passes with the credential
// unchanged. Retiring the credential still fails the re-check, so a real
// credential change still ends the stream.
func TestReadersAnswerWhileACommitIsStalledInTheStore(t *testing.T) {
	service := staticCommitService(t)
	store := newHeldStore(storage.NewMemory())
	result := make(chan error, 1)
	go func() {
		result <- service.commitCredential(t.Context(), func(ctx context.Context) error {
			return storage.WriteBlobContext(ctx, store, "kubernetes/commit-fence-test/stalled", []byte(`{}`))
		})
	}()
	waitFor(t, store.held, "the commit to stall in the store")

	answered := make(chan error, 2)
	go func() {
		_, _, _, err := service.acceptCredentialForRequest(t.Context(), "Bearer commit-fence-token")
		answered <- err
	}()
	go func() { answered <- service.validateCredential(t.Context()) }()
	for range 2 {
		if err := waitFor(t, answered, "a reader to answer while the commit is stalled"); err != nil {
			t.Fatalf("a reader failed while a commit stalled with the credential unchanged: %v", err)
		}
	}

	waitFor(t, retireAsync(service), "retirement while the commit is stalled")
	if err := service.validateCredential(t.Context()); err == nil {
		t.Fatal("the credential re-check passed after the credential was retired")
	}
	var conflict commitConflictError
	if err := waitFor(t, result, "the stalled commit to end"); !errors.As(err, &conflict) {
		t.Fatalf("the stalled commit answered %v, want a commit conflict", err)
	}
}

// tokenFileFixture is a token file source with its pace and clock seams set:
// pauses return at once and are counted, and the clock is the test's.
func tokenFileFixture(t *testing.T, token string) (*tokenFileSource, string, *time.Time, *int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	pauses := 0
	source := &tokenFileSource{path: path, now: func() time.Time { return now }, wait: func(ctx context.Context, _ time.Duration) error {
		pauses++
		return ctx.Err()
	}}
	return source, path, &now, &pauses
}

// A token file replaced in place reads as missing or empty for a moment. Over
// 1,000 such swaps no read becomes a credential fault: the file is read again,
// and the last good token stands in while it is recent.
func TestTokenFileSwapsAreNeverACredentialFault(t *testing.T) {
	source, path, _, pauses := tokenFileFixture(t, "first-token")
	if _, err := source.Authorization(t.Context()); err != nil {
		t.Fatal(err)
	}
	for swap := 0; swap < 1000; swap++ {
		token := "token-" + strconv.Itoa(swap)
		var err error
		if swap%2 == 0 {
			err = os.Remove(path)
		} else {
			err = os.WriteFile(path, nil, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		// Mid-swap: every read is answered.
		header, readErr := source.Authorization(t.Context())
		if readErr != nil || header == "" {
			t.Fatalf("swap %d: a token file mid-replacement was a credential fault: %v", swap, readErr)
		}
		if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
		if header, readErr = source.Authorization(t.Context()); readErr != nil || header != "Bearer "+token {
			t.Fatalf("swap %d: the new token was not read: %q %v", swap, header, readErr)
		}
	}
	if *pauses != 1000*len(tokenFileRetryPauses) {
		t.Fatalf("re-reads paced %d times, want %d", *pauses, 1000*len(tokenFileRetryPauses))
	}
}

// The last good token bridges a swap only: a removed file is a fault once
// the token is older than the grace, a rejected token is never served from
// memory, and an expired JWT is never served from memory.
func TestTokenFileLastGoodTokenHasLimits(t *testing.T) {
	t.Run("removed file after the grace", func(t *testing.T) {
		source, path, now, _ := tokenFileFixture(t, "kept-token")
		if _, err := source.Authorization(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(tokenFileGrace + time.Second)
		if _, err := source.Authorization(t.Context()); !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("a token file removed for longer than the grace answered %v, want the credential fault", err)
		}
	})
	t.Run("rejected token", func(t *testing.T) {
		source, path, _, _ := tokenFileFixture(t, "rejected-token")
		header, err := source.Authorization(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if !source.rejectBearer(header) {
			t.Fatal("a rejection with the file missing did not retire the epoch")
		}
		if got, err := source.Authorization(t.Context()); err == nil || got == header {
			t.Fatalf("a rejected token was served again: %q %v", got, err)
		}
	})
	t.Run("expired token", func(t *testing.T) {
		source, path, now, _ := tokenFileFixture(t, "")
		expires := now.Add(5 * time.Second).Unix()
		token := "eyJhbGciOiJub25lIn0." + base64URL(`{"exp":`+strconv.FormatInt(expires, 10)+`}`) + ".sig"
		if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := source.Authorization(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(6 * time.Second)
		if _, err := source.Authorization(t.Context()); !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("an expired token was served from memory: %v", err)
		}
	})
}

func base64URL(text string) string { return base64.RawURLEncoding.EncodeToString([]byte(text)) }

// A discovery whose deadline fires inside the API group round trip records
// the groups as partial and still answers request_timeout: the API server was
// what the request waited on. A deadline spent waiting on another discovery
// in this server stays server_timeout.
func TestDiscoveryDeadlineIsNamedForWhatItWaitedOn(t *testing.T) {
	holdCore := make(chan struct{})
	coreHeld := make(chan struct{}, 1)
	var holding atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			if holding.Load() {
				coreHeld <- struct{}{}
				<-holdCore
			}
			writeJSON(w, map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			writeJSON(w, map[string]any{"groupVersion": "v1", "resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"list"}}}})
		case "/apis":
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	t.Run("inside the round trip", func(t *testing.T) {
		service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond) // testlint:allow the request deadline under test must fire inside the held group round trip
		defer cancel()
		_, err := service.Discover(ctx)
		if code := DiagnoseError(err).Code; code != "request_timeout" {
			t.Fatalf("a deadline inside the API round trip answered %q (%v), want request_timeout", code, err)
		}
	})
	t.Run("waiting inside this server", func(t *testing.T) {
		service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
		holding.Store(true)
		first := make(chan error, 1)
		go func() { _, err := service.Discover(t.Context()); first <- err }()
		waitFor(t, coreHeld, "the first discovery to hold the core group")
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond) // testlint:allow the request deadline under test must fire while waiting on the shared discovery
		defer cancel()
		_, err := service.Discover(ctx)
		holding.Store(false)
		close(holdCore)
		if code := DiagnoseError(err).Code; code != "server_timeout" {
			t.Fatalf("a deadline spent waiting on another discovery answered %q (%v), want server_timeout", code, err)
		}
		<-first
	})
}

// Saving the alert settings is a commit whose store calls ran without a
// context: retirement now cancels it mid-write too, it answers a conflict,
// and the settings it was writing never land.
func TestRetirementCancelsASettingsSaveHeldInTheStore(t *testing.T) {
	service := staticCommitService(t)
	base := storage.NewMemory()
	store := newHeldStore(base)
	store.disarmed.Store(true)
	service.SetChangeStorage(store)
	store.disarmed.Store(false)
	result := make(chan error, 1)
	go func() { result <- service.SaveIssueAlertSettings(t.Context(), true, "critical", "admin") }()
	waitFor(t, store.held, "the settings save to reach the store")
	waitFor(t, retireAsync(service), "retirement while the settings save is held in the store")
	var conflict commitConflictError
	if err := waitFor(t, result, "the cancelled settings save to return"); !errors.As(err, &conflict) {
		t.Fatalf("a settings save cancelled by retirement answered %v, want a commit conflict", err)
	}
	_, key := service.logSettingsStore()
	if data, err := base.ReadBlob(key); err == nil && len(data) > 0 {
		t.Fatalf("the cancelled settings save landed after retirement: %s", data)
	}
}

// Conflicts do not spin: once the epoch is retired, each commit a cycle makes
// is refused at admission without reaching the store, so a cycle's store
// attempts are bounded by its commits, not by retries. The next cycle
// resolves a new epoch, whose commits land.
func TestRetiredEpochCommitsAreRefusedAndTheNextCycleResolvesANewEpoch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			writeJSON(w, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"uid": "physical", "name": "kube-system"}})
			return
		}
		writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
	}))
	t.Cleanup(server.Close)
	credentials, err := staticTokenCredential("commit-fence-token")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{Endpoint: server.URL, Credentials: credentials, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "configured"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	registry := NewServiceRegistry(service)
	t.Cleanup(registry.Close)
	waitFor(t, retireAsync(service), "retirement")

	attempts := 0
	const commits = 5
	for range commits {
		err := service.commitCredential(t.Context(), func(context.Context) error { attempts++; return nil })
		var conflict commitConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("a commit on a retired epoch answered %v, want a commit conflict", err)
		}
	}
	if attempts != 0 {
		t.Fatalf("a retired epoch reached the store %d times in one cycle, want 0", attempts)
	}

	next, err := registry.ResolveCluster(service.scope.OrgID, service.scope.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if next == service || next.identity == service.identity {
		t.Fatal("the next cycle resolved the retired epoch again")
	}
	if err := next.commitCredential(t.Context(), func(context.Context) error { attempts++; return nil }); err != nil || attempts != 1 {
		t.Fatalf("the new epoch's commit answered %v after %d attempts, want one landed commit", err, attempts)
	}
}

// A token the cluster refused is never served again from memory, whichever
// site saw the refusal; a newer, different token in the file is served.
func TestRejectedTokenIsNeverServedFromMemory(t *testing.T) {
	t.Run("rejected while the file still holds it, then the file goes", func(t *testing.T) {
		for _, gone := range []string{"removed", "emptied"} {
			source, path, _, _ := tokenFileFixture(t, "refused-token")
			header, err := source.Authorization(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !source.rejectBearer(header) {
				t.Fatal("a rejection of the token the file holds did not retire the epoch")
			}
			if gone == "removed" {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, nil, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := source.Authorization(t.Context()); err == nil || got == header {
				t.Fatalf("file %s after the rejection: the refused token was served from memory: %q %v", gone, got, err)
			}
		}
	})
	t.Run("a newer token after a rejection is served", func(t *testing.T) {
		source, path, _, _ := tokenFileFixture(t, "refused-token")
		header, err := source.Authorization(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		source.rejectBearer(header)
		if err := os.WriteFile(path, []byte("renewed-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := source.Authorization(t.Context()); err != nil || got != "Bearer renewed-token" {
			t.Fatalf("the renewed token was not served: %q %v", got, err)
		}
		// And it is the one memory keeps from now on.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if got, err := source.Authorization(t.Context()); err != nil || got != "Bearer renewed-token" {
			t.Fatalf("the renewed token was not kept to bridge a swap: %q %v", got, err)
		}
	})
	t.Run("refused at the identity review, then the file goes", func(t *testing.T) {
		var refuse atomic.Bool
		config, _ := projectedAuthFixture(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if refuse.Load() && strings.HasSuffix(request.URL.Path, "/selfsubjectreviews") {
					writer.WriteHeader(http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(writer, request)
			})
		})
		config.TokenFile = filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(config.TokenFile, []byte("opaque-first"), 0o600); err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(config)
		if err != nil {
			t.Fatal(err)
		}
		service := NewService(client, Scope{ClusterID: "cluster"}, 0)
		if err := os.WriteFile(config.TokenFile, []byte("opaque-second"), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse.Store(true)
		if err := service.validateCredential(t.Context()); err == nil {
			t.Fatal("a token the identity review refused was accepted")
		}
		if err := os.Remove(config.TokenFile); err != nil {
			t.Fatal(err)
		}
		if got, err := client.credentials.Authorization(t.Context()); err == nil {
			t.Fatalf("the token the identity review refused was served from memory: %q", got)
		}
	})
	t.Run("refused at the projected token check, then the file goes", func(t *testing.T) {
		var refuse atomic.Bool
		config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if refuse.Load() && request.URL.Path == "/.well-known/openid-configuration" {
					writer.WriteHeader(http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(writer, request)
			})
		})
		config.TokenFile = filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(config.TokenFile, []byte(sign("first", nil)), 0o600); err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(config)
		if err != nil {
			t.Fatal(err)
		}
		service := NewService(client, Scope{ClusterID: "cluster"}, 0)
		refused := sign("second", nil)
		if err := os.WriteFile(config.TokenFile, []byte(refused), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse.Store(true)
		if err := service.validateCredential(t.Context()); err == nil {
			t.Fatal("a token the projected token check refused was accepted")
		}
		if err := os.Remove(config.TokenFile); err != nil {
			t.Fatal(err)
		}
		if got, err := client.credentials.Authorization(t.Context()); err == nil && got == "Bearer "+refused {
			t.Fatal("the token the projected token check refused was served from memory")
		}
	})
}

// Rejecting the client credential cancels a commit held in the store, and the
// commit answers a conflict. The epoch here is not derived from the client's
// credential context, so only the client fence itself can end the commit.
func TestClientRejectionCancelsACommitHeldInTheStore(t *testing.T) {
	service := staticCommitService(t)
	service.identity.ctx, service.identity.cancel = context.WithCancel(context.Background())
	t.Cleanup(service.identity.cancel)
	store := newHeldStore(storage.NewMemory())
	result := make(chan error, 1)
	go func() {
		result <- service.commitCredential(t.Context(), func(ctx context.Context) error {
			return storage.WriteBlobContext(ctx, store, "kubernetes/commit-fence-test/rejected", []byte(`{}`))
		})
	}()
	waitFor(t, store.held, "the commit to reach the store")
	service.client.rejectCredential()
	var conflict commitConflictError
	if err := waitFor(t, result, "the commit to end at the rejection"); !errors.As(err, &conflict) {
		t.Fatalf("a commit cancelled by a client rejection answered %v, want a commit conflict", err)
	}
	if data, err := store.Provider.ReadBlob("kubernetes/commit-fence-test/rejected"); err == nil && len(data) > 0 {
		t.Fatal("the cancelled commit landed after the rejection")
	}
}

// stalledJournal is a file journal whose reads stall until the read's context
// ends, then fail with the store's own error.
type stalledJournal struct {
	storage.Provider
	held chan struct{}
	// deadline is the deadline the stalled read ran under.
	deadline    time.Time
	hasDeadline bool
}

func (store *stalledJournal) ReadBlob(name string) ([]byte, error) {
	return store.ReadBlobContext(context.Background(), name)
}

func (store *stalledJournal) ReadBlobContext(ctx context.Context, name string) ([]byte, error) {
	if strings.HasPrefix(name, "kubernetes/changes/") {
		store.deadline, store.hasDeadline = ctx.Deadline()
		select {
		case store.held <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, errStalledJournal
	}
	return store.Provider.ReadBlob(name)
}

func (store *stalledJournal) WriteBlobContext(ctx context.Context, name string, data []byte) error {
	return storage.WriteBlobContext(ctx, store.Provider, name, data)
}

var errStalledJournal = errors.New("storage: read blob: journal stalled")

// Listing changes from a stalled file journal ends at the request's deadline
// with the store's own error, not at the store's own bound.
func TestStalledFileJournalAnswersAtTheRequestDeadline(t *testing.T) {
	file, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	journal := &stalledJournal{Provider: file, held: make(chan struct{}, 1)}
	service.SetChangeStorage(journal)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond) // testlint:allow the request deadline under test must end the stalled journal read
	defer cancel()
	requestDeadline, _ := ctx.Deadline()
	result := make(chan error, 1)
	go func() { _, err := service.Changes(ctx, ChangeQuery{}); result <- err }()
	if err := waitFor(t, result, "the change list to answer at its deadline"); !errors.Is(err, errStalledJournal) {
		t.Fatalf("a stalled journal answered %v, want the store's own error at the request deadline", err)
	}
	// The read ran under the request's deadline, not only the store's own
	// bound.
	if !journal.hasDeadline || !journal.deadline.Equal(requestDeadline) {
		t.Fatalf("the stalled read ran under deadline %v (set %t), want the request's %v", journal.deadline, journal.hasDeadline, requestDeadline)
	}
}

// Two refusals in a row: the file's token T is refused, then a late refusal
// of an older token O arrives. O is not what memory or the file holds, so T
// stays refused, and once the file is unreadable the answer is a fault,
// never T from memory.
func TestALateRefusalOfAnOlderTokenKeepsTheFilesTokenRefused(t *testing.T) {
	source, path, _, _ := tokenFileFixture(t, "older-token")
	if _, err := source.Authorization(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused, err := source.Authorization(t.Context())
	if err != nil || refused != "Bearer file-token" {
		t.Fatalf("the file's token was not read: %q %v", refused, err)
	}
	if !source.rejectBearer(refused) {
		t.Fatal("refusing the file's token did not retire the epoch")
	}
	source.rejectBearer("Bearer older-token")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := source.Authorization(t.Context()); err == nil || got == refused {
		t.Fatalf("after a late refusal of an older token, the refused file token was served from memory: %q %v", got, err)
	}
}

// A read that began before a rejection changed is not remembered: call A
// reads the refused token T and is held; the file then holds U, and call B
// reads U and clears the rejection; when A resumes it must not put T in
// memory. With the file then unreadable, memory answers U, never T.
func TestAReadOlderThanTheRejectionIsNotRemembered(t *testing.T) {
	source, path, _, _ := tokenFileFixture(t, "refused-token")
	refused, err := source.Authorization(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source.forgetBearer(refused)
	readDone, resume := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	source.afterRead = func() {
		if first.CompareAndSwap(true, false) {
			close(readDone)
			<-resume
		}
	}
	answered := make(chan string, 1)
	go func() { header, _ := source.Authorization(t.Context()); answered <- header }()
	waitFor(t, readDone, "call A to read the refused token")
	if err := os.WriteFile(path, []byte("renewed-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := source.Authorization(t.Context()); err != nil || got != "Bearer renewed-token" {
		t.Fatalf("call B did not read the renewed token: %q %v", got, err)
	}
	close(resume)
	if got := waitFor(t, answered, "call A to answer"); got != refused {
		t.Fatalf("call A answered %q, want the file's answer at its read, %q", got, refused)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := source.Authorization(t.Context()); err != nil || got != "Bearer renewed-token" {
		t.Fatalf("memory answered %q (%v), want the renewed token and never the refused one", got, err)
	}
}

// The refusals form a set, so their order against reads does not matter. A
// read that has the newer token in hand while the previous one is refused,
// followed by a refusal of the newer token: with the file then unreadable the
// newer token is never served from memory.
func TestRefusedTokensAreRememberedWhateverTheOrder(t *testing.T) {
	t.Run("a token read during a rejection and then refused", func(t *testing.T) {
		source, path, _, _ := tokenFileFixture(t, "previous-token")
		previous, err := source.Authorization(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("newer-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		var once atomic.Bool
		source.afterRead = func() {
			if once.CompareAndSwap(false, true) {
				source.forgetBearer(previous)
			}
		}
		newer, err := source.Authorization(t.Context())
		if err != nil || newer != "Bearer newer-token" {
			t.Fatalf("the read during the rejection answered %q %v", newer, err)
		}
		source.rejectBearer(newer)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if got, err := source.Authorization(t.Context()); err == nil && got == newer {
			t.Fatal("the refused newer token was served from memory")
		}
	})
	t.Run("the set keeps the eight most recent refusals", func(t *testing.T) {
		source, path, _, _ := tokenFileFixture(t, "token-0")
		for refusal := 0; refusal <= tokenFileRefusals; refusal++ {
			source.forgetBearer("Bearer token-" + strconv.Itoa(refusal))
		}
		// token-0 was the oldest of nine refusals and has left the set: a read
		// of it is remembered again, so memory answers it once the file goes.
		if got, err := source.Authorization(t.Context()); err != nil || got != "Bearer token-0" {
			t.Fatalf("the file's token was not read: %q %v", got, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if got, err := source.Authorization(t.Context()); err != nil || got != "Bearer token-0" {
			t.Fatalf("the oldest refusal is still remembered past the bound: %q %v", got, err)
		}
		// token-1, the oldest still in the set, is never remembered.
		if err := os.WriteFile(path, []byte("token-1"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := source.Authorization(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if got, err := source.Authorization(t.Context()); err == nil && got == "Bearer token-1" {
			t.Fatal("a token still in the refused set was served from memory")
		}
	})
}
