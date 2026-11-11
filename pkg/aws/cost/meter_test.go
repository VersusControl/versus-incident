package cost

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fixtures/storagefix"
)

func TestCapIsEnforcedInTheMeterAndPagesCount(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) { o.MaxPaidRequests = 9 })
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	src := h.adapter.Source()
	r := finops.DateRange{Start: finops.MustDate("2026-11-06"), End: finops.MustDate("2026-11-09")}
	before, _ := src.Lines(ctx, r)

	// The next day: AWS restated a day and pages hold one group each, so
	// the plan's estimate fits but the pages do not.
	h.clock.set(at("2026-11-10", 10))
	sc.Series[0].Steps = append(sc.Series[0].Steps, scenarioStep{From: finops.MustDate("2026-11-07"), Cost: "999", Quantity: "1"})
	h.fake.pageGroups = 1
	sent := h.fake.requests()
	paidBefore := paidCount(h.fake)
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if used := h.adapter.Meter().Used(ctx); used != 9 {
		t.Fatalf("meter used %d, want exactly the cap", used)
	}
	if got := paidCount(h.fake) - paidBefore; got != 9 {
		t.Fatalf("paid requests sent %d, want 9 (none past the cap): %v", got, h.fake.calls)
	}
	if len(res.Committed) != 0 {
		t.Fatalf("a day was written without all of its tables: %v", res.Committed)
	}
	if h.fake.requests()-sent < 9 {
		t.Fatal("free calls still run")
	}
	after, _ := src.Lines(ctx, r)
	if sumLines(after, nil) != sumLines(before, nil) {
		t.Fatal("a failed refresh must keep the old data")
	}
	st, _ := src.Status(ctx)
	if !st.Stale || st.StaleReason != finops.SkipCapMidRun {
		t.Fatalf("stale %v reason %q", st.Stale, st.StaleReason)
	}
	if !slices.Contains(st.SkippedCalls, callRecord) {
		t.Fatalf("skipped %v", st.SkippedCalls)
	}
	// The cap holds for every caller, including a drill-down on click.
	if err := h.adapter.DrillDown(ctx, "111111111111", "EC2 - Other"); !errors.Is(err, finops.ErrCapReached) {
		t.Fatalf("drill-down past the cap: %v", err)
	}
}

func TestMeterPersistsAndRollsOverDaily(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) { o.MaxPaidRequests = 3 })
	m := h.adapter.Meter()
	if err := m.Take(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := m.Take(ctx, 2); !errors.Is(err, finops.ErrCapReached) || m.Used(ctx) != 2 {
		t.Fatalf("over-cap take must take nothing: %v used %d", err, m.Used(ctx))
	}
	// A new adapter over the same storage sees today's use.
	b, err := New(h.provider, h.adapter.cache.scope, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if b.Meter().Used(ctx) != 2 {
		t.Fatalf("persisted use %d", b.Meter().Used(ctx))
	}
	rec := meterRecord{}
	if ok, _ := h.adapter.cache.read(func(org string) string { return meterKey(org, "2026-11-09") }, &rec); !ok || rec.Dollars != 20_000 {
		t.Fatalf("meter record %+v", rec)
	}
	h.clock.set(at("2026-11-10", 0))
	if m.Used(ctx) != 0 || m.Take(ctx, 3) != nil {
		t.Fatal("the cap resets at midnight in the deployment time zone")
	}
}

// TestMeterCountsWhatAnotherReplicaSpent: two replicas share the storage and
// each has its own meter. One that read the day's use before the other spent
// still sees the other's requests, so together they never pass the cap.
func TestMeterCountsWhatAnotherReplicaSpent(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) { o.MaxPaidRequests = 5 })
	other, err := New(h.provider, h.adapter.cache.scope, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if used := other.Meter().Used(ctx); used != 0 {
		t.Fatalf("a fresh day reads %d", used)
	}
	if err := h.adapter.Meter().Take(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if used := other.Meter().Used(ctx); used != 4 {
		t.Fatalf("the other replica reads %d used, want 4", used)
	}
	if err := other.Meter().Take(ctx, 2); !errors.Is(err, finops.ErrCapReached) {
		t.Fatalf("the other replica passed the shared cap: %v", err)
	}
	if err := other.Meter().Take(ctx, 1); err != nil || h.adapter.Meter().Used(ctx) != 5 {
		t.Fatalf("the last request of the day: %v, used %d", err, h.adapter.Meter().Used(ctx))
	}
}

// meetingProvider holds the first two reads of one key until both have
// read it, so two meters compute their next count from the same stored
// value before either stores. It passes compare-and-swap through.
type meetingProvider struct {
	storage.Provider
	key   string
	met   sync.WaitGroup
	mu    sync.Mutex
	reads int
}

func newMeetingProvider(t *testing.T, base storage.Provider, key string) *meetingProvider {
	t.Helper()
	if _, ok := base.(storage.BlobCAS); !ok {
		t.Fatalf("%T has no compare-and-swap", base)
	}
	p := &meetingProvider{Provider: base, key: key}
	p.met.Add(2)
	return p
}

func (p *meetingProvider) ReadBlob(name string) ([]byte, error) {
	data, err := p.Provider.ReadBlob(name)
	if name == p.key {
		p.mu.Lock()
		p.reads++
		meet := p.reads <= 2
		p.mu.Unlock()
		if meet {
			p.met.Done()
			p.met.Wait()
		}
	}
	return data, err
}

func (p *meetingProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return p.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

// TestTwoMetersOnOneSwapStoreNeverPassTheCap: two replicas' meters share one
// store with compare-and-swap and no replica lock. When both read the day's
// count before either stores, only one reservation lands; and many takes
// racing on both meters never reserve more than the cap, with the stored
// count equal to what was granted.
func TestTwoMetersOnOneSwapStoreNeverPassTheCap(t *testing.T) {
	day := finops.MustDate("2026-11-09")
	scope := tenancy.DefaultOrgScope().Normalized()
	meters := func(provider storage.Provider, limit int) (*Meter, *Meter) {
		c := cache{provider: provider, scope: scope}
		build := func() *Meter {
			return newMeter(c, func() int { return limit }, func() finops.Date { return day }, func() time.Time { return at("2026-11-09", 10) })
		}
		return build(), build()
	}
	stored := func(t *testing.T, provider storage.Provider) int {
		t.Helper()
		rec := meterRecord{}
		if _, err := (cache{provider: provider, scope: scope}).read(func(org string) string { return meterKey(org, day.String()) }, &rec); err != nil {
			t.Fatal(err)
		}
		return rec.Requests
	}

	t.Run("both read the same count", func(t *testing.T) {
		provider := newMeetingProvider(t, storagefix.Provider(t), meterKey(scope.Write, day.String()))
		first, second := meters(provider, 1)
		errs := make([]error, 2)
		var wait sync.WaitGroup
		for i, m := range []*Meter{first, second} {
			wait.Add(1)
			go func() {
				defer wait.Done()
				errs[i] = m.Take(ctx, 1)
			}()
		}
		wait.Wait()
		granted := 0
		for _, err := range errs {
			switch {
			case err == nil:
				granted++
			case !errors.Is(err, finops.ErrCapReached):
				t.Fatalf("take error = %v", err)
			}
		}
		if granted != 1 {
			t.Fatalf("two meters granted %d requests against a cap of 1 (%v)", granted, errs)
		}
		if got := stored(t, provider); got != 1 {
			t.Fatalf("stored count %d, want 1", got)
		}
	})

	t.Run("many takes at once", func(t *testing.T) {
		provider := storagefix.Provider(t)
		const limit = 20
		first, second := meters(provider, limit)
		var granted, refused sync.Map
		var wait sync.WaitGroup
		for worker := range 16 {
			m := first
			if worker%2 == 1 {
				m = second
			}
			wait.Add(1)
			go func() {
				defer wait.Done()
				for take := range 5 {
					err := m.Take(ctx, 1)
					switch {
					case err == nil:
						granted.Store([2]int{worker, take}, true)
					case errors.Is(err, finops.ErrCapReached), errors.Is(err, errMeterContended):
						refused.Store([2]int{worker, take}, err)
					default:
						t.Errorf("take error = %v", err)
					}
				}
			}()
		}
		wait.Wait()
		count := 0
		granted.Range(func(any, any) bool { count++; return true })
		if count != limit {
			t.Fatalf("two meters granted %d requests against a cap of %d; 80 were asked", count, limit)
		}
		if got := stored(t, provider); got != count {
			t.Fatalf("stored count %d, granted %d", got, count)
		}
	})
}

func TestDayCommitsOnlyWhenAllTablesSucceed(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	src := h.adapter.Source()
	day := finops.DateRange{Start: finops.MustDate("2026-11-07"), End: finops.MustDate("2026-11-07")}
	before, _ := src.Lines(ctx, day)

	h.clock.set(at("2026-11-10", 10))
	sc.Series[0].Steps = append(sc.Series[0].Steps, scenarioStep{From: finops.MustDate("2026-11-07"), Cost: "999", Quantity: "1"})
	h.fake.failures[callTeam] = fakeFailure{Type: "LimitExceededException", Message: "Rate exceeded for account 111111111111"}
	used := h.adapter.Meter().Used(ctx)
	records := h.fake.count(callRecord)
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) != 0 {
		t.Fatalf("committed %v", res.Committed)
	}
	// Calls 1–3 were spent; the record table and the optional calls were
	// not, because the day could no longer be written.
	if got := h.adapter.Meter().Used(ctx) - used; got != 3 {
		t.Fatalf("spent %d, want 3", got)
	}
	if h.fake.count(callRecord) != records {
		t.Fatal("the rest of a failed day group must not be sent")
	}
	after, _ := src.Lines(ctx, day)
	if sumLines(after, nil) != sumLines(before, nil) {
		t.Fatal("the day keeps its old data")
	}
	st, _ := src.Status(ctx)
	if !st.Stale || st.StaleReason != CodeThrottled || !st.Accounts[0].Readable {
		t.Fatalf("throttling makes data stale, not unreadable: %+v", st)
	}
	if strings.Contains(st.StaleReason, "Rate exceeded") {
		t.Fatal("provider text must never be echoed")
	}
}

type apiErr struct{ code, msg string }

func (e apiErr) Error() string                 { return e.code + ": " + e.msg }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.msg }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestErrorVocabularyIsClosed(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{apiErr{"AccessDeniedException", "User: arn:aws:sts::111111111111:assumed-role/x is not authorized to perform: ce:GetCostAndUsage"}, CodeDenied},
		{apiErr{"AccessDeniedException", "User not enabled for cost explorer access"}, CodeUnavailable},
		{apiErr{"DataUnavailableException", "Data is not available"}, CodeDataNotReady},
		{apiErr{"LimitExceededException", "Rate exceeded"}, CodeThrottled},
		{apiErr{"ThrottlingException", "slow down"}, CodeThrottled},
		{apiErr{"BillExpirationException", "too old"}, CodeUnavailable},
		{context.DeadlineExceeded, CodeTimeout},
		{errMalformed, CodeUnavailable},
		{errors.New("dial tcp: connection refused"), CodeTimeout},
	}
	for _, c := range cases {
		got := ErrorCode(c.err)
		if got != c.want {
			t.Errorf("%v → %q, want %q", c.err, got, c.want)
		}
		if !slices.Contains(Codes, got) {
			t.Errorf("%q is outside the vocabulary", got)
		}
	}
	if ErrorCode(finops.ErrCapReached) != "" || ErrorCode(nil) != "" {
		t.Fatal("the cap is not a provider error")
	}
}

func TestDeniedPayerNamesTheGapNeverZero(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.MaxPaidRequests = 40
		o.Payers = []Payer{
			{AccountID: "111111111111", Credentials: credsFor("111111111111")},
			{AccountID: "222222222222", Credentials: credsFor("222222222222")},
			{AccountID: "333333333333", Credentials: credsFor("333333333333")},
		}
	})
	// The fake serves the same bill to every payer; deny the third one.
	h.fake.denyPayer = "333333333333"
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	svc := finops.NewService(h.adapter.Source(), nil, finops.Config{Now: h.clock.Now})
	sum, err := svc.Summary(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	s := sum.Header.Sync
	if s == nil || s.AccountsTotal != 3 || s.AccountsReadable != 2 || len(s.MissingAccounts) != 1 || s.MissingAccounts[0].Reason != CodeDenied {
		t.Fatalf("sync %+v", s)
	}
	if !slices.Contains(s.Notes, "2 of 3 accounts") && !containsPrefix(s.Notes, "2 of 3") {
		t.Fatalf("notes %v", s.Notes)
	}
}

func TestCostExplorerOffExplainsInsteadOfZero(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	h.fake.failures[callAccount] = fakeFailure{Type: "AccessDeniedException", Message: "User not enabled for cost explorer access"}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	st, err := h.adapter.Source().Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Enabled || st.DisabledReason != CodeUnavailable {
		t.Fatalf("status %+v", st)
	}
}

func paidCount(f *fakeAWS) int {
	n := 0
	for _, c := range []string{callAccount, callUsage, callTeam, callRecord, callForecast, callCoverage, callDrill, callResources} {
		n += f.count(c)
	}
	return n
}

func containsPrefix(notes []string, p string) bool {
	for _, n := range notes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// Credentials AWS rejects (expired session token, revoked key) are a
// credentials cause, not a missing Cost Explorer or a network fault.
func TestExpiredCredentialsAreACredentialsCause(t *testing.T) {
	for _, failure := range []fakeFailure{
		{Type: "ExpiredTokenException", Message: "The security token included in the request is expired"},
		{Type: "UnrecognizedClientException", Message: "The security token included in the request is invalid"},
		{Type: "InvalidClientTokenId", Message: "The security token included in the request is invalid"},
	} {
		sc := loadScenario(t, "single-account")
		h := newHarness(t, sc, nil)
		h.fake.failures[callAccount] = failure
		if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
			t.Fatal(err)
		}
		st, err := h.adapter.Source().Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Enabled || st.DisabledReason != CodeCredentials {
			t.Errorf("%s: status enabled=%v reason=%q, want %q", failure.Type, st.Enabled, st.DisabledReason, CodeCredentials)
		}
	}
	// The signer could not load credentials at all.
	if got := ErrorCode(&v4.SigningError{Err: errors.New("failed to refresh cached credentials")}); got != CodeCredentials {
		t.Fatalf("signing error = %q", got)
	}
}

// A payer whose credentials cannot be loaded at all (the provider fails,
// for example a metadata endpoint that times out or a failed role
// assumption) is a credentials cause, not a timeout or network fault, and
// no request reaches AWS.
func TestCredentialsThatCannotLoadAreACredentialsCause(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, errors.New("no EC2 instance role found")} {
		failing := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return aws.Credentials{}, failure })
		h := newHarness(t, loadScenario(t, "single-account"), func(o *Options) {
			o.Payers = []Payer{{AccountID: "111111111111", Credentials: failing}}
		})
		if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
			t.Fatal(err)
		}
		st, err := h.adapter.Source().Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Enabled || st.DisabledReason != CodeCredentials {
			t.Errorf("provider failing %q: status enabled=%v reason=%q, want %q", failure, st.Enabled, st.DisabledReason, CodeCredentials)
		}
		if h.fake.requests() != 0 {
			t.Errorf("provider failing %q: %d requests reached AWS unsigned", failure, h.fake.requests())
		}
	}
}
