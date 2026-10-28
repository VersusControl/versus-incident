package utils

import (
	"slices"
	"strings"
)

// serviceKeys is the shared key set for service attribution, read at the top
// level of a payload and inside nested Alertmanager label maps.
var serviceKeys = []string{"ServiceName", "Service", "service", "service_name", "servicename", "app", "component"}

// severityKeys is the shared key set for severity attribution. It mirrors the
// UI's severityFromContent (severity.ts) so the API, the report and the web
// console band the same incident identically.
var severityKeys = []string{"Severity", "severity", "level", "priority"}

// titleKeys is the shared key set for an alert's human-facing title, read at
// the top level of a payload and inside nested Alertmanager label maps.
// alarmname covers a CloudWatch alarm delivered via SNS, whose title key is
// AlarmName; matching is case-insensitive, so every spelling collapses to one
// entry.
var titleKeys = []string{"title", "alertname", "alarmname", "summary", "subject", "name"}

// sourceKeys is the shared key set for the ingress/source label an alert
// names itself with ("sns", "agent:elasticsearch:prod", …).
var sourceKeys = []string{"source"}

// patternKeys is the shared key set for the pattern identity an alert carries.
// Order is the priority order: an explicit PatternID beats the generic "key".
var patternKeys = []string{"patternid", "pattern_id", "key"}

// labelParents are the nested maps Alertmanager/Prometheus webhooks carry
// their labels in.
var labelParents = []string{"labels", "commonLabels"}

// cloudWatchServiceDimensions lists CloudWatch alarm dimension names in the
// order we trust them to name a service: an explicit service name first, then
// the resource that identifies the workload (Lambda function, RDS instance,
// DynamoDB table, SQS queue, ...), then the coarser placement dimensions, with
// the bare instance id last because it identifies a host, not a service.
var cloudWatchServiceDimensions = []string{
	"ServiceName",
	"service",
	"FunctionName",
	"DBInstanceIdentifier",
	"DBClusterIdentifier",
	"TableName",
	"QueueName",
	"TopicName",
	"ApiName",
	"ClusterName",
	"TargetGroup",
	"LoadBalancer",
	"AutoScalingGroupName",
	"InstanceId",
}

// cloudWatchSeverityDimensions are the dimension names that may carry a
// severity on custom CloudWatch alarms. A CloudWatch alarm state is never
// translated into a severity — we do not invent one. It is spelled out rather
// than aliased to severityKeys so the two sets keep separate backing arrays and
// appending to one can never rewrite the other.
var cloudWatchSeverityDimensions = []string{"Severity", "severity", "level", "priority"}

// FoldedPayload is a reusable, case-folded view of ONE alert payload.
//
// Every extractor below resolves its keys case-insensitively, and a single
// alert runs several of them. Resolving one candidate key by ranging the
// payload costs O(len(payload)), so a wide payload used to pay that scan once
// per candidate key per extractor — dozens of full scans per alert.
// FoldedPayload pays it once instead: the fold map, each nested label map and
// the parsed CloudWatch dimension set are built on first use and reused by
// every later lookup on the same payload.
//
// It is a pure read-side cache — the payload itself is never written to — and
// it resolves exactly the values the one-shot Extract* functions resolve,
// duplicate-spelling tie-break included. A FoldedPayload is not safe for
// concurrent use; build one per payload on the goroutine that reads it.
//
// Build ONE per payload and read every field off it — Service, Severity,
// Title, Source, PatternID, and String for a caller's own key set. The
// Extract* / PayloadString functions are one-shot wrappers that build a
// throwaway index, so a caller that needs several fields should hold the
// FoldedPayload instead of calling them in sequence.
type FoldedPayload struct {
	content map[string]interface{}
	folded  map[string]string         // lowercased key -> the spelling that wins
	dupes   map[string][]string       // lowercased key -> every spelling, ordered; only when a fold has more than one
	nested  map[string]*FoldedPayload // resolved child maps, including cached misses (nil)
	dims    map[string]string         // parsed CloudWatch Trigger.Dimensions
}

// NewFoldedPayload wraps a content map. The fold index is built lazily, so a
// caller whose keys all hit their exact spelling never pays for it.
func NewFoldedPayload(content map[string]interface{}) *FoldedPayload {
	return &FoldedPayload{content: content}
}

// Service resolves the service this payload is about: top-level keys, then
// nested Alertmanager labels, then CloudWatch alarm trigger dimensions.
func (p *FoldedPayload) Service() string {
	if s := p.String(serviceKeys...); s != "" {
		return s
	}
	if s := p.nestedString(labelParents, serviceKeys); s != "" {
		return s
	}
	return p.dimension(cloudWatchServiceDimensions)
}

// Severity resolves the payload's raw severity in the same lookup order as
// Service. Callers band or normalize it.
func (p *FoldedPayload) Severity() string {
	if s := p.String(severityKeys...); s != "" {
		return s
	}
	if s := p.nestedString(labelParents, severityKeys); s != "" {
		return s
	}
	return p.dimension(cloudWatchSeverityDimensions)
}

// Title resolves the payload's human-facing title: top-level keys first, then
// the nested Alertmanager label maps.
func (p *FoldedPayload) Title() string {
	if s := p.foldedString(titleKeys...); s != "" {
		return s
	}
	for _, parent := range labelParents {
		nested, ok := p.child(parent)
		if !ok {
			continue
		}
		if s := nested.foldedString(titleKeys...); s != "" {
			return s
		}
	}
	return ""
}

// Source resolves the ingress/source label the payload names itself with.
func (p *FoldedPayload) Source() string {
	return p.foldedString(sourceKeys...)
}

// PatternID resolves the pattern identity the payload carries, in priority
// order: PatternID, then pattern_id, then the generic key.
func (p *FoldedPayload) PatternID() string {
	return p.foldedString(patternKeys...)
}

// String returns the first non-empty string value found at any of the given
// keys: exact match across all keys first, then case-insensitive.
func (p *FoldedPayload) String(keys ...string) string {
	if p == nil || p.content == nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := p.content[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return p.foldedString(keys...)
}

// ExtractService resolves the service a free-form alert payload is about.
// It is the ONE implementation every caller (the durable
// IncidentRecord.Service column, the incidents list, the service detail
// aggregation, the report and the emit fingerprint) routes through, so they
// can never disagree. Lookup order: top-level keys, then nested
// Alertmanager labels, then CloudWatch alarm trigger dimensions. Matching is
// case-insensitive and fail-soft: an unrecognised payload yields "".
func ExtractService(content map[string]interface{}) string {
	return NewFoldedPayload(content).Service()
}

// ExtractSeverity resolves the best-effort severity of a free-form alert
// payload, using the same lookup order as ExtractService: top-level severity
// keys, then the nested Alertmanager label maps, then the CloudWatch alarm's
// Trigger.Dimensions[]. Raw — callers band or normalize it. Returns "" when
// the payload carries no severity.
//
// Only real severity fields are read. An agent verdict is deliberately NOT a
// severity here: it is operator-set free text (POST /agent/patterns/:id), and
// this value feeds the priority floor and the dedup fingerprint, which must
// never be steerable by a typed-in label. A cosmetic verdict fallback lives in
// the report's own banding instead.
func ExtractSeverity(content map[string]interface{}) string {
	return NewFoldedPayload(content).Severity()
}

// ExtractTitle resolves the human-facing title of a free-form alert payload:
// top-level keys first, then the nested Alertmanager label maps
// (labels.alertname / commonLabels.alertname). It is the ONE implementation
// behind the durable IncidentRecord.Title, the report's title fallback and the
// emit fingerprint's title fallback, so an alert can never be titled one way
// and fingerprinted another. Matching is case-insensitive and fail-soft: a
// payload that names no title yields "" rather than a title invented from
// unrelated fields.
//
// Operator note: recognising AlarmName re-keys CloudWatch alerts once. Their
// title used to resolve empty, so every alarm on one service+severity hashed
// to the same fingerprint and deduped against its neighbours; distinct alarms
// now hash distinctly. Fatigue rows written under the old key no longer match
// and new rows are created — a one-time re-key, after which the stale rows age
// out on their normal retention.
func ExtractTitle(content map[string]interface{}) string {
	return NewFoldedPayload(content).Title()
}

// ExtractSource resolves the ingress/source label a free-form alert payload
// names itself with. Like the other extractors it is case-insensitive,
// trimmed, and fail-soft, and it resolves a payload carrying two spellings of
// the key ("Source" and "source") the same way on every read — the value feeds
// the dedup fingerprint, so two reads of one payload must never land in two
// buckets.
func ExtractSource(content map[string]interface{}) string {
	return NewFoldedPayload(content).Source()
}

// ExtractPatternID resolves the pattern identity of a free-form alert payload
// in priority order (PatternID, pattern_id, key). Same determinism guarantee as
// ExtractSource: it feeds the dedup fingerprint.
func ExtractPatternID(content map[string]interface{}) string {
	return NewFoldedPayload(content).PatternID()
}

// PayloadString returns the first non-empty string value found at any of the
// given keys: exact match first, then case-insensitive. It is the shared
// content accessor behind the extractors, exported so callers that need one
// extra key of their own (the report's verdict fallback) read a payload the
// same trimmed, case-insensitive way instead of hand-rolling a map lookup.
func PayloadString(content map[string]interface{}, keys ...string) string {
	return NewFoldedPayload(content).String(keys...)
}

// foldedString returns the first non-empty string value whose key matches any
// of keys case-insensitively, walking keys in the caller's priority order.
func (p *FoldedPayload) foldedString(keys ...string) string {
	for _, k := range keys {
		if ck, ok := p.key(k, isNonEmptyString); ok {
			s, _ := p.content[ck].(string)
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// key resolves name against the payload's actual spellings case-insensitively
// and returns the single content key every caller must read. A payload may
// legitimately carry two spellings of the same field (AlarmName and
// alarmname), so the winner is picked by a fixed rule rather than by map
// iteration, which Go randomises: the exact spelling wins, else the
// lexicographically smallest match. Without it the extracted title — and
// therefore the dedup fingerprint built from it — would differ between two
// reads of the same payload. Only values accept() approves are considered, so
// a null or empty duplicate never shadows the usable spelling.
func (p *FoldedPayload) key(name string, accept func(interface{}) bool) (string, bool) {
	if p == nil || p.content == nil {
		return "", false
	}
	if v, ok := p.content[name]; ok && accept(v) {
		return name, true
	}
	p.ensureFolded()
	lowered := strings.ToLower(name)
	if spellings, ok := p.dupes[lowered]; ok {
		for _, ck := range spellings {
			if accept(p.content[ck]) {
				return ck, true
			}
		}
		return "", false
	}
	ck, ok := p.folded[lowered]
	if !ok || !accept(p.content[ck]) {
		return "", false
	}
	return ck, true
}

// ensureFolded builds the lowercased-key index in a single pass. Folds carrying
// more than one spelling — the rare case — additionally keep the ordered list
// of spellings so key() can fall past a spelling accept() rejects.
func (p *FoldedPayload) ensureFolded() {
	if p.folded != nil {
		return
	}
	p.folded = make(map[string]string, len(p.content))
	for k := range p.content {
		lowered := strings.ToLower(k)
		won, seen := p.folded[lowered]
		if !seen {
			p.folded[lowered] = k
			continue
		}
		spellings, ok := p.dupes[lowered]
		if !ok {
			if p.dupes == nil {
				p.dupes = make(map[string][]string, 1)
			}
			spellings = []string{won}
		}
		i, _ := slices.BinarySearch(spellings, k)
		spellings = slices.Insert(spellings, i, k)
		p.dupes[lowered] = spellings
		p.folded[lowered] = spellings[0]
	}
}

func isNonEmptyString(v interface{}) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

func isMap(v interface{}) bool {
	_, ok := v.(map[string]interface{})
	return ok
}

func isAnyValue(interface{}) bool { return true }

// nestedString digs one level into each parent map (case-insensitive) and
// returns the first non-empty string found at any of keys.
func (p *FoldedPayload) nestedString(parents, keys []string) string {
	for _, parent := range parents {
		nested, ok := p.child(parent)
		if !ok {
			continue
		}
		if s := nested.String(keys...); s != "" {
			return s
		}
	}
	return ""
}

// child returns the folded view of the nested map stored at name
// (case-insensitive), memoised — a cached miss is stored as a nil entry so a
// repeated lookup never re-scans the payload.
func (p *FoldedPayload) child(name string) (*FoldedPayload, bool) {
	if p == nil {
		return nil, false
	}
	if c, cached := p.nested[name]; cached {
		return c, c != nil
	}
	var c *FoldedPayload
	if ck, ok := p.key(name, isMap); ok {
		m, _ := p.content[ck].(map[string]interface{})
		c = NewFoldedPayload(m)
	}
	if p.nested == nil {
		p.nested = make(map[string]*FoldedPayload, len(labelParents)+1)
	}
	p.nested[name] = c
	return c, c != nil
}

// dimension returns the value of the first CloudWatch alarm dimension matching
// names, in the caller's priority order.
func (p *FoldedPayload) dimension(names []string) string {
	dims := p.dimensions()
	for _, want := range names {
		if v := dims[strings.ToLower(want)]; v != "" {
			return v
		}
	}
	return ""
}

// dimensions parses Trigger.Dimensions from a CloudWatch alarm payload into a
// lowercased name -> value map, once per payload. Both the {"name","value"}
// and {"Name","Value"} spellings are accepted; the first entry for a name
// wins.
func (p *FoldedPayload) dimensions() map[string]string {
	if p == nil {
		return nil
	}
	if p.dims != nil {
		return p.dims
	}
	p.dims = map[string]string{}
	trigger, ok := p.child("Trigger")
	if !ok {
		return p.dims
	}
	var raw interface{}
	if k, ok := trigger.key("Dimensions", isAnyValue); ok {
		raw = trigger.content[k]
	}
	collect := func(m map[string]interface{}) {
		name := dimensionField(m, "name")
		value := dimensionField(m, "value")
		if name == "" || value == "" {
			return
		}
		key := strings.ToLower(name)
		if _, dup := p.dims[key]; !dup {
			p.dims[key] = value
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
	}
	return p.dims
}

// dimensionField reads one field of a single CloudWatch dimension entry. These
// entries hold two keys, so they are scanned directly rather than folded — an
// index per entry would cost more than the scan it saves.
func dimensionField(m map[string]interface{}, key string) string {
	if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	best := ""
	found := false
	for ck, cv := range m {
		if !strings.EqualFold(ck, key) || !isNonEmptyString(cv) {
			continue
		}
		if !found || ck < best {
			best, found = ck, true
		}
	}
	if !found {
		return ""
	}
	s, _ := m[best].(string)
	return strings.TrimSpace(s)
}
