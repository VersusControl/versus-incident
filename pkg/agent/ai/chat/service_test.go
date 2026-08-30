package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type blockingRunner struct {
	started chan struct{}
	once    sync.Once
}

func (runner *blockingRunner) RunChat(ctx context.Context, _ core.ChatTask) (*core.ChatTurnResult, error) {
	runner.once.Do(func() { close(runner.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

type fixedRunner struct{}

func (fixedRunner) RunChat(_ context.Context, _ core.ChatTask) (*core.ChatTurnResult, error) {
	return &core.ChatTurnResult{Markdown: "**Validated**\nHealthy [tool-call-1]", Citations: []core.ChatCitation{{Tool: "get_system_overview"}}}, nil
}

type panicRunner struct{}

func (panicRunner) RunChat(context.Context, core.ChatTask) (*core.ChatTurnResult, error) {
	panic("provider panic")
}

type nilRunner struct{}

func (nilRunner) RunChat(context.Context, core.ChatTask) (*core.ChatTurnResult, error) {
	return nil, nil
}

type throttledRunner struct{}

func (throttledRunner) RunChat(context.Context, core.ChatTask) (*core.ChatTurnResult, error) {
	return nil, router.ErrRateLimited
}

type captureTaskRunner struct{ task core.ChatTask }

func (runner *captureTaskRunner) RunChat(_ context.Context, task core.ChatTask) (*core.ChatTurnResult, error) {
	runner.task = task
	return &core.ChatTurnResult{Markdown: "answer"}, nil
}

type countingSeeder struct{ calls int }

func (seeder *countingSeeder) Seed(context.Context) []core.ToolCallTrace {
	seeder.calls++
	return []core.ToolCallTrace{{Name: "get_system_overview", Output: `{"found":true}`}}
}

func newTestService(t *testing.T, runner TurnRunner) (*Service, string) {
	t.Helper()
	store := NewSessionStore(storage.NewMemory(), tenancy.DefaultOrgScope(), time.Now)
	service := NewService(store, runner, nil, time.Now)
	session, err := service.Create()
	if err != nil {
		t.Fatal(err)
	}
	return service, session.ID
}

func TestServicePersistsUserAndAssistantTurns(t *testing.T) {
	service, id := newTestService(t, fixedRunner{})
	result, err := service.Send(context.Background(), id, "is it healthy?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Markdown == "" {
		t.Fatal("empty result")
	}
	session, err := service.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Turns) != 2 || session.Turns[0].Role != TurnUser || session.Turns[1].Role != TurnAssistant {
		t.Fatalf("turns = %+v", session.Turns)
	}
	if session.Status != SessionIdle {
		t.Fatalf("status = %q", session.Status)
	}
}

func TestServiceRejectsConcurrentRunAndCancelStopsWork(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{})}
	service, id := newTestService(t, runner)
	done := make(chan error, 1)
	go func() {
		_, err := service.Send(context.Background(), id, "wait", nil)
		done <- err
	}()
	<-runner.started
	if _, err := service.Send(context.Background(), id, "second", nil); !errors.Is(err, ErrRunActive) {
		t.Fatalf("concurrent Send error = %v, want ErrRunActive", err)
	}
	if err := service.Cancel(id); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop running model")
	}
}

func TestServiceDurableLeaseRejectsSecondInstance(t *testing.T) {
	provider := storage.NewMemory()
	storeA := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	storeB := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	runner := &blockingRunner{started: make(chan struct{})}
	serviceA := NewService(storeA, runner, nil, time.Now)
	serviceB := NewService(storeB, fixedRunner{}, nil, time.Now)
	session, err := serviceA.Create()
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := serviceA.Start(context.Background(), session.ID, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	if _, err := serviceB.Start(context.Background(), session.ID, "second", nil); !errors.Is(err, ErrRunActive) {
		t.Fatalf("second instance error = %v, want ErrRunActive", err)
	}
	if err := serviceA.Cancel(session.ID); err != nil {
		t.Fatal(err)
	}
	<-outcomes
}

func TestServiceReleaseReportsFencingWithoutDeletingNewerLease(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), func() time.Time { return now })
	service := NewService(store, fixedRunner{}, nil, func() time.Time { return now })
	epoch, acquired, err := store.AcquireLease("session-fenced", service.owner, time.Second)
	if err != nil || !acquired {
		t.Fatalf("initial lease acquired=%v err=%v", acquired, err)
	}
	_, cancel := context.WithCancel(context.Background())
	service.active["session-fenced"] = activeRun{cancel: cancel, epoch: epoch}
	now = now.Add(2 * time.Second)
	newEpoch, acquired, err := store.AcquireLease("session-fenced", "new-owner", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("replacement lease acquired=%v err=%v", acquired, err)
	}
	if err := service.release("session-fenced"); !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("stale service release error=%v, want ErrLeaseFenced", err)
	}
	active, err := store.LeaseActive("session-fenced")
	if err != nil || !active {
		t.Fatalf("replacement lease active=%v err=%v", active, err)
	}
	if err := store.ReleaseLease("session-fenced", "new-owner", newEpoch); err != nil {
		t.Fatalf("release replacement lease: %v", err)
	}
}

func TestServiceCrossReplicaCancelAndDelete(t *testing.T) {
	provider := storage.NewMemory()
	storeA := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	storeB := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	runner := &blockingRunner{started: make(chan struct{})}
	serviceA := NewService(storeA, runner, nil, time.Now)
	serviceA.leaseRenewal = 5 * time.Millisecond
	serviceB := NewService(storeB, fixedRunner{}, nil, time.Now)
	session, err := serviceA.Create()
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := serviceA.Start(context.Background(), session.ID, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	if err := serviceB.Delete(session.ID); !errors.Is(err, ErrRunActive) {
		t.Fatalf("cross-replica delete error = %v, want active", err)
	}
	if err := serviceB.Cancel(session.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-outcomes:
		if !errors.Is(outcome.Err, context.Canceled) {
			t.Fatalf("cross-replica cancel error = %v", outcome.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("lease owner did not observe durable cancellation")
	}
	got, err := serviceB.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, turn := range got.Turns {
		for _, event := range turn.Events {
			if event.Kind == core.ChatEventRunFinished || event.Kind == core.ChatEventRunFailed || event.Kind == core.ChatEventRunCancelled || event.Kind == "run_throttled" {
				terminals++
			}
		}
	}
	if terminals != 1 || got.Status != SessionFailed {
		t.Fatalf("terminals=%d status=%q", terminals, got.Status)
	}
}

func TestServiceSecondReplicaReacquiresImmediatelyAfterRelease(t *testing.T) {
	provider := storage.NewMemory()
	storeA := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	storeB := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	serviceA := NewService(storeA, fixedRunner{}, nil, time.Now)
	serviceB := NewService(storeB, fixedRunner{}, nil, time.Now)
	session, err := serviceA.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serviceA.Send(context.Background(), session.ID, "first", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceB.Send(context.Background(), session.ID, "second", nil); err != nil {
		t.Fatalf("second replica did not reacquire immediately: %v", err)
	}
}

func TestServiceHardSignalsLeaseReleaseFailure(t *testing.T) {
	provider := &observedProvider{Provider: storage.NewMemory(), failLeaseDelete: true}
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), time.Now)
	service := NewService(store, fixedRunner{}, nil, time.Now)
	session, err := service.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), session.ID, "run", nil); err == nil || !strings.Contains(err.Error(), "release lease") {
		t.Fatalf("release error = %v", err)
	}
}

func TestServiceSeedsSessionOnlyOnce(t *testing.T) {
	store := NewSessionStore(storage.NewMemory(), tenancy.DefaultOrgScope(), time.Now)
	seeder := &countingSeeder{}
	service := NewService(store, fixedRunner{}, seeder, time.Now)
	session, err := service.Create()
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"first", "second"} {
		if _, err := service.Send(context.Background(), session.ID, message, nil); err != nil {
			t.Fatal(err)
		}
	}
	if seeder.calls != 1 {
		t.Fatalf("seed calls = %d, want 1", seeder.calls)
	}
	got, err := service.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Seeded || len(got.Turns) != 5 || got.Turns[0].Role != TurnCompaction {
		t.Fatalf("seeded session = %+v", got)
	}
}

func TestServiceDisconnectDoesNotCancelDetachedRun(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{})}
	service, id := newTestService(t, runner)
	requestCtx, disconnect := context.WithCancel(context.Background())
	outcomes, err := service.Start(requestCtx, id, "keep running", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	disconnect()
	select {
	case outcome := <-outcomes:
		t.Fatalf("request disconnect stopped detached run: %v", outcome.Err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := service.Cancel(id); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-outcomes:
		if !errors.Is(outcome.Err, context.Canceled) {
			t.Fatalf("explicit cancel error = %v, want canceled", outcome.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit cancel did not stop detached run")
	}
}

func TestServiceOverallTimeoutReleasesSession(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{})}
	service, id := newTestService(t, runner)
	service.runTimeout = 20 * time.Millisecond
	outcomes, err := service.Start(context.Background(), id, "wait", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	select {
	case outcome := <-outcomes:
		if !errors.Is(outcome.Err, context.DeadlineExceeded) {
			t.Fatalf("timeout error = %v", outcome.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("overall deadline did not stop run")
	}
	if _, err := service.Send(context.Background(), id, "after timeout", nil); err != nil && errors.Is(err, ErrRunActive) {
		t.Fatalf("session remained active: %v", err)
	}
}

func TestServiceRecoversDetachedPanicAndNilResult(t *testing.T) {
	for _, runner := range []TurnRunner{panicRunner{}, nilRunner{}} {
		service, id := newTestService(t, runner)
		outcomes, err := service.Start(context.Background(), id, "run", nil)
		if err != nil {
			t.Fatal(err)
		}
		outcome := <-outcomes
		if outcome.Err == nil {
			t.Fatal("expected safe run failure")
		}
		session, err := service.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if session.Status != SessionFailed {
			t.Fatalf("status = %q, want failed", session.Status)
		}
	}
}

func TestServiceThrottlingIsRetryableAndDistinct(t *testing.T) {
	service, id := newTestService(t, throttledRunner{})
	observer := &captureChatObserver{}
	_, err := service.Send(core.WithChatObserver(context.Background(), observer), id, "run", nil)
	if !errors.Is(err, router.ErrRateLimited) {
		t.Fatalf("error = %v, want rate limited", err)
	}
	session, err := service.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != SessionIdle {
		t.Fatalf("status = %q, want idle", session.Status)
	}
	if len(observer.events) != 1 || observer.events[0].Kind != "run_throttled" || observer.events[0].Error == "" {
		t.Fatalf("events = %+v", observer.events)
	}
}

func TestSeedEvidenceIsValidReplaySizedJSON(t *testing.T) {
	encoded := seedEvidence([]core.ToolCallTrace{{Name: "get_system_overview", Output: `{"services":["` + strings.Repeat("x", MaxToolPayloadBytes) + `"]}`}})
	if len(encoded) > seedReplayBudget || !json.Valid(encoded) {
		t.Fatalf("seed evidence len=%d valid=%v: %q", len(encoded), json.Valid(encoded), encoded)
	}
	turns := []Turn{{Role: TurnCompaction, Content: string(encoded)}}
	messages, _ := historyMessages(withHistory(context.Background(), turns))
	if len(messages) != 1 || !json.Valid([]byte(messages[0].Content)) {
		t.Fatalf("replayed seed is invalid JSON: %#v", messages)
	}
}

func TestValidateAttachment(t *testing.T) {
	now := time.Now()
	if err := validateAttachment(&core.ChatAttachment{Time: &core.ChatTimeRange{Start: now, End: now.Add(-time.Minute)}}); !errors.Is(err, ErrInvalidAttachment) {
		t.Fatalf("error = %v, want invalid attachment", err)
	}
	if err := validateAttachment(&core.ChatAttachment{Service: "api", Time: &core.ChatTimeRange{Start: now.Add(-time.Hour), End: now}}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceResolvesTimeHintBeforeModel(t *testing.T) {
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, location)
	store := NewSessionStore(storage.NewMemory(), tenancy.DefaultOrgScope(), func() time.Time { return now })
	runner := &captureTaskRunner{}
	service := NewServiceWithLocation(store, runner, nil, func() time.Time { return now }, location)
	session, err := service.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), session.ID, "What changed over the past week?", nil); err != nil {
		t.Fatal(err)
	}
	if runner.task.Attachment == nil || runner.task.Attachment.Time == nil {
		t.Fatal("model task missing resolved time attachment")
	}
	wantStart := now.AddDate(0, 0, -7)
	if !runner.task.Attachment.Time.Start.Equal(wantStart) || !runner.task.Attachment.Time.End.Equal(now) {
		t.Fatalf("time range = %+v, want [%s, %s]", runner.task.Attachment.Time, wantStart, now)
	}
}

func TestServiceIgnoresAmbiguousTimeHintsAndExplicitAttachmentWins(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	store := NewSessionStore(storage.NewMemory(), tenancy.DefaultOrgScope(), func() time.Time { return now })
	runner := &captureTaskRunner{}
	service := NewService(store, runner, nil, func() time.Time { return now })
	session, err := service.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), session.ID, "did errors increase today compared to yesterday?", nil); err != nil {
		t.Fatal(err)
	}
	if runner.task.Attachment != nil && runner.task.Attachment.Time != nil {
		t.Fatalf("ambiguous hint attached range %+v", runner.task.Attachment.Time)
	}
	explicit := &core.ChatAttachment{Time: &core.ChatTimeRange{Start: now.Add(-time.Hour), End: now}}
	if _, err := service.Send(context.Background(), session.ID, "compare today and yesterday", explicit); err != nil {
		t.Fatal(err)
	}
	if runner.task.Attachment == nil || runner.task.Attachment.Time == nil || !runner.task.Attachment.Time.Start.Equal(explicit.Time.Start) {
		t.Fatalf("explicit attachment lost: %+v", runner.task.Attachment)
	}
}

func TestServiceIgnoresOversizedTimeHintButRejectsOversizedAttachment(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	store := NewSessionStore(storage.NewMemory(), tenancy.DefaultOrgScope(), func() time.Time { return now })
	runner := &captureTaskRunner{}
	service := NewService(store, runner, nil, func() time.Time { return now })
	session, err := service.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), session.ID, "summarize incidents in the last 90 days", nil); err != nil {
		t.Fatalf("opportunistic hint rejected: %v", err)
	}
	if runner.task.Attachment != nil && runner.task.Attachment.Time != nil {
		t.Fatalf("oversized hint attached range %+v", runner.task.Attachment.Time)
	}
	explicit := &core.ChatAttachment{Time: &core.ChatTimeRange{Start: now.AddDate(0, 0, -90), End: now}}
	if _, err := service.Send(context.Background(), session.ID, "summarize incidents", explicit); !errors.Is(err, ErrInvalidAttachment) {
		t.Fatalf("explicit oversized attachment error = %v, want ErrInvalidAttachment", err)
	}
}

func TestServiceThirtyOneDayFallbackRangeAndRuntimeTimezone(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	provider := storage.NewMemory()
	if err := provider.WriteBlob(reportSettingsBlobName, []byte(`{"timezone":"UTC"}`)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2025, 11, 10, 12, 0, 0, 0, newYork)
	store := NewSessionStore(provider, tenancy.DefaultOrgScope(), func() time.Time { return now })
	runner := &captureTaskRunner{}
	service := NewServiceWithLocationProvider(store, runner, nil, func() time.Time { return now }, func() *time.Location {
		return LocationFromReportSettings(provider)
	})
	session, err := service.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), session.ID, "errors in the last 31 days", nil); err != nil {
		t.Fatal(err)
	}
	if runner.task.Attachment.Time.Start.Location() != time.UTC {
		t.Fatalf("initial timezone = %s", runner.task.Attachment.Time.Start.Location())
	}
	if err := provider.WriteBlob(reportSettingsBlobName, []byte(`{"timezone":"America/New_York"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), session.ID, "errors in the last 31 days", nil); err != nil {
		t.Fatal(err)
	}
	_, startOffset := runner.task.Attachment.Time.Start.Zone()
	_, endOffset := runner.task.Attachment.Time.End.Zone()
	if startOffset != -4*60*60 || endOffset != -5*60*60 || runner.task.Attachment.Time.End.Sub(runner.task.Attachment.Time.Start) != 745*time.Hour {
		t.Fatalf("runtime range = %+v", runner.task.Attachment.Time)
	}
}

func TestServiceGetAndListReconcileExpiredRunningStatus(t *testing.T) {
	store := NewSessionStore(storage.NewMemory(), tenancy.DefaultOrgScope(), time.Now)
	service := NewService(store, fixedRunner{}, nil, time.Now)
	session, err := service.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetStatus(session.ID, SessionRunning, false); err != nil {
		t.Fatal(err)
	}
	got, err := service.Get(session.ID)
	if err != nil || got.Status != SessionFailed {
		t.Fatalf("get status=%q err=%v", got.Status, err)
	}
	if err := store.SetStatus(session.ID, SessionRunning, false); err != nil {
		t.Fatal(err)
	}
	listed, err := service.List()
	if err != nil || len(listed) != 1 || listed[0].Status != SessionFailed || listed[0].Turns != nil {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
}
