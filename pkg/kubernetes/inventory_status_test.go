package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"log"
	"regexp"
	"strings"
	"testing"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// Every inventory failure carries its own class, so an operator never has to
// guess whether a blank tab is a permission problem, a timeout, a size limit, a
// record the index refused, or a listing that could not be paged through.
func TestInventoryFailureClassesAreExplicit(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		class string
	}{
		{name: "forbidden", err: ErrForbidden, class: "forbidden"},
		{name: "timeout", err: context.DeadlineExceeded, class: "timeout"},
		{name: "throttled", err: &ErrThrottled{}, class: "throttled"},
		{name: "incomplete pages", err: ErrIncompleteInventory, class: "incomplete_pages"},
		{name: "record limit", err: kubeindex.ErrRecordLimit, class: "record_limit"},
		{name: "record rejected", err: kubeindex.ErrInvalidRecord, class: "record_rejected"},
		{name: "commit conflict", err: commitConflictError{cause: ErrCredentialUnavailable}, class: "commit_conflict"},
		{name: "unclassified", err: errors.New("boom"), class: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if class := errorClass(test.err); class != test.class || class == "sync_failed" {
				t.Fatalf("class=%q want %q", class, test.class)
			}
		})
	}
	// A commit refused because the credential epoch moved is still a credential
	// problem for every caller that only understands the credential sentinel.
	if err := (commitConflictError{cause: ErrCredentialUnavailable}); !errors.Is(err, ErrCredentialUnavailable) || err.Class() != "commit_conflict" {
		t.Fatalf("commit conflict lost its credential cause: %v", err)
	}
}

var inventoryLogLinePattern = regexp.MustCompile(`^kubernetes: inventory (kind=[A-Za-z0-9._-]+ state=[a-z]+( class=[A-Za-z0-9._-]+)?( skipped=[0-9]+)?( absent=true)? records=[0-9]+|cycle failed class=[A-Za-z0-9._-]+)$`)

// A failing kind is written once per state change — not once per cycle, which
// would bury the log, and not never, which is how a broken kind stayed silent.
// The line carries only the kind, its state, its class and counts: no record
// name, label, value, payload, or credential can reach it.
func TestInventoryKindFailureLogsOncePerStateChangeWithoutRecordData(t *testing.T) {
	var logs bytes.Buffer
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	state := newInventoryKindLog()
	scope := kubeindex.Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "epoch"}
	failing := map[string]kubeindex.KindStatus{"Node": {State: "partial", Partial: true, Error: "record_rejected", Skipped: 3, Records: 12}}
	state.report(scope, failing)
	state.report(scope, failing)
	if lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n"); len(lines) != 1 || lines[0] != "kubernetes: inventory kind=Node state=partial class=record_rejected skipped=3 records=12" {
		t.Fatalf("repeated state wrote %q", logs.String())
	}
	// A changed skip count is a new state and produces exactly one more line.
	state.report(scope, map[string]kubeindex.KindStatus{"Node": {State: "partial", Partial: true, Error: "record_rejected", Skipped: 4, Records: 11}})
	// A cluster that really denies the kind reports a different class.
	state.report(scope, map[string]kubeindex.KindStatus{"Secret": {State: "error", Partial: true, Error: "forbidden"}})
	// Recovery is a state change too.
	state.report(scope, map[string]kubeindex.KindStatus{"Secret": {State: "ready", Records: 4}})
	// A cycle that fails before any kind is listed still says why.
	state.cycle(scope, ErrIncompleteInventory)
	state.cycle(scope, ErrIncompleteInventory)
	lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
	want := []string{
		"kubernetes: inventory kind=Node state=partial class=record_rejected skipped=3 records=12",
		"kubernetes: inventory kind=Node state=partial class=record_rejected skipped=4 records=11",
		"kubernetes: inventory kind=Secret state=error class=forbidden records=0",
		"kubernetes: inventory kind=Secret state=ready records=4",
		"kubernetes: inventory cycle failed class=incomplete_pages",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("log lines:\n%s", logs.String())
	}
	for _, line := range lines {
		if !inventoryLogLinePattern.MatchString(line) {
			t.Fatalf("line can carry more than a kind, state, class and counts: %q", line)
		}
	}
}

// A cycle that failed before relisting its kinds marks them with the cycle's own
// class, so no kind ever falls back to the old generic marker.
func TestFailedCycleMarksKindsWithItsOwnClass(t *testing.T) {
	if class := inventoryCycleClass(""); class != "unavailable" {
		t.Fatalf("empty cycle class = %q", class)
	}
	if class := inventoryCycleClass("sync_failed"); class != "unavailable" {
		t.Fatalf("the retired generic marker was reused: %q", class)
	}
	if class := inventoryCycleClass("record_rejected"); class != "record_rejected" {
		t.Fatalf("a known class was replaced: %q", class)
	}
	marked := failedInventoryKinds(map[string]kubeindex.KindStatus{"Node": {State: "ready", Records: 4}}, "forbidden")
	if state := marked["Node"]; state.Error != "forbidden" || state.State != "error" || !state.Partial {
		t.Fatalf("cycle failure mark = %+v", state)
	}
	if state := failedInventoryKinds(map[string]kubeindex.KindStatus{"Node": {State: "ready"}}, "")["Node"]; state.Error == "sync_failed" {
		t.Fatalf("a kind reported the retired generic marker: %+v", state)
	}
}

// The cycle mark is cleared by a healthy cycle, so the same failure returning
// later is written again instead of being suppressed forever.
func TestInventoryCycleFailureIsLoggedAgainAfterRecovery(t *testing.T) {
	var logs bytes.Buffer
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	state := newInventoryKindLog()
	scope := kubeindex.Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "epoch"}
	state.cycle(scope, ErrIncompleteInventory)
	state.cycle(scope, ErrIncompleteInventory)
	if lines := strings.Count(logs.String(), "\n"); lines != 1 {
		t.Fatalf("repeated cycle failure wrote %d lines: %q", lines, logs.String())
	}
	state.cycleSucceeded(scope)
	state.cycle(scope, ErrIncompleteInventory)
	if lines := strings.Count(logs.String(), "\n"); lines != 2 {
		t.Fatalf("a failure after recovery stayed silent: %q", logs.String())
	}
}

// One logger is shared by every org-scoped service of a cluster, so its state has
// to be per scope: one org's failure must not silence another org's line.
func TestInventoryKindLogIsIsolatedPerScope(t *testing.T) {
	var logs bytes.Buffer
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	state := newInventoryKindLog()
	first := kubeindex.Scope{OrgID: "org-a", ClusterID: "cluster", CredentialID: "epoch"}
	second := kubeindex.Scope{OrgID: "org-b", ClusterID: "cluster", CredentialID: "epoch"}
	failing := map[string]kubeindex.KindStatus{"Node": {State: "error", Partial: true, Error: "forbidden"}}
	state.report(first, failing)
	state.report(second, failing)
	if lines := strings.Count(logs.String(), "\n"); lines != 2 {
		t.Fatalf("one org's line suppressed another's: %q", logs.String())
	}
	state.report(first, failing)
	if lines := strings.Count(logs.String(), "\n"); lines != 2 {
		t.Fatalf("repeat state logged again: %q", logs.String())
	}
	// A credential rotation for one org is its own scope and logs once more.
	rotated := first
	rotated.CredentialID = "next-epoch"
	state.report(rotated, failing)
	if lines := strings.Count(logs.String(), "\n"); lines != 3 {
		t.Fatalf("credential epoch shared bookkeeping: %q", logs.String())
	}
}
