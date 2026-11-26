package issues

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// historyOwner is one owner the scripted history drives.
type historyOwner struct {
	arn      string
	region   string
	severity func(cycle int) Severity
	// present says whether the rule finds the owner in a cycle.
	present func(cycle int) bool
	// read is the read of the owner's Region in a cycle.
	read func(cycle int) Read
	// material is the owner's material evidence in a cycle.
	material func(cycle int) string
}

func fixed(severity Severity) func(int) Severity { return func(int) Severity { return severity } }

func readOK(int) Read { return ReadOK }

func everyCycle(int) bool { return true }

func sameMaterial(int) string { return "*" }

const historyCycles = 200

// historyOwners is the script: each owner sits in its own Region, so the
// reads of one never change another.
//
//   - crit-flap (critical): found 10 cycles, gone 10, all read: it opens
//     once and regresses 9 times.
//   - low-flap (low): flaps the same way, offset by 5 cycles.
//   - med-rise (medium): always found; it turns high at cycle 170, and its
//     Region is not read in cycles 60-64 (while medium) and 180-184.
//   - digest-a..e (high): found from cycle 30 on, all at once.
//   - solo-a..c (high): found from cycle 100 on, three at once, 70 minutes
//     after the digest.
//   - rejected (critical): always found; rejected at cycle 5; its Region
//     is not read every 7th cycle until 120; its material changes at
//     cycle 120, and its Region is not read in cycles 130-134 and 150-154.
//   - coverage (critical): a 40-cycle period: found and read 0-9, not read
//     10-14, read 15-19, not read 20-24, gone and read 25-29 (resolves),
//     gone and capped 30-34, gone and read 35-39.
//   - capped (critical): found and read 0-49; gone and capped from 50.
func historyOwners() []historyOwner {
	owners := []historyOwner{
		{arn: "arn:aws:s3:::crit-flap", region: "eu-west-1", severity: fixed(SeverityCritical),
			present: func(cycle int) bool { return (cycle/10)%2 == 0 }, read: readOK, material: sameMaterial},
		{arn: "arn:aws:s3:::low-flap", region: "eu-west-1", severity: fixed(SeverityLow),
			present: func(cycle int) bool { return ((cycle+5)/10)%2 == 0 }, read: readOK, material: sameMaterial},
		{arn: "arn:aws:s3:::med-rise", region: "eu-west-2",
			severity: func(cycle int) Severity {
				if cycle >= 170 {
					return SeverityHigh
				}
				return SeverityMedium
			},
			present: everyCycle, material: sameMaterial,
			read: func(cycle int) Read {
				if cycle >= 60 && cycle < 65 || cycle >= 180 && cycle < 185 {
					return ReadNotRead
				}
				return ReadOK
			}},
		{arn: "arn:aws:s3:::rejected", region: "eu-west-3", severity: fixed(SeverityCritical), present: everyCycle,
			read: func(cycle int) Read {
				switch {
				case cycle > 5 && cycle < 120 && cycle%7 == 3:
					return ReadNotRead
				case cycle >= 130 && cycle < 135, cycle >= 150 && cycle < 155:
					return ReadNotRead
				}
				return ReadOK
			},
			material: func(cycle int) string {
				if cycle >= 120 {
					return "arn:aws:iam::999988887777:root"
				}
				return "*"
			}},
		{arn: "arn:aws:s3:::coverage", region: "us-east-1", severity: fixed(SeverityCritical), material: sameMaterial,
			present: func(cycle int) bool { return cycle%40 < 25 },
			read: func(cycle int) Read {
				switch phase := cycle % 40; {
				case phase >= 10 && phase < 15, phase >= 20 && phase < 25:
					return ReadNotRead
				case phase >= 30 && phase < 35:
					return ReadCapped
				}
				return ReadOK
			}},
		{arn: "arn:aws:s3:::capped", region: "ap-south-1", severity: fixed(SeverityCritical), material: sameMaterial,
			present: func(cycle int) bool { return cycle < 50 },
			read: func(cycle int) Read {
				if cycle >= 50 {
					return ReadCapped
				}
				return ReadOK
			}},
	}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		owners = append(owners, historyOwner{arn: "arn:aws:s3:::digest-" + name, region: "eu-central-1", severity: fixed(SeverityHigh),
			present: func(cycle int) bool { return cycle >= 30 }, read: readOK, material: sameMaterial})
	}
	for _, name := range []string{"a", "b", "c"} {
		owners = append(owners, historyOwner{arn: "arn:aws:s3:::solo-" + name, region: "sa-east-1", severity: fixed(SeverityHigh),
			present: func(cycle int) bool { return cycle >= 100 }, read: readOK, material: sameMaterial})
	}
	return owners
}

// ownerTally is what one owner was sent.
type ownerTally struct {
	opened, regressed, coverage, inDigest int
	// cycles are the cycles an opened or regressed alert was sent in.
	cycles []int
}

// failingOnce is a sink that records every attempt and fails the sends of
// one cycle, as a channel that is down for a minute. Only the sends that
// succeed reach the recorded alerts, so those are what a user received.
type failingOnce struct {
	recordingSink
	failAt int
	cycle  int
	// tries are the cycles each owner and transition key was tried in.
	tries map[string][]int
	// delivered counts the sends that succeeded per owner and key.
	delivered map[string]int
}

func (sink *failingOnce) Send(ctx context.Context, alert Alert) error {
	key := alert.Issue.Owner + " " + alert.Key
	sink.mu.Lock()
	sink.tries[key] = append(sink.tries[key], sink.cycle)
	fail := sink.cycle == sink.failAt
	sink.mu.Unlock()
	if fail {
		return errors.New("slack: connection refused")
	}
	if err := sink.recordingSink.Send(ctx, alert); err != nil {
		return err
	}
	sink.mu.Lock()
	sink.delivered[key]++
	sink.mu.Unlock()
	return nil
}

// A scripted 200-cycle history over memory and file, one evaluation and
// alert pump a minute. Critical is sent once per opening and regress; high
// goes in one digest when more than 3 open in an hour and alone otherwise;
// medium and low are never sent, and neither is a rejected issue, which
// comes back as a regress; medium rising to high is sent when it rises;
// one coverage notice goes out per opening or regress, only when an input
// is not read, never when it is capped; and a send that fails in one
// cycle stays pending and is sent again once, under the same key, on the
// next evaluation a minute later, so each transition is delivered once.
func TestScriptedHistorySendsEachAlertOnce(t *testing.T) {
	for _, backend := range flowBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			owners := historyOwners()
			rule := &scriptedRule{id: "history-public-bucket", inputs: []Input{{Service: "s3"}}}
			registry := NewRegistry()
			if err := registry.Register(rule); err != nil {
				t.Fatal(err)
			}
			source := newScriptedSource(flowAccount)
			now := &clock{now: start}
			scope := newScope("history")
			sink := &failingOnce{failAt: 60, tries: map[string][]int{}, delivered: map[string]int{}}
			lines := &logLines{}
			alerter, err := NewAlerter(backend.provider, scope, sink, WithAlertClock(now.Now),
				WithUnreadFinder(NewUnreadFinder(source, registry)), WithAlertLogger(lines.logf))
			if err != nil {
				t.Fatal(err)
			}
			evaluator, err := NewEvaluator(backend.provider, scope, source, WithRules(registry), WithClock(now.Now),
				WithAfterEvaluate(func(ctx context.Context, report Report) { _ = alerter.Pump(ctx, report) }))
			if err != nil {
				t.Fatal(err)
			}
			actions, err := NewActions(backend.provider, scope, now.Now)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			rejectedID := IssueID(rule.id, "arn:aws:s3:::rejected")
			tally := map[string]*ownerTally{}
			for _, owner := range owners {
				tally[owner.arn] = &ownerTally{}
			}
			seenSent, seenDigests := 0, 0
			for cycle := range historyCycles {
				var findings []Finding
				for _, owner := range owners {
					source.read(flowAccount, "s3", owner.region, owner.read(cycle))
					if owner.present(cycle) {
						findings = append(findings, publicFinding(owner.arn, owner.region, owner.severity(cycle), owner.material(cycle), now.Now().Format(time.Kitchen)))
					}
				}
				rule.set(findings...)
				sink.mu.Lock()
				sink.cycle = cycle
				sink.mu.Unlock()
				if _, err := evaluator.Evaluate(ctx); err != nil {
					t.Fatalf("cycle %d: %v", cycle, err)
				}
				if cycle == 5 {
					if _, err := actions.Reject(ctx, rejectedID, "ana@example.com", "public on purpose: it serves the docs site", ""); err != nil {
						t.Fatalf("reject: %v", err)
					}
				}
				sent, digests := sink.alerts()
				for _, alert := range sent[seenSent:] {
					entry := tally[alert.Issue.Owner]
					switch alert.Kind {
					case AlertOpened:
						entry.opened++
						entry.cycles = append(entry.cycles, cycle)
					case AlertRegressed:
						entry.regressed++
						entry.cycles = append(entry.cycles, cycle)
						if alert.Issue.Owner == "arn:aws:s3:::rejected" && !strings.Contains(alert.Detail, "material evidence changed") {
							t.Errorf("the rejected issue came back saying %q", alert.Detail)
						}
					case AlertCoverage:
						entry.coverage++
					}
				}
				for _, digest := range digests[seenDigests:] {
					for _, alert := range digest {
						tally[alert.Issue.Owner].inDigest++
						tally[alert.Issue.Owner].cycles = append(tally[alert.Issue.Owner].cycles, cycle)
					}
				}
				seenSent, seenDigests = len(sent), len(digests)
				now.Add(time.Minute)
			}

			want := map[string]ownerTally{
				"arn:aws:s3:::crit-flap": {opened: 1, regressed: 9},
				"arn:aws:s3:::low-flap":  {},
				"arn:aws:s3:::med-rise":  {opened: 1, coverage: 1, cycles: []int{170}},
				"arn:aws:s3:::rejected":  {opened: 1, regressed: 1, coverage: 1, cycles: []int{0, 120}},
				"arn:aws:s3:::coverage":  {opened: 1, regressed: 4, coverage: 5, cycles: []int{0, 40, 80, 120, 160}},
				"arn:aws:s3:::capped":    {opened: 1, cycles: []int{0}},
			}
			for _, name := range []string{"a", "b", "c", "d", "e"} {
				want["arn:aws:s3:::digest-"+name] = ownerTally{inDigest: 1, cycles: []int{30}}
			}
			for _, name := range []string{"a", "b", "c"} {
				want["arn:aws:s3:::solo-"+name] = ownerTally{opened: 1, cycles: []int{100}}
			}
			for owner, expect := range want {
				got := tally[owner]
				if got.opened != expect.opened || got.regressed != expect.regressed || got.coverage != expect.coverage || got.inDigest != expect.inDigest {
					t.Errorf("%s: opened %d regressed %d coverage %d in a digest %d; want %d %d %d %d",
						owner, got.opened, got.regressed, got.coverage, got.inDigest, expect.opened, expect.regressed, expect.coverage, expect.inDigest)
				}
				if expect.cycles != nil && !equalInts(got.cycles, expect.cycles) {
					t.Errorf("%s alerted in cycles %v; want %v", owner, got.cycles, expect.cycles)
				}
			}
			if _, digests := sink.alerts(); len(digests) != 1 || len(digests[0]) != 5 {
				t.Errorf("digests = %d; want one of the five high issues", len(digests))
			}

			// Each opening and regress of an alertable issue was sent once,
			// and the store agrees: one alerted event per transition.
			store, _ := storage.AWSIssuesOf(backend.provider)
			for _, owner := range []string{"arn:aws:s3:::crit-flap", "arn:aws:s3:::coverage"} {
				id := IssueID(rule.id, owner)
				moved, alerted := 0, 0
				for _, kind := range eventKinds(t, store, scope, id) {
					switch kind {
					case storage.AWSIssueEventOpened, storage.AWSIssueEventRegressed:
						moved++
					case storage.AWSIssueEventAlerted:
						alerted++
					}
				}
				got := tally[owner]
				if moved != got.opened+got.regressed || alerted != moved+got.coverage {
					t.Errorf("%s: %d transitions, %d alerted events; sent %d alerts and %d notices", owner, moved, alerted, got.opened+got.regressed, got.coverage)
				}
			}

			// The failed cycle: every send that failed at cycle 60 was tried
			// again once, under the same key, at the next evaluation a minute
			// later (the first wait), and delivered once; every other send was
			// tried once. The crit-flap regress at cycle 60 is one of them.
			retried := 0
			for key, cycles := range sink.tries {
				switch {
				case len(cycles) == 2 && cycles[0] == sink.failAt && cycles[1] == sink.failAt+1:
					retried++
				case len(cycles) != 1 || cycles[0] == sink.failAt:
					t.Errorf("%s was tried in cycles %v; want once, or at %d and again at %d", key, cycles, sink.failAt, sink.failAt+1)
				}
				if sink.delivered[key] != 1 {
					t.Errorf("%s was delivered %d times; want once", key, sink.delivered[key])
				}
			}
			critRetried := false
			for key, cycles := range sink.tries {
				if strings.HasPrefix(key, "arn:aws:s3:::crit-flap regressed:") && equalInts(cycles, []int{sink.failAt, sink.failAt + 1}) {
					critRetried = true
				}
			}
			if !critRetried || retried == 0 {
				t.Errorf("the crit-flap regress that failed at cycle %d was not tried again at %d (%d sends retried)", sink.failAt, sink.failAt+1, retried)
			}
			if !containsInt(tally["arn:aws:s3:::crit-flap"].cycles, sink.failAt+1) || containsInt(tally["arn:aws:s3:::crit-flap"].cycles, sink.failAt) {
				t.Errorf("crit-flap regress delivered in cycles %v; want at %d, not %d", tally["arn:aws:s3:::crit-flap"].cycles, sink.failAt+1, sink.failAt)
			}
			if logged := lines.all(); len(logged) != 1 || !strings.Contains(logged[0], "not delivered") ||
				!strings.Contains(logged[0], "slack: connection refused") || !strings.Contains(logged[0], "sent again") {
				t.Errorf("log = %q; want one line for the failed cycle naming the channel error and the retry", logged)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func containsInt(list []int, value int) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
