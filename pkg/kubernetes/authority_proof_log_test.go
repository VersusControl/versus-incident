package kubernetes

import (
	"strings"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// authorityProofLogFixture returns a service, an empty inventory cache and
// the clock its warning throttle reads, with the standard logger captured at
// info.
func authorityProofLogFixture(t *testing.T) (*Service, *inventoryCache, *fakeClock, *lockedLog) {
	t.Helper()
	logs := captureLogLines(t, "info")
	clock := newFakeClock()
	isolateProblems(t, 10*time.Minute, clock.Now)
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "prod"})
	return service, &inventoryCache{}, clock, logs
}

func kindSet(kinds ...string) map[string]bool {
	set := map[string]bool{}
	for _, kind := range kinds {
		set[kind] = true
	}
	return set
}

// A kind whose reads put the connector's access in question three times in a
// row writes one warning naming the cluster, the kind and what to check. It
// repeats once per throttle window with a count, and its end clears it with
// nothing at info or above.
func TestAuthorityProofWarnsOncePerWindowForAKindRecheckedOnEveryRead(t *testing.T) {
	service, cache, clock, logs := authorityProofLogFixture(t)
	secret := kindSet("Secret")
	for range 2 {
		service.noteAuthorityProofs(cache, secret, kindSet("Secret", "Pod"))
		clock.advance(30 * time.Second)
	}
	if lines := logs.all(); len(lines) != 0 {
		t.Fatalf("two proofs in a row warned: %q", lines)
	}
	service.noteAuthorityProofs(cache, secret, kindSet("Secret", "Pod"))
	lines := logs.all()
	want := `WARN kubernetes: cluster "prod" kind=Secret puts the connector's access in question on every read (permission keeps changing); check that list on Secret is granted steadily to the reader role and that nothing rewrites its role binding`
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("third proof in a row lines = %q\nwant %q", lines, want)
	}
	for range 5 {
		clock.advance(30 * time.Second)
		service.noteAuthorityProofs(cache, secret, kindSet("Secret", "Pod"))
	}
	if lines := logs.all(); len(lines) != 1 {
		t.Fatalf("proofs inside the throttle window wrote more: %q", lines[1:])
	}
	clock.advance(10 * time.Minute)
	service.noteAuthorityProofs(cache, secret, kindSet("Secret", "Pod"))
	lines = logs.all()
	if len(lines) != 2 || !strings.HasPrefix(lines[1], `WARN kubernetes: cluster "prod" kind=Secret`) || !strings.HasSuffix(lines[1], "(still failing, 7 times since 14:06)") {
		t.Fatalf("a lasting streak after one window lines = %q", lines)
	}
	// The kind is read again without a proof: its access settled.
	clock.advance(30 * time.Second)
	service.noteAuthorityProofs(cache, nil, kindSet("Secret", "Pod"))
	if lines := logs.all(); len(lines) != 2 {
		t.Fatalf("the end of the streak wrote at info or above: %q", lines[2:])
	}
	if _, kept := cache.proofStreaks["Secret"]; kept {
		t.Fatal("a settled kind kept its streak")
	}
}

// Healthy cycles write nothing, and a streak needs reads in a row: a read
// without a proof ends it, while a cycle that does not read the kind, since
// kinds are read on their own cadence, leaves it as it was.
func TestAuthorityProofStreakCountsReadsInARow(t *testing.T) {
	service, cache, _, logs := authorityProofLogFixture(t)
	for range 2 {
		service.noteAuthorityProofs(cache, nil, kindSet("Secret", "Pod"))
	}
	if lines := logs.all(); len(lines) != 0 || len(cache.proofStreaks) != 0 {
		t.Fatalf("healthy cycles wrote %q, streaks %v", lines, cache.proofStreaks)
	}

	secret := kindSet("Secret")
	service.noteAuthorityProofs(cache, secret, kindSet("Secret"))
	service.noteAuthorityProofs(cache, secret, kindSet("Secret"))
	service.noteAuthorityProofs(cache, nil, kindSet("Secret"))
	service.noteAuthorityProofs(cache, secret, kindSet("Secret"))
	service.noteAuthorityProofs(cache, secret, kindSet("Secret"))
	if lines := logs.all(); len(lines) != 0 {
		t.Fatalf("a streak broken by a settled read warned: %q", lines)
	}

	// Two cycles that read only Pod sit between the proofs.
	service.noteAuthorityProofs(cache, nil, kindSet("Pod"))
	service.noteAuthorityProofs(cache, nil, nil)
	service.noteAuthorityProofs(cache, secret, kindSet("Secret"))
	lines := logs.all()
	if len(lines) != 1 || !strings.Contains(lines[0], `cluster "prod" kind=Secret`) {
		t.Fatalf("three proofs in a row across cycles that skipped the kind = %q", lines)
	}

	// Another kind starting its own streak is warned about separately.
	gateway := kindSet("gateway.networking.k8s.io/v1/gateways")
	for range 3 {
		service.noteAuthorityProofs(cache, gateway, gateway)
	}
	lines = logs.all()
	if len(lines) != 2 || !strings.Contains(lines[1], "kind=gateway.networking.k8s.io/v1/gateways") {
		t.Fatalf("a second kind's streak = %q", lines)
	}
}

// The kinds a cycle questioned and read are named the same way whether they
// were read cluster-wide, inside a namespace, or as a discovered API resource.
func TestAuthorityProofNamesQuestionedAndReadKinds(t *testing.T) {
	attempt := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	questioned := inventoryQuestionedKinds{
		kinds:     map[string]bool{"Secret": true},
		resources: map[string]bool{"gateway.networking.k8s.io~v1~gateways": true},
		scoped:    map[string]map[string]bool{"shop": {"Pod": true}},
	}
	names := map[string]bool{}
	questionedKindNames(questioned, names)
	if len(names) != 3 || !names["Secret"] || !names["Pod"] || !names["gateway.networking.k8s.io/v1/gateways"] {
		t.Fatalf("questioned names = %v", names)
	}
	generation := inventoryGeneration{
		View:           kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Secret": {LastAttemptAt: attempt}, "Node": {LastAttemptAt: attempt.Add(-time.Minute)}}},
		ResourceStatus: map[string]kubeindex.KindStatus{"gateway.networking.k8s.io~v1~gateways": {LastAttemptAt: attempt.Add(time.Second)}},
		Namespaces:     map[string]kubeindex.Snapshot{"shop": {Kinds: map[string]kubeindex.KindStatus{"Pod": {LastAttemptAt: attempt.Add(2 * time.Second)}, "ConfigMap": {LastAttemptAt: attempt.Add(-time.Second)}}}},
	}
	read := readInventoryKinds(generation, attempt)
	if len(read) != 3 || !read["Secret"] || !read["Pod"] || !read["gateway.networking.k8s.io/v1/gateways"] {
		t.Fatalf("read names = %v", read)
	}
}

// A real collection cycle feeds the streak: a new denial of a kind read
// before starts one, and the next read of the same, now steady, denial ends it
// without a warning.
func TestNewDenialStartsAnAuthorityProofStreakThatASteadyGapEnds(t *testing.T) {
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	logs := captureLogLines(t, "info")
	streak := func() int {
		cluster.cache.mu.RLock()
		defer cluster.cache.mu.RUnlock()
		return cluster.cache.proofStreaks["Secret"]
	}
	checks := cluster.cache.authorityChecks.Load()
	cluster.denySecrets.Store(true)
	cluster.cycle(map[string]bool{"Secret": true}, "Secret")
	if cluster.cache.authorityChecks.Load() == checks {
		t.Fatal("the new denial ran no authority proof")
	}
	if got := streak(); got != 1 {
		t.Fatalf("Secret streak after its new denial = %d, want 1", got)
	}
	cluster.cycle(map[string]bool{"Secret": true}, "Secret")
	if got := streak(); got != 0 {
		t.Fatalf("Secret streak after a steady denial = %d, want 0", got)
	}
	for _, line := range logs.all() {
		if strings.Contains(line, "access in question") {
			t.Fatalf("one new denial warned: %q", line)
		}
	}
}

// A discovered API resource is named by the cluster's discovery answer. A
// name holding a line break or other control characters cannot shape the
// warning: it stays one line, with the name bounded to safe words.
func TestAuthorityProofWarningBoundsADiscoveredResourceName(t *testing.T) {
	service, cache, clock, logs := authorityProofLogFixture(t)
	// Discovery refuses only a slash in a resource name.
	hostile := "widgets\n2026-11-22 09:00:00 WARN kubernetes: forged\r\tline\x1b[31m\x00end"
	triggered := map[string]bool{}
	questionedKindNames(inventoryQuestionedKinds{resources: map[string]bool{resourceID("example.com", "v1", hostile): true}}, triggered)
	for range authorityProofWarnAfter {
		service.noteAuthorityProofs(cache, triggered, map[string]bool{})
		clock.advance(30 * time.Second)
	}
	lines := logs.all()
	if len(lines) != 1 {
		t.Fatalf("one warning wrote %d lines: %q", len(lines), lines)
	}
	want := "kind=example.com/v1/" + boundedLogToken(hostile) + " "
	if !strings.Contains(lines[0], want) {
		t.Fatalf("warning %q does not carry the bounded name %q", lines[0], want)
	}
	for _, r := range lines[0] {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("warning carries control character %q: %q", r, lines[0])
		}
	}
	// The clear line is bounded the same way.
	if got := kindLogName("example.com/v1/" + hostile); strings.ContainsAny(got, "\n\r\t\x00\x1b ") {
		t.Fatalf("kind name for the clear line is not bounded: %q", got)
	}
}
