package chat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type observedProvider struct {
	storage.Provider
	mu               sync.Mutex
	reads            int
	lists            int
	failIndexCAS     bool
	failLeaseDelete  bool
	panicSessionRead bool
}

func (provider *observedProvider) ReadBlob(name string) ([]byte, error) {
	provider.mu.Lock()
	panicSessionRead := provider.panicSessionRead
	provider.reads++
	provider.mu.Unlock()
	if panicSessionRead && strings.Contains(name, "/sessions/") {
		panic("List read a session transcript")
	}
	return provider.Provider.ReadBlob(name)
}

func (provider *observedProvider) ListBlobs(prefix string) ([]storage.Blob, error) {
	provider.mu.Lock()
	provider.lists++
	provider.mu.Unlock()
	return provider.Provider.ListBlobs(prefix)
}

func (provider *observedProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	if provider.failIndexCAS && strings.HasSuffix(name, "/index") {
		return false, errors.New("injected index failure")
	}
	if provider.failLeaseDelete && strings.Contains(name, "/leases/") && replacement == nil {
		return false, errors.New("injected lease release failure")
	}
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

func (provider *observedProvider) resetCounts() {
	provider.mu.Lock()
	provider.reads = 0
	provider.lists = 0
	provider.mu.Unlock()
}

func (provider *observedProvider) counts() (int, int) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.reads, provider.lists
}

func TestSessionStoreRoundTripAndClone(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	store := NewSessionStore(storage.NewMemory(), tenancy.NewOrgScope("org/a"), func() time.Time { return now })
	created, err := store.Create("session-1")
	if err != nil {
		t.Fatal(err)
	}
	created.Status = SessionFailed
	if _, err := store.Append("session-1", Turn{ID: "turn-1", Role: TurnUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SessionIdle || len(got.Turns) != 1 || got.Turns[0].Content != "hello" {
		t.Fatalf("session = %+v", got)
	}
	got.Turns[0].Content = "mutated"
	again, _ := store.Get("session-1")
	if again.Turns[0].Content != "hello" {
		t.Fatal("Get returned mutable store state")
	}
}

func TestSessionStoreConcurrentAppendDoesNotLoseTurns(t *testing.T) {
	provider := storage.NewMemory()
	first := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	second := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := first.Create("session-1"); err != nil {
		t.Fatal(err)
	}
	const count = 40
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			store := first
			if index%2 == 1 {
				store = second
			}
			if _, err := store.Append("session-1", Turn{ID: fmt.Sprintf("turn-%d", index), Role: TurnUser, Content: "message"}); err != nil {
				t.Errorf("append: %v", err)
			}
		}(index)
	}
	wait.Wait()
	got, err := first.Get("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != count {
		t.Fatalf("turns = %d, want %d", len(got.Turns), count)
	}
}

func TestSessionStoreRejectsUnsafeIDAndScopesBlob(t *testing.T) {
	provider := storage.NewMemory()
	storeA := NewSessionStore(provider, tenancy.NewOrgScope("a/../../b"), time.Now)
	storeB := NewSessionStore(provider, tenancy.NewOrgScope("other"), time.Now)
	if _, err := storeA.Create("../escape"); err == nil {
		t.Fatal("unsafe session id accepted")
	}
	if _, err := storeA.Create("session-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := storeB.Get("session-a"); err != ErrSessionNotFound {
		t.Fatalf("cross-org Get error = %v, want not found", err)
	}
}

func TestSessionStoreFileRoundTrip(t *testing.T) {
	directory := t.TempDir()
	provider, err := storage.NewFile(storage.FileOptions{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-file"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("session-file", Turn{ID: "turn", Role: TurnUser, Content: "durable"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.NewFile(storage.FileOptions{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewSessionStore(reopened, tenancy.DefaultOrgScope(), time.Now).Get("session-file")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 || got.Turns[0].Content != "durable" {
		t.Fatalf("session = %+v", got)
	}
}

func TestSessionStoreIndependentFileProvidersDoNotClobber(t *testing.T) {
	directory := t.TempDir()
	firstProvider, err := storage.NewFile(storage.FileOptions{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	secondProvider, err := storage.NewFile(storage.FileOptions{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	first := NewSessionStore(firstProvider, tenancy.DefaultOrgScope(), time.Now)
	second := NewSessionStore(secondProvider, tenancy.DefaultOrgScope(), time.Now)
	for _, item := range []struct {
		store *SessionStore
		id    string
	}{{first, "session-a"}, {second, "session-b"}} {
		if _, err := item.store.Create(item.id); err != nil {
			t.Fatal(err)
		}
	}
	const count = 40
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		for _, item := range []struct {
			store *SessionStore
			id    string
		}{{first, "session-a"}, {second, "session-b"}} {
			wait.Add(1)
			go func(store *SessionStore, id string, index int) {
				defer wait.Done()
				if _, err := store.Append(id, Turn{ID: fmt.Sprintf("%s-%d", id, index), Role: TurnUser, Content: "message"}); err != nil {
					t.Errorf("append %s: %v", id, err)
				}
			}(item.store, item.id, index)
		}
	}
	wait.Wait()
	for _, id := range []string{"session-a", "session-b"} {
		got, err := first.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Turns) != count {
			t.Fatalf("%s turns = %d, want %d", id, len(got.Turns), count)
		}
	}
	if _, err := first.Create("session-c"); err != nil {
		t.Fatal(err)
	}
	wait = sync.WaitGroup{}
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			store := first
			if index%2 == 1 {
				store = second
			}
			if _, err := store.Append("session-c", Turn{ID: fmt.Sprintf("shared-%d", index), Role: TurnUser, Content: "message"}); err != nil {
				t.Errorf("shared append: %v", err)
			}
		}(index)
	}
	wait.Wait()
	shared, err := second.Get("session-c")
	if err != nil {
		t.Fatal(err)
	}
	if len(shared.Turns) != count {
		t.Fatalf("shared turns = %d, want %d", len(shared.Turns), count)
	}
}

func TestSessionStoreLeaseConflictAndExpiry(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	storeA := NewSessionStore(provider, tenancy.DefaultOrgScope(), func() time.Time { return now })
	storeB := NewSessionStore(provider, tenancy.DefaultOrgScope(), func() time.Time { return now })
	epochA, acquired, err := storeA.AcquireLease("session", "owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first lease acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err := storeB.AcquireLease("session", "owner-b", time.Minute); err != nil || acquired {
		t.Fatalf("conflicting lease acquired=%v err=%v", acquired, err)
	}
	now = now.Add(time.Minute + time.Second)
	epochB, acquired, err := storeB.AcquireLease("session", "owner-b", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("expired lease acquired=%v err=%v", acquired, err)
	}
	if err := storeA.ReleaseLease("session", "owner-a", epochA); !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("stale release error=%v, want ErrLeaseFenced", err)
	}
	active, err := storeB.LeaseActive("session")
	if err != nil || !active {
		t.Fatalf("replacement lease active=%v err=%v", active, err)
	}
	if cancelled, err := storeA.RequestCancel("session"); err != nil || !cancelled {
		t.Fatalf("cancel requested=%v err=%v", cancelled, err)
	}
	if renewed, cancelRequested, err := storeB.RenewLease("session", "owner-b", epochB, time.Minute); err != nil || !renewed || !cancelRequested {
		t.Fatalf("renewed=%v cancel=%v err=%v", renewed, cancelRequested, err)
	}
}

func TestSessionStoreDeleteRemovesExpiredLease(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), func() time.Time { return now })
	if _, err := store.Create("session-expired-lease"); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := store.AcquireLease("session-expired-lease", "replica-a", time.Second); err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}
	now = now.Add(2 * time.Second)
	if err := store.Delete("session-expired-lease"); err != nil {
		t.Fatal(err)
	}
	lease, err := provider.ReadBlob(store.leaseBlobName("session-expired-lease"))
	if err != nil {
		t.Fatal(err)
	}
	if lease != nil {
		t.Fatalf("expired lease blob survived session deletion: %s", lease)
	}
}

func TestSessionStoreListReadsOnlySummaryIndex(t *testing.T) {
	provider := &observedProvider{Provider: storage.NewMemory()}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-list"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("session-list", Turn{ID: "turn", Role: TurnUser, Content: "payload", Events: []core.ChatEvent{{Kind: "event", Output: "secret"}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetStatus("session-list", SessionFailed, true); err != nil {
		t.Fatal(err)
	}
	data, err := provider.Provider.ReadBlob(store.sessionBlobName("session-list"))
	if err != nil {
		t.Fatal(err)
	}
	var large Session
	if err := json.Unmarshal(data, &large); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < MaxTurnsPerSession; index++ {
		large.Turns = append(large.Turns, Turn{ID: fmt.Sprintf("large-%d", index), Role: TurnAssistant, Content: strings.Repeat("x", MaxMessageBytes)})
	}
	replacement, _ := json.Marshal(&large)
	if len(replacement) < 750*1024 {
		t.Fatalf("large transcript = %d bytes, want a substantial session blob", len(replacement))
	}
	if swapped, err := provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(store.sessionBlobName("session-list"), data, replacement); err != nil || !swapped {
		t.Fatalf("install large transcript swapped=%v err=%v", swapped, err)
	}
	provider.resetCounts()
	provider.panicSessionRead = true
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "session-list" || listed[0].Status != SessionFailed || !listed[0].Seeded || listed[0].Turns != nil {
		t.Fatalf("list projection = %+v", listed)
	}
	reads, lists := provider.counts()
	if reads != 1 || lists != 0 {
		t.Fatalf("list reads=%d batch lists=%d, want one index read only", reads, lists)
	}
}

func TestSessionStoreListReadsOnlyIndexWithMaximumHugeSessions(t *testing.T) {
	provider := &observedProvider{Provider: storage.NewMemory()}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	largeTurns := make([]Turn, MaxTurnsPerSession)
	for index := range largeTurns {
		largeTurns[index] = Turn{ID: fmt.Sprintf("turn-%03d", index), Role: TurnAssistant, Content: strings.Repeat("x", MaxMessageBytes)}
	}
	for index := 0; index < MaxSessions; index++ {
		id := fmt.Sprintf("session-%03d", index)
		created, err := store.Create(id)
		if err != nil {
			t.Fatal(err)
		}
		current, err := provider.Provider.ReadBlob(store.sessionBlobName(id))
		if err != nil {
			t.Fatal(err)
		}
		created.Turns = largeTurns
		replacement, err := json.Marshal(created)
		if err != nil {
			t.Fatal(err)
		}
		if len(replacement) < 750*1024 {
			t.Fatalf("large transcript = %d bytes, want at least 750 KiB", len(replacement))
		}
		if swapped, err := provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(store.sessionBlobName(id), current, replacement); err != nil || !swapped {
			t.Fatalf("install large transcript %q swapped=%v err=%v", id, swapped, err)
		}
	}
	provider.resetCounts()
	provider.panicSessionRead = true
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != MaxSessions {
		t.Fatalf("listed sessions = %d, want %d", len(listed), MaxSessions)
	}
	for _, session := range listed {
		if session.Turns != nil {
			t.Fatalf("list projection for %q contains transcript turns", session.ID)
		}
	}
	reads, lists := provider.counts()
	if reads != 1 || lists != 0 {
		t.Fatalf("list reads=%d batch lists=%d, want one index read only", reads, lists)
	}
}

func TestSessionStoreListHydratesLegacyIndexOnceAndPrunesMissing(t *testing.T) {
	provider := &observedProvider{Provider: storage.NewMemory()}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-legacy-index"); err != nil {
		t.Fatal(err)
	}
	if err := store.updateIndex(func(index *sessionIndex) error {
		index.Sessions[0].Status = ""
		index.Sessions = append(index.Sessions, sessionIndexEntry{ID: "missing-session", CreatedAt: time.Now(), UpdatedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List()
	if err != nil || len(listed) != 1 || listed[0].ID != "session-legacy-index" {
		t.Fatalf("legacy list=%+v err=%v", listed, err)
	}
	provider.resetCounts()
	provider.panicSessionRead = true
	listed, err = store.List()
	if err != nil || len(listed) != 1 {
		t.Fatalf("steady-state list=%+v err=%v", listed, err)
	}
	reads, lists := provider.counts()
	if reads != 1 || lists != 0 {
		t.Fatalf("steady-state reads=%d lists=%d, want index only", reads, lists)
	}
}

func TestSessionStoreListReconcilesStaleRunningFromIndexAndLeases(t *testing.T) {
	provider := &observedProvider{Provider: storage.NewMemory()}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-stale-running"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetStatus("session-stale-running", SessionRunning, false); err != nil {
		t.Fatal(err)
	}
	provider.panicSessionRead = true
	listed, err := store.List()
	if err != nil || len(listed) != 1 || listed[0].Status != SessionFailed {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	index, _, err := store.readIndex()
	if err != nil || index.Sessions[0].Status != SessionFailed {
		t.Fatalf("index=%+v err=%v", index, err)
	}
}

func TestSessionStoreEvictionRemovesLease(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	provider := storage.NewMemory()
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), func() time.Time { return now })
	if _, err := store.Create("session-000"); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := store.AcquireLease("session-000", "owner-a", time.Hour); err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}
	for index := 1; index <= MaxSessions; index++ {
		if _, err := store.Create(fmt.Sprintf("session-%03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := provider.ReadBlob(store.leaseBlobName("session-000"))
	if err != nil || lease != nil {
		t.Fatalf("evicted lease=%s err=%v", lease, err)
	}
}

func TestSessionStoreDeleteCleanupFailureKeepsSessionHiddenAndRetryable(t *testing.T) {
	provider := &observedProvider{Provider: storage.NewMemory()}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-cleanup-failure"); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := store.AcquireLease("session-cleanup-failure", "owner-a", time.Minute); err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}
	provider.failLeaseDelete = true
	if err := store.Delete("session-cleanup-failure"); err == nil {
		t.Fatal("delete succeeded despite lease cleanup failure")
	}
	if _, err := store.Get("session-cleanup-failure"); err != nil {
		t.Fatalf("orphaned session artifact unavailable after cleanup failure: %v", err)
	}
	listed, err := store.List()
	if err != nil || len(listed) != 0 {
		t.Fatalf("deleted session remained visible: list=%+v err=%v", listed, err)
	}
	provider.failLeaseDelete = false
	if err := store.Delete("session-cleanup-failure"); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if _, err := store.Get("session-cleanup-failure"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("session artifact survived retry: %v", err)
	}
}

func TestSessionStoreCreateRollsBackBlobWhenIndexFails(t *testing.T) {
	provider := &observedProvider{Provider: storage.NewMemory(), failIndexCAS: true}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-rollback"); err == nil {
		t.Fatal("create succeeded despite index failure")
	}
	blobs, err := provider.Provider.ListBlobs(store.blobName + "/sessions/")
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 0 {
		t.Fatalf("create left phantom session blobs: %+v", blobs)
	}
}

func TestSessionStoreDeleteRetryRemovesPhantomIndexEntry(t *testing.T) {
	provider := &observedProvider{Provider: storage.NewMemory()}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-partial-delete"); err != nil {
		t.Fatal(err)
	}
	provider.failIndexCAS = true
	if err := store.Delete("session-partial-delete"); err == nil {
		t.Fatal("delete succeeded despite index failure")
	}
	if data, err := provider.ReadBlob(store.sessionBlobName("session-partial-delete")); err != nil || data == nil {
		t.Fatalf("session blob was removed before index commit=%q err=%v", data, err)
	}
	listed, err := store.List()
	if err != nil || len(listed) != 1 {
		t.Fatalf("failed index commit changed visibility: listed=%+v err=%v", listed, err)
	}
	provider.failIndexCAS = false
	if err := store.Delete("session-partial-delete"); err != nil {
		t.Fatalf("delete retry: %v", err)
	}
	listed, err = store.List()
	if err != nil || len(listed) != 0 {
		t.Fatalf("listed after delete retry=%+v err=%v", listed, err)
	}
	if err := store.Delete("session-partial-delete"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestSessionStoreCreateIgnoresCommittedEvictionCleanupFailure(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	provider := &observedProvider{Provider: storage.NewMemory()}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), func() time.Time { return now })
	for index := 0; index < MaxSessions; index++ {
		if _, err := store.Create(fmt.Sprintf("session-%03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if _, acquired, err := store.AcquireLease("session-000", "owner", time.Hour); err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}
	provider.failLeaseDelete = true
	if _, err := store.Create("session-new"); err != nil {
		t.Fatalf("committed create reported cleanup failure: %v", err)
	}
	listed, err := store.List()
	if err != nil || len(listed) != MaxSessions {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	for _, session := range listed {
		if session.ID == "session-000" {
			t.Fatal("evicted session remained visible after cleanup failure")
		}
	}
}

func TestSessionStoreTouchIndexReinsertsMissingEntry(t *testing.T) {
	provider := storage.NewMemory()
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-reinsert"); err != nil {
		t.Fatal(err)
	}
	index, current, err := store.readIndex()
	if err != nil {
		t.Fatal(err)
	}
	index.Sessions = nil
	replacement, _ := json.Marshal(index)
	if swapped, err := provider.(storage.BlobCAS).CompareAndSwapBlob(store.indexBlobName(), current, replacement); err != nil || !swapped {
		t.Fatalf("remove index entry swapped=%v err=%v", swapped, err)
	}
	if _, err := store.Append("session-reinsert", Turn{ID: "turn", Role: TurnUser, Content: "landed"}); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List()
	if err != nil || len(listed) != 1 || listed[0].ID != "session-reinsert" {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

func TestSessionStoreBoundsAggregateTraceAndBlobGrowth(t *testing.T) {
	provider := storage.NewMemory()
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-growth"); err != nil {
		t.Fatal(err)
	}
	prefix := `"\\世界`
	payload := prefix + strings.Repeat("x", MaxToolPayloadBytes-len(prefix))
	events := make([]core.ChatEvent, 4)
	for index := range events {
		events[index] = core.ChatEvent{Kind: core.ChatEventModelDelta, Delta: payload, Args: payload, Output: payload}
	}
	events[len(events)-1].Kind = core.ChatEventRunFinished
	toolCalls := []core.ToolCallTrace{
		{Name: "query_logs", Args: payload, Output: payload},
		{Name: "query_metrics", Args: payload, Output: payload},
	}
	citations := make([]core.ChatCitation, MaxCitationsPerTrace)
	for index := range citations {
		citations[index] = core.ChatCitation{Tool: strings.Repeat("t", 128), Label: strings.Repeat("l", 256), Locator: strings.Repeat("c", 512)}
	}
	for index := 0; index < MaxTurnsPerSession; index++ {
		if _, err := store.Append("session-growth", Turn{ID: fmt.Sprintf("turn-%d", index), Role: TurnAssistant, Content: strings.Repeat("m", MaxMessageBytes), ToolCalls: toolCalls, Citations: citations, Events: events}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := provider.ReadBlob(store.sessionBlobName("session-growth"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 5*1024*1024 {
		t.Fatalf("session blob = %d bytes, want <= 5 MiB", len(got))
	}
	session, err := store.Get("session-growth")
	if err != nil {
		t.Fatal(err)
	}
	compacted := false
	for _, turn := range session.Turns {
		if turn.Role == TurnCompaction {
			compacted = true
		}
		for _, event := range turn.Events {
			if event.Kind == "trace_compacted" {
				compacted = true
			}
		}
	}
	if !compacted {
		encodedEvents, _ := json.Marshal(events)
		encodedTools, _ := json.Marshal(toolCalls)
		t.Fatalf("100 worst-case turns did not exercise aggregate compaction: blob=%d events=%d tools=%d", len(got), len(encodedEvents), len(encodedTools))
	}
	last := session.Turns[len(session.Turns)-1].Events
	encoded, _ := json.Marshal(last)
	if len(encoded) > MaxEventBytesPerTurn || last[len(last)-1].Kind != core.ChatEventRunFinished {
		t.Fatalf("events bytes=%d last=%q", len(encoded), last[len(last)-1].Kind)
	}
	boundedTools, _ := json.Marshal(session.Turns[len(session.Turns)-1].ToolCalls)
	if len(boundedTools) > MaxToolBytesPerTurn {
		t.Fatalf("tool calls bytes=%d, want <=%d", len(boundedTools), MaxToolBytesPerTurn)
	}
}

func TestSessionStoreRejectsIrreducibleOversizeBeforeCommit(t *testing.T) {
	provider := storage.NewMemory()
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-oversize"); err != nil {
		t.Fatal(err)
	}
	before, err := provider.ReadBlob(store.sessionBlobName("session-oversize"))
	if err != nil {
		t.Fatal(err)
	}
	attachment := &core.ChatAttachment{Incident: &core.ChatIncidentContext{Title: strings.Repeat("s", MaxSessionBlobBytes+1)}}
	_, err = store.Append("session-oversize", Turn{ID: "oversize", Role: TurnAssistant, Content: "answer", Attachment: attachment})
	if !errors.Is(err, ErrSessionTooLarge) {
		t.Fatalf("append error = %v, want ErrSessionTooLarge", err)
	}
	after, readErr := provider.ReadBlob(store.sessionBlobName("session-oversize"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("oversized append changed the committed session blob")
	}
}

func TestSessionStoreCreateSurvivesFixedClockPruning(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	provider := storage.NewMemory()
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), func() time.Time { return now })
	for index := 0; index < MaxSessions+1; index++ {
		if _, err := store.Create(fmt.Sprintf("session-%03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Get("session-100"); err != nil {
		t.Fatalf("new session was pruned: %v", err)
	}
	if _, err := store.Get("session-000"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("oldest session error = %v, want not found", err)
	}
	current, err := provider.ReadBlob(store.sessionBlobName("session-001"))
	if err != nil {
		t.Fatal(err)
	}
	if swapped, err := provider.(storage.BlobCAS).CompareAndSwapBlob(store.sessionBlobName("session-001"), current, nil); err != nil || !swapped {
		t.Fatalf("remove indexed session swapped=%v err=%v", swapped, err)
	}
	now = now.Add(SessionRetention + time.Second)
	if _, err := store.Create("session-after-stale-index"); err != nil {
		t.Fatalf("create after indexed session blob disappeared: %v", err)
	}
}

func TestBoundsPreserveRunesWhitespaceAndTerminal(t *testing.T) {
	value := " \n世界abc"
	if got := capRawString(value, 6); got != " \n世" {
		t.Fatalf("bounded value = %q", got)
	}
	expandingPayload := strings.Repeat(`"`, MaxMessageBytes)
	expandingEvents := make([]core.ChatEvent, MaxEventsPerTurn)
	for index := range expandingEvents {
		expandingEvents[index] = core.ChatEvent{Kind: core.ChatEventModelDelta, CallID: expandingPayload, Delta: expandingPayload, Args: expandingPayload, Output: expandingPayload}
	}
	expandingEvents[len(expandingEvents)-1].Kind = core.ChatEventRunFinished
	expandingEncoded, _ := json.Marshal(boundEvents(expandingEvents))
	if len(expandingEncoded) > MaxEventBytesPerTurn {
		t.Fatalf("JSON-expanded events = %d bytes, want <= %d", len(expandingEncoded), MaxEventBytesPerTurn)
	}
	expandingCalls := []core.ToolCallTrace{
		{CallID: expandingPayload, Name: expandingPayload, Args: expandingPayload, Output: expandingPayload, Error: expandingPayload},
		{Name: strings.Repeat(`\\`, MaxMessageBytes), Args: strings.Repeat(`\\`, MaxMessageBytes), Output: strings.Repeat(`\\`, MaxMessageBytes), Error: strings.Repeat(`\\`, MaxMessageBytes)},
	}
	expandingToolsEncoded, _ := json.Marshal(boundToolCalls(expandingCalls))
	if len(expandingToolsEncoded) > MaxToolBytesPerTurn {
		t.Fatalf("JSON-expanded tool calls = %d bytes, want <= %d", len(expandingToolsEncoded), MaxToolBytesPerTurn)
	}
	if got := boundToolCalls([]core.ToolCallTrace{{CallID: "call-42"}})[0].CallID; got != "call-42" {
		t.Fatalf("bounded CallID = %q, want call-42", got)
	}
	events := make([]core.ChatEvent, MaxEventsPerTurn+20)
	for index := range events {
		events[index] = core.ChatEvent{Seq: int64(index + 1), Kind: core.ChatEventModelDelta, Delta: " \n"}
	}
	events[len(events)-1].Kind = core.ChatEventRunFinished
	bounded := boundEvents(events)
	if len(bounded) != MaxEventsPerTurn || bounded[MaxEventsPerTurn/2].Kind != "events_elided" || bounded[len(bounded)-1].Kind != core.ChatEventRunFinished {
		t.Fatalf("bounded events lost marker or terminal: first=%q middle=%q last=%q", bounded[0].Kind, bounded[MaxEventsPerTurn/2].Kind, bounded[len(bounded)-1].Kind)
	}
}

func TestSessionStorePostgresRoundTrip(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres chat session test")
	}
	dsn = isolatedPostgresDSN(t, dsn)
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	secondProvider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondProvider.Close() })
	orgID := fmt.Sprintf("chat-test-%d", time.Now().UnixNano())
	store := NewSessionStore(provider, tenancy.NewOrgScope(orgID), time.Now)
	if _, err := store.Create("session-postgres"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("session-postgres", Turn{ID: "turn", Role: TurnUser, Content: "durable"}); err != nil {
		t.Fatal(err)
	}
	got, err := NewSessionStore(provider, tenancy.NewOrgScope(orgID), time.Now).Get("session-postgres")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 || got.Turns[0].Content != "durable" {
		t.Fatalf("session = %+v", got)
	}
	if err := store.Delete("session-postgres"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("session-postgres"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("deleted session error = %v", err)
	}
	if _, err := store.Create("session-postgres"); err != nil {
		t.Fatalf("recreate after delete: %v", err)
	}
	epoch, acquired, err := store.AcquireLease("session-postgres", "owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire lease=%v err=%v", acquired, err)
	}
	if err := store.ReleaseLease("session-postgres", "owner-a", epoch); err != nil {
		t.Fatal(err)
	}
	secondStore := NewSessionStore(secondProvider, tenancy.NewOrgScope(orgID), time.Now)
	secondEpoch, acquired, err := secondStore.AcquireLease("session-postgres", "owner-b", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("second replica immediate acquire=%v err=%v", acquired, err)
	}
	if err := secondStore.ReleaseLease("session-postgres", "owner-b", secondEpoch); err != nil {
		t.Fatal(err)
	}
	leaseNow := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	leaseStore := NewSessionStore(provider, tenancy.NewOrgScope(orgID+"-lease-lifecycle"), func() time.Time { return leaseNow })
	if _, err := leaseStore.Create("session-fenced"); err != nil {
		t.Fatal(err)
	}
	staleEpoch, acquired, err := leaseStore.AcquireLease("session-fenced", "owner-a", time.Second)
	if err != nil || !acquired {
		t.Fatalf("postgres initial fenced lease acquired=%v err=%v", acquired, err)
	}
	leaseNow = leaseNow.Add(2 * time.Second)
	currentEpoch, acquired, err := leaseStore.AcquireLease("session-fenced", "owner-b", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("postgres replacement fenced lease acquired=%v err=%v", acquired, err)
	}
	if err := leaseStore.ReleaseLease("session-fenced", "owner-a", staleEpoch); !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("postgres stale lease release error=%v, want ErrLeaseFenced", err)
	}
	if active, err := leaseStore.LeaseActive("session-fenced"); err != nil || !active {
		t.Fatalf("postgres replacement lease active=%v err=%v", active, err)
	}
	if err := leaseStore.ReleaseLease("session-fenced", "owner-b", currentEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err := leaseStore.Create("session-expired-lease"); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := leaseStore.AcquireLease("session-expired-lease", "owner-a", time.Second); err != nil || !acquired {
		t.Fatalf("postgres expired lease acquired=%v err=%v", acquired, err)
	}
	leaseNow = leaseNow.Add(2 * time.Second)
	if err := leaseStore.Delete("session-expired-lease"); err != nil {
		t.Fatal(err)
	}
	lease, err := provider.ReadBlob(leaseStore.leaseBlobName("session-expired-lease"))
	if err != nil || lease != nil {
		t.Fatalf("postgres expired lease after delete=%s err=%v", lease, err)
	}
	concurrentScope := tenancy.NewOrgScope(orgID + "-concurrent")
	concurrentA := NewSessionStore(provider, concurrentScope, time.Now)
	concurrentB := NewSessionStore(secondProvider, concurrentScope, time.Now)
	if _, err := concurrentA.Create("session-concurrent"); err != nil {
		t.Fatal(err)
	}
	const postgresAppendCount = 40
	var wait sync.WaitGroup
	for index := 0; index < postgresAppendCount; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			store := concurrentA
			if index%2 == 1 {
				store = concurrentB
			}
			if _, err := store.Append("session-concurrent", Turn{ID: fmt.Sprintf("turn-%d", index), Role: TurnUser, Content: "message"}); err != nil {
				t.Errorf("postgres append: %v", err)
			}
		}(index)
	}
	wait.Wait()
	concurrentSession, err := concurrentB.Get("session-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if len(concurrentSession.Turns) != postgresAppendCount {
		t.Fatalf("postgres concurrent turns=%d, want %d", len(concurrentSession.Turns), postgresAppendCount)
	}
	serviceScope := tenancy.NewOrgScope(orgID + "-services")
	serviceA := NewService(NewSessionStore(provider, serviceScope, time.Now), fixedRunner{}, nil, time.Now)
	serviceB := NewService(NewSessionStore(secondProvider, serviceScope, time.Now), fixedRunner{}, nil, time.Now)
	serviceSession, err := serviceA.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serviceA.Send(context.Background(), serviceSession.ID, "first", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceB.Send(context.Background(), serviceSession.ID, "second", nil); err != nil {
		t.Fatalf("postgres second service immediate reacquire: %v", err)
	}
	cancelRunner := &blockingRunner{started: make(chan struct{})}
	cancelA := NewService(NewSessionStore(provider, serviceScope, time.Now), cancelRunner, nil, time.Now)
	cancelA.leaseRenewal = 5 * time.Millisecond
	cancelB := NewService(NewSessionStore(secondProvider, serviceScope, time.Now), fixedRunner{}, nil, time.Now)
	cancelSession, err := cancelA.Create()
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := cancelA.Start(context.Background(), cancelSession.ID, "block", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-cancelRunner.started
	if err := cancelB.Delete(cancelSession.ID); !errors.Is(err, ErrRunActive) {
		t.Fatalf("postgres cross-replica active delete error=%v, want ErrRunActive", err)
	}
	if err := cancelB.Cancel(cancelSession.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-outcomes:
		if !errors.Is(outcome.Err, context.Canceled) {
			t.Fatalf("postgres cross-replica cancel error=%v", outcome.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("postgres lease owner did not observe cross-replica cancellation")
	}
	if err := cancelB.Delete(cancelSession.ID); err != nil {
		t.Fatalf("postgres cross-replica delete after cancellation: %v", err)
	}
	if _, err := cancelA.Get(cancelSession.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("postgres deleted session error=%v, want ErrSessionNotFound", err)
	}

	evictionStore := NewSessionStore(provider, tenancy.NewOrgScope(orgID+"-eviction"), func() time.Time {
		return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	})
	for index := 0; index < MaxSessions+1; index++ {
		if _, err := evictionStore.Create(fmt.Sprintf("session-%03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := evictionStore.Get("session-000"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("postgres eviction error = %v", err)
	}

	staleNow := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	staleStore := NewSessionStore(provider, tenancy.NewOrgScope(orgID+"-stale-index"), func() time.Time { return staleNow })
	for index := 0; index < MaxSessions+1; index++ {
		if _, err := staleStore.Create(fmt.Sprintf("session-%03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	current, err := provider.ReadBlob(staleStore.sessionBlobName("session-001"))
	if err != nil {
		t.Fatal(err)
	}
	if swapped, err := provider.(storage.BlobCAS).CompareAndSwapBlob(staleStore.sessionBlobName("session-001"), current, nil); err != nil || !swapped {
		t.Fatalf("postgres remove indexed session swapped=%v err=%v", swapped, err)
	}
	staleNow = staleNow.Add(SessionRetention + time.Second)
	if _, err := staleStore.Create("session-after-stale-index"); err != nil {
		t.Fatalf("postgres create after indexed session disappeared: %v", err)
	}
	listed, err := staleStore.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range listed {
		if session.ID == "session-001" {
			t.Fatal("postgres stale index entry survived pruning")
		}
	}
}

func isolatedPostgresDSN(t *testing.T, dsn string) string {
	t.Helper()
	schema := fmt.Sprintf("chat_test_%d", time.Now().UnixNano())
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres for isolated schema: %v", err)
	}
	if _, err := db.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		db.Close()
		t.Fatalf("create isolated postgres schema: %v", err)
	}
	db.Close()
	t.Cleanup(func() {
		cleanupDB, openErr := sql.Open("pgx", dsn)
		if openErr != nil {
			t.Errorf("open postgres for schema cleanup: %v", openErr)
			return
		}
		defer cleanupDB.Close()
		if _, dropErr := cleanupDB.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`); dropErr != nil {
			t.Errorf("drop isolated postgres schema: %v", dropErr)
		}
	})
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse postgres DSN: %v", err)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return dsn + " search_path=" + schema
}

func TestSessionStoreCompactionMarkerIsPersisted(t *testing.T) {
	store := NewSessionStore(storage.NewMemory(), tenancy.DefaultOrgScope(), time.Now)
	if _, err := store.Create("session-compact"); err != nil {
		t.Fatal(err)
	}
	turns := make([]Turn, MaxTurnsPerSession+5)
	for index := range turns {
		turns[index] = Turn{ID: fmt.Sprintf("turn-%d", index), Role: TurnUser, Content: "message"}
	}
	got, err := store.Append("session-compact", turns...)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != MaxTurnsPerSession {
		t.Fatalf("turns = %d, want %d", len(got.Turns), MaxTurnsPerSession)
	}
	if got.Turns[0].Role != TurnCompaction || got.Turns[0].Content == "" {
		t.Fatalf("first turn = %+v, want visible compaction marker", got.Turns[0])
	}
	reloaded, err := store.Get("session-compact")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Turns[0].Role != TurnCompaction || reloaded.Turns[0].Content != got.Turns[0].Content {
		t.Fatalf("reloaded marker = %+v, want %+v", reloaded.Turns[0], got.Turns[0])
	}
}
