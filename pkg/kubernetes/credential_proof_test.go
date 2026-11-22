package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// proofEvent is one request the fake cluster or the fake provider saw.
type proofEvent struct {
	kind   string // review, data or provider
	header string
	// proven is the identity a proof answered for header, empty when it did
	// not prove it.
	proven string
}

// proofCluster is a fake API server that records the Authorization header of
// every request, answers identity reviews per token, and serves data reads.
type proofCluster struct {
	server *httptest.Server
	mu     sync.Mutex
	events []proofEvent
	// review answers a review for a header: an HTTP status and, for 200,
	// the identity's username. extra adds per-token review fields.
	review func(header string) (int, string)
	// rejectData answers data reads for these headers with 401.
	rejectData map[string]bool
}

func newProofCluster(t *testing.T) *proofCluster {
	t.Helper()
	cluster := &proofCluster{rejectData: map[string]bool{}}
	cluster.review = func(string) (int, string) { return http.StatusOK, "principal-value-reader" }
	cluster.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if r.URL.Path == "/apis/authentication.k8s.io/v1/selfsubjectreviews" {
			cluster.mu.Lock()
			review := cluster.review
			cluster.mu.Unlock()
			status, username := review(header)
			event := proofEvent{kind: "review", header: header}
			if status == http.StatusOK {
				event.proven = username
			}
			cluster.record(event)
			if status != http.StatusOK {
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "120")
				}
				w.WriteHeader(status)
				return
			}
			writeJSON(w, map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview", "status": map[string]any{"userInfo": map[string]any{
				"username": username, "uid": "uid-value-1", "groups": []string{"readers", "system:authenticated"},
				"extra": map[string][]string{"iam.gke.io/user-assertion": {"assertion-for-" + header}},
			}}})
			return
		}
		cluster.record(proofEvent{kind: "data", header: header})
		cluster.mu.Lock()
		rejected := cluster.rejectData[header]
		cluster.mu.Unlock()
		if rejected {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"kind": "NamespaceList", "apiVersion": "v1", "metadata": map[string]any{}, "items": []any{}})
	}))
	t.Cleanup(cluster.server.Close)
	return cluster
}

func (cluster *proofCluster) record(event proofEvent) {
	cluster.mu.Lock()
	cluster.events = append(cluster.events, event)
	cluster.mu.Unlock()
}

func (cluster *proofCluster) setReview(review func(header string) (int, string)) {
	cluster.mu.Lock()
	cluster.review = review
	cluster.mu.Unlock()
}

func (cluster *proofCluster) snapshot() []proofEvent {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	return slices.Clone(cluster.events)
}

func (cluster *proofCluster) count(kind string) int {
	n := 0
	for _, event := range cluster.snapshot() {
		if event.kind == kind {
			n++
		}
	}
	return n
}

// proofFixture is one cluster connection with a fake clock, a renewing GKE
// source and a controllable provider proof.
type proofFixture struct {
	t       *testing.T
	clock   *fakeClock
	cluster *proofCluster
	service *Service
	source  *gkeCredentialSource
	issued  atomic.Int64
	// lifetime is the lifetime of the next token the source issues.
	lifetime atomic.Int64
	// provider answers the provider proof for a header.
	providerMu sync.Mutex
	provider   func(ctx context.Context, header string) (string, proofOutcome, error)
	calls      atomic.Int64
}

const proofHostName = "proof-host.example.test"

func newProofFixture(t *testing.T) *proofFixture {
	t.Helper()
	fixture := &proofFixture{t: t, clock: newFakeClock(), cluster: newProofCluster(t)}
	fixture.lifetime.Store(int64(time.Hour))
	fixture.provider = func(context.Context, string) (string, proofOutcome, error) { return "", proofNotApplicable, nil }
	fixture.service = newTestService(t, fixture.cluster.server.URL, Scope{OrgID: "default", ClusterID: "proof", CredentialID: "configured"})
	fixture.service.client.proofs = newCredentialProofs(fixture.clock.Now)
	fixture.source = &gkeCredentialSource{expiringBearerSource: &expiringBearerSource{now: fixture.clock.Now, skew: 2 * time.Minute, refresh: func(context.Context) (string, time.Time, error) {
		n := fixture.issued.Add(1)
		return fmt.Sprintf("token-value-%04d", n), fixture.clock.Now().Add(time.Duration(fixture.lifetime.Load())), nil
	}}, host: proofHostName}
	fixture.source.proof = func(ctx context.Context, header string) (string, proofOutcome, error) {
		fixture.calls.Add(1)
		fixture.providerMu.Lock()
		provider := fixture.provider
		fixture.providerMu.Unlock()
		principal, outcome, err := provider(ctx, header)
		event := proofEvent{kind: "provider", header: header}
		if outcome == proofProven {
			event.proven = principal
		}
		fixture.cluster.record(event)
		return principal, outcome, err
	}
	fixture.service.client.credentials = fixture.source
	fixture.service.SetChangeStorage(storage.NewMemory())
	return fixture
}

func (fixture *proofFixture) setProvider(provider func(ctx context.Context, header string) (string, proofOutcome, error)) {
	fixture.providerMu.Lock()
	fixture.provider = provider
	fixture.providerMu.Unlock()
}

// read performs one data request through the request path.
func (fixture *proofFixture) read() error {
	ctx, cancel := fixture.service.credentialContext(fixture.t.Context())
	defer cancel()
	var out map[string]any
	return fixture.service.client.GetJSON(ctx, "/api/v1/namespaces", &out)
}

// renew moves the clock to just past the point where the source refreshes the
// current token: two minutes before its expiry.
func (fixture *proofFixture) renew() {
	current := fixture.source.current.Load()
	if current == nil {
		fixture.t.Fatal("no token issued yet")
	}
	if delta := current.expires.Add(-2*time.Minute + time.Second).Sub(fixture.clock.Now()); delta > 0 {
		fixture.clock.advance(delta)
	}
	// The refresh runs in the background once the token is in its renewal
	// window; wait for it, so the next read carries the renewed token.
	_, _ = fixture.source.Authorization(fixture.t.Context())
	settleRefresh(fixture.t, fixture.source.expiringBearerSource)
}

// settle waits until no proof is running.
func (fixture *proofFixture) settle() {
	fixture.t.Helper()
	settleProofs(fixture.t, fixture.service.client)
}

// settleProofs waits until no proof of client is running.
func settleProofs(t *testing.T, client *Client) {
	t.Helper()
	eventually(t, "proofs to finish", func() bool {
		proofs := client.proofCache()
		proofs.mu.Lock()
		defer proofs.mu.Unlock()
		for _, entry := range proofs.entries {
			for _, slot := range entry.slots {
				if slot.running {
					return false
				}
			}
		}
		return true
	})
}

func (fixture *proofFixture) alive() bool { return fixture.service.identity.ctx.Err() == nil }

// assertOnlyProvenTokensReadData checks that every data request carried a
// token that a proof had already proven for identity before that request.
func (fixture *proofFixture) assertOnlyProvenTokensReadData(identity string) {
	fixture.t.Helper()
	proven := map[string]bool{}
	for _, event := range fixture.cluster.snapshot() {
		switch event.kind {
		case "review", "provider":
			if event.proven == identity {
				proven[event.header] = true
			}
		case "data":
			if !proven[event.header] {
				fixture.t.Fatal("a data request carried a token that was not proven for the epoch's identity")
			}
		}
	}
}

func proofCode(err error) string { return DiagnoseError(err).Code }

func reviewDown(status int) func(string) (int, string) {
	return func(string) (int, string) { return status, "" }
}

func reviewFor(username string) func(string) (int, string) {
	return func(string) (int, string) { return http.StatusOK, username }
}

// Candidate isolation and the serving token: across 100 renewals whose first
// identity review fails and second succeeds, with concurrent readers and
// commits, no data request ever carries a token before it is proven, and the
// epoch, its generation chain and its storage key never retire.
func TestRenewalsWithAFlakyProofNeverSendAnUnprovenToken(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	attempts := map[string]int{}
	var attemptsMu sync.Mutex
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	fixture.cluster.setReview(func(header string) (int, string) {
		attemptsMu.Lock()
		defer attemptsMu.Unlock()
		attempts[header]++
		if attempts[header]%2 == 1 {
			return http.StatusServiceUnavailable, ""
		}
		return http.StatusOK, "principal-value-reader"
	})
	key := fixture.service.captureCredentialID()
	for renewal := 0; renewal < 100; renewal++ {
		fixture.renew()
		for step := 0; step < 6; step++ {
			var readers sync.WaitGroup
			for range 4 {
				readers.Add(1)
				go func() {
					defer readers.Done()
					if err := fixture.read(); err != nil {
						t.Errorf("renewal %d: read failed inside the grace window: %v", renewal, err)
					}
				}()
			}
			if err := fixture.service.commitCredential(t.Context(), func() error { return nil }); err != nil {
				t.Errorf("renewal %d: commit failed inside the grace window: %v", renewal, err)
			}
			readers.Wait()
			fixture.settle()
			fixture.clock.advance(5 * time.Second)
		}
		if t.Failed() {
			return
		}
	}
	if !fixture.alive() || fixture.service.captureCredentialID() != key {
		t.Fatal("a flaky proof retired the epoch or changed its storage key")
	}
	fixture.assertOnlyProvenTokensReadData("principal-value-reader")
	if served := fixture.service.identity.servingHeader; served != fmt.Sprintf("Bearer token-value-%04d", fixture.issued.Load()) {
		t.Fatal("the last renewal was not promoted after its proof")
	}
}

// The bound: with the renewal's proof unavailable, reads use the previous
// token until 30 s before its expiry, without waiting on the proof. From then
// on every read and commit fails fast with proof_unreachable and no data
// request leaves the process; nothing retires and the generation and storage
// key stay. A later proof of the same identity resumes with no rebuild.
func TestProofUnavailableServesThePreviousTokenUntilTheBound(t *testing.T) {
	logs := captureLogLines(t, "info")
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	served := fixture.service.identity.servingHeader
	expires := fixture.service.identity.servingExpires
	key, generation := fixture.service.captureCredentialID(), fixture.service.identity.requestGeneration.Load()
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	fixture.renew()
	for fixture.clock.Now().Before(expires.Add(-proofGraceMargin - time.Second)) {
		if err := fixture.read(); err != nil {
			t.Fatalf("read inside the bound failed: %v", err)
		}
		fixture.settle()
		fixture.clock.advance(10 * time.Second)
	}
	for _, event := range fixture.cluster.snapshot() {
		if event.kind == "data" && event.header != served {
			t.Fatal("a read inside the bound did not use the previous token")
		}
	}
	fixture.clock.advance(expires.Add(-proofGraceMargin).Sub(fixture.clock.Now()))
	before := fixture.cluster.count("data")
	for range 5 {
		if err := fixture.read(); proofCode(err) != "proof_unreachable" {
			t.Fatalf("read past the bound = %v, want proof_unreachable", err)
		}
	}
	if err := fixture.service.commitCredential(t.Context(), func() error { t.Fatal("committed past the bound"); return nil }); proofCode(err) != "proof_unreachable" {
		t.Fatalf("commit past the bound = %v", err)
	}
	if fixture.cluster.count("data") != before {
		t.Fatal("a data request left the process past the bound")
	}
	if !fixture.alive() || fixture.service.captureCredentialID() != key || fixture.service.identity.requestGeneration.Load() != generation {
		t.Fatal("a proof outage retired the epoch or changed its generation or storage key")
	}
	status := fixture.service.effectiveCaptureStatus()
	if status.Reason != "proof_unreachable" || status.ProofSince == nil {
		t.Fatalf("capture status = %+v, want the proof named as the cause", status)
	}
	warnings := visible(logs.all())
	if len(warnings) != 1 || !strings.Contains(warnings[0], `cluster "proof" cannot prove its renewed credential (5xx)`) {
		t.Fatalf("failing episode lines = %q", warnings)
	}

	// The proof comes back: the next retry proves the same identity and the
	// renewed token serves, with no rebuild.
	fixture.cluster.setReview(reviewFor("principal-value-reader"))
	fixture.clock.advance(proofBackoffCap)
	eventually(t, "reads to resume", func() bool {
		err := fixture.read()
		fixture.settle()
		return err == nil
	})
	if !fixture.alive() || fixture.service.captureCredentialID() != key || fixture.service.identity.requestGeneration.Load() != generation+1 {
		t.Fatal("resuming after the proof rebuilt the epoch")
	}
	if status := fixture.service.effectiveCaptureStatus(); status.Reason != "" {
		t.Fatalf("capture status after resume = %+v", status)
	}
	lines := visible(logs.all())
	if len(lines) != 2 || !strings.Contains(lines[1], "requests resumed") || strings.HasPrefix(lines[1], "WARN") {
		t.Fatalf("recovery lines = %q, want one info line", lines)
	}
	fixture.assertOnlyProvenTokensReadData("principal-value-reader")
}

// Public reads the caller did not bind to the credential epoch (the admin
// controller and the AI tools call them that way) still send only the proven
// token while a renewal is being proved: inside the grace window they read with
// the previous token, and past the bound no data request leaves the process.
// A request made straight on the client with no epoch at all is refused.
func TestUnboundReadsNeverSendAnUnprovenRenewal(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	served := fixture.service.identity.servingHeader
	expires := fixture.service.identity.servingExpires
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	fixture.renew()
	if err := fixture.read(); err != nil {
		t.Fatalf("bound read inside the grace window failed: %v", err)
	}
	fixture.settle()
	if candidate := fmt.Sprintf("Bearer token-value-%04d", fixture.issued.Load()); candidate == served {
		t.Fatal("the source did not renew")
	}
	resource := func(id, version, name, kind string) ResourceDefinition {
		return ResourceDefinition{ID: id, Version: version, Resource: name, Kind: kind, Namespaced: true, Verbs: []string{"get", "list"}, Available: true}
	}
	discovered := context.WithValue(t.Context(), discoveryContextKey{}, Discovery{Resources: []ResourceDefinition{
		resource("core~v1~configmaps", "v1", "configmaps", "ConfigMap"),
		resource("core~v1~events", "v1", "events", "Event"),
	}})
	reads := []struct {
		name string
		call func() error
		// partial marks a read that reports a failed request inside its
		// answer instead of returning it.
		partial bool
	}{
		{"PodLogs", func() error {
			_, err := fixture.service.PodLogs(t.Context(), "default", "pod", "", false, 0, 0)
			return err
		}, false},
		{"List", func() error {
			_, err := fixture.service.List(discovered, ListOptions{ResourceID: "core~v1~configmaps", Namespace: "default"})
			return err
		}, false},
		{"ListEvents", func() error {
			_, err := fixture.service.ListEvents(discovered, EventOptions{Namespace: "default"})
			return err
		}, false},
		{"Releases", func() error {
			page, err := fixture.service.Releases(t.Context(), HelmReleaseOptions{})
			// The fake answers every path with a namespace list, so the
			// Secret read itself is unsupported; only a credential or proof
			// class means the read was refused before it left.
			for _, failure := range page.Partial {
				if strings.HasPrefix(failure.Class, "proof_") || failure.Class == "unauthorized" {
					return errors.New(failure.Class)
				}
			}
			return err
		}, true},
	}
	unproven := func(from int) int {
		count := 0
		for _, event := range fixture.cluster.snapshot()[from:] {
			if event.kind == "data" && event.header != served {
				count++
			}
		}
		return count
	}
	for _, read := range reads {
		before := len(fixture.cluster.snapshot())
		data := fixture.cluster.count("data")
		if err := read.call(); err != nil {
			t.Fatalf("%s inside the grace window = %v, want the previous token to serve", read.name, err)
		}
		fixture.settle()
		if fixture.cluster.count("data") == data {
			t.Fatalf("%s inside the grace window sent no data request", read.name)
		}
		if n := unproven(before); n != 0 {
			t.Fatalf("%s inside the grace window sent %d data requests without the proven token", read.name, n)
		}
	}

	fixture.clock.advance(expires.Add(-proofGraceMargin + time.Second).Sub(fixture.clock.Now()))
	for _, read := range reads {
		data := fixture.cluster.count("data")
		if err := read.call(); err == nil || !read.partial && proofCode(err) != "proof_unreachable" {
			t.Fatalf("%s past the bound = %v, want proof_unreachable", read.name, err)
		}
		fixture.settle()
		if fixture.cluster.count("data") != data {
			t.Fatalf("%s past the bound sent a data request", read.name)
		}
	}

	data := fixture.cluster.count("data")
	var out map[string]any
	if err := fixture.service.client.GetJSON(t.Context(), "/api/v1/namespaces", &out); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("unbound client read = %v, want ErrCredentialUnavailable", err)
	}
	if _, err := fixture.service.client.openPodLogStream(t.Context(), "/api/v1/namespaces/default/pods/pod/log", false); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("unbound log stream = %v, want ErrCredentialUnavailable", err)
	}
	if fixture.cluster.count("data") != data {
		t.Fatal("an unbound client request reached the cluster")
	}
	if !fixture.alive() {
		t.Fatal("a proof outage retired the epoch")
	}
}

// A proof that shows a different identity retires the epoch at once, inside
// the grace window and after it. Reads before that moment used the previous,
// proven token, and commits after it are refused.
func TestProofOfADifferentPrincipalRetiresAtOnce(t *testing.T) {
	for _, pastBound := range []bool{false, true} {
		t.Run(fmt.Sprintf("past_bound=%t", pastBound), func(t *testing.T) {
			isolateProblems(t, 10*time.Minute, newFakeClock().Now)
			fixture := newProofFixture(t)
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			expires := fixture.service.identity.servingExpires
			fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
			fixture.renew()
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.commitCredential(t.Context(), func() error { return nil }); err != nil {
				t.Fatalf("commit before the identity change failed: %v", err)
			}
			fixture.settle()
			if pastBound {
				fixture.clock.advance(expires.Sub(fixture.clock.Now()))
				if err := fixture.read(); proofCode(err) != "proof_unreachable" {
					t.Fatalf("read past the bound = %v", err)
				}
				// The read retried the proof; it ends before the identity
				// changes, so its backoff runs from this moment.
				fixture.settle()
			}
			fixture.cluster.setReview(reviewFor("principal-value-other"))
			if pastBound {
				fixture.clock.advance(proofBackoffCap)
			} else {
				// Inside the window: the first retry is due after one second.
				fixture.clock.advance(2 * time.Second)
			}
			eventually(t, "the epoch to retire", func() bool {
				err := fixture.read()
				fixture.settle()
				return errors.Is(err, ErrCredentialUnavailable) && !fixture.alive()
			})
			if err := fixture.service.commitCredential(t.Context(), func() error { t.Fatal("committed after retire"); return nil }); err == nil {
				t.Fatal("a commit after the identity change was accepted")
			}
			fixture.assertOnlyProvenTokensReadData("principal-value-reader")
		})
	}
}

// A newer candidate arriving while the proof is unavailable does not move the
// bound: it still ends 30 s before the previous, proven token's expiry.
func TestANewerCandidateDoesNotMoveTheBound(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	expires := fixture.service.identity.servingExpires
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	// The first candidate lives two minutes, so the source renews it at
	// half its life, inside the grace window.
	fixture.lifetime.Store(int64(2 * time.Minute))
	fixture.renew()
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	first := fixture.issued.Load()
	fixture.clock.advance(65 * time.Second)
	if err := fixture.read(); err != nil {
		t.Fatalf("read inside the bound failed: %v", err)
	}
	if fixture.issued.Load() == first {
		t.Fatal("no second candidate was issued")
	}
	fixture.settle()
	fixture.clock.advance(expires.Add(-proofGraceMargin).Sub(fixture.clock.Now()))
	if err := fixture.read(); proofCode(err) != "proof_unreachable" {
		t.Fatalf("a newer candidate moved the bound: %v", err)
	}
}

// No grace when the previous token is past its bound already (a clock jump),
// was answered 401 while a candidate waited, or was never proven.
func TestNoGraceForAnExpiredRejectedOrNeverProvenPreviousToken(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		isolateProblems(t, 10*time.Minute, newFakeClock().Now)
		fixture := newProofFixture(t)
		if err := fixture.read(); err != nil {
			t.Fatal(err)
		}
		fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
		fixture.clock.advance(2 * time.Hour)
		before := fixture.cluster.count("data")
		if err := fixture.read(); proofCode(err) != "proof_unreachable" {
			t.Fatalf("read after a clock jump = %v", err)
		}
		if fixture.cluster.count("data") != before || !fixture.alive() {
			t.Fatal("an expired previous token served or the epoch retired")
		}
	})
	t.Run("answered 401", func(t *testing.T) {
		isolateProblems(t, 10*time.Minute, newFakeClock().Now)
		fixture := newProofFixture(t)
		if err := fixture.read(); err != nil {
			t.Fatal(err)
		}
		served := fixture.service.identity.servingHeader
		fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
		fixture.renew()
		fixture.cluster.mu.Lock()
		fixture.cluster.rejectData[served] = true
		fixture.cluster.mu.Unlock()
		if err := fixture.read(); err == nil {
			t.Fatal("a rejected token read data")
		}
		fixture.settle()
		before := fixture.cluster.count("data")
		if err := fixture.read(); proofCode(err) != "proof_unreachable" {
			t.Fatalf("read after the previous token was answered 401 = %v", err)
		}
		if fixture.cluster.count("data") != before {
			t.Fatal("a token answered 401 kept serving")
		}
	})
	t.Run("never proven", func(t *testing.T) {
		isolateProblems(t, 10*time.Minute, newFakeClock().Now)
		fixture := newProofFixture(t)
		fixture.cluster.setReview(reviewDown(http.StatusForbidden))
		if err := fixture.read(); err != nil {
			t.Fatal(err)
		}
		if fixture.service.identity.known {
			t.Fatal("an unproved token proved an identity")
		}
		fixture.renew()
		before := fixture.cluster.count("data")
		if err := fixture.read(); !errors.Is(err, ErrCredentialUnavailable) || fixture.alive() {
			t.Fatalf("a never-proven epoch bridged a token change: %v", err)
		}
		if fixture.cluster.count("data") != before {
			t.Fatal("a never-proven epoch sent the new token")
		}
	})
}

// A review-based epoch does not depend on the provider proof: with review
// healthy and the provider proof down, 100 renewals fail no read, write no
// warning and never call the provider.
func TestReviewBasedEpochIgnoresAProviderProofThatIsDown(t *testing.T) {
	logs := captureLogLines(t, "info")
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
		return "", proofUnavailable, &proofError{class: "unreachable"}
	})
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		fixture.renew()
		if err := fixture.read(); err != nil {
			t.Fatalf("a renewal failed a read: %v", err)
		}
		fixture.settle()
	}
	if fixture.calls.Load() != 0 || !fixture.alive() {
		t.Fatalf("the provider proof was called %d times on a review-based epoch", fixture.calls.Load())
	}
	if lines := visible(logs.all()); len(lines) != 0 {
		t.Fatalf("a provider proof outage warned on a review-based epoch: %q", lines)
	}
	if status := fixture.service.effectiveCaptureStatus(); status.Reason != "" {
		t.Fatalf("capture status = %+v", status)
	}
}

// Identity review that is throttled, failing or timing out for the candidate
// is unavailable, never a retire: the previous token serves inside the bound.
func TestReviewOutagesAtARenewalNeverRetire(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolateProblems(t, 10*time.Minute, newFakeClock().Now)
			fixture := newProofFixture(t)
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			fixture.cluster.setReview(reviewDown(status))
			fixture.renew()
			for range 3 {
				if err := fixture.read(); err != nil {
					t.Fatalf("read inside the bound failed: %v", err)
				}
				fixture.settle()
			}
			if !fixture.alive() {
				t.Fatal("a review outage retired the epoch")
			}
			fixture.assertOnlyProvenTokensReadData("principal-value-reader")
		})
	}
}

// A definite answer gets no grace: a candidate the cluster answers 401 in its
// review, or one the provider rejects, retires the epoch at once.
func TestDefiniteRejectionsGetNoGrace(t *testing.T) {
	t.Run("review 401", func(t *testing.T) {
		isolateProblems(t, 10*time.Minute, newFakeClock().Now)
		fixture := newProofFixture(t)
		if err := fixture.read(); err != nil {
			t.Fatal(err)
		}
		fixture.cluster.setReview(reviewDown(http.StatusUnauthorized))
		fixture.renew()
		// The previous token may serve while the review is in flight; its
		// answer retires the epoch with no grace.
		_ = fixture.read()
		fixture.settle()
		if err := fixture.service.validateCredential(t.Context()); !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrCredentialUnavailable) || fixture.alive() {
			t.Fatalf("a candidate answered 401 got grace: %v", err)
		}
		fixture.assertOnlyProvenTokensReadData("principal-value-reader")
	})
	t.Run("provider rejects", func(t *testing.T) {
		isolateProblems(t, 10*time.Minute, newFakeClock().Now)
		fixture := newProofFixture(t)
		fixture.cluster.setReview(reviewDown(http.StatusForbidden))
		fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
			return "provider:reader", proofProven, nil
		})
		if err := fixture.read(); err != nil {
			t.Fatal(err)
		}
		if fixture.service.identity.source != principalProvider {
			t.Fatal("the epoch did not start on the provider proof")
		}
		fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
			return "", proofUnavailable, fmt.Errorf("signature does not match: %w", ErrUnauthorized)
		})
		fixture.renew()
		_ = fixture.read()
		fixture.settle()
		// The rejection retires the epoch as soon as any request sees it.
		if err := fixture.service.validateCredential(t.Context()); !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrCredentialUnavailable) || fixture.alive() {
			t.Fatalf("a candidate the provider rejected got grace: %v", err)
		}
		fixture.assertOnlyProvenTokensReadData("provider:reader")
	})
}

// A never-proven epoch with an unreachable proof host fails no read because of
// the proof, reports proof_unreachable, warns exactly once naming the host,
// and calls the host at most once per backoff window, also from the
// successor epoch a new token starts.
func TestNeverProvenEpochWithAnUnreachableProofHost(t *testing.T) {
	logs := captureLogLines(t, "info")
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	fixture.cluster.setReview(reviewDown(http.StatusForbidden))
	fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
		return "", proofUnavailable, &proofError{class: "unreachable"}
	})
	fixture.service.SetChangeStorage(storage.NewMemory())
	for range 20 {
		if err := fixture.read(); err != nil {
			t.Fatalf("the proof failed a read on a never-proven epoch: %v", err)
		}
		fixture.settle()
		fixture.clock.advance(10 * time.Millisecond)
	}
	if calls := fixture.calls.Load(); calls != 1 {
		t.Fatalf("the unreachable host was called %d times inside one backoff window", calls)
	}
	if status := fixture.service.effectiveCaptureStatus(); status.Mode != "demand" || status.Reason != "proof_unreachable" {
		t.Fatalf("capture status = %+v", status)
	}
	// A new token inside the backoff window starts a new byte-bound epoch.
	fixture.source.rejectBearer(fixture.service.identity.servingHeader)
	if err := fixture.read(); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("a new token kept the never-proven epoch: %v", err)
	}
	successor := newService(t.Context(), fixture.service.client, fixture.service.Scope(), time.Minute)
	if successor.identity.ctx.Err() != nil || fixture.calls.Load() != 1 {
		t.Fatalf("the successor epoch waited on the host again: calls=%d", fixture.calls.Load())
	}
	want := `WARN kubernetes: cluster "proof" cannot reach the identity proof host ` + proofHostName + ` (unreachable); allow outbound HTTPS to it so credential renewals keep collection continuous`
	if lines := visible(logs.all()); len(lines) != 1 || lines[0] != want {
		t.Fatalf("unreachable host lines = %q", lines)
	}
}

// A proof-based epoch whose proof host goes dark for three simulated hours
// never retires and never sends an unproven token; requests fail with
// proof_unreachable once the bound passes, the warning repeats at most once
// per throttle window, and when the host returns requests resume with no
// rebuild.
func TestProofBasedEpochSurvivesAThreeHourProofOutage(t *testing.T) {
	logs := captureLogLines(t, "info")
	fixture := newProofFixture(t)
	clock := fixture.clock
	isolateProblems(t, 10*time.Minute, clock.Now)
	fixture.cluster.setReview(reviewDown(http.StatusForbidden))
	var down atomic.Bool
	fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
		if down.Load() {
			return "", proofUnavailable, &proofError{class: "unreachable"}
		}
		return "provider:reader", proofProven, nil
	})
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	key, generation := fixture.service.captureCredentialID(), fixture.service.identity.requestGeneration.Load()
	down.Store(true)
	start := clock.Now()
	failed := 0
	for clock.Now().Sub(start) < 3*time.Hour {
		if err := fixture.read(); err != nil {
			if proofCode(err) != "proof_unreachable" {
				t.Fatalf("read during the outage = %v", err)
			}
			failed++
		}
		fixture.settle()
		clock.advance(time.Minute)
	}
	if failed == 0 || !fixture.alive() || fixture.service.captureCredentialID() != key || fixture.service.identity.requestGeneration.Load() != generation {
		t.Fatalf("a three-hour proof outage failed %d reads, alive=%t, or changed the epoch", failed, fixture.alive())
	}
	if status := fixture.service.effectiveCaptureStatus(); status.Reason != "proof_unreachable" {
		t.Fatalf("capture status during the outage = %+v", status)
	}
	warnings := visible(logs.all())
	if len(warnings) < 1 || len(warnings) > 19 {
		t.Fatalf("outage wrote %d warnings, want one per throttle window at most", len(warnings))
	}
	for _, line := range warnings[1:] {
		if !strings.Contains(line, "still failing") {
			t.Fatalf("a repeated warning has no count: %q", line)
		}
	}
	down.Store(false)
	clock.advance(proofBackoffCap)
	eventually(t, "reads to resume", func() bool {
		err := fixture.read()
		fixture.settle()
		return err == nil
	})
	if !fixture.alive() || fixture.service.captureCredentialID() != key {
		t.Fatal("the epoch rebuilt when the proof host returned")
	}
	fixture.assertOnlyProvenTokensReadData("provider:reader")
}

// No token, token digest, claim or principal value reaches a log line at any
// level, an error or the capture status, and a grace window that ends in a
// proof writes nothing at default level.
func TestProofLogsAndErrorsCarryNoSecretValues(t *testing.T) {
	logs := captureLogLines(t, "debug")
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	// A grace window that ends in a proof.
	var failOnce atomic.Bool
	failOnce.Store(true)
	fixture.cluster.setReview(func(string) (int, string) {
		if failOnce.Swap(false) {
			return http.StatusServiceUnavailable, ""
		}
		return http.StatusOK, "principal-value-reader"
	})
	fixture.renew()
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	fixture.settle()
	fixture.clock.advance(2 * time.Second)
	eventually(t, "the candidate to be promoted", func() bool {
		_ = fixture.read()
		fixture.settle()
		return fixture.service.identity.servingHeader == fmt.Sprintf("Bearer token-value-%04d", fixture.issued.Load())
	})
	if lines := visible(logs.all()); len(lines) != 0 {
		t.Fatalf("a grace window that ended in a proof wrote at default level: %q", lines)
	}
	// Then an outage past the bound and a different identity.
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	fixture.renew()
	_ = fixture.read()
	fixture.settle()
	fixture.clock.advance(3 * time.Minute)
	failure := fixture.read()
	fixture.cluster.setReview(reviewFor("principal-value-other"))
	fixture.clock.advance(proofBackoffCap)
	eventually(t, "the epoch to retire", func() bool {
		_ = fixture.read()
		fixture.settle()
		return !fixture.alive()
	})
	status := fmt.Sprintf("%+v", fixture.service.effectiveCaptureStatus())
	texts := append(logs.all(), failure.Error(), DiagnoseError(failure).Message, DiagnoseError(failure).Action, status)
	secrets := []string{"token-value-", "principal-value-", "uid-value-", "assertion-for-"}
	for n := int64(1); n <= fixture.issued.Load(); n++ {
		digest := sha256.Sum256(fmt.Appendf(nil, "Bearer token-value-%04d", n))
		secrets = append(secrets, hex.EncodeToString(digest[:])[:16])
	}
	for _, text := range texts {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Fatalf("a secret value reached a log line, error or status: %q", text)
			}
		}
	}
}

// lateProofCase is a process that booted while the provider proof host was
// down, so its epoch started unknown and byte-bound, collected an inventory and
// committed under it; then the host came back. Review is refused, so the
// provider proof is the principal source.
type lateProofCase struct {
	fixture  *proofFixture
	provider storage.Provider
	first    *Service
	registry *ServiceRegistry
	warm     inventoryGeneration
	down     *atomic.Bool
}

func bootDuringProofOutage(t *testing.T) *lateProofCase {
	t.Helper()
	fixture := newProofFixture(t)
	fixture.cluster.setReview(reviewDown(http.StatusForbidden))
	down := &atomic.Bool{}
	down.Store(true)
	fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
		if down.Load() {
			return "", proofUnavailable, &proofError{class: "unreachable"}
		}
		return "provider:reader", proofProven, nil
	})
	provider := storage.NewMemory()
	first := newService(t.Context(), fixture.service.client, Scope{OrgID: "default", ClusterID: "proof", CredentialID: "configured"}, time.Minute)
	first.SetChangeStorage(provider)
	enableEvidence(first)
	if first.identity.known || first.identity.ctx.Err() != nil {
		t.Fatal("the epoch did not boot unknown with the proof host down")
	}
	observed := time.Now().UTC()
	warm := inventoryGeneration{
		Revision:     "warm-1",
		FirstReadyAt: observed.Add(-time.Hour),
		View: kubeindex.Snapshot{
			Kinds:   map[string]kubeindex.KindStatus{"Pod": {State: "ready", Records: 1, ObservedAt: observed, CompletedAt: observed}},
			Records: []kubeindex.Record{{Kind: "Pod", UID: "warm-pod", Name: "checkout", Namespace: "shop"}},
		},
	}
	first.changes.mu.Lock()
	first.changes.inventories[first.indexScope()] = &inventoryCache{generation: warm}
	first.changes.mu.Unlock()
	for range 6 {
		if err := first.validateCredential(t.Context()); err != nil {
			t.Fatalf("the proof host being down failed a request: %v", err)
		}
		if err := first.commitCredential(t.Context(), func() error { return nil }); err != nil {
			t.Fatalf("the proof host being down failed a commit: %v", err)
		}
		settleProofs(t, first.client)
		fixture.clock.advance(5 * time.Second)
	}
	if status := first.effectiveCaptureStatus(); status.Mode != "demand" {
		t.Fatalf("an unproven epoch captured %q", status.Mode)
	}
	registry := NewServiceRegistry(first)
	t.Cleanup(registry.Close)
	return &lateProofCase{fixture: fixture, provider: provider, first: first, registry: registry, warm: warm, down: down}
}

// prove brings the proof host back and returns the service the registry hands
// the next request.
func (c *lateProofCase) prove(t *testing.T) *Service {
	t.Helper()
	c.down.Store(false)
	c.fixture.clock.advance(proofBackoffCap)
	eventually(t, "the late proof", func() bool {
		if err := c.first.validateCredential(t.Context()); err != nil {
			t.Fatalf("the late proof failed a request: %v", err)
		}
		settleProofs(t, c.first.client)
		c.first.identity.mu.Lock()
		defer c.first.identity.mu.Unlock()
		return c.first.identity.upgradeReady
	})
	resolved, err := c.registry.ResolveCluster("default", "proof")
	if err != nil {
		t.Fatalf("the request that found the epoch proven failed: %v", err)
	}
	if resolved == c.first || !resolved.identity.known || c.first.identity.ctx.Err() == nil {
		t.Fatal("the late proof did not move the cluster to a proven successor epoch")
	}
	return resolved
}

// enableEvidence turns on the persisted capture state, as the registry does
// when capture starts.
func enableEvidence(service *Service) {
	service.changes.mu.Lock()
	service.changes.stores = make(map[kubeindex.Scope]*kubechanges.Store)
	service.changes.evidenceEnabled = true
	service.changes.mu.Unlock()
}

// bootHealthyReplica starts a second process on the same cluster and store
// with the proof host up from its first request.
func (c *lateProofCase) bootHealthyReplica(t *testing.T) *Service {
	t.Helper()
	client, err := NewClient(Config{Endpoint: c.fixture.cluster.server.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client.proofs = newCredentialProofs(c.fixture.clock.Now)
	var issued atomic.Int64
	source := &gkeCredentialSource{expiringBearerSource: &expiringBearerSource{now: c.fixture.clock.Now, skew: 2 * time.Minute, refresh: func(context.Context) (string, time.Time, error) {
		return fmt.Sprintf("replica-token-%04d", issued.Add(1)), c.fixture.clock.Now().Add(time.Hour), nil
	}}, host: proofHostName}
	source.proof = func(context.Context, string) (string, proofOutcome, error) {
		return "provider:reader", proofProven, nil
	}
	client.credentials = source
	replica := newService(t.Context(), client, Scope{OrgID: "default", ClusterID: "proof", CredentialID: "configured"}, time.Minute)
	replica.SetChangeStorage(c.provider)
	enableEvidence(replica)
	if !replica.identity.known {
		t.Fatal("the healthy replica did not prove its identity")
	}
	return replica
}

// After a restart with the proof host down, the never-proven epoch collects
// and commits; when the host returns, the late proof moves the cluster to a
// proven successor quietly. The successor serves the inventory its
// predecessor collected at once: no first sync, nothing listed again, and no
// line at the default level.
func TestRestartWithTheProofHostDownNeverRebuilds(t *testing.T) {
	logs := captureLogLines(t, "info")
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	c := bootDuringProofOutage(t)
	before := len(visible(logs.all()))
	dataReads := func() int {
		n := 0
		for _, event := range c.fixture.cluster.snapshot() {
			if event.kind == "data" {
				n++
			}
		}
		return n
	}
	reads := dataReads()
	successor := c.prove(t)
	snapshot, status, err := successor.CachedIndexSnapshot(t.Context(), "Pod")
	if err != nil {
		t.Fatalf("the successor's first read failed: %v", err)
	}
	if status.State != "ready" || len(snapshot.Records) != 1 || snapshot.Records[0].UID != "warm-pod" {
		t.Fatalf("the successor did not show the warm inventory: state=%s records=%d", status.State, len(snapshot.Records))
	}
	successor.changes.mu.Lock()
	cache := successor.changes.inventories[successor.indexScope()]
	successor.changes.mu.Unlock()
	if cache == nil || cache.generation.Revision != c.warm.Revision || !cache.generation.FirstReadyAt.Equal(c.warm.FirstReadyAt) {
		t.Fatal("the successor started a new inventory instead of the warm one")
	}
	if cache.generation.EvidenceBatch != nil {
		t.Fatal("the successor took over its predecessor's pending history save")
	}
	if dataReads() != reads {
		t.Fatal("the upgrade read the cluster again before the successor's own cycle")
	}
	if lines := visible(logs.all()); len(lines) != before {
		t.Fatalf("the quiet upgrade wrote at the default level: %q", lines[before:])
	}
}

// The successor of a late proof captures continuously, under the lease and
// storage keys a replica that booted healthy computes, so the two share one
// lease holder instead of each capturing under its own key.
func TestLateProofMovesTheEpochOntoTheKeysHealthyReplicasShare(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	c := bootDuringProofOutage(t)
	replica := c.bootHealthyReplica(t)
	if c.first.persistentCredentialID == replica.persistentCredentialID {
		t.Fatal("the unknown epoch already shared the proven keys; the case proves nothing")
	}
	successor := c.prove(t)
	if successor.persistentCredentialID != replica.persistentCredentialID || successor.historyPrincipal() != replica.historyPrincipal() {
		t.Fatal("the successor and a healthy replica use different lease or storage keys")
	}
	now := time.Now().UTC()
	if err := replica.changeStore().ClaimLease("owner-replica", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := successor.changeStore().ClaimLease("owner-upgraded", now, time.Minute); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatalf("the successor took a capture lease of its own beside the healthy replica: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	successor.StartCapture(ctx)
	defer successor.StopCapture()
	status, effective := successor.CaptureStatus(), successor.effectiveCaptureStatus()
	if status.Mode != "continuous" || effective.Mode != "continuous" {
		t.Fatalf("after the late proof the capture loop runs %q (effective %q), want continuous", status.Mode, effective.Mode)
	}
}

// With capture running, the supervisor hands the cluster to the proven
// successor while the old epoch's workers still cycle. The old inventory
// worker meets the ended epoch on its next commit; that is a stop, not a
// failure, so the successor still takes the warm inventory: it keeps its
// revision and first ready time and never shows the first sync again.
func TestLateProofHandoverWithCaptureRunningKeepsTheInventory(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	c := bootDuringProofOutage(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.registry.StartCapture(ctx)
	c.down.Store(false)
	c.fixture.clock.advance(proofBackoffCap)
	var successor *Service
	eventually(t, "the supervisor to hand over", func() bool {
		c.fixture.settle()
		c.registry.mu.Lock()
		defer c.registry.mu.Unlock()
		for _, entry := range c.registry.entries {
			if entry.Service != nil && entry.Service.identity != c.first.identity && entry.Service.identity.known {
				successor = entry.Service
				return true
			}
		}
		return false
	})
	resolved, err := c.registry.ResolveCluster("default", "proof")
	if err != nil || resolved.identity != successor.identity {
		t.Fatalf("the request after the handover did not get the successor: %v", err)
	}
	eventually(t, "the successor to capture continuously", func() bool { return resolved.CaptureStatus().Mode == "continuous" })
	resolved.changes.mu.Lock()
	cache := resolved.changes.inventories[resolved.indexScope()]
	resolved.changes.mu.Unlock()
	if cache == nil {
		t.Fatal("the successor has no inventory")
	}
	cache.mu.RLock()
	revision, firstReady := cache.generation.Revision, cache.generation.FirstReadyAt
	cache.mu.RUnlock()
	if revision == "" || firstReady.IsZero() {
		t.Fatalf("the successor lost the warm inventory: revision=%q first ready=%v", revision, firstReady)
	}
	if report, err := resolved.SyncHealth(t.Context()); err == nil && report.FirstReadyAt.IsZero() {
		t.Fatal("the successor reports no first ready publication, so it shows the first sync again")
	}
	if c.first.identity.ctx.Err() == nil {
		t.Fatal("the old epoch is still alive after the handover")
	}
}

// The successor takes the warm inventory only when its predecessor would serve
// it as healthy. A failed last cycle, an inventory authority that could not be
// verified, or kinds held back until the authority is proven leave the
// successor to collect its own inventory, so it never shows rows its
// predecessor withheld from readers.
func TestLateProofSuccessorInheritsOnlyAHealthyCache(t *testing.T) {
	for _, test := range []struct {
		name    string
		mark    func(cache *inventoryCache)
		inherit bool
	}{
		{"healthy", func(*inventoryCache) {}, true},
		{"failed commit_conflict", func(cache *inventoryCache) { cache.failed, cache.failedClass = true, "commit_conflict" }, false},
		{"failed timeout", func(cache *inventoryCache) { cache.failed, cache.failedClass = true, "timeout" }, false},
		{"failed forbidden", func(cache *inventoryCache) { cache.failed, cache.failedClass = true, "forbidden" }, false},
		{"failed unauthorized", func(cache *inventoryCache) { cache.failed, cache.failedClass = true, "unauthorized" }, false},
		{"authority unavailable", func(cache *inventoryCache) {
			cache.authorityFailure = unverifiedInventoryAuthority(errors.New("authority check failed"))
		}, false},
		{"authority unavailable, failed cycle", func(cache *inventoryCache) {
			unverified := unverifiedInventoryAuthority(errors.New("authority check failed"))
			cache.failed, cache.failedClass, cache.authorityFailure = true, errorClass(unverified), unverified
		}, false},
		{"questioned kinds", func(cache *inventoryCache) {
			cache.authorityQuestioned = inventoryQuestionedKinds{kinds: map[string]bool{"Pod": true}}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateProblems(t, 10*time.Minute, newFakeClock().Now)
			c := bootDuringProofOutage(t)
			c.first.changes.mu.Lock()
			old := c.first.changes.inventories[c.first.indexScope()]
			c.first.changes.mu.Unlock()
			old.mu.Lock()
			test.mark(old)
			old.mu.Unlock()
			successor := c.prove(t)
			successor.changes.mu.Lock()
			cache := successor.changes.inventories[successor.indexScope()]
			successor.changes.mu.Unlock()
			inherited := false
			if cache != nil {
				cache.mu.RLock()
				inherited = cache.generation.Revision == c.warm.Revision
				cache.mu.RUnlock()
			}
			if inherited != test.inherit {
				t.Fatalf("the successor inherited = %t, want %t", inherited, test.inherit)
			}
		})
	}
}

// The successor of a late-proven epoch takes the warm inventory only when it
// reads as the identity that collected it: the same token, or a renewed token
// proven for the same principal. A renewed token proven for another
// principal, or one the proof host cannot prove, starts its own inventory.
func TestLateProofSuccessorInheritsOnlyUnderTheSameIdentity(t *testing.T) {
	for _, test := range []struct {
		name    string
		renew   bool
		answer  func() (string, proofOutcome, error)
		inherit bool
	}{
		{"same token", false, nil, true},
		{"renewed token, same principal", true, func() (string, proofOutcome, error) { return "provider:reader", proofProven, nil }, true},
		{"renewed token, different principal", true, func() (string, proofOutcome, error) { return "provider:other", proofProven, nil }, false},
		{"renewed token, proof host down again", true, func() (string, proofOutcome, error) {
			return "", proofUnavailable, &proofError{class: "unreachable"}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateProblems(t, 10*time.Minute, newFakeClock().Now)
			c := bootDuringProofOutage(t)
			first := c.first.identity.servingHeader
			c.fixture.setProvider(func(_ context.Context, header string) (string, proofOutcome, error) {
				if header == first {
					return "provider:reader", proofProven, nil
				}
				return test.answer()
			})
			c.fixture.clock.advance(proofBackoffCap)
			eventually(t, "the late proof", func() bool {
				if err := c.first.validateCredential(t.Context()); err != nil {
					t.Fatalf("the late proof failed a request: %v", err)
				}
				settleProofs(t, c.first.client)
				c.first.identity.mu.Lock()
				defer c.first.identity.mu.Unlock()
				return c.first.identity.upgradeReady
			})
			if err := c.first.commitCredential(t.Context(), func() error { return nil }); err == nil || !c.first.identity.upgraded {
				t.Fatalf("the commit did not end the ready epoch: %v", err)
			}
			if test.renew {
				c.fixture.renew()
			}
			successor, err := c.registry.ResolveCluster("default", "proof")
			if err != nil {
				if test.inherit {
					t.Fatalf("the registry refused the successor: %v", err)
				}
				return
			}
			settleProofs(t, successor.client)
			successor.changes.mu.Lock()
			cache := successor.changes.inventories[successor.indexScope()]
			successor.changes.mu.Unlock()
			inherited := cache != nil && cache.generation.Revision == c.warm.Revision
			if inherited != test.inherit {
				t.Fatalf("the successor inherited the warm inventory = %t, want %t", inherited, test.inherit)
			}
			shown := 0
			if snapshot, _, err := successor.CachedIndexSnapshot(t.Context(), "Pod"); err == nil {
				shown = len(snapshot.Records)
			}
			if !test.inherit && shown != 0 {
				t.Fatalf("the successor served %d records it never read", shown)
			}
		})
	}
}

// A provider outage that asks for Retry-After: 120 gets at most one call per
// token per window.
func TestProofOutageHonoursRetryAfter(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	fixture.cluster.setReview(reviewDown(http.StatusForbidden))
	var throttled atomic.Bool
	var calls sync.Map
	fixture.setProvider(func(_ context.Context, header string) (string, proofOutcome, error) {
		if throttled.Load() {
			count, _ := calls.LoadOrStore(header, new(atomic.Int64))
			count.(*atomic.Int64).Add(1)
			return "", proofUnavailable, &proofError{class: "429", retryAfter: 120 * time.Second}
		}
		return "provider:reader", proofProven, nil
	})
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	throttled.Store(true)
	fixture.renew()
	start := fixture.clock.Now()
	for fixture.clock.Now().Sub(start) < 10*time.Minute {
		_ = fixture.read()
		fixture.settle()
		fixture.clock.advance(10 * time.Second)
	}
	calls.Range(func(_, value any) bool {
		if n := value.(*atomic.Int64).Load(); n > 6 {
			t.Fatalf("a throttled proof was called %d times in 10 minutes with Retry-After 120", n)
		}
		return true
	})
}

// The cache keeps at most two tokens across 1,000 renewals, and two headers
// used in turn are proved once each.
func TestProofCacheHoldsTwoTokens(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		fixture.renew()
		if err := fixture.service.validateCredential(t.Context()); err != nil {
			t.Fatal(err)
		}
		if size := fixture.service.client.proofCache().size(); size > maxProofEntries {
			t.Fatalf("proof cache holds %d tokens", size)
		}
	}
	if !fixture.alive() {
		t.Fatal("renewals retired the epoch")
	}
	alternate := newProofFixture(t)
	if err := alternate.service.acceptCredential(t.Context(), "Bearer header-a"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		header := "Bearer header-b"
		if i%2 == 1 {
			header = "Bearer header-a"
		}
		if err := alternate.service.acceptCredential(t.Context(), header); err != nil {
			t.Fatal(err)
		}
	}
	if reviews := alternate.cluster.count("review"); reviews != 2 {
		t.Fatalf("two alternating headers were reviewed %d times", reviews)
	}
}

// The capture mode follows the source's rotating() method and the proof
// outcome: an unproven rotating source runs on demand, naming an unreachable
// proof host; a proven one runs continuously; a static source is always
// continuous; EKS is rotating.
func TestEffectiveCaptureStatusForEachModeAndOutcome(t *testing.T) {
	for _, source := range []CredentialSource{&eksCredentialSource{expiringBearerSource: &expiringBearerSource{}}, &aksCredentialSource{expiringBearerSource: &expiringBearerSource{}}, &gkeCredentialSource{expiringBearerSource: &expiringBearerSource{}}, &tokenFileSource{}} {
		if rotating, ok := source.(interface{ rotating() bool }); !ok || !rotating.rotating() {
			t.Fatalf("%T is not rotating", source)
		}
	}
	if (&credentialSource{}).rotating() {
		t.Fatal("a static source is rotating")
	}
	for _, test := range []struct {
		name     string
		review   int
		provider func(context.Context, string) (string, proofOutcome, error)
		mode     string
		reason   string
	}{
		{"review proven", http.StatusOK, nil, "continuous", ""},
		{"provider proven", http.StatusForbidden, func(context.Context, string) (string, proofOutcome, error) {
			return "provider:reader", proofProven, nil
		}, "continuous", ""},
		{"not applicable", http.StatusForbidden, nil, "demand", "stable_principal_unverified"},
		{"provider unreachable", http.StatusForbidden, func(context.Context, string) (string, proofOutcome, error) {
			return "", proofUnavailable, &proofError{class: "unreachable"}
		}, "demand", "proof_unreachable"},
		{"provider throttled", http.StatusForbidden, func(context.Context, string) (string, proofOutcome, error) {
			return "", proofUnavailable, &proofError{class: "429"}
		}, "demand", "stable_principal_unverified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateProblems(t, 10*time.Minute, newFakeClock().Now)
			fixture := newProofFixture(t)
			fixture.service.SetChangeStorage(storage.NewMemory())
			fixture.cluster.setReview(func(string) (int, string) { return test.review, "principal-value-reader" })
			if test.provider != nil {
				fixture.setProvider(test.provider)
			}
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			if status := fixture.service.effectiveCaptureStatus(); status.Mode != test.mode || status.Reason != test.reason {
				t.Fatalf("capture status = %+v, want %s %q", status, test.mode, test.reason)
			}
		})
	}
	static := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "static"})
	static.SetChangeStorage(storage.NewMemory())
	static.client.credentials = &credentialSource{}
	if status := static.effectiveCaptureStatus(); status.Mode != "continuous" {
		t.Fatalf("static source capture status = %+v", status)
	}
}

// One principal source per epoch: a review-based epoch whose renewal the
// cluster now refuses to review does not fall back to the provider proof and
// does not retire; the previous token serves until the bound.
func TestReviewBasedEpochNeverFallsBackToTheProviderProof(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	expires := fixture.service.identity.servingExpires
	fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
		return "provider:reader", proofProven, nil
	})
	fixture.cluster.setReview(reviewDown(http.StatusForbidden))
	fixture.renew()
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	fixture.settle()
	fixture.clock.advance(expires.Add(-proofGraceMargin).Sub(fixture.clock.Now()))
	if err := fixture.read(); proofCode(err) != "proof_unavailable" || !fixture.alive() {
		t.Fatalf("a refused review on a review-based epoch = %v", err)
	}
	if fixture.calls.Load() != 0 {
		t.Fatal("a review-based epoch fell back to the provider proof")
	}
}

// One principal source per epoch: an epoch that started on the provider
// proof keeps it when identity review starts answering for the same identity.
func TestProofBasedEpochIgnoresALaterReview(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	fixture.cluster.setReview(reviewDown(http.StatusForbidden))
	fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
		return "provider:reader", proofProven, nil
	})
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	if fixture.service.identity.source != principalProvider {
		t.Fatal("the epoch did not start on the provider proof")
	}
	fixture.cluster.setReview(reviewFor("principal-value-reader"))
	for range 3 {
		fixture.renew()
		if err := fixture.read(); err != nil {
			t.Fatal(err)
		}
		fixture.settle()
		if err := fixture.read(); err != nil || !fixture.alive() {
			t.Fatalf("a later review retired a proof-based epoch: %v", err)
		}
	}
	if fixture.service.identity.source != principalProvider {
		t.Fatal("the epoch changed its principal source")
	}
}

// bootReplica starts a process on its own client against the fixture's
// cluster, sharing the fixture's storage, as a restart or a peer replica does.
func (fixture *proofFixture) bootReplica(provider storage.Provider) *Service {
	fixture.t.Helper()
	client, err := NewClient(Config{Endpoint: fixture.cluster.server.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		fixture.t.Fatal(err)
	}
	client.proofs = newCredentialProofs(fixture.clock.Now)
	var issued atomic.Int64
	client.credentials = &gkeCredentialSource{expiringBearerSource: &expiringBearerSource{now: fixture.clock.Now, skew: 2 * time.Minute, refresh: func(context.Context) (string, time.Time, error) {
		return fmt.Sprintf("replica-token-%04d", issued.Add(1)), fixture.clock.Now().Add(time.Hour), nil
	}}, host: proofHostName}
	service := newService(fixture.t.Context(), client, Scope{OrgID: "default", ClusterID: "proof", CredentialID: "configured"}, time.Minute)
	service.SetChangeStorage(provider)
	return service
}

// An unknown epoch whose token is proven before anything is committed ends
// quietly and restarts under the proven identity: its storage key and lease
// are the ones a fresh start computes, so a replica booted after the upgrade
// shares them. Review unavailable at boot never falls back to the provider
// proof, so both replicas end on the review principal.
func TestUnknownEpochProvenBeforeAnyCommitRestartsUnderTheFreshKey(t *testing.T) {
	logs := captureLogLines(t, "info")
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	provider := storage.NewMemory()
	fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
		return "provider:reader", proofProven, nil
	})
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	first := newService(t.Context(), fixture.service.client, fixture.service.Scope(), time.Minute)
	first.SetChangeStorage(provider)
	if first.identity.known || fixture.calls.Load() != 0 {
		t.Fatalf("review unavailable at boot fell back to the provider proof: known=%t calls=%d", first.identity.known, fixture.calls.Load())
	}
	fixture.cluster.setReview(reviewFor("principal-value-reader"))
	fixture.clock.advance(time.Minute)
	eventually(t, "the proof to land", func() bool {
		if err := first.validateCredential(t.Context()); err != nil {
			t.Fatalf("a read failed for the upgrade: %v", err)
		}
		settleProofs(t, first.client)
		first.identity.mu.Lock()
		defer first.identity.mu.Unlock()
		return first.identity.upgradeReady
	})
	// Reads go on with the same, now proven, token; the next commit ends the
	// epoch quietly before anything is written under its keys.
	ended := first.commitCredential(t.Context(), func() error { t.Fatal("committed under the unknown keys"); return nil })
	if ended == nil || countsAgainstCredential(ended) || first.identity.ctx.Err() == nil || !first.identity.upgraded {
		t.Fatalf("the upgrade did not end the epoch quietly: %v", ended)
	}
	successor := newService(t.Context(), first.client, first.Scope(), time.Minute)
	successor.SetChangeStorage(provider)
	if !successor.identity.known || successor.identity.source != principalReview {
		t.Fatal("the successor did not start from the cached review proof")
	}
	replica := fixture.bootReplica(provider)
	if !replica.identity.known {
		t.Fatal("the replica did not prove its identity")
	}
	if successor.scope.CredentialID != replica.scope.CredentialID || successor.persistentCredentialID != replica.persistentCredentialID || successor.historyPrincipal() != replica.historyPrincipal() {
		t.Fatal("the upgraded process and a fresh replica use different storage keys")
	}
	now := time.Now().UTC()
	if err := successor.changeStore().ClaimLease("owner-upgraded", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := replica.changeStore().ClaimLease("owner-replica", now, time.Minute); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatalf("the replica claimed a lease of its own: %v", err)
	}
	if fixture.calls.Load() != 0 {
		t.Fatal("a review-capable cluster called the provider proof")
	}
	if lines := visible(logs.all()); len(lines) != 0 {
		t.Fatalf("the quiet restart wrote at default level: %q", lines)
	}
}

// Review unavailable at boot on one replica and healthy on another: once
// review answers, both end on the same principal and storage key.
func TestReplicasConvergeWhenReviewWasDownAtOneBoot(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	provider := storage.NewMemory()
	fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
		return "provider:reader", proofProven, nil
	})
	healthy := fixture.bootReplica(provider)
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	degraded := fixture.bootReplica(provider)
	if degraded.identity.known {
		t.Fatal("review unavailable at boot proved an identity")
	}
	fixture.cluster.setReview(reviewFor("principal-value-reader"))
	fixture.clock.advance(time.Minute)
	eventually(t, "the degraded replica to converge", func() bool {
		if err := degraded.validateCredential(t.Context()); err != nil {
			t.Fatalf("a read failed while review recovered: %v", err)
		}
		settleProofs(t, degraded.client)
		if degraded.commitCredential(t.Context(), func() error { return nil }) != nil {
			degraded = newService(t.Context(), degraded.client, degraded.Scope(), time.Minute)
		}
		return degraded.identity.known
	})
	if degraded.historyPrincipal() != healthy.historyPrincipal() || degraded.persistentCredentialID != healthy.persistentCredentialID {
		t.Fatal("replicas booted with and without review ended on different keys")
	}
}

// A review the cluster refuses (403) for a renewal of a review-based epoch is
// unavailable, retried after its backoff: when review answers again the
// renewal is promoted within one backoff interval, with no retire.
func TestRefusedReviewOfARenewalIsRetried(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolateProblems(t, 10*time.Minute, newFakeClock().Now)
			fixture := newProofFixture(t)
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			var reviews atomic.Int32
			fixture.cluster.setReview(func(string) (int, string) {
				if reviews.Add(1) == 1 {
					return status, ""
				}
				return http.StatusOK, "principal-value-reader"
			})
			fixture.renew()
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			fixture.settle()
			fixture.clock.advance(2 * time.Second)
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			fixture.settle()
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			if !fixture.alive() || fixture.service.identity.servingHeader != fmt.Sprintf("Bearer token-value-%04d", fixture.issued.Load()) || reviews.Load() != 2 {
				t.Fatalf("the refused review was not retried within one backoff: alive=%t reviews=%d", fixture.alive(), reviews.Load())
			}
		})
	}
}

// A definite rejection retires the epoch once and makes the source drop the
// token, so the next epoch mints and proves a fresh one.
func TestRejectedCandidateIsDroppedAndTheNextEpochRecovers(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	var reviews atomic.Int32
	fixture.cluster.setReview(func(string) (int, string) {
		if reviews.Add(1) == 1 {
			return http.StatusUnauthorized, ""
		}
		return http.StatusOK, "principal-value-reader"
	})
	fixture.renew()
	_ = fixture.read()
	fixture.settle()
	rejected := fmt.Sprintf("Bearer token-value-%04d", fixture.issued.Load())
	if err := fixture.service.validateCredential(t.Context()); !errors.Is(err, ErrUnauthorized) || fixture.alive() {
		t.Fatalf("the rejected candidate did not retire the epoch: %v", err)
	}
	successor := newService(t.Context(), fixture.service.client, fixture.service.Scope(), time.Minute)
	if successor.identity.ctx.Err() != nil || !successor.identity.known || successor.identity.servingHeader == rejected {
		t.Fatal("the next epoch did not recover on a fresh token")
	}
}

// A candidate the cluster answers 401 gets no grace even when requests come
// further apart than the rejection's backoff: the epoch retires on the first
// request after the rejection instead of serving the previous token until the
// grace bound.
func TestRejectedCandidateRetiresWithSparseRequests(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	fixture.cluster.setReview(reviewDown(http.StatusUnauthorized))
	fixture.renew()
	_ = fixture.read()
	fixture.settle()
	fixture.clock.advance(time.Minute)
	if err := fixture.read(); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("read after the rejection's backoff = %v, want the rejection", err)
	}
	if fixture.alive() {
		t.Fatal("the epoch kept serving the previous token after its candidate was rejected")
	}
}

// A rejection is not cached for good: after its backoff it is handed to the
// first caller once more, and the same token is then proved again, for a
// provider that reissues it.
func TestRejectionExpiresAfterItsBackoff(t *testing.T) {
	clock := newFakeClock()
	proofs := newCredentialProofs(clock.Now)
	var calls atomic.Int32
	run := func(context.Context) (string, proofOutcome, error) {
		if calls.Add(1) == 1 {
			return "", proofUnavailable, ErrUnauthorized
		}
		return "provider:reader", proofProven, nil
	}
	wait := func(ch <-chan struct{}) {
		if ch != nil {
			<-ch
		}
	}
	_, done := proofs.lookup(proofByProvider, "Bearer same", run)
	wait(done)
	if slot, _ := proofs.lookup(proofByProvider, "Bearer same", run); slot.rejected == nil {
		t.Fatal("the rejection was not recorded")
	}
	clock.advance(2 * time.Second)
	if slot, done := proofs.lookup(proofByProvider, "Bearer same", run); slot.rejected == nil || done != nil {
		t.Fatal("the first lookup past the backoff did not get the rejection")
	}
	slot, done := proofs.lookup(proofByProvider, "Bearer same", run)
	if slot.attempted || done == nil {
		t.Fatal("an expired rejection was served from the cache")
	}
	wait(done)
	if slot, _ := proofs.lookup(proofByProvider, "Bearer same", run); slot.outcome != proofProven {
		t.Fatalf("the token was not proved again: %+v", slot)
	}
}

// STS answers: a throttling code is unavailable like a 429, other 400s are
// unavailable, and only a 401 or 403 rejects the signed request.
func TestSTSAnswersAreClassified(t *testing.T) {
	for _, test := range []struct {
		name, body, retryAfter string
		status                 int
		rejected               bool
		class                  string
	}{
		{"throttling json", `{"Error":{"Code":"Throttling","Message":"Rate exceeded","Type":"Sender"}}`, "", http.StatusBadRequest, false, "429"},
		{"throttling xml", `<ErrorResponse><Error><Type>Sender</Type><Code>Throttling</Code></Error></ErrorResponse>`, "", http.StatusBadRequest, false, "429"},
		{"request limit", `{"Error":{"Code":"RequestLimitExceeded"}}`, "", http.StatusBadRequest, false, "429"},
		{"other 400", `{"Error":{"Code":"ValidationError"}}`, "", http.StatusBadRequest, false, "4xx"},
		{"invalid client token", `{"Error":{"Code":"InvalidClientTokenId"}}`, "", http.StatusForbidden, true, ""},
		{"signature", `<ErrorResponse><Error><Code>SignatureDoesNotMatch</Code></Error></ErrorResponse>`, "", http.StatusForbidden, true, ""},
		{"expired", `{"Error":{"Code":"ExpiredToken"}}`, "", http.StatusForbidden, true, ""},
		{"429", ``, "7", http.StatusTooManyRequests, false, "429"},
		{"503", ``, "", http.StatusServiceUnavailable, false, "5xx"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer sts.Close()
			fake := &fakeSTS{server: sts}
			verifier := newEKSIdentityVerifier(sts.URL, "production", sts.Client())
			principal, outcome, err := verifier.principal(t.Context(), fake.token("reader", "s1", nil))
			if principal != "" || outcome != proofUnavailable || errors.Is(err, ErrUnauthorized) != test.rejected {
				t.Fatalf("outcome=%d rejected=%t err=%v", outcome, errors.Is(err, ErrUnauthorized), err)
			}
			var failure *proofError
			if !test.rejected && (!errors.As(err, &failure) || failure.class != test.class) {
				t.Fatalf("class = %v, want %s", err, test.class)
			}
			if test.retryAfter != "" && failure.retryAfter != 7*time.Second {
				t.Fatalf("Retry-After lost: %s", failure.retryAfter)
			}
		})
	}
}

// While the capture loop runs, CaptureStatus names a proof outage and since
// when, not only the status computed before the loop started.
func TestRunningCaptureStatusNamesTheProofOutage(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	expires := fixture.service.identity.servingExpires
	fixture.service.changes.mu.Lock()
	fixture.service.changes.captures[fixture.service.indexScope()] = &captureLoop{done: make(chan struct{}), status: CaptureStatus{Mode: "continuous"}}
	fixture.service.changes.mu.Unlock()
	if status := fixture.service.CaptureStatus(); status.Reason != "" || status.ProofSince != nil {
		t.Fatalf("healthy running status = %+v", status)
	}
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	fixture.renew()
	_ = fixture.read()
	fixture.settle()
	fixture.clock.advance(expires.Sub(fixture.clock.Now()))
	if err := fixture.read(); proofCode(err) != "proof_unreachable" {
		t.Fatal(err)
	}
	status := fixture.service.CaptureStatus()
	if status.Mode != "continuous" || status.Reason != "proof_unreachable" || status.ProofSince == nil {
		t.Fatalf("running capture status = %+v, want the proof outage named", status)
	}
}

// Only the source's current token becomes the serving one: a slow request
// still holding the previous proven token may send it, but never moves the
// serving token back.
func TestAStaleProvenTokenIsNotPromotedAgain(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	previous := fixture.service.identity.servingHeader
	fixture.renew()
	_ = fixture.read()
	fixture.settle()
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	current := fixture.service.identity.servingHeader
	if current == previous {
		t.Fatal("the renewal was not promoted")
	}
	generation := fixture.service.identity.requestGeneration.Load()
	header, _, _, err := fixture.service.acceptCredentialForRequest(t.Context(), previous)
	if err != nil || header != previous {
		t.Fatalf("a slow request with the previous proven token was refused: %v", err)
	}
	if fixture.service.identity.servingHeader != current || fixture.service.identity.requestGeneration.Load() != generation {
		t.Fatal("a stale token moved the serving token back")
	}
}

// A proof that hangs does not hold up reads or commits while the previous
// token serves: they all complete while the proof is still in flight.
func TestAHangingProofDoesNotDelayReadsOrCommits(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	fixture.cluster.setReview(reviewDown(http.StatusForbidden))
	release := make(chan struct{})
	var hang atomic.Bool
	fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
		if hang.Load() {
			// The proof ignores its own deadline, so it is certainly still
			// in flight while the reads below run.
			<-release
			return "", proofUnavailable, &proofError{class: "timeout"}
		}
		return "provider:reader", proofProven, nil
	})
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	hang.Store(true)
	fixture.renew()
	for range 50 {
		if err := fixture.read(); err != nil {
			t.Fatalf("read during a hanging proof failed: %v", err)
		}
		if err := fixture.service.commitCredential(t.Context(), func() error { return nil }); err != nil {
			t.Fatalf("commit during a hanging proof failed: %v", err)
		}
	}
	proofs := fixture.service.client.proofCache()
	proofs.mu.Lock()
	running := false
	for _, entry := range proofs.entries {
		running = running || entry.slots[proofByProvider].running
	}
	proofs.mu.Unlock()
	close(release)
	if !running {
		t.Fatal("the proof finished before the reads, so they were not shown independent of it")
	}
	fixture.settle()
}

// The request that finds an unknown epoch proven never fails for it: the
// registry hands it the successor epoch, and a read already in flight on the
// old epoch completes with the same token.
func TestProvenAfterStartUpgradeIsInvisibleToRequests(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	first := newService(t.Context(), fixture.service.client, Scope{OrgID: "default", ClusterID: "proof", CredentialID: "configured"}, time.Minute)
	registry := NewServiceRegistry(first)
	t.Cleanup(registry.Close)
	fixture.cluster.setReview(reviewFor("principal-value-reader"))
	fixture.clock.advance(time.Minute)
	read := func(service *Service) error {
		ctx, cancel := service.credentialContext(t.Context())
		defer cancel()
		var out map[string]any
		return service.client.GetJSON(ctx, "/api/v1/namespaces", &out)
	}
	eventually(t, "the proof to land", func() bool {
		if err := read(first); err != nil {
			t.Fatalf("a read on the unknown epoch failed: %v", err)
		}
		settleProofs(t, first.client)
		first.identity.mu.Lock()
		defer first.identity.mu.Unlock()
		return first.identity.upgradeReady
	})
	if err := read(first); err != nil {
		t.Fatalf("a read on the epoch ready to restart failed: %v", err)
	}
	resolved, err := registry.ResolveCluster("default", "proof")
	if err != nil {
		t.Fatalf("the request that found the epoch proven failed: %v", err)
	}
	if resolved == first || !resolved.identity.known || first.identity.ctx.Err() == nil {
		t.Fatal("the registry did not hand the request the successor epoch")
	}
	if err := read(resolved); err != nil {
		t.Fatalf("a read on the successor failed: %v", err)
	}
}

// A proof outage never hides a capture reason of the loop's own.
func TestRunningCaptureStatusKeepsItsOwnReason(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	expires := fixture.service.identity.servingExpires
	fixture.service.changes.mu.Lock()
	fixture.service.changes.captures[fixture.service.indexScope()] = &captureLoop{done: make(chan struct{}), status: CaptureStatus{Mode: "demand", Reason: "atomic_storage_unavailable"}}
	fixture.service.changes.mu.Unlock()
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	fixture.renew()
	_ = fixture.read()
	fixture.settle()
	fixture.clock.advance(expires.Sub(fixture.clock.Now()))
	_ = fixture.read()
	if status := fixture.service.CaptureStatus(); status.Reason != "atomic_storage_unavailable" {
		t.Fatalf("running capture status = %+v, want the loop's own reason", status)
	}
}

// When a background commit ends an epoch ready to restart before the
// registry does, the request still gets the successor, not the retired epoch.
func TestUpgradeRestartRaceStillHandsOverTheSuccessor(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	first := newService(t.Context(), fixture.service.client, Scope{OrgID: "default", ClusterID: "proof", CredentialID: "configured"}, time.Minute)
	registry := NewServiceRegistry(first)
	t.Cleanup(registry.Close)
	fixture.cluster.setReview(reviewFor("principal-value-reader"))
	fixture.clock.advance(time.Minute)
	eventually(t, "the proof to land", func() bool {
		if err := first.validateCredential(t.Context()); err != nil {
			t.Fatal(err)
		}
		settleProofs(t, first.client)
		first.identity.mu.Lock()
		defer first.identity.mu.Unlock()
		return first.identity.upgradeReady
	})
	// The commit wins the race and ends the epoch first.
	if err := first.commitCredential(t.Context(), func() error { return nil }); err == nil || !first.identity.upgraded {
		t.Fatalf("the commit did not end the ready epoch: %v", err)
	}
	if !first.endIfUpgradeReady() {
		t.Fatal("an epoch a commit already ended was not handed over")
	}
	resolved, err := registry.ResolveCluster("default", "proof")
	if err != nil || resolved == first || !resolved.identity.known {
		t.Fatalf("the request did not get the successor: %v", err)
	}
}

// A never-proven epoch whose provider proof fails for a reason a setting
// must fix says so at the default level, once, naming the host, and the
// capture status names the cause: a key URL off the allowed hosts, or a host
// that stops answering after start.
func TestNeverProvenEpochSurfacesProofHostProblems(t *testing.T) {
	t.Run("key host not allowed", func(t *testing.T) {
		logs := captureLogLines(t, "info")
		isolateProblems(t, 10*time.Minute, newFakeClock().Now)
		fixture := newProofFixture(t)
		fixture.cluster.setReview(reviewDown(http.StatusForbidden))
		fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
			return "", proofUnavailable, &proofError{class: "jwks_host_not_allowed"}
		})
		for range 3 {
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			fixture.settle()
		}
		if status := fixture.service.CaptureStatus(); status.Reason != "proof_host_not_allowed" {
			t.Fatalf("capture status = %+v", status)
		}
		lines := visible(logs.all())
		if len(lines) != 1 || !strings.Contains(lines[0], "(host not allowed)") || !strings.Contains(lines[0], proofHostName) {
			t.Fatalf("lines = %q", lines)
		}
	})
	t.Run("unreachable after start", func(t *testing.T) {
		logs := captureLogLines(t, "info")
		isolateProblems(t, 10*time.Minute, newFakeClock().Now)
		fixture := newProofFixture(t)
		fixture.cluster.setReview(reviewDown(http.StatusForbidden))
		var down atomic.Bool
		fixture.setProvider(func(context.Context, string) (string, proofOutcome, error) {
			if down.Load() {
				return "", proofUnavailable, &proofError{class: "unreachable"}
			}
			return "", proofUnavailable, &proofError{class: "5xx"}
		})
		if err := fixture.read(); err != nil {
			t.Fatal(err)
		}
		fixture.settle()
		if lines := visible(logs.all()); len(lines) != 0 {
			t.Fatalf("a transient proof failure warned: %q", lines)
		}
		down.Store(true)
		for range 4 {
			fixture.clock.advance(proofBackoffCap)
			if err := fixture.read(); err != nil {
				t.Fatal(err)
			}
			fixture.settle()
		}
		if status := fixture.service.CaptureStatus(); status.Reason != "proof_unreachable" {
			t.Fatalf("capture status = %+v", status)
		}
		lines := visible(logs.all())
		if len(lines) != 1 || !strings.Contains(lines[0], "cannot reach the identity proof host "+proofHostName) {
			t.Fatalf("lines = %q", lines)
		}
	})
}

// A proof that fails because the token's times disagree with this server's
// clock tells the operator to check the clock, not the proof host.
func TestClockSkewProofFailureNamesTheClock(t *testing.T) {
	detail := DiagnoseError(credentialFault(&proofError{class: "clock_skew"}))
	if detail.Code != "proof_unavailable" || !strings.Contains(detail.Action, "clock") || strings.Contains(detail.Action, "proof host") {
		t.Fatalf("clock skew detail = %+v", detail)
	}
	if detail := DiagnoseError(credentialFault(&proofError{class: "5xx"})); strings.Contains(detail.Action, "clock") {
		t.Fatalf("an unavailable proof host named the clock: %+v", detail)
	}
}

// Past the grace bound of a renewal whose proof host does not answer, the sync
// report names the proof as an unreachable dependency, never a rejected
// credential, on the whole read and on every kind.
func TestSyncHealthDuringAProofOutageNamesTheProof(t *testing.T) {
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	fixture := newProofFixture(t)
	if err := fixture.read(); err != nil {
		t.Fatal(err)
	}
	expires := fixture.service.identity.servingExpires
	fixture.cluster.setReview(reviewDown(http.StatusServiceUnavailable))
	fixture.renew()
	_ = fixture.read()
	fixture.settle()
	fixture.clock.advance(expires.Add(-proofGraceMargin + time.Second).Sub(fixture.clock.Now()))
	_, err := fixture.service.SyncHealth(t.Context())
	report, ok := SyncHealthReport(err)
	if !ok || !IsCredentialProofFailure(err) {
		t.Fatalf("sync health past the bound = %v, want a proof failure with a report", err)
	}
	if report.State != "error" || report.Class != "proof_unreachable" {
		t.Fatalf("sync report state=%q class=%q, want error/proof_unreachable", report.State, report.Class)
	}
	for kind, state := range report.Kinds {
		if state.Error != "proof_unreachable" {
			t.Fatalf("kind %s class = %q, want proof_unreachable", kind, state.Error)
		}
	}
	if detail := DiagnoseError(err); detail.Code == "connector_credential_unresolved" || strings.Contains(detail.Action, "rotate") {
		t.Fatalf("proof outage detail = %+v", detail)
	}
}

// A proof the identity service could not confirm because the cluster refused
// the review, or because the token's times disagree with this server's clock,
// keeps its own sync class with a clock or permission remedy. Proof keys on a
// host outside the cloud's allowed hosts are reported as a host setting, not
// as outbound access. A proof host that gave no answer, or answered with a
// failure of its own, is reported unreachable.
func TestProofFailureSyncClassKeepsItsCause(t *testing.T) {
	for _, tc := range []struct {
		class, sync, code, action string
	}{
		{"review_denied", "proof_unavailable", "proof_unavailable", "identity reviews"},
		{"clock_skew", "proof_unavailable", "proof_unavailable", "clock is synchronized"},
		{"unreachable", "proof_unreachable", "proof_unreachable", "reach the identity proof host"},
		{"timeout", "proof_unreachable", "proof_unreachable", "reach the identity proof host"},
		{"5xx", "proof_unreachable", "proof_unreachable", "reach the identity proof host"},
		{"jwks_host_not_allowed", "proof_host_not_allowed", "proof_host_not_allowed", "cloud environment matches"},
	} {
		err := credentialFault(&proofError{class: tc.class})
		if got := syncFailureClass(err); got != tc.sync {
			t.Errorf("%s: sync class = %q, want %q", tc.class, got, tc.sync)
		}
		if got := inventoryCycleClass(tc.sync); got != tc.sync {
			t.Errorf("%s: cycle class %q became %q", tc.class, tc.sync, got)
		}
		if detail := DiagnoseError(err); detail.Code != tc.code || !strings.Contains(detail.Action, tc.action) {
			t.Errorf("%s: detail = %s %q, want %s naming %q", tc.class, detail.Code, detail.Action, tc.code, tc.action)
		}
	}
}
