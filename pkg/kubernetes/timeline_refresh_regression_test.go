package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// The reviewer's compressed 30-second re-windowing probe, strengthened to eight
// concurrent viewers and assertions on every refresh rather than a logged 429.
func TestTimelineRollingViewersReclaimPreviewAndPreservePinnedCursor(t *testing.T) {
	service, provider, base := timelineAdmissionFixture(t)
	pinnedActor := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "viewer-0"})
	first, err := service.Timeline(pinnedActor, base)
	if err != nil || first.Next == "" {
		t.Fatalf("first=%+v %v", first, err)
	}
	pinnedQuery := base
	pinnedQuery.Cursor = first.Next
	pinnedPage, err := service.Timeline(pinnedActor, pinnedQuery)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	failures := make(chan error, 8)
	var workers sync.WaitGroup
	for user := 0; user < 8; user++ {
		workers.Add(1)
		go func(user int) {
			defer workers.Done()
			ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: fmt.Sprintf("viewer-%d", user)})
			<-start
			for tick := 0; tick < 20; tick++ {
				query := base
				query.Until = base.Until.Add(time.Duration(user*20+tick+1) * time.Millisecond)
				page, err := service.Timeline(ctx, query)
				if err != nil || page.Next == "" {
					failures <- fmt.Errorf("viewer%d tick%d lost pagination: next=%q err=%v", user, tick, page.Next, err)
					return
				}
			}
		}(user)
	}
	close(start)
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	again, err := service.Timeline(pinnedActor, pinnedQuery)
	if err != nil || len(again.Items) != len(pinnedPage.Items) || again.Items[0].ID != pinnedPage.Items[0].ID {
		t.Fatalf("refresh reclaimed pinned data: %+v %v", again, err)
	}
	raw, err := provider.ReadBlob(service.timelineKey(0) + "/index")
	if err != nil {
		t.Fatal(err)
	}
	var manifest timelineManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, slot := range manifest.Slots {
		if slot.ID != "" && slot.Expires.After(time.Now()) {
			active++
		}
	}
	if active > 9 {
		t.Fatalf("rolling previews accumulated instead of replacing: %d", active)
	}
}

func TestTimelineReplacedUnconsumedCursorRequiresExplicitRestart(t *testing.T) {
	service, _, query := timelineAdmissionFixture(t)
	ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "same-viewer"})
	first, err := service.Timeline(ctx, query)
	if err != nil {
		t.Fatal(err)
	}

	refresh := query
	refresh.Until = refresh.Until.Add(time.Millisecond)
	next, err := service.Timeline(ctx, refresh)
	if err != nil || next.Next == "" {
		t.Fatalf("refresh=%+v %v", next, err)
	}
	query.Cursor = first.Next
	if _, err := service.Timeline(ctx, query); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("obsolete preview did not require restart: %v", err)
	}
}

func TestTimelineAnonymousTabsSharePreviewButNotPinnedInvalidation(t *testing.T) {
	service, provider, query := timelineAdmissionFixture(t)
	old, err := service.Timeline(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	refresh := query
	refresh.Until = refresh.Until.Add(time.Millisecond)
	current, err := service.Timeline(t.Context(), refresh)
	if err != nil || current.Next == "" {
		t.Fatalf("refresh=%+v %v", current, err)
	}
	oldQuery := query
	oldQuery.Cursor = old.Next
	if _, err := service.Timeline(t.Context(), oldQuery); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("other tab's obsolete preview accepted: %v", err)
	}
	pinned := refresh
	pinned.Cursor = current.Next
	expected, err := service.Timeline(t.Context(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		refresh.Until = refresh.Until.Add(time.Millisecond)
		untrusted := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Actor: fmt.Sprintf("not-authenticated-%d", i)})
		page, err := service.Timeline(untrusted, refresh)
		if err != nil || page.Next == "" {
			t.Fatalf("anonymous rolling refresh=%+v %v", page, err)
		}
	}
	got, err := service.Timeline(t.Context(), pinned)
	if err != nil || got.Items[0].ID != expected.Items[0].ID {
		t.Fatalf("anonymous refresh invalidated active pagination: %+v %v", got, err)
	}
	raw, _ := provider.ReadBlob(service.timelineKey(0) + "/index")
	var manifest timelineManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, entry := range manifest.Slots {
		if entry.ID != "" && entry.Expires.After(time.Now()) {
			active++
		}
	}
	if active != 2 {
		t.Fatalf("anonymous caller-chosen actors created buckets: %d", active)
	}
}

type timelineCASBarrier struct {
	storage.Provider
	key, id          string
	slot             int
	pin              bool
	armed            atomic.Bool
	started, release chan struct{}
}

func (barrier *timelineCASBarrier) CompareAndSwapBlob(key string, old, replacement []byte) (bool, error) {
	var manifest timelineManifest
	if key == barrier.key && json.Unmarshal(replacement, &manifest) == nil {
		entry := manifest.Slots[barrier.slot]
		matches := barrier.pin && entry.ID == barrier.id && entry.Pinned || !barrier.pin && entry.ID != barrier.id
		if matches && barrier.armed.CompareAndSwap(true, false) {
			close(barrier.started)
			<-barrier.release
		}
	}
	return barrier.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, old, replacement)
}

func TestTimelinePinAndRefreshRaceUsesOnePublicationFence(t *testing.T) {
	for _, pinWins := range []bool{true, false} {
		t.Run(fmt.Sprint(pinWins), func(t *testing.T) {
			service, base, query := timelineAdmissionFixture(t)
			ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "viewer"})
			first, err := service.Timeline(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			var cursor timelineCursor
			raw, _ := base64.RawURLEncoding.DecodeString(first.Next)
			if err := json.Unmarshal(raw, &cursor); err != nil {
				t.Fatal(err)
			}
			barrier := &timelineCASBarrier{Provider: base, key: service.timelineKey(0) + "/index", id: cursor.ID, slot: cursor.Slot, pin: !pinWins, started: make(chan struct{}), release: make(chan struct{})}
			barrier.armed.Store(true)
			service.SetChangeStorage(barrier)
			replica := NewService(nil, service.Scope(), 0)
			replica.SetChangeStorage(base)
			seedInventory(t, replica, nil)
			pagination := query
			pagination.Cursor = first.Next
			refresh := query
			refresh.Until = refresh.Until.Add(time.Millisecond)
			result := make(chan error, 1)
			go func() {
				if pinWins {
					_, err := service.Timeline(ctx, refresh)
					result <- err
				} else {
					_, err := service.Timeline(ctx, pagination)
					result <- err
				}
			}()
			select {
			case <-barrier.started:
			case <-time.After(time.Second):
				t.Fatal("CAS race barrier not reached")
			}
			if pinWins {
				if _, err := replica.Timeline(ctx, pagination); err != nil {
					t.Fatal(err)
				}
			} else if page, err := replica.Timeline(ctx, refresh); err != nil || page.Next == "" {
				t.Fatalf("refresh=%+v %v", page, err)
			}
			close(barrier.release)
			select {
			case err := <-result:
				if pinWins && err != nil {
					t.Fatalf("refresh could not preserve a racing pin: %v", err)
				}
				if !pinWins && !errors.Is(err, ErrInvalidArguments) {
					t.Fatalf("lost consume race returned wrong/partial data: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("CAS race did not finish")
			}
			_, err = replica.Timeline(ctx, pagination)
			if pinWins && err != nil {
				t.Fatalf("pinned cursor reclaimed: %v", err)
			}
			if !pinWins && !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("replaced preview stayed consumable: %v", err)
			}
		})
	}
}

func TestTimelinePinnedAdmissionIsExplicitAndNeverDropsNext(t *testing.T) {
	service, _, query := timelineAdmissionFixture(t)
	ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "viewer"})
	var pinned TimelineQuery
	for i := 0; i < timelineRequesterSnapshots; i++ {
		query.Until = query.Until.Add(time.Millisecond)
		page, err := service.Timeline(ctx, query)
		if err != nil || page.Next == "" {
			t.Fatalf("pin %d: %+v %v", i, page, err)
		}
		pinned = query
		pinned.Cursor = page.Next
		if _, err := service.Timeline(ctx, pinned); err != nil {
			t.Fatal(err)
		}
	}
	query.Until = query.Until.Add(time.Millisecond)
	if _, err := service.Timeline(ctx, query); !errors.Is(err, ErrIndexStreamBusy) {
		t.Fatalf("pinned overload silently returned truncated first page: %v", err)
	}
	if _, err := service.Timeline(ctx, pinned); err != nil {
		t.Fatalf("admission blocked existing pagination: %v", err)
	}
}

func TestTimelineSyncingHistoryStillHasContinuationWhenNeeded(t *testing.T) {
	service, _, query := timelineAdmissionFixture(t)
	service.changes.inventories[service.indexScope()].generation.View.Kinds = nil
	page, err := service.Timeline(context.Background(), query)
	if err != nil || page.Total <= len(page.Items) || page.Next == "" {
		t.Fatalf("syncing silently truncated retained history: %+v %v", page, err)
	}

}

type timelinePublicationBarrier struct {
	storage.Provider
	armed                     atomic.Bool
	started, release, pending chan struct{}
}

func (barrier *timelinePublicationBarrier) WriteBlob(key string, value []byte) error {
	if strings.HasPrefix(key, "kubernetes/timeline/") && !strings.HasSuffix(key, "/index") && barrier.armed.CompareAndSwap(true, false) {
		close(barrier.started)
		<-barrier.release
	}
	return barrier.Provider.WriteBlob(key, value)
}
func (barrier *timelinePublicationBarrier) ReadBlob(key string) ([]byte, error) {
	value, err := barrier.Provider.ReadBlob(key)
	if strings.HasPrefix(key, "kubernetes/timeline/") && !strings.HasSuffix(key, "/index") && len(value) == 0 {
		select {
		case barrier.pending <- struct{}{}:
		default:
		}
	}
	return value, err
}
func (barrier *timelinePublicationBarrier) CompareAndSwapBlob(key string, old, replacement []byte) (bool, error) {
	return barrier.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, old, replacement)
}

func TestTimelineIdenticalTabsWaitForReservedPayloadPublication(t *testing.T) {
	first, base, query := timelineAdmissionFixture(t)
	barrier := &timelinePublicationBarrier{Provider: base, started: make(chan struct{}), release: make(chan struct{}), pending: make(chan struct{}, 1)}
	barrier.armed.Store(true)
	first.SetChangeStorage(barrier)
	second := NewService(nil, first.Scope(), 0)
	second.SetChangeStorage(barrier)
	seedInventory(t, second, nil)
	first.changes.inventories[first.indexScope()].generation.Revision = "one-generation"
	second.changes.inventories[second.indexScope()].generation.Revision = "one-generation"
	ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "same-viewer"})
	type result struct {
		page TimelinePage
		err  error
	}
	a, b := make(chan result, 1), make(chan result, 1)
	go func() { page, err := first.Timeline(ctx, query); a <- result{page, err} }()
	select {
	case <-barrier.started:
	case <-time.After(time.Second):
		t.Fatal("publisher did not reserve before payload")
	}
	go func() { page, err := second.Timeline(ctx, query); b <- result{page, err} }()
	select {
	case <-barrier.pending:
	case <-time.After(time.Second):
		t.Fatal("identical reader did not reach pending publication")
	}
	select {
	case got := <-b:
		t.Fatalf("pending publication was exposed as capacity/data error: %+v", got)
	default:
	}
	close(barrier.release)
	var one, two result
	select {
	case one = <-a:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not finish")
	}
	select {
	case two = <-b:
	case <-time.After(2 * time.Second):
		t.Fatal("pending reader did not retry publication")
	}
	if one.err != nil || two.err != nil || one.page.Next == "" || one.page.Next != two.page.Next {
		t.Fatalf("identical tabs did not share committed frame: first=%+v second=%+v", one, two)
	}
}

type timelineAlwaysContended struct{ storage.Provider }

func (timelineAlwaysContended) CompareAndSwapBlob(string, []byte, []byte) (bool, error) {
	return false, nil
}
func TestTimelineWriteContentionIsCancellableAndNotCapacity(t *testing.T) {
	service, base, query := timelineAdmissionFixture(t)
	service.SetChangeStorage(timelineAlwaysContended{base})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := service.Timeline(ctx, query); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contention ignored cancellation: %v", err)
	}
	detail := DiagnoseError(ErrTimelineContention)
	if detail.Code == "stream_busy" || !detail.Retryable {
		t.Fatalf("write contention misreported capacity: %+v", detail)
	}
}
