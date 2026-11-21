package breaklogs

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Operator text placed in the value-hidden settings. None of it may ever
// appear in a change summary.
const (
	canaryInclude = "canary-include-ns"
	canaryExclude = "canary-exclude-ns"
	canarySource  = "canary-linked-source"
	canaryKind    = "canary-kind"
	canaryField   = "canary.field.path"
)

var canaries = []string{canaryInclude, canaryExclude, canarySource, canaryKind, canaryField}

func intPointer(value int) *int { return &value }

func baseSettings() Settings {
	return Settings{
		Learning:  Learning{Scope: Scope{Include: []string{}, Exclude: []string{}}},
		BreakLogs: CaptureSettings{Fields: map[string]Fields{}},
	}
}

func assertNoCanary(t *testing.T, changes []string) {
	t.Helper()
	joined := strings.Join(changes, ",")
	for _, canary := range canaries {
		if strings.Contains(joined, canary) {
			t.Fatalf("change summary leaks operator text %q: %q", canary, joined)
		}
	}
}

func TestSettingsChangesNamesEachField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(before, after *Settings)
		want   []string
	}{
		{"identical", func(before, after *Settings) {}, []string{}},
		{"logs on", func(_, after *Settings) { after.Learning.Logs = true }, []string{"learning.logs:false->true"}},
		{"logs off", func(before, _ *Settings) { before.Learning.Logs = true }, []string{"learning.logs:true->false"}},
		{"metrics on", func(_, after *Settings) { after.Learning.Metrics = true }, []string{"learning.metrics:false->true"}},
		{"traces off", func(before, _ *Settings) { before.Learning.Traces = true }, []string{"learning.traces:true->false"}},
		{"scope include added", func(_, after *Settings) { after.Learning.Scope.Include = []string{canaryInclude} }, []string{"learning.scope:changed"}},
		{"scope exclude added", func(_, after *Settings) { after.Learning.Scope.Exclude = []string{canaryExclude} }, []string{"learning.scope:changed"}},
		{"scope reordered", func(before, after *Settings) {
			before.Learning.Scope.Include = []string{canaryInclude, "b"}
			after.Learning.Scope.Include = []string{"b", canaryInclude}
		}, []string{"learning.scope:changed"}},
		{"linked source set", func(_, after *Settings) { after.BreakLogs.LinkedSource = canarySource }, []string{"break_logs.linked_source:changed"}},
		{"linked source cleared", func(before, _ *Settings) { before.BreakLogs.LinkedSource = canarySource }, []string{"break_logs.linked_source:changed"}},
		{"field mapping added", func(_, after *Settings) {
			after.BreakLogs.Fields = map[string]Fields{canaryKind: {Namespace: canaryField}}
		}, []string{"break_logs.fields:changed"}},
		{"field mapping value edited", func(before, after *Settings) {
			before.BreakLogs.Fields = map[string]Fields{canaryKind: {Pod: canaryField}}
			after.BreakLogs.Fields = map[string]Fields{canaryKind: {Pod: canaryField + "2"}}
		}, []string{"break_logs.fields:changed"}},
		{"ingest delay default to number", func(_, after *Settings) { after.BreakLogs.IngestDelay = intPointer(90) }, []string{"break_logs.ingest_delay:default->90"}},
		{"ingest delay number to default", func(before, _ *Settings) { before.BreakLogs.IngestDelay = intPointer(45) }, []string{"break_logs.ingest_delay:45->default"}},
		{"ingest delay zero is not default", func(_, after *Settings) { after.BreakLogs.IngestDelay = intPointer(0) }, []string{"break_logs.ingest_delay:default->0"}},
		{"ingest delay number to number", func(before, after *Settings) {
			before.BreakLogs.IngestDelay = intPointer(3600)
			after.BreakLogs.IngestDelay = intPointer(1)
		}, []string{"break_logs.ingest_delay:3600->1"}},
		{"ingest delay same value distinct pointers", func(before, after *Settings) {
			before.BreakLogs.IngestDelay = intPointer(30)
			after.BreakLogs.IngestDelay = intPointer(30)
		}, []string{}},
		{"nil and empty lists are equal", func(before, after *Settings) {
			before.Learning.Scope = Scope{}
			before.BreakLogs.Fields = nil
		}, []string{}},
		{"alerts are not compared", func(before, after *Settings) {
			after.Alerts = Alerts{Enabled: true, Level: "warning", EnabledAt: time.Unix(1, 0), UpdatedBy: canarySource}
		}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, after := baseSettings(), baseSettings()
			tc.mutate(&before, &after)
			got := SettingsChanges(before, after)
			if got == nil || !slices.Equal(got, tc.want) {
				t.Fatalf("SettingsChanges = %#v, want %#v", got, tc.want)
			}
			assertNoCanary(t, got)
		})
	}
}

func TestSettingsChangesAllFieldsInStableOrder(t *testing.T) {
	before := baseSettings()
	before.Learning.Traces = true
	before.Learning.Scope = Scope{Include: []string{canaryInclude}, Exclude: []string{canaryExclude}}
	before.BreakLogs = CaptureSettings{LinkedSource: canarySource, Fields: map[string]Fields{canaryKind: {Namespace: canaryField}}, IngestDelay: intPointer(120)}
	after := baseSettings()
	after.Learning.Logs, after.Learning.Metrics = true, true
	want := []string{
		"learning.logs:false->true",
		"learning.metrics:false->true",
		"learning.traces:true->false",
		"learning.scope:changed",
		"break_logs.linked_source:changed",
		"break_logs.fields:changed",
		"break_logs.ingest_delay:120->default",
	}
	for range 50 {
		got := SettingsChanges(before, after)
		if !slices.Equal(got, want) {
			t.Fatalf("SettingsChanges order = %#v, want %#v", got, want)
		}
		assertNoCanary(t, got)
	}
	reverse := SettingsChanges(after, before)
	assertNoCanary(t, reverse)
	if len(reverse) != len(want) || reverse[6] != "break_logs.ingest_delay:default->120" || reverse[0] != "learning.logs:true->false" {
		t.Fatalf("reverse summary = %#v", reverse)
	}
}

func TestSettingsChangesDoesNotMutateInputs(t *testing.T) {
	before := baseSettings()
	before.Learning.Scope.Include = []string{"a"}
	before.BreakLogs.Fields = map[string]Fields{"loki": {Pod: "pod"}}
	after := baseSettings()
	after.BreakLogs.IngestDelay = intPointer(5)
	SettingsChanges(before, after)
	if !slices.Equal(before.Learning.Scope.Include, []string{"a"}) || before.BreakLogs.Fields["loki"].Pod != "pod" || *after.BreakLogs.IngestDelay != 5 || before.BreakLogs.IngestDelay != nil {
		t.Fatalf("inputs mutated: before=%+v after=%+v", before, after)
	}
}

// Every name SettingsChanges can return comes from this vocabulary; the
// summary length is bounded by the vocabulary, never by operator input.
var settingsChangePrefixes = []string{
	"learning.logs:", "learning.metrics:", "learning.traces:", "learning.scope:changed",
	"break_logs.linked_source:changed", "break_logs.fields:changed", "break_logs.ingest_delay:",
}

var settingsChangeGrammar = regexp.MustCompile(`^(learning\.(logs|metrics|traces):(true->false|false->true)|(learning\.scope|break_logs\.linked_source|break_logs\.fields):changed|break_logs\.ingest_delay:(default|-?[0-9]+)->(default|-?[0-9]+))$`)

func FuzzSettingsChanges(f *testing.F) {
	f.Add(false, true, false, true, true, false, "", "ns", "", "x", canarySource, "", "loki", "pod", true, int64(0), false, int64(0))
	f.Add(true, true, true, false, false, false, canaryInclude, canaryInclude, canaryExclude, "", "", canarySource, canaryKind, canaryField, false, int64(-1), true, int64(1<<40))
	f.Add(false, false, false, false, false, false, "\x00", "\xff\xfe", "名前/空間", "", strings.Repeat("s", 4096), "", "", "", true, int64(3600), true, int64(-3600))
	f.Fuzz(func(t *testing.T, logsBefore, logsAfter, metricsBefore, metricsAfter, tracesBefore, tracesAfter bool, includeBefore, includeAfter, excludeBefore, excludeAfter, sourceBefore, sourceAfter, kind, field string, delayBeforeSet bool, delayBefore int64, delayAfterSet bool, delayAfter int64) {
		build := func(logs, metrics, traces bool, include, exclude, source string, fields map[string]Fields, delaySet bool, delay int64) Settings {
			settings := Settings{Learning: Learning{Logs: logs, Metrics: metrics, Traces: traces, Scope: Scope{Include: strings.Split(include, ","), Exclude: strings.Split(exclude, ",")}}, BreakLogs: CaptureSettings{LinkedSource: source, Fields: fields}}
			if delaySet {
				settings.BreakLogs.IngestDelay = intPointer(int(delay))
			}
			return settings
		}
		before := build(logsBefore, metricsBefore, tracesBefore, includeBefore, excludeBefore, sourceBefore, map[string]Fields{kind: {Namespace: field}}, delayBeforeSet, delayBefore)
		after := build(logsAfter, metricsAfter, tracesAfter, includeAfter, excludeAfter, sourceAfter, map[string]Fields{kind: {Namespace: field + field}}, delayAfterSet, delayAfter)
		changes := SettingsChanges(before, after)
		if changes == nil || len(changes) > len(settingsChangePrefixes) {
			t.Fatalf("summary not bounded: %#v", changes)
		}
		total := 0
		last := -1
		for _, change := range changes {
			total += len(change)
			// A closed grammar: a name that matches it cannot carry operator text.
			if !settingsChangeGrammar.MatchString(change) {
				t.Fatalf("name %q outside the fixed vocabulary in %#v", change, changes)
			}
			index := slices.IndexFunc(settingsChangePrefixes, func(prefix string) bool { return strings.HasPrefix(change, prefix) })
			if index <= last {
				t.Fatalf("name %q out of order in %#v", change, changes)
			}
			last = index
		}
		// Seven names, the longest two integers plus fixed text.
		if total > 512 {
			t.Fatalf("summary too long (%d bytes): %#v", total, changes)
		}
		if field != "" && !slices.Contains(changes, "break_logs.fields:changed") {
			t.Fatalf("edited field mapping not reported: %#v", changes)
		}
	})
}
