package utils

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Reference implementation — the extraction as it read before one folded index
// was shared per payload: every candidate key re-ranged the whole map. It is
// kept here so the optimised path can be proven byte-identical rather than
// merely self-consistent.
// ---------------------------------------------------------------------------

func refService(content map[string]interface{}) string {
	if s := refPayloadString(content, serviceKeys...); s != "" {
		return s
	}
	if s := refNestedString(content, labelParents, serviceKeys); s != "" {
		return s
	}
	return refDimension(content, cloudWatchServiceDimensions)
}

func refSeverity(content map[string]interface{}) string {
	if s := refPayloadString(content, severityKeys...); s != "" {
		return s
	}
	if s := refNestedString(content, labelParents, severityKeys); s != "" {
		return s
	}
	return refDimension(content, cloudWatchSeverityDimensions)
}

func refTitle(content map[string]interface{}) string {
	if s := refFoldedString(content, titleKeys...); s != "" {
		return s
	}
	for _, p := range labelParents {
		nested, ok := refMapValue(content, p)
		if !ok {
			continue
		}
		if s := refFoldedString(nested, titleKeys...); s != "" {
			return s
		}
	}
	return ""
}

func refPayloadString(content map[string]interface{}, keys ...string) string {
	if content == nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := content[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return refFoldedString(content, keys...)
}

func refFoldedString(content map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if ck, ok := refFoldedKey(content, k, isNonEmptyString); ok {
			s, _ := content[ck].(string)
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func refFoldedKey(content map[string]interface{}, key string, accept func(interface{}) bool) (string, bool) {
	if content == nil {
		return "", false
	}
	if v, ok := content[key]; ok && accept(v) {
		return key, true
	}
	best := ""
	found := false
	for ck, cv := range content {
		if !strings.EqualFold(ck, key) || !accept(cv) {
			continue
		}
		if !found || ck < best {
			best, found = ck, true
		}
	}
	return best, found
}

func refNestedString(content map[string]interface{}, parents, keys []string) string {
	for _, p := range parents {
		nested, ok := refMapValue(content, p)
		if !ok {
			continue
		}
		if s := refPayloadString(nested, keys...); s != "" {
			return s
		}
	}
	return ""
}

func refDimension(content map[string]interface{}, names []string) string {
	trigger, ok := refMapValue(content, "Trigger")
	if !ok {
		return ""
	}
	var raw interface{}
	if k, ok := refFoldedKey(trigger, "Dimensions", isAnyValue); ok {
		raw = trigger[k]
	}
	dims := map[string]string{}
	collect := func(m map[string]interface{}) {
		name := refPayloadString(m, "name")
		value := refPayloadString(m, "value")
		if name == "" || value == "" {
			return
		}
		key := strings.ToLower(name)
		if _, dup := dims[key]; !dup {
			dims[key] = value
		}
	}
	switch list := raw.(type) {
	case []interface{}:
		for _, item := range list {
			if m, ok := item.(map[string]interface{}); ok {
				collect(m)
			}
		}
	case []map[string]interface{}:
		for _, m := range list {
			collect(m)
		}
	default:
		return ""
	}
	for _, want := range names {
		if v := dims[strings.ToLower(want)]; v != "" {
			return v
		}
	}
	return ""
}

func refMapValue(content map[string]interface{}, key string) (map[string]interface{}, bool) {
	ck, ok := refFoldedKey(content, key, isMap)
	if !ok {
		return nil, false
	}
	m, _ := content[ck].(map[string]interface{})
	return m, true
}

// ---------------------------------------------------------------------------
// Equivalence
// ---------------------------------------------------------------------------

// payloadCorpus is every shape the intake sees, plus the awkward ones that
// exercise the tie-break and the fail-soft paths.
func payloadCorpus() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"nil":   nil,
		"empty": {},
		"top level": {
			"ServiceName": "checkout", "Severity": "critical", "title": "boom",
		},
		"lowercase spellings": {
			"service": "checkout", "severity": "warning", "alertname": "HighErrorRate",
		},
		"odd casing": {
			"SeRvIcE": "checkout", "SEVERITY": "high", "AlErTnAmE": "Flap",
		},
		"duplicate spellings": {
			"AlarmName": "alpha", "alarmname": "beta", "ALARMNAME": "gamma",
			"Service": "one", "service": "two", "SERVICE": "three",
		},
		"duplicate spellings with unusable winner": {
			"AlarmName": "", "alarmname": nil, "ALARMNAME": "gamma",
			"SERVICE": "  ", "Service": nil, "service": "two",
		},
		"nested labels": {
			"alertname": "HighErrorRate",
			"labels":    map[string]interface{}{"service": "checkout", "severity": "warning"},
		},
		"nested commonLabels": {
			"commonLabels": map[string]interface{}{
				"Service": "checkout", "SEVERITY": "warning", "AlertName": "Flap",
			},
		},
		"nested wins nothing": {
			"labels": map[string]interface{}{"team": "payments"},
		},
		"nested is not a map": {
			"labels": "not-a-map", "service": "checkout",
		},
		"nested duplicate parents": {
			"Labels": map[string]interface{}{"service": "upper"},
			"labels": map[string]interface{}{"service": "lower"},
		},
		"nested nil map": {
			"labels": map[string]interface{}(nil),
		},
		"cloudwatch lower dimension keys": {
			"AlarmName": "cpu-high",
			"Trigger": map[string]interface{}{"Dimensions": []interface{}{
				map[string]interface{}{"name": "ClusterName", "value": "prod"},
				map[string]interface{}{"name": "ServiceName", "value": "checkout"},
				map[string]interface{}{"name": "Severity", "value": "high"},
			}},
		},
		"cloudwatch upper dimension keys": {
			"Trigger": map[string]interface{}{"dimensions": []interface{}{
				map[string]interface{}{"Name": "FunctionName", "Value": "charge"},
				map[string]interface{}{"Name": "severity", "Value": "medium"},
			}},
		},
		"cloudwatch typed dimension slice": {
			"Trigger": map[string]interface{}{"Dimensions": []map[string]interface{}{
				{"name": "TableName", "value": "orders"},
			}},
		},
		"cloudwatch duplicate dimension names": {
			"Trigger": map[string]interface{}{"Dimensions": []interface{}{
				map[string]interface{}{"name": "ServiceName", "value": "first"},
				map[string]interface{}{"name": "servicename", "value": "second"},
			}},
		},
		"cloudwatch dimensions not a list": {
			"Trigger": map[string]interface{}{"Dimensions": "nope"},
		},
		"cloudwatch dimensions missing": {
			"Trigger": map[string]interface{}{"Namespace": "AWS/ECS"},
		},
		"cloudwatch trigger not a map": {
			"Trigger": []interface{}{"nope"},
		},
		"cloudwatch dimension entries junk": {
			"Trigger": map[string]interface{}{"Dimensions": []interface{}{
				"scalar", 42, nil,
				map[string]interface{}{"name": "", "value": "x"},
				map[string]interface{}{"name": "ServiceName"},
				map[string]interface{}{"name": "ServiceName", "value": "checkout"},
			}},
		},
		"non string values": {
			"service": 42, "severity": true, "title": []interface{}{"a"},
		},
		"whitespace only": {
			"service": "   ", "severity": "\t", "title": "\n",
		},
		"padded values": {
			"service": "  checkout  ", "severity": " high ", "title": "  boom  ",
		},
		"priority order across key set": {
			"component": "last", "app": "middle", "ServiceName": "first",
			"priority": "p3", "Severity": "critical",
		},
		"top level beats nested beats dimensions": {
			"service": "top",
			"labels":  map[string]interface{}{"service": "nested"},
			"Trigger": map[string]interface{}{"Dimensions": []interface{}{
				map[string]interface{}{"name": "ServiceName", "value": "dimension"},
			}},
		},
		"nested beats dimensions": {
			"labels": map[string]interface{}{"service": "nested"},
			"Trigger": map[string]interface{}{"Dimensions": []interface{}{
				map[string]interface{}{"name": "ServiceName", "value": "dimension"},
			}},
		},
	}
}

// TestFoldedPayload_MatchesReferenceExtraction proves the folded index is a
// pure optimisation: for every payload shape the intake sees, the shared index
// resolves exactly the value the per-key rescan resolved.
func TestFoldedPayload_MatchesReferenceExtraction(t *testing.T) {
	for name, content := range payloadCorpus() {
		t.Run(name, func(t *testing.T) {
			if got, want := ExtractService(content), refService(content); got != want {
				t.Fatalf("ExtractService = %q, want %q", got, want)
			}
			if got, want := ExtractSeverity(content), refSeverity(content); got != want {
				t.Fatalf("ExtractSeverity = %q, want %q", got, want)
			}
			if got, want := ExtractTitle(content), refTitle(content); got != want {
				t.Fatalf("ExtractTitle = %q, want %q", got, want)
			}
			for _, keys := range [][]string{serviceKeys, severityKeys, titleKeys, {"Verdict", "verdict"}} {
				if got, want := PayloadString(content, keys...), refPayloadString(content, keys...); got != want {
					t.Fatalf("PayloadString(%v) = %q, want %q", keys, got, want)
				}
			}
		})
	}
}

// TestFoldedPayload_MatchesReferenceOnRandomPayloads widens the equivalence
// check past the hand-written shapes: random spellings, casings, duplicates and
// unusable values, each checked against the reference.
func TestFoldedPayload_MatchesReferenceOnRandomPayloads(t *testing.T) {
	rng := rand.New(rand.NewSource(20260819))
	spellings := []string{
		"service", "Service", "SERVICE", "ServiceName", "servicename", "SERVICENAME",
		"severity", "Severity", "SEVERITY", "level", "LEVEL", "priority",
		"title", "Title", "alertname", "AlertName", "alarmname", "AlarmName", "summary",
		"labels", "Labels", "commonLabels", "COMMONLABELS", "Trigger", "trigger",
		"unrelated", "Unrelated",
	}
	values := []interface{}{"alpha", "beta", "  gamma  ", "", "   ", nil, 7, true,
		map[string]interface{}{"service": "nested", "severity": "low", "alertname": "Nested"},
		map[string]interface{}{"Dimensions": []interface{}{
			map[string]interface{}{"name": "ServiceName", "value": "dim"},
		}},
	}

	for i := 0; i < 3000; i++ {
		content := map[string]interface{}{}
		for n := rng.Intn(8); n >= 0; n-- {
			content[spellings[rng.Intn(len(spellings))]] = values[rng.Intn(len(values))]
		}
		if got, want := ExtractService(content), refService(content); got != want {
			t.Fatalf("ExtractService = %q, want %q for %v", got, want, content)
		}
		if got, want := ExtractSeverity(content), refSeverity(content); got != want {
			t.Fatalf("ExtractSeverity = %q, want %q for %v", got, want, content)
		}
		if got, want := ExtractTitle(content), refTitle(content); got != want {
			t.Fatalf("ExtractTitle = %q, want %q for %v", got, want, content)
		}
	}
}

// TestFoldedPayload_ReusesOneIndex checks the sharing itself: several labels
// read off one FoldedPayload agree with the one-shot functions, and reading the
// same label twice keeps answering the same thing.
func TestFoldedPayload_ReusesOneIndex(t *testing.T) {
	for name, content := range payloadCorpus() {
		t.Run(name, func(t *testing.T) {
			p := NewFoldedPayload(content)
			for i := 0; i < 3; i++ {
				if got, want := p.Service(), ExtractService(content); got != want {
					t.Fatalf("read %d: Service = %q, want %q", i, got, want)
				}
				if got, want := p.Severity(), ExtractSeverity(content); got != want {
					t.Fatalf("read %d: Severity = %q, want %q", i, got, want)
				}
				if got, want := p.Title(), ExtractTitle(content); got != want {
					t.Fatalf("read %d: Title = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// TestFoldedPayload_DuplicateSpellingDeterminism keeps the guarantee the dedup
// fingerprint rests on: a payload carrying several spellings of one key
// resolves to the same spelling on every read, despite Go randomising map
// iteration.
func TestFoldedPayload_DuplicateSpellingDeterminism(t *testing.T) {
	content := map[string]interface{}{
		"AlarmName": "alpha", "alarmname": "beta", "ALARMNAME": "gamma", "AlarmNAME": "delta",
		"Service": "one", "service": "two", "SERVICE": "three",
		"Severity": "critical", "severity": "low", "SEVERITY": "medium",
	}
	wantTitle, wantService, wantSeverity := ExtractTitle(content), ExtractService(content), ExtractSeverity(content)
	for i := 0; i < 5000; i++ {
		if got := ExtractTitle(content); got != wantTitle {
			t.Fatalf("title drifted on read %d: %q vs %q", i, got, wantTitle)
		}
		if got := ExtractService(content); got != wantService {
			t.Fatalf("service drifted on read %d: %q vs %q", i, got, wantService)
		}
		if got := ExtractSeverity(content); got != wantSeverity {
			t.Fatalf("severity drifted on read %d: %q vs %q", i, got, wantSeverity)
		}
	}
	// The spelling the key set actually asks for wins outright, so a
	// lexicographically smaller duplicate cannot steal the slot.
	if wantTitle != "beta" {
		t.Fatalf("title = %q, want beta (the exact alarmname spelling from titleKeys)", wantTitle)
	}
	if wantService != "one" {
		t.Fatalf("service = %q, want one (the exact Service spelling from serviceKeys)", wantService)
	}
}

// TestFoldedPayload_UnusableDuplicateDoesNotShadow keeps the fail-soft rule:
// an empty or null duplicate spelling never hides the spelling that carries a
// usable value.
func TestFoldedPayload_UnusableDuplicateDoesNotShadow(t *testing.T) {
	content := map[string]interface{}{
		"AlarmName": "", "ALARMNAME": nil, "AlarmNAME": "  ", "alarmname": "usable",
	}
	if got := ExtractTitle(content); got != "usable" {
		t.Fatalf("ExtractTitle = %q, want usable", got)
	}
}

// TestSeverityDimensionsHaveTheirOwnArray guards the CloudWatch severity
// dimension set against sharing severityKeys' backing array, where appending to
// one set would rewrite the other.
func TestSeverityDimensionsHaveTheirOwnArray(t *testing.T) {
	if len(cloudWatchSeverityDimensions) != len(severityKeys) {
		t.Fatalf("dimension set drifted from the severity key set: %v vs %v",
			cloudWatchSeverityDimensions, severityKeys)
	}
	for i := range severityKeys {
		if cloudWatchSeverityDimensions[i] != severityKeys[i] {
			t.Fatalf("dimension set drifted at %d: %q vs %q", i,
				cloudWatchSeverityDimensions[i], severityKeys[i])
		}
	}
	if &cloudWatchSeverityDimensions[0] == &severityKeys[0] {
		t.Fatal("cloudWatchSeverityDimensions shares severityKeys' backing array")
	}
}

// ---------------------------------------------------------------------------
// Cost
// ---------------------------------------------------------------------------

func widePayload(keys int) map[string]interface{} {
	content := map[string]interface{}{
		"labels": map[string]interface{}{"service": "checkout", "severity": "warning"},
	}
	for i := 0; i < keys; i++ {
		content[fmt.Sprintf("field_%06d", i)] = "value"
	}
	return content
}

func manyDimensions(n int) map[string]interface{} {
	dims := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		dims = append(dims, map[string]interface{}{
			"Name": fmt.Sprintf("Dim%06d", i), "Value": "value",
		})
	}
	dims = append(dims, map[string]interface{}{"Name": "ServiceName", "Value": "checkout"})
	return map[string]interface{}{
		"AlarmName": "cpu-high",
		"Trigger":   map[string]interface{}{"Dimensions": dims},
	}
}

// alertLabels is the read every alert performs: service, severity and title off
// one payload.
func BenchmarkAlertLabels_WidePayload_Reference(b *testing.B) {
	content := widePayload(100_000)
	b.ResetTimer()
	for b.Loop() {
		_, _, _ = refService(content), refSeverity(content), refTitle(content)
	}
}

func BenchmarkAlertLabels_WidePayload_Folded(b *testing.B) {
	content := widePayload(100_000)
	b.ResetTimer()
	for b.Loop() {
		p := NewFoldedPayload(content)
		_, _, _ = p.Service(), p.Severity(), p.Title()
	}
}

func BenchmarkAlertLabels_ManyDimensions_Reference(b *testing.B) {
	content := manyDimensions(200_000)
	b.ResetTimer()
	for b.Loop() {
		_, _, _ = refService(content), refSeverity(content), refTitle(content)
	}
}

func BenchmarkAlertLabels_ManyDimensions_Folded(b *testing.B) {
	content := manyDimensions(200_000)
	b.ResetTimer()
	for b.Loop() {
		p := NewFoldedPayload(content)
		_, _, _ = p.Service(), p.Severity(), p.Title()
	}
}

func BenchmarkAlertLabels_TypicalPayload_Reference(b *testing.B) {
	content := payloadCorpus()["cloudwatch lower dimension keys"]
	b.ResetTimer()
	for b.Loop() {
		_, _, _ = refService(content), refSeverity(content), refTitle(content)
	}
}

func BenchmarkAlertLabels_TypicalPayload_Folded(b *testing.B) {
	content := payloadCorpus()["cloudwatch lower dimension keys"]
	b.ResetTimer()
	for b.Loop() {
		p := NewFoldedPayload(content)
		_, _, _ = p.Service(), p.Severity(), p.Title()
	}
}
