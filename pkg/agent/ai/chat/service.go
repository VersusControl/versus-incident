package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
	"github.com/VersusControl/versus-incident/pkg/core"
)

const (
	DefaultRunTimeout   = 10 * time.Minute
	defaultLeaseTTL     = 30 * time.Second
	defaultLeaseRenewal = 10 * time.Second
	seedReplayBudget    = 512
)

var (
	ErrRunActive         = errors.New("chat: run already active")
	ErrNoActiveRun       = errors.New("chat: no active run")
	ErrInvalidMessage    = errors.New("chat: invalid message")
	ErrInvalidAttachment = errors.New("chat: invalid attachment")
	ErrInvalidTime       = errors.New("chat: invalid time range")
)

type TurnRunner interface {
	RunChat(context.Context, core.ChatTask) (*core.ChatTurnResult, error)
}

type discoverySeeder interface {
	Seed(context.Context) []core.ToolCallTrace
}

type Service struct {
	store        *SessionStore
	router       TurnRunner
	seeder       discoverySeeder
	now          func() time.Time
	location     func() *time.Location
	owner        string
	runTimeout   time.Duration
	leaseTTL     time.Duration
	leaseRenewal time.Duration

	mu     sync.Mutex
	active map[string]activeRun
}

type activeRun struct {
	cancel context.CancelFunc
	epoch  uint64
}

type TurnOutcome struct {
	Result *core.ChatTurnResult
	Err    error
}

func NewService(store *SessionStore, chatRouter TurnRunner, seeder discoverySeeder, now func() time.Time) *Service {
	return NewServiceWithLocation(store, chatRouter, seeder, now, time.UTC)
}

// NewServiceWithLocation constructs a service with deterministic date phrase
// resolution in loc. Nil clocks and locations default to time.Now and UTC.
func NewServiceWithLocation(store *SessionStore, chatRouter TurnRunner, seeder discoverySeeder, now func() time.Time, loc *time.Location) *Service {
	return NewServiceWithLocationProvider(store, chatRouter, seeder, now, func() *time.Location { return loc })
}

// NewServiceWithLocationProvider resolves the current report timezone for
// every turn so runtime settings changes do not require a process restart.
func NewServiceWithLocationProvider(store *SessionStore, chatRouter TurnRunner, seeder discoverySeeder, now func() time.Time, location func() *time.Location) *Service {
	if now == nil {
		now = time.Now
	}
	if location == nil {
		location = func() *time.Location { return time.UTC }
	}
	return &Service{
		store: store, router: chatRouter, seeder: seeder, now: now, location: location,
		owner: uuid.NewString(), runTimeout: DefaultRunTimeout, leaseTTL: defaultLeaseTTL,
		leaseRenewal: defaultLeaseRenewal, active: map[string]activeRun{},
	}
}

func (service *Service) Available() bool {
	return service != nil && service.store != nil && service.router != nil
}

func (service *Service) Create() (*Session, error) {
	if !service.Available() {
		return nil, router.ErrNoAgent
	}
	return service.store.Create(uuid.NewString())
}

func (service *Service) List() ([]*Session, error) {
	if service == nil || service.store == nil {
		return nil, fmt.Errorf("chat: storage unavailable")
	}
	return service.store.List()
}

func (service *Service) Get(id string) (*Session, error) {
	if service == nil || service.store == nil {
		return nil, fmt.Errorf("chat: storage unavailable")
	}
	return service.store.Get(id)
}

func (service *Service) Delete(id string) error {
	if _, err := service.store.Get(id); err != nil {
		if !errors.Is(err, ErrSessionNotFound) {
			return err
		}
		indexed, indexErr := service.store.List()
		if indexErr != nil {
			return indexErr
		}
		found := false
		for _, session := range indexed {
			if session.ID == id {
				found = true
				break
			}
		}
		if !found {
			return ErrSessionNotFound
		}
	}
	active, err := service.store.LeaseActive(id)
	if err != nil {
		return err
	}
	if active {
		return ErrRunActive
	}
	return service.store.Delete(id)
}

func (service *Service) Send(ctx context.Context, id, message string, attachment *core.ChatAttachment) (result *core.ChatTurnResult, err error) {
	runCtx, session, message, attachment, err := service.prepare(ctx, id, message, attachment)
	if err != nil {
		return nil, err
	}
	result, err = service.execute(runCtx, session, id, message, attachment)
	if releaseErr := service.release(id); releaseErr != nil {
		return result, errors.Join(err, releaseErr)
	}
	return result, err
}

// Start atomically admits and starts a detached turn. Admission errors such as
// an already-active session are returned before an SSE response is committed.
func (service *Service) Start(ctx context.Context, id, message string, attachment *core.ChatAttachment) (<-chan TurnOutcome, error) {
	runCtx, session, message, attachment, err := service.prepare(ctx, id, message, attachment)
	if err != nil {
		return nil, err
	}
	outcome := make(chan TurnOutcome, 1)
	go func() {
		defer close(outcome)
		defer func() {
			if recover() != nil {
				log.Printf("chat service panic: operation=run session_id=%q code=panic\n%s", id, debug.Stack())
				service.persistFailure(runCtx, id, "Chat run failed before an assistant answer.")
				outcome <- TurnOutcome{Err: errors.Join(errors.New("chat: run failed"), service.release(id))}
			}
		}()
		result, runErr := service.execute(runCtx, session, id, message, attachment)
		outcome <- TurnOutcome{Result: result, Err: errors.Join(runErr, service.release(id))}
	}()
	return outcome, nil
}

func (service *Service) prepare(ctx context.Context, id, message string, attachment *core.ChatAttachment) (context.Context, *Session, string, *core.ChatAttachment, error) {
	if !service.Available() {
		return nil, nil, "", nil, router.ErrNoAgent
	}
	message = strings.TrimSpace(message)
	if message == "" || len(message) > MaxMessageBytes {
		return nil, nil, "", nil, fmt.Errorf("%w: must be between 1 and %d bytes", ErrInvalidMessage, MaxMessageBytes)
	}
	if err := validateAttachment(attachment); err != nil {
		return nil, nil, "", nil, err
	}
	if attachment == nil {
		attachment = &core.ChatAttachment{}
	}
	if attachment.Time == nil {
		location := service.location()
		if location == nil {
			location = time.UTC
		}
		resolved, ok, resolveErr := resolveTimeHint(message, service.now(), location)
		if resolveErr != nil {
			return nil, nil, "", nil, resolveErr
		}
		if ok {
			attachment.Time = resolved
		}
	}
	if err := validateAttachment(attachment); err != nil {
		return nil, nil, "", nil, err
	}
	session, err := service.store.Get(id)
	if err != nil {
		return nil, nil, "", nil, err
	}

	if session.Status == SessionRunning {
		active, leaseErr := service.store.LeaseActive(id)
		if leaseErr != nil {
			return nil, nil, "", nil, leaseErr
		}
		if !active {
			if err := service.store.SetStatus(id, SessionFailed, false); err != nil {
				return nil, nil, "", nil, err
			}
			session.Status = SessionFailed
		}
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), service.runTimeout)
	recorder := &eventRecorder{delegate: core.ChatObserverFrom(ctx)}
	runCtx = core.WithChatObserver(runCtx, recorder)
	service.mu.Lock()
	if _, exists := service.active[id]; exists {
		service.mu.Unlock()
		cancel()
		return nil, nil, "", nil, ErrRunActive
	}
	service.active[id] = activeRun{cancel: cancel}
	service.mu.Unlock()
	epoch, acquired, err := service.store.AcquireLease(id, service.owner, service.leaseTTL)
	if err != nil || !acquired {
		service.mu.Lock()
		delete(service.active, id)
		service.mu.Unlock()
		cancel()
		if err != nil {
			return nil, nil, "", nil, err
		}
		return nil, nil, "", nil, ErrRunActive
	}
	service.mu.Lock()
	service.active[id] = activeRun{cancel: cancel, epoch: epoch}
	service.mu.Unlock()
	go service.renewLease(runCtx, id, epoch, cancel)
	return runCtx, session, message, cloneAttachment(attachment), nil
}

func (service *Service) renewLease(ctx context.Context, id string, epoch uint64, cancel context.CancelFunc) {
	ticker := time.NewTicker(service.leaseRenewal)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewed, cancelRequested, err := service.store.RenewLease(id, service.owner, epoch, service.leaseTTL)
			if err != nil || !renewed || cancelRequested {
				cancel()
				return
			}
		}
	}
}

func (service *Service) release(id string) error {
	service.mu.Lock()
	run := service.active[id]
	delete(service.active, id)
	service.mu.Unlock()
	if run.cancel != nil {
		run.cancel()
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = service.store.ReleaseLease(id, service.owner, run.epoch)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrLeaseFenced) {
			break
		}
	}
	return fmt.Errorf("chat: release lease for session %q: %w", id, err)
}

func (service *Service) persistFailure(ctx context.Context, id, message string) {
	events := recordedEvents(ctx)
	if err := service.persistTerminal(ctx, id, core.ChatEvent{Kind: core.ChatEventRunFailed, Error: "chat run failed"}, message, events); err != nil {
		log.Printf("chat persistence failed: operation=persist_panic session_id=%q code=backend_failure", id)
	}
	if err := service.store.SetStatus(id, SessionFailed, false); err != nil {
		log.Printf("chat persistence failed: operation=set_failed_status session_id=%q code=backend_failure", id)
	}
}

func (service *Service) execute(runCtx context.Context, session *Session, id, message string, attachment *core.ChatAttachment) (result *core.ChatTurnResult, runErr error) {
	if err := service.store.SetStatus(id, SessionRunning, false); err != nil {
		return nil, err
	}
	finalStatus := SessionFailed
	defer func() {
		if statusErr := service.store.SetStatus(id, finalStatus, false); statusErr != nil && runErr == nil {
			result = nil
			runErr = statusErr
		}
	}()
	if !session.Seeded {
		if err := service.seed(runCtx, id); err != nil {
			return nil, err
		}
		var err error
		session, err = service.store.Get(id)
		if err != nil {
			return nil, err
		}
	}
	history := append([]Turn(nil), session.Turns...)
	userTurn := Turn{ID: uuid.NewString(), Role: TurnUser, Content: message, CreatedAt: service.now().UTC(), Attachment: cloneAttachment(attachment)}
	if _, err := service.store.Append(id, userTurn); err != nil {
		return nil, err
	}

	runCtx = withHistory(runCtx, history)
	result, runErr = service.router.RunChat(runCtx, core.ChatTask{SessionID: id, Message: message, Attachment: cloneAttachment(attachment)})
	events := recordedEvents(runCtx)
	if runErr != nil {
		terminal := core.ChatEvent{Kind: core.ChatEventRunFailed, Error: "chat run failed"}
		message := "Chat run ended without an assistant answer."
		if errors.Is(runErr, context.Canceled) {
			terminal.Kind = core.ChatEventRunCancelled
			terminal.Error = "run cancelled"
		} else if errors.Is(runErr, context.DeadlineExceeded) {
			terminal.Error = "run timed out"
		} else if errors.Is(runErr, router.ErrRateLimited) {
			terminal.Kind = "run_throttled"
			terminal.Error = "rate limit reached; retry next hour"
			message = "Chat run was throttled before an assistant answer."
			finalStatus = SessionIdle
		}
		if err := service.persistTerminal(runCtx, id, terminal, message, events); err != nil {
			return nil, err
		}
		return result, runErr
	}
	if result == nil {
		return nil, errors.New("chat: model returned no answer")
	}
	assistantID := uuid.NewString()
	terminal := nextTerminalEvent(events, core.ChatEvent{Kind: core.ChatEventRunFinished, DurationMs: result.DurationMs, Output: assistantID, Citations: append([]core.ChatCitation(nil), result.Citations...)}, service.now())
	events = append(events, terminal)
	assistant := Turn{ID: assistantID, Role: TurnAssistant, Content: result.Markdown, CreatedAt: service.now().UTC(), ToolCalls: result.ToolCalls, Citations: result.Citations, Events: events}
	if _, err := service.store.Append(id, assistant); err != nil {
		return nil, err
	}
	deliverTerminal(runCtx, terminal)
	finalStatus = SessionIdle
	return result, nil
}

func nextTerminalEvent(events []core.ChatEvent, terminal core.ChatEvent, now time.Time) core.ChatEvent {
	for _, event := range events {
		if event.Seq >= terminal.Seq {
			terminal.Seq = event.Seq + 1
		}
	}
	terminal.At = now.UTC()
	return boundEvents([]core.ChatEvent{terminal})[0]
}

func deliverTerminal(ctx context.Context, terminal core.ChatEvent) {
	recorder, _ := core.ChatObserverFrom(ctx).(*eventRecorder)
	if recorder != nil && recorder.delegate != nil {
		recorder.delegate.OnChatEvent(terminal)
	}
}

func (service *Service) persistTerminal(ctx context.Context, id string, terminal core.ChatEvent, message string, events []core.ChatEvent) error {
	terminal = nextTerminalEvent(events, terminal, service.now())
	events = append(events, terminal)
	if _, err := service.store.Append(id, Turn{ID: uuid.NewString(), Role: TurnCompaction, Content: message, CreatedAt: service.now().UTC(), Events: events}); err != nil {
		return err
	}
	deliverTerminal(ctx, terminal)
	return nil
}

func recordedEvents(ctx context.Context) []core.ChatEvent {
	recorder, _ := core.ChatObserverFrom(ctx).(*eventRecorder)
	if recorder == nil {
		return nil
	}
	return recorder.snapshot()
}

type eventRecorder struct {
	mu       sync.Mutex
	events   []core.ChatEvent
	dropped  int
	delegate core.ChatObserver
}

func (recorder *eventRecorder) OnChatEvent(event core.ChatEvent) {
	if event.Kind == core.ChatEventRunFinished || event.Kind == core.ChatEventRunFailed || event.Kind == core.ChatEventRunCancelled || event.Kind == "run_throttled" {
		return
	}
	event = boundEvents([]core.ChatEvent{event})[0]
	recorder.mu.Lock()
	if len(recorder.events) < MaxEventsPerTurn-1 {
		recorder.events = append(recorder.events, event)
	} else {
		head := MaxEventsPerTurn / 2
		copy(recorder.events[head:], recorder.events[head+1:])
		recorder.events[len(recorder.events)-1] = event
		recorder.dropped++
	}
	recorder.mu.Unlock()
	if recorder.delegate != nil {
		recorder.delegate.OnChatEvent(event)
	}
}

func (recorder *eventRecorder) snapshot() []core.ChatEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	events := append([]core.ChatEvent(nil), recorder.events...)
	if recorder.dropped > 0 {
		head := MaxEventsPerTurn / 2
		events = append(events, core.ChatEvent{})
		copy(events[head+1:], events[head:])
		events[head] = core.ChatEvent{Kind: "events_elided", Output: fmt.Sprintf("%d events omitted", recorder.dropped)}
	}
	return boundEvents(events)
}

func (service *Service) seed(ctx context.Context, id string) error {
	if service.seeder == nil {
		return service.store.SetStatus(id, SessionRunning, true)
	}
	traces := service.seeder.Seed(ctx)
	if len(traces) > 0 {
		if _, err := service.store.Append(id, Turn{
			ID: uuid.NewString(), Role: TurnCompaction, CreatedAt: service.now().UTC(),
			Content: string(seedEvidence(traces)), ToolCalls: traces,
		}); err != nil {
			return err
		}
	}
	return service.store.SetStatus(id, SessionRunning, true)
}

func seedEvidence(traces []core.ToolCallTrace) []byte {
	type evidence struct {
		Kind  string               `json:"kind"`
		Tools []core.ToolCallTrace `json:"tools,omitempty"`
	}
	bounded := boundToolCalls(traces)
	for outputLimit := 256; ; outputLimit /= 2 {
		candidate := append([]core.ToolCallTrace(nil), bounded...)
		for index := range candidate {
			candidate[index].Args = ""
			candidate[index].Output = capRawString(candidate[index].Output, outputLimit)
		}
		encoded, err := json.Marshal(evidence{Kind: "session_discovery", Tools: candidate})
		if err == nil && len(encoded) <= seedReplayBudget {
			return encoded
		}
		if outputLimit == 0 {
			break
		}
	}
	encoded, _ := json.Marshal(evidence{Kind: "session_discovery"})
	return encoded
}

func (service *Service) Cancel(id string) error {
	requested, err := service.store.RequestCancel(id)
	if err != nil {
		return err
	}
	if !requested {
		return ErrNoActiveRun
	}
	service.mu.Lock()
	run := service.active[id]
	service.mu.Unlock()
	if run.cancel != nil {
		run.cancel()
	}
	return nil
}

func validateAttachment(attachment *core.ChatAttachment) error {
	if attachment == nil {
		return nil
	}
	if len(attachment.Service) > 256 {
		return ErrInvalidAttachment
	}
	if attachment.Time != nil && (attachment.Time.Start.IsZero() || attachment.Time.End.IsZero() || !attachment.Time.Start.Before(attachment.Time.End) || attachment.Time.End.Sub(attachment.Time.Start) > maxRelativeTimeRange+24*time.Hour) {
		return ErrInvalidAttachment
	}
	if incident := attachment.Incident; incident != nil {
		if len(incident.ID) > 128 || len(incident.Title) > 512 || len(incident.Service) > 256 || len(incident.Severity) > 32 || len(incident.Status) > 32 {
			return ErrInvalidAttachment
		}
	}
	return nil
}
