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
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// The reviewer's compressed 30-second re-windowing probe, strengthened to eight
// concurrent viewers and assertions on every refresh rather than a logged 429.
func TestTimelineReservationRollingViewersReclaimPreviewAndPreservePinnedCursor(t *testing.T) {
	service, provider, base := timelineReservationFixture(t)
	pinnedActor := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "viewer-0"})
	first, err := service.reservationFixturePage(pinnedActor, base)
	if err != nil || first.Next == "" {
		t.Fatalf("first=%+v %v", first, err)
	}
	pinnedQuery := base
	pinnedQuery.Cursor = first.Next
	pinnedPage, err := service.reservationFixturePage(pinnedActor, pinnedQuery)
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
				page, err := service.reservationFixturePage(ctx, query)
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
	again, err := service.reservationFixturePage(pinnedActor, pinnedQuery)
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

func TestTimelineReservationReplacedUnconsumedCursorRequiresExplicitRestart(t *testing.T) {
	service, _, query := timelineReservationFixture(t)
	ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "same-viewer"})
	first, err := service.reservationFixturePage(ctx, query)
	if err != nil {
		t.Fatal(err)
	}

	refresh := query
	refresh.Until = refresh.Until.Add(time.Millisecond)
	next, err := service.reservationFixturePage(ctx, refresh)
	if err != nil || next.Next == "" {
		t.Fatalf("refresh=%+v %v", next, err)
	}
	query.Cursor = first.Next
	if _, err := service.reservationFixturePage(ctx, query); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("obsolete preview did not require restart: %v", err)
	}
}

func TestTimelineReservationAnonymousTabsSharePreviewButNotPinnedInvalidation(t *testing.T) {
	service, provider, query := timelineReservationFixture(t)
	old, err := service.reservationFixturePage(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	refresh := query
	refresh.Until = refresh.Until.Add(time.Millisecond)
	current, err := service.reservationFixturePage(t.Context(), refresh)
	if err != nil || current.Next == "" {
		t.Fatalf("refresh=%+v %v", current, err)
	}
	oldQuery := query
	oldQuery.Cursor = old.Next
	if _, err := service.reservationFixturePage(t.Context(), oldQuery); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("other tab's obsolete preview accepted: %v", err)
	}
	pinned := refresh
	pinned.Cursor = current.Next
	expected, err := service.reservationFixturePage(t.Context(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		refresh.Until = refresh.Until.Add(time.Millisecond)
		untrusted := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Actor: fmt.Sprintf("not-authenticated-%d", i)})
		page, err := service.reservationFixturePage(untrusted, refresh)
		if err != nil || page.Next == "" {
			t.Fatalf("anonymous rolling refresh=%+v %v", page, err)
		}
	}
	got, err := service.reservationFixturePage(t.Context(), pinned)
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

func TestTimelineReservationPinAndRefreshRaceUsesOnePublicationFence(t *testing.T) {
	for _, pinWins := range []bool{true, false} {
		t.Run(fmt.Sprint(pinWins), func(t *testing.T) {
			service, base, query := timelineReservationFixture(t)
			ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "viewer"})
			first, err := service.reservationFixturePage(ctx, query)
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
			pagination := query
			pagination.Cursor = first.Next
			refresh := query
			refresh.Until = refresh.Until.Add(time.Millisecond)
			result := make(chan error, 1)
			go func() {
				if pinWins {
					_, err := service.reservationFixturePage(ctx, refresh)
					result <- err
				} else {
					_, err := service.reservationFixturePage(ctx, pagination)
					result <- err
				}
			}()
			select {
			case <-barrier.started:
			case <-time.After(time.Second):
				t.Fatal("CAS race barrier not reached")
			}
			if pinWins {
				if _, err := replica.reservationFixturePage(ctx, pagination); err != nil {
					t.Fatal(err)
				}
			} else if page, err := replica.reservationFixturePage(ctx, refresh); err != nil || page.Next == "" {
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
			_, err = replica.reservationFixturePage(ctx, pagination)
			if pinWins && err != nil {
				t.Fatalf("pinned cursor reclaimed: %v", err)
			}
			if !pinWins && !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("replaced preview stayed consumable: %v", err)
			}
		})
	}
}

func TestTimelineReservationPinnedAdmissionIsExplicitAndNeverDropsNext(t *testing.T) {
	service, _, query := timelineReservationFixture(t)
	ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "viewer"})
	var pinned TimelineQuery
	for i := 0; i < timelineRequesterSnapshots; i++ {
		query.Until = query.Until.Add(time.Millisecond)
		page, err := service.reservationFixturePage(ctx, query)
		if err != nil || page.Next == "" {
			t.Fatalf("pin %d: %+v %v", i, page, err)
		}
		pinned = query
		pinned.Cursor = page.Next
		if _, err := service.reservationFixturePage(ctx, pinned); err != nil {
			t.Fatal(err)
		}
	}
	query.Until = query.Until.Add(time.Millisecond)
	if _, err := service.reservationFixturePage(ctx, query); !errors.Is(err, ErrIndexStreamBusy) {
		t.Fatalf("pinned overload silently returned truncated first page: %v", err)
	}
	if _, err := service.reservationFixturePage(ctx, pinned); err != nil {
		t.Fatalf("admission blocked existing pagination: %v", err)
	}
}

func TestTimelineSyncingHistoryStillHasContinuationWhenNeeded(t *testing.T) {
	service, _, store, scope, reads := timelineHistoryFixture(t)
	now := time.Now().UTC()
	commitTimelineHistory(t, store, scope, storage.EvidenceBatch{ID: "syncing-history", At: now, Epoch: "fixture", Records: []storage.EvidenceRecord{
		timelineChangeRecord(Change{ID: "one", UID: "one", Kind: "Pod", Type: kubechanges.Created, At: now.Add(-time.Minute)}),
		timelineChangeRecord(Change{ID: "two", UID: "two", Kind: "Pod", Type: kubechanges.Created, At: now.Add(-2 * time.Minute)}),
	}})
	seedInventory(t, service, nil)
	service.changes.inventories[service.indexScope()].generation.View.Kinds = nil
	query := TimelineQuery{Since: now.Add(-time.Hour), Until: now, Limit: 1}
	before := reads.Load()
	page, err := service.Timeline(context.Background(), query)
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Counts["change"] != 2 || page.CountCapped || page.Total <= len(page.Items) || page.Next == "" || len(page.Gaps) != 0 {
		t.Fatalf("syncing silently truncated retained history: %+v %v", page, err)
	}
	// Committed history reports checkpoint authority, not inventory loading.
	if page.Authority != "current" || page.Sync.State != "ready" || page.Sync.Partial || len(page.Sync.Pending) != 0 || page.Sync.PendingKinds != 0 {
		t.Fatalf("inventory loading changed committed history freshness: %+v", page)
	}
	query.Cursor = page.Next
	next, err := service.Timeline(context.Background(), query)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID || next.Next != "" || next.Total != 0 || next.Counts["change"] != 0 || next.CountCapped || len(next.Gaps) != 0 {
		t.Fatalf("syncing continuation lost retained membership: %+v %v", next, err)
	}
	if next.Authority != "current" || next.Sync.State != "ready" || next.Sync.Partial || len(next.Sync.Pending) != 0 || next.Sync.PendingKinds != 0 {
		t.Fatalf("inventory loading changed continuation freshness: %+v", next)
	}
	if reads.Load() != before {
		t.Fatal("syncing history called the cluster")
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

func TestTimelineReservationIdenticalTabsWaitForReservedPayloadPublication(t *testing.T) {
	first, base, query := timelineReservationFixture(t)
	barrier := &timelinePublicationBarrier{Provider: base, started: make(chan struct{}), release: make(chan struct{}), pending: make(chan struct{}, 1)}
	barrier.armed.Store(true)
	first.SetChangeStorage(barrier)
	second := NewService(nil, first.Scope(), 0)
	second.SetChangeStorage(barrier)
	ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "same-viewer"})
	type result struct {
		page TimelinePage
		err  error
	}
	a, b := make(chan result, 1), make(chan result, 1)
	go func() { page, err := first.reservationFixturePage(ctx, query); a <- result{page, err} }()
	select {
	case <-barrier.started:
	case <-time.After(time.Second):
		t.Fatal("publisher did not reserve before payload")
	}
	go func() { page, err := second.reservationFixturePage(ctx, query); b <- result{page, err} }()
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
func TestTimelineReservationWriteContentionIsCancellableAndNotCapacity(t *testing.T) {
	service, base, query := timelineReservationFixture(t)
	service.SetChangeStorage(timelineAlwaysContended{base})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := service.reservationFixturePage(ctx, query); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contention ignored cancellation: %v", err)
	}
	detail := DiagnoseError(ErrTimelineContention)
	if detail.Code == "stream_busy" || !detail.Retryable {
		t.Fatalf("write contention misreported capacity: %+v", detail)
	}
}
