package kubernetes

import (
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// A retrying kind captured in a pass during which the inventory publishes is
// stamped with the revision read before that pass, so the publication still
// makes it due on the very next pass instead of after its delay.
func TestRetryingKindCapturedDuringAPublicationIsDueOnTheNextPass(t *testing.T) {
	fixture := newCaptureCadenceFixture(t)
	fixture.state = func(kind string, now time.Time) kubeindex.KindStatus {
		if kind == "Job" {
			return kubeindex.KindStatus{State: "warming", Error: "syncing"}
		}
		return kubeindex.KindStatus{State: "ready", ObservedAt: now, LastAttemptAt: now}
	}
	fixture.minute()
	fixture.untilWaiting("Job")
	// Make Job due now, so this pass captures it while it is still warming.
	fixture.loop.mu.Lock()
	fixture.loop.next["Job"] = time.Time{}
	fixture.loop.mu.Unlock()
	ran := fixture.service.captureTick(fixture.loop, func(due []string) {
		if !capturedKind(due, "Job") {
			t.Fatalf("Job was not due after its delay was cleared: %v", due)
		}
		now := fixture.clock()
		snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{}}
		for _, kind := range due {
			snapshot.Kinds[kind] = fixture.state(kind, now)
		}
		if err := fixture.service.captureCycle(fixture.service.changeStore(), nil, snapshot, due, now, "owner"); err != nil {
			t.Fatal(err)
		}
		fixture.publish("during")
	})
	fixture.loop.mu.Lock()
	retries, next := fixture.loop.retries["Job"], fixture.loop.next["Job"]
	fixture.loop.mu.Unlock()
	if !ran || retries == 0 || !next.After(fixture.clock()) {
		t.Fatalf("Job did not go back to waiting out a delay: ran=%t retries=%d next=%s", ran, retries, next.Sub(fixture.clock()))
	}
	if captured := fixture.tick(); !capturedKind(captured, "Job") {
		t.Fatalf("a publication made while Job was being captured did not make it due on the next pass: captured %v", captured)
	}
}
