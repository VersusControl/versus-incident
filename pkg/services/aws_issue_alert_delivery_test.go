package services

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// channelServer is one alert channel endpoint whose answer a test sets.
type channelServer struct {
	*httptest.Server
	mu        sync.Mutex
	status    int
	delivered int
}

func newChannelServer(t *testing.T) *channelServer {
	t.Helper()
	server := &channelServer{status: http.StatusOK}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		server.mu.Lock()
		defer server.mu.Unlock()
		if server.status == http.StatusOK {
			server.delivered++
		}
		w.WriteHeader(server.status)
	}))
	t.Cleanup(server.Close)
	return server
}

func (server *channelServer) answer(status int) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.status = status
}

func (server *channelServer) count() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.delivered
}

// flakyOnCall fails its first failures pages, then pages.
type flakyOnCall struct {
	failures atomic.Int64
	paged    atomic.Int64
}

func (p *flakyOnCall) TriggerOnCall(context.Context, string, *config.OnCallConfig) error {
	if p.failures.Add(-1) >= 0 {
		return errors.New("pagerduty: unreachable")
	}
	p.paged.Add(1)
	return nil
}

// deliveryFixture is the alert path with two channels (Teams and Lark),
// on-call on, a memory store and a movable episode clock.
type deliveryFixture struct {
	teams, lark *channelServer
	onCall      *flakyOnCall
	memory      storage.Provider
	now         time.Time
}

func newDeliveryFixture(t *testing.T, onCallFailures int64) *deliveryFixture {
	t.Helper()
	loadAgentTestConfig(t)
	// The channel templates are read from the config directory.
	t.Chdir("../..")
	f := &deliveryFixture{teams: newChannelServer(t), lark: newChannelServer(t), onCall: &flakyOnCall{},
		memory: storage.NewMemory(), now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	f.onCall.failures.Store(onCallFailures)
	cfg := config.GetConfig()
	previous := *cfg
	t.Cleanup(func() { *cfg = previous })
	cfg.Alert.MSTeams = config.MSTeamsConfig{Enable: true, PowerAutomateURL: f.teams.URL, TemplatePath: "config/msteams_message.tmpl"}
	cfg.Alert.Lark = config.LarkConfig{Enable: true, WebhookURL: f.lark.URL, TemplatePath: "config/lark_message.tmpl"}
	cfg.OnCall.Enable, cfg.OnCall.WaitMinutes = true, 0
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(redisClient, f.onCall))
	previousStore := store
	SetStorage(f.memory)
	previousClock := detectionNow
	detectionNow = func() time.Time { return f.now }
	t.Cleanup(func() {
		core.SetOnCallWorkflow(nil)
		_ = redisClient.Close()
		SetStorage(previousStore)
		detectionNow = previousClock
	})
	return f
}

func (f *deliveryFixture) incidents(t *testing.T) []*storage.IncidentRecord {
	t.Helper()
	list, err := f.memory.ListIncidents(0)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

var deliveryAlert = issues.Alert{Kind: issues.AlertOpened, Issue: awsAttackIssue(), Key: "opened:1"}

// A send that reached one channel and failed on the other went out: it is
// final, and the alert path does not send it again.
func TestAWSIssuePartialSendIsNeverRetried(t *testing.T) {
	f := newDeliveryFixture(t, 0)
	f.lark.answer(http.StatusInternalServerError)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
		t.Fatalf("a partial send = %v; want it final", err)
	}
	if list := f.incidents(t); len(list) != 1 || list[0].NotifyStatus != "partial" || f.teams.count() != 1 || f.onCall.paged.Load() != 1 {
		t.Fatalf("incidents %d, teams %d, pages %d", len(list), f.teams.count(), f.onCall.paged.Load())
	}
}

// A send of which nothing went out (every channel failed, on-call did not
// page) is sent again. A retry half an hour and an hour later, well past
// the ten-minute default, joins the same episode: one incident, one
// delivered message per channel, one page.
func TestAWSIssueFailedSendRetriedLaterStaysOneIncidentAndPagesOnce(t *testing.T) {
	f := newDeliveryFixture(t, 1)
	f.teams.answer(http.StatusInternalServerError)
	f.lark.answer(http.StatusInternalServerError)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); !errors.Is(err, ErrAWSIssueAlertNotDelivered) {
		t.Fatalf("a send of which nothing went out = %v; want it retried", err)
	}
	f.teams.answer(http.StatusOK)
	f.lark.answer(http.StatusOK)
	f.now = f.now.Add(31 * time.Minute)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
		t.Fatalf("the retry = %v", err)
	}
	f.now = f.now.Add(32 * time.Minute)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
		t.Fatalf("a repeat after the delivery = %v", err)
	}
	if list := f.incidents(t); len(list) != 1 {
		t.Fatalf("incidents = %d; want the retries in one", len(list))
	}
	if f.teams.count() != 1 || f.lark.count() != 1 || f.onCall.paged.Load() != 1 {
		t.Fatalf("teams %d, lark %d, pages %d; want one each", f.teams.count(), f.lark.count(), f.onCall.paged.Load())
	}
}

// When every channel fails but on-call pages, something went out: final,
// so the page is never repeated.
func TestAWSIssueFailedChannelsWithAPageAreNotRetried(t *testing.T) {
	f := newDeliveryFixture(t, 0)
	f.teams.answer(http.StatusInternalServerError)
	f.lark.answer(http.StatusInternalServerError)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
		t.Fatalf("channels failed, on-call paged = %v; want it final", err)
	}
	if f.onCall.paged.Load() != 1 {
		t.Fatalf("pages = %d", f.onCall.paged.Load())
	}
}

// An on-call error after the channels went out is final: the channels are
// not sent to again.
func TestAWSIssueOnCallErrorAfterChannelsIsNotRetried(t *testing.T) {
	f := newDeliveryFixture(t, 1)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
		t.Fatalf("channels sent, on-call failed = %v; want it final", err)
	}
	if f.teams.count() != 1 || f.lark.count() != 1 {
		t.Fatalf("teams %d, lark %d", f.teams.count(), f.lark.count())
	}
}

// crashAttempt records what an attempt that died mid-send leaves behind:
// the episode's notification claimed under a live lease and, when saved,
// the incident it stored as pending before the fan-out, with on-call not
// yet started.
func (f *deliveryFixture) crashAttempt(t *testing.T, alert issues.Alert, savedPending bool) {
	t.Helper()
	episodes := f.memory.(storage.DetectionEpisodeStore)
	decision, err := episodes.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "aws", Source: awsIssueSource, Service: alert.Issue.Account,
			SignalKind: alert.Issue.Rule, ConditionKey: alert.Issue.ID + ":" + alert.Key,
		},
		Frequency: 1, Severity: string(alert.Issue.Severity), ReceivedAt: f.now,
		ClaimLease: storage.DefaultDetectionNotificationLease, QuietPeriod: awsIssueQuietPeriod,
	})
	if err != nil || !decision.NotificationClaimed {
		t.Fatalf("crashed attempt claim = %+v, %v", decision, err)
	}
	if !savedPending {
		return
	}
	err = f.memory.SaveIncident(&storage.IncidentRecord{
		ID: decision.IncidentID, OrgID: storage.DefaultOrgID, CreatedAt: f.now, Origin: storage.OriginAIDetect,
		Content: map[string]interface{}{"AlertName": alert.Issue.Title}, NotifyStatus: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *deliveryFixture) wantDeliveredOnce(t *testing.T) {
	t.Helper()
	if list := f.incidents(t); len(list) != 1 {
		t.Fatalf("incidents = %d; want one", len(list))
	}
	if f.teams.count() != 1 || f.lark.count() != 1 || f.onCall.paged.Load() != 1 {
		t.Fatalf("teams %d, lark %d, pages %d; want one each", f.teams.count(), f.lark.count(), f.onCall.paged.Load())
	}
}

// A retry that lands on the claim of an attempt that died mid-send, while
// that claim's lease still runs, sent nothing: it is retried, and the
// retry after the lease ends delivers the alert once and pages once.
func TestAWSIssueRetryOnDeadAttemptClaimIsRetried(t *testing.T) {
	for _, savedPending := range []bool{false, true} {
		name := "incident not saved"
		if savedPending {
			name = "incident saved pending"
		}
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t, 0)
			f.crashAttempt(t, deliveryAlert, savedPending)
			f.now = f.now.Add(10 * time.Second)
			if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); !errors.Is(err, ErrAWSIssueAlertNotDelivered) {
				t.Fatalf("a retry inside the dead attempt's lease = %v; want it retried", err)
			}
			if f.teams.count() != 0 || f.lark.count() != 0 || f.onCall.paged.Load() != 0 {
				t.Fatalf("the coalesced retry sent: teams %d, lark %d, pages %d", f.teams.count(), f.lark.count(), f.onCall.paged.Load())
			}
			f.now = f.now.Add(storage.DefaultDetectionNotificationLease)
			if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
				t.Fatalf("the retry after the lease = %v", err)
			}
			f.now = f.now.Add(time.Minute)
			if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
				t.Fatalf("a repeat after the delivery = %v; want it final", err)
			}
			f.wantDeliveredOnce(t)
		})
	}
}

// The first retry after a dead attempt's lease ended takes the claim over
// and delivers once; a later repeat coalesces with the finished send.
func TestAWSIssueRetryAfterDeadAttemptLeaseDeliversOnce(t *testing.T) {
	f := newDeliveryFixture(t, 0)
	f.crashAttempt(t, deliveryAlert, true)
	f.now = f.now.Add(2 * storage.DefaultDetectionNotificationLease)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
		t.Fatalf("the retry after the lease = %v", err)
	}
	f.now = f.now.Add(time.Hour)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
		t.Fatalf("a repeat after the delivery = %v; want it final", err)
	}
	f.wantDeliveredOnce(t)
}

// Whether a failed send paged is what that send did, never the stored
// incident: a save that failed to walk back the optimistic flag must not
// end the alert, and a page the store does not show must not be repeated.
func TestAWSIssueFailedSendTrustsItsOwnOnCallResult(t *testing.T) {
	memory := storage.NewMemory()
	previous := store
	SetStorage(memory)
	t.Cleanup(func() { SetStorage(previous) })
	for _, stored := range []bool{true, false} {
		if err := memory.SaveIncident(&storage.IncidentRecord{ID: "incident-1", OrgID: storage.DefaultOrgID, OnCallTriggered: stored}); err != nil {
			t.Fatal(err)
		}
		paged := !stored
		err := awsDeliveryError(&core.DetectionEmissionResult{IncidentID: "incident-1", NotificationOutcome: "failed", OnCallTriggered: paged}, errors.New("send: every channel failed"))
		if paged && err != nil {
			t.Fatalf("the send paged, the store says not = %v; want it final", err)
		}
		if !paged && !errors.Is(err, ErrAWSIssueAlertNotDelivered) {
			t.Fatalf("the send did not page, the store says it did = %v; want it retried", err)
		}
	}
}

// A send whose channels all failed but whose page went out is final. When
// the same alert is sent again (the channels are retried by hand, or the
// alert path failed to record that the send was final), the channels are
// delivered and on-call is not paged a second time.
func TestAWSIssueResendAfterAPagedFailureDoesNotPageAgain(t *testing.T) {
	for _, channelsBack := range []bool{true, false} {
		name := "channels back"
		if !channelsBack {
			name = "channels still failing"
		}
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t, 0)
			f.teams.answer(http.StatusInternalServerError)
			f.lark.answer(http.StatusInternalServerError)
			if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
				t.Fatalf("channels failed, on-call paged = %v; want it final", err)
			}
			f.now = f.now.Add(2 * time.Minute)
			if !channelsBack {
				// The resend fails again: nothing new went out, so it is
				// retried, and still pages no one.
				if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); !errors.Is(err, ErrAWSIssueAlertNotDelivered) {
					t.Fatalf("a resend whose channels failed again = %v; want it retried", err)
				}
				f.now = f.now.Add(5 * time.Minute)
			}
			f.teams.answer(http.StatusOK)
			f.lark.answer(http.StatusOK)
			if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
				t.Fatalf("the resend = %v", err)
			}
			f.wantDeliveredOnce(t)
		})
	}
}

// A resend after a send whose channels failed but whose page went out dies
// mid-send: the incident is pending again and still records the page. The
// retry after that attempt's lease delivers the channels and does not page
// a second time.
func TestAWSIssueRetryAfterADeadResendDoesNotPageAgain(t *testing.T) {
	f := newDeliveryFixture(t, 0)
	f.teams.answer(http.StatusInternalServerError)
	f.lark.answer(http.StatusInternalServerError)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
		t.Fatalf("channels failed, on-call paged = %v; want it final", err)
	}
	f.now = f.now.Add(2 * time.Minute)
	f.crashAttempt(t, deliveryAlert, false)
	list := f.incidents(t)
	if len(list) != 1 || !list[0].OnCallTriggered {
		t.Fatalf("the first send's incident = %+v; want it to record the page", list)
	}
	// The dead resend saved the incident as pending before its fan-out,
	// as every attempt does; it skipped on-call, so the page stays recorded.
	list[0].NotifyStatus = "pending"
	if err := f.memory.(storage.DetectionIncidentStore).SaveDetectionIncident(list[0]); err != nil {
		t.Fatal(err)
	}
	if saved, err := f.memory.GetIncident(list[0].ID); err != nil || saved.NotifyStatus != "pending" || !saved.OnCallTriggered {
		t.Fatalf("the dead resend's incident = %+v, %v", saved, err)
	}
	f.now = f.now.Add(2 * storage.DefaultDetectionNotificationLease)
	f.teams.answer(http.StatusOK)
	f.lark.answer(http.StatusOK)
	if err := CreateIncidentFromAWSIssue(context.Background(), "", deliveryAlert); err != nil {
		t.Fatalf("the retry after the dead resend = %v", err)
	}
	f.wantDeliveredOnce(t)
}
