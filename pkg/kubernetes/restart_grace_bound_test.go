package kubernetes

import (
	"testing"
	"time"
)

// The restart grace lasts as long as the stale rule's own bound, three
// minutes for a Pod view: with an observation that was still fresh at the
// restart, a first publication still missing just under three minutes after
// the start keeps the restored cluster ready, and one missing just over it
// turns the view stale, as an observation of that age would.
func TestRestartGraceEndsAtThreeMinutesWhenTheFirstCycleNeverEnds(t *testing.T) {
	for _, check := range []struct {
		startedAgo time.Duration
		want       string
	}{
		{2*time.Minute + 45*time.Second, "ready"},
		{3*time.Minute + 15*time.Second, "stale"},
	} {
		observedAgo := check.startedAgo + time.Minute
		service, _ := restartFreshnessService(t, restoredGeneration(observedAgo), check.startedAgo, false)
		state, age, view := syncAndViewState(t, service)
		if state != check.want || view != check.want {
			t.Fatalf("a first publication still missing %s after a restart reads sync=%s view=%s, want %s", check.startedAgo, state, view, check.want)
		}
		if age < observedAgo.Seconds() {
			t.Fatalf("the reported age %.0fs hides the stored observation's age", age)
		}
	}
}
