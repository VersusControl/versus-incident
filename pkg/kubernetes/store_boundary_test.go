package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// blobOnlyProvider is a store with no compare-and-swap, delete or evidence.
type blobOnlyProvider struct{ storage.Provider }

// failingBlobProvider fails every blob read with cause.
type failingBlobProvider struct {
	storage.Provider
	cause error
}

func (provider failingBlobProvider) ReadBlob(string) ([]byte, error) { return nil, provider.cause }

// The boundary claims exactly the capabilities of the store behind it, because
// this package chooses demand capture, timeline cleanup and saved history by
// asking the store for them. Wrapping it again, as a rebuilt connector does,
// returns the same wrapped store.
func TestStoreBoundaryKeepsTheStoreCapabilities(t *testing.T) {
	memory := storage.NewMemory()
	t.Cleanup(func() { _ = memory.Close() })
	_, memoryCAS := memory.(storage.BlobCAS)
	_, memoryEvidence := memory.(storage.EvidenceStore)
	_, memoryDelete := memory.(storage.BlobDeleter)
	wrapped := wrapStore(memory)
	if _, ok := wrapped.(storage.BlobCAS); ok != memoryCAS {
		t.Fatalf("compare-and-swap = %t, store has %t", ok, memoryCAS)
	}
	if _, ok := wrapped.(storage.EvidenceStore); ok != memoryEvidence {
		t.Fatalf("evidence store = %t, store has %t", ok, memoryEvidence)
	}
	if _, ok := wrapped.(storage.BlobDeleter); ok != memoryDelete {
		t.Fatalf("delete = %t, store has %t", ok, memoryDelete)
	}
	if unwrapStore(wrapped) != memory {
		t.Fatal("the wrapped store is not recognised as wrapped")
	}
	if again := wrapStore(wrapped); again != wrapped {
		t.Fatal("wrapping a wrapped store again wrapped it twice")
	}

	plain := wrapStore(blobOnlyProvider{memory})
	if _, ok := plain.(storage.BlobCAS); ok {
		t.Fatal("a store without compare-and-swap was given one")
	}
	if _, ok := plain.(storage.EvidenceStore); ok {
		t.Fatal("a store without saved history was given one")
	}
	if unwrapStore(plain) == plain {
		t.Fatal("the wrapped store is not recognised as wrapped")
	}
}

// blockingStore holds every context read until its context ends, and says
// when a read has started.
type blockingStore struct {
	storage.Provider
	entered chan struct{}
	reads   atomic.Int32
}

func (store *blockingStore) ReadBlobContext(ctx context.Context, name string) ([]byte, error) {
	store.reads.Add(1)
	store.entered <- struct{}{}
	<-ctx.Done()
	return nil, fmt.Errorf("storage: read blob %q: %w", name, ctx.Err())
}

func (store *blockingStore) WriteBlobContext(context.Context, string, []byte) error { return nil }

// A failure is named for a component only when it came out of that
// component's own call. A store call that runs out of time is named for the
// store. A request whose deadline passed before its store call began, or that
// waited on a lock someone holds across a store call, is not: it gets the
// neutral server_timeout, never another component's name.
func TestStoreBoundaryNamesOnlyItsOwnFailures(t *testing.T) {
	memory := storage.NewMemory()
	t.Cleanup(func() { _ = memory.Close() })
	wrapped := wrapStore(failingBlobProvider{Provider: memory, cause: fmt.Errorf("read blob on 10.0.0.7: %w", context.DeadlineExceeded)})
	_, err := wrapped.ReadBlob("kubernetes/inventory/x")
	if !isStoreFailure(err) || DiagnoseError(err).Code != "history_store_timeout" {
		t.Fatalf("store deadline = %v (%s), want history_store_timeout", err, DiagnoseError(err).Code)
	}

	blocking := &blockingStore{Provider: memory, entered: make(chan struct{}, 1)}
	held := wrapStore(blocking)
	expired := newEventDeadline(t.Context())
	expired.pass()
	_, err = storage.ReadBlobContext(expired, held, "kubernetes/inventory/x")
	if isStoreFailure(err) || DiagnoseError(err).Code != "server_timeout" || blocking.reads.Load() != 0 {
		t.Fatalf("read with a deadline already passed = %v (%s, store reads %d), want server_timeout without asking the store", err, DiagnoseError(err).Code, blocking.reads.Load())
	}

	// A commit holds a lock across a store read that began after the
	// request arrived; the request's deadline passes while it waits on the
	// lock.
	var lock sync.Mutex
	request := newEventDeadline(t.Context())
	commit := make(chan error, 1)
	commitCtx, stopCommit := context.WithCancel(t.Context())
	defer stopCommit()
	lock.Lock()
	go func() {
		defer lock.Unlock()
		_, err := storage.ReadBlobContext(commitCtx, held, "kubernetes/capture-state/x")
		commit <- err
	}()
	waitFor(t, blocking.entered, "the commit's store read to start")
	waited := make(chan error, 1)
	go func() { waited <- lockContext(request, &lock) }()
	request.pass()
	if code := DiagnoseError(waitFor(t, waited, "the request to stop waiting")).Code; code != "server_timeout" {
		t.Fatalf("request behind a lock held across a store call = %s, want server_timeout", code)
	}
	stopCommit()
	if err := waitFor(t, commit, "the commit to end"); isStoreFailure(err) {
		t.Fatalf("a cancelled commit read was named a store failure: %v", err)
	}
}

// lookupStore answers every evidence scope lookup with cause.
type lookupStore struct {
	storage.EvidenceStore
	cause error
}

func (store lookupStore) FindEvidenceScope(context.Context, string, string, string) (storage.EvidenceScope, error) {
	return storage.EvidenceScope{}, store.cause
}

// slowAPIServerTimeout is a deadline that passes while a slow API server holds
// a read: the request reaches the server, and its deadline passes there.
func slowAPIServerTimeout(t *testing.T) error {
	t.Helper()
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		arrived <- struct{}{}
		<-request.Context().Done()
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	deadline := newEventDeadline(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := client.get(deadline, "/api/v1/namespaces", "application/json")
		result <- err
	}()
	waitFor(t, arrived, "the API server to receive the read")
	deadline.pass()
	return waitFor(t, result, "the read to end")
}

// A store that answers (a record that is not there, a moved checkpoint, a
// page cursor that no longer matches, a caller that stopped waiting) is a
// working store: its answer is not named for the store. A slow API server
// stays a Kubernetes timeout, and a store that does not answer is named for
// the store.
func TestStoreAnswersAreNotStoreFailures(t *testing.T) {
	memory := storage.NewMemory()
	t.Cleanup(func() { _ = memory.Close() })
	evidence := memory.(storage.EvidenceStore)
	if code := DiagnoseError(slowAPIServerTimeout(t)).Code; code != "request_timeout" {
		t.Fatalf("slow API server = %s, want request_timeout", code)
	}
	for _, test := range []struct {
		name   string
		answer error
		lookup bool
	}{
		{name: "scope not found", answer: storage.ErrNotFound, lookup: true},
		{name: "checkpoint conflict", answer: fmt.Errorf("commit: %w", storage.ErrEvidenceConflict)},
		{name: "cursor expired", answer: fmt.Errorf("page: %w", storage.ErrEvidenceCursorExpired)},
		{name: "caller cancelled", answer: fmt.Errorf("storage: read blob: %w", context.Canceled)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.lookup {
				wrapped := wrapStore(struct {
					storage.Provider
					lookupStore
				}{memory, lookupStore{EvidenceStore: evidence, cause: test.answer}})
				_, err = wrapped.(storage.EvidenceScopeLookup).FindEvidenceScope(t.Context(), "default", "c", "uid")
			} else {
				_, err = wrapStore(failingBlobProvider{Provider: memory, cause: test.answer}).ReadBlob("kubernetes/inventory/x")
			}
			if !errors.Is(err, test.answer) || isStoreFailure(err) {
				t.Fatalf("store answer = %v (store failure %t), want the answer unmarked", err, isStoreFailure(err))
			}
			if code := DiagnoseError(err).Code; code == "history_store_timeout" || code == "history_store_unavailable" {
				t.Fatalf("store answer was named a store outage: %s", code)
			}
		})
	}
	t.Run("store unreachable", func(t *testing.T) {
		_, err := wrapStore(failingBlobProvider{Provider: memory, cause: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}).ReadBlob("kubernetes/inventory/x")
		if code := DiagnoseError(err).Code; code != "history_store_unavailable" {
			t.Fatalf("unreachable store = %s, want history_store_unavailable", code)
		}
	})
}

// The optional interfaces this package asks the change store for. Each one the
// configured provider has must survive the boundary, or this package changes
// behaviour (demand capture, timeline cleanup, saved history, scope lookup).
func TestStoreBoundaryKeepsEveryCapabilityThePackageAsksFor(t *testing.T) {
	memory := storage.NewMemory()
	t.Cleanup(func() { _ = memory.Close() })
	cas := memory.(storage.BlobCAS)
	evidence := memory.(storage.EvidenceStore)
	var deleter storage.BlobDeleter = deleteOnly{}
	lookup := lookupStore{EvidenceStore: evidence}
	checks := map[string]func(any) bool{
		"BlobCAS":                func(p any) bool { _, ok := p.(storage.BlobCAS); return ok },
		"ContextBlobCAS":         func(p any) bool { _, ok := p.(storage.ContextBlobCAS); return ok },
		"BlobDeleter":            func(p any) bool { _, ok := p.(storage.BlobDeleter); return ok },
		"EvidenceStore":          func(p any) bool { _, ok := p.(storage.EvidenceStore); return ok },
		"EvidenceAuthorityStore": func(p any) bool { _, ok := p.(storage.EvidenceAuthorityStore); return ok },
		"EvidenceScopeLookup":    func(p any) bool { _, ok := p.(storage.EvidenceScopeLookup); return ok },
	}
	// ContextBlobCAS is kept as a capability of compare-and-swap: the boundary
	// routes it to the provider's own context form or the plain one.
	expectedOf := func(provider storage.Provider) map[string]bool {
		expected := map[string]bool{}
		for name, check := range checks {
			expected[name] = check(provider)
		}
		expected["ContextBlobCAS"] = expected["BlobCAS"]
		return expected
	}
	type casP = storage.BlobCAS
	type delP = storage.BlobDeleter
	type evP = storage.EvidenceStore
	providers := map[string]storage.Provider{
		"blobs only": blobOnlyProvider{memory},
		"compare-and-swap": struct {
			storage.Provider
			casP
		}{memory, cas},
		"delete": struct {
			storage.Provider
			delP
		}{memory, deleter},
		"compare-and-swap, delete": struct {
			storage.Provider
			casP
			delP
		}{memory, cas, deleter},
		"evidence": struct {
			storage.Provider
			evP
		}{memory, evidence},
		"evidence, compare-and-swap": struct {
			storage.Provider
			casP
			evP
		}{memory, cas, evidence},
		"evidence, delete": struct {
			storage.Provider
			delP
			evP
		}{memory, deleter, evidence},
		"evidence, compare-and-swap, delete": struct {
			storage.Provider
			casP
			delP
			evP
		}{memory, cas, deleter, evidence},
		"evidence with lookup": struct {
			storage.Provider
			lookupStore
		}{memory, lookup},
		"evidence with lookup, compare-and-swap": struct {
			storage.Provider
			casP
			lookupStore
		}{memory, cas, lookup},
		"evidence with lookup, delete": struct {
			storage.Provider
			delP
			lookupStore
		}{memory, deleter, lookup},
		"evidence with lookup, compare-and-swap, delete": struct {
			storage.Provider
			casP
			delP
			lookupStore
		}{memory, cas, deleter, lookup},
		"memory": memory,
	}
	// Every capability this package asks a store for is covered here: a new
	// type assertion or type switch on a storage interface fails this test
	// until it is checked above.
	for _, asked := range storageInterfacesAskedFor(t) {
		if checks[asked] == nil {
			t.Errorf("this package asks a store for storage.%s, which the boundary test does not check", asked)
		}
	}
	for name, provider := range providers {
		expected := expectedOf(provider)
		wrapped := wrapStore(provider)
		for capability, check := range checks {
			if got := check(wrapped); got != expected[capability] {
				t.Errorf("%s: %s = %t behind the boundary, provider has %t", name, capability, got, expected[capability])
			}
		}
	}
}

// deleteOnly is a blob deleter and nothing else.
type deleteOnly struct{}

func (deleteOnly) DeleteBlob(string) error { return nil }

// storageInterfacesAskedFor lists every storage type this package's source
// (tests excluded) asserts a value to or switches on.
func storageInterfacesAskedFor(t *testing.T) []string {
	t.Helper()
	asked := map[string]bool{}
	note := func(expression ast.Expr) {
		if selector, ok := expression.(*ast.SelectorExpr); ok {
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "storage" {
				asked[selector.Sel.Name] = true
			}
		}
	}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.TypeAssertExpr:
				if node.Type != nil {
					note(node.Type)
				}
			case *ast.TypeSwitchStmt:
				for _, clause := range node.Body.List {
					for _, listed := range clause.(*ast.CaseClause).List {
						note(listed)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 {
		t.Fatal("found no storage type assertions; the source scan is broken")
	}
	names := make([]string, 0, len(asked))
	for name := range asked {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
