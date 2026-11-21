//go:build integration

package kubernetes

// Live-cluster sample probe. It measures the inventory shape,
// metrics.k8s.io lists, warning events, rollouts and Pod log streams of the
// clusters in a connectors file, building each client through the product's
// own auth path (ResolveAuthentication, NewClient, NewService) exactly as
// pkg/agent.NewKubernetesService maps the connector config.
//
// Read-only: GET lists, one watch on Warning events and pods/log reads only.
// It skips unless KUBE_SAMPLE_CONNECTORS names the connectors file and
// HARNESS_DATA_DIR and HARNESS_RESULTS_DIR name the output directories. It writes
// only below HARNESS_DATA_DIR and HARNESS_RESULTS_DIR, and every line it logs
// or keeps passes through the product redactor first.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/redaction"
)

// ---------------------------------------------------------------- types

type sampleOwner struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller,omitempty"`
}

type sampleMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Generation        int64             `json:"generation,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []sampleOwner     `json:"ownerReferences,omitempty"`
}

func (meta sampleMeta) controller() (sampleOwner, bool) {
	for _, owner := range meta.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			return owner, true
		}
	}
	if len(meta.OwnerReferences) == 1 {
		return meta.OwnerReferences[0], true
	}
	return sampleOwner{}, false
}

type sampleResources struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

type sampleContainer struct {
	Name          string          `json:"name"`
	Image         string          `json:"image,omitempty"`
	RestartPolicy string          `json:"restartPolicy,omitempty"`
	Resources     sampleResources `json:"resources"`
}

type sampleCondition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
}

type sampleContainerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
}

type samplePod struct {
	Metadata sampleMeta `json:"metadata"`
	Spec     struct {
		NodeName       string            `json:"nodeName,omitempty"`
		Containers     []sampleContainer `json:"containers"`
		InitContainers []sampleContainer `json:"initContainers,omitempty"`
	} `json:"spec"`
	Status struct {
		Phase             string                  `json:"phase"`
		StartTime         *time.Time              `json:"startTime,omitempty"`
		Conditions        []sampleCondition       `json:"conditions,omitempty"`
		ContainerStatuses []sampleContainerStatus `json:"containerStatuses,omitempty"`
	} `json:"status"`
}

func (pod samplePod) ready() bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == "Ready" {
			return condition.Status == "True"
		}
	}
	return false
}

type sampleWorkload struct {
	Metadata sampleMeta `json:"metadata"`
	Spec     struct {
		Replicas       *int32                `json:"replicas,omitempty"`
		Strategy       struct{ Type string } `json:"strategy"`
		UpdateStrategy struct{ Type string } `json:"updateStrategy"`
		Schedule       string                `json:"schedule,omitempty"`
	} `json:"spec"`
	Status struct {
		Replicas               int32  `json:"replicas,omitempty"`
		ReadyReplicas          int32  `json:"readyReplicas,omitempty"`
		UpdatedReplicas        int32  `json:"updatedReplicas,omitempty"`
		ObservedGeneration     int64  `json:"observedGeneration,omitempty"`
		CurrentRevision        string `json:"currentRevision,omitempty"`
		UpdateRevision         string `json:"updateRevision,omitempty"`
		DesiredNumberScheduled int32  `json:"desiredNumberScheduled,omitempty"`
		NumberReady            int32  `json:"numberReady,omitempty"`
		UpdatedNumberScheduled int32  `json:"updatedNumberScheduled,omitempty"`
		Active                 int32  `json:"active,omitempty"`
		Succeeded              int32  `json:"succeeded,omitempty"`
		Failed                 int32  `json:"failed,omitempty"`
	} `json:"status"`
	Revision int64 `json:"revision,omitempty"` // ControllerRevision
}

// sanitized drops annotations (last-applied configuration can hold env
// values) except the Deployment revision.
func (workload sampleWorkload) sanitized() sampleWorkload {
	revision := workload.Metadata.Annotations["deployment.kubernetes.io/revision"]
	workload.Metadata.Annotations = nil
	if revision != "" {
		workload.Metadata.Annotations = map[string]string{"deployment.kubernetes.io/revision": revision}
	}
	return workload
}

type samplePodMetric struct {
	Metadata   sampleMeta `json:"metadata"`
	Timestamp  time.Time  `json:"timestamp"`
	Window     string     `json:"window"`
	Containers []struct {
		Name  string            `json:"name"`
		Usage map[string]string `json:"usage"`
	} `json:"containers"`
}

type sampleEvent struct {
	Metadata       sampleMeta `json:"metadata"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"involvedObject"`
	Reason         string          `json:"reason"`
	Message        string          `json:"message"`
	Type           string          `json:"type"`
	Count          int64           `json:"count"`
	FirstTimestamp *time.Time      `json:"firstTimestamp"`
	LastTimestamp  *time.Time      `json:"lastTimestamp"`
	EventTime      json.RawMessage `json:"eventTime"`
	Series         *struct {
		Count            int64  `json:"count"`
		LastObservedTime string `json:"lastObservedTime"`
	} `json:"series"`
	ReportingComponent string `json:"reportingComponent"`
	Source             struct {
		Component string `json:"component"`
	} `json:"source"`
}

func (event sampleEvent) occurrences() int64 {
	if event.Series != nil && event.Series.Count > event.Count {
		return event.Series.Count
	}
	if event.Count == 0 {
		return 1
	}
	return event.Count
}

type sampleCall struct {
	Name      string  `json:"name"`
	Page      int     `json:"page"`
	LatencyMS float64 `json:"latency_ms"`
	Bytes     int     `json:"bytes"`
	Items     int     `json:"items"`
	Continue  bool    `json:"continue"`
	ItemsNull bool    `json:"items_null"`
	Error     string  `json:"error,omitempty"`
}

type sampleListStat struct {
	Name      string  `json:"name"`
	Pages     int     `json:"pages"`
	Bytes     int     `json:"bytes"`
	Items     int     `json:"items"`
	LatencyMS float64 `json:"latency_ms"`
	ItemsNull bool    `json:"items_null"`
	Remaining *int64  `json:"remaining_item_count,omitempty"`
}

// ---------------------------------------------------------------- probe

type sampleProbe struct {
	t        *testing.T
	cluster  string
	client   *Client
	service  *Service
	ctx      context.Context
	redactor *redaction.Redactor
	hide     []string
	dataDir  string
	mu       sync.Mutex
	calls    []sampleCall
	logLines []string
}

func (probe *sampleProbe) scrub(value string) string {
	value = probe.redactor.Scrub(value)
	for _, hidden := range probe.hide {
		if hidden != "" {
			value = strings.ReplaceAll(value, hidden, "<eks-endpoint>")
		}
	}
	return value
}

func (probe *sampleProbe) logf(format string, args ...any) {
	line := probe.scrub(fmt.Sprintf(format, args...))
	probe.mu.Lock()
	probe.logLines = append(probe.logLines, time.Now().UTC().Format(time.RFC3339)+" "+line)
	probe.mu.Unlock()
	probe.t.Log(line)
}

func (probe *sampleProbe) record(call sampleCall) {
	probe.mu.Lock()
	probe.calls = append(probe.calls, call)
	probe.mu.Unlock()
}

func (probe *sampleProbe) get(ctx context.Context, name, apiPath, accept string, maxBytes int64) ([]byte, bool, time.Duration, error) {
	started := time.Now()
	data, truncated, err := probe.client.getBounded(ctx, apiPath, accept, maxBytes)
	return data, truncated, time.Since(started), err
}

// list pages through one collection at limit and returns the raw items.
func (probe *sampleProbe) list(name, basePath string, limit int) ([]json.RawMessage, sampleListStat, []byte, error) {
	stat := sampleListStat{Name: name}
	items := []json.RawMessage{}
	continueToken := ""
	var firstPage []byte
	for page := 1; page <= 200; page++ {
		apiPath := basePath
		separator := "?"
		if strings.Contains(apiPath, "?") {
			separator = "&"
		}
		if limit > 0 {
			apiPath += separator + "limit=" + strconv.Itoa(limit)
			separator = "&"
		}
		if continueToken != "" {
			apiPath += separator + "continue=" + url.QueryEscape(continueToken)
		}
		data, truncated, latency, err := probe.get(probe.ctx, name, apiPath, "application/json", 32<<20)
		call := sampleCall{Name: name, Page: page, LatencyMS: float64(latency.Microseconds()) / 1000, Bytes: len(data)}
		if err == nil && truncated {
			err = ErrResponseTooLarge
		}
		if err != nil {
			call.Error = probe.scrub(err.Error())
			probe.record(call)
			return items, stat, firstPage, err
		}
		var envelope struct {
			Metadata struct {
				Continue           string `json:"continue"`
				RemainingItemCount *int64 `json:"remainingItemCount"`
				ResourceVersion    string `json:"resourceVersion"`
			} `json:"metadata"`
			Items json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			call.Error = "invalid JSON"
			probe.record(call)
			return items, stat, firstPage, errors.New("invalid list JSON")
		}
		if page == 1 {
			firstPage = data
			stat.Remaining = envelope.Metadata.RemainingItemCount
		}
		var pageItems []json.RawMessage
		if len(envelope.Items) == 0 || string(envelope.Items) == "null" {
			call.ItemsNull = true
			stat.ItemsNull = true
		} else if err := json.Unmarshal(envelope.Items, &pageItems); err != nil {
			call.Error = "invalid items"
			probe.record(call)
			return items, stat, firstPage, errors.New("invalid list items")
		}
		call.Items = len(pageItems)
		call.Continue = envelope.Metadata.Continue != ""
		probe.record(call)
		items = append(items, pageItems...)
		stat.Pages++
		stat.Bytes += len(data)
		stat.Items += len(pageItems)
		stat.LatencyMS += call.LatencyMS
		if envelope.Metadata.Continue == "" {
			return items, stat, firstPage, nil
		}
		continueToken = envelope.Metadata.Continue
	}
	return items, stat, firstPage, errors.New("list did not finish in 200 pages")
}

func sampleDecode[T any](raw []json.RawMessage) ([]T, int) {
	out := make([]T, 0, len(raw))
	bad := 0
	for _, item := range raw {
		var value T
		if json.Unmarshal(item, &value) != nil {
			bad++
			continue
		}
		out = append(out, value)
	}
	return out, bad
}

// listResourceVersion returns the collection resourceVersion of a first page.
func sampleListResourceVersion(page []byte) string {
	var envelope struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(page, &envelope)
	return envelope.Metadata.ResourceVersion
}

// ---------------------------------------------------------------- inventory

type samplePodInfo struct {
	Key        string
	Namespace  string
	Name       string
	Kind       string // top workload kind
	Workload   string // kind/ns/name
	Version    string // RS name or controller-revision-hash, "" when unknown
	Ready      bool
	Eligible   bool // Ready and older than 5 minutes
	Running    bool
	Containers []string
	Created    time.Time
}

type sampleInventory struct {
	At          time.Time
	Pods        []samplePod
	ReplicaSets []sampleWorkload
	Deployments []sampleWorkload
	StatefulSet []sampleWorkload
	DaemonSets  []sampleWorkload
	Jobs        []sampleWorkload
	CronJobs    []sampleWorkload
	Revisions   []sampleWorkload
	Nodes       int
	PodInfo     map[string]*samplePodInfo
	Groups      map[string][]*samplePodInfo // workload \x00 version -> eligible pods
}

func (inventory *sampleInventory) index(now time.Time) {
	replicaSets := map[string]sampleWorkload{}
	for _, rs := range inventory.ReplicaSets {
		replicaSets[rs.Metadata.UID] = rs
	}
	jobs := map[string]sampleWorkload{}
	for _, job := range inventory.Jobs {
		jobs[job.Metadata.UID] = job
	}
	inventory.PodInfo = map[string]*samplePodInfo{}
	inventory.Groups = map[string][]*samplePodInfo{}
	for _, pod := range inventory.Pods {
		info := &samplePodInfo{Key: pod.Metadata.Namespace + "/" + pod.Metadata.Name, Namespace: pod.Metadata.Namespace, Name: pod.Metadata.Name, Created: pod.Metadata.CreationTimestamp}
		info.Ready = pod.ready()
		info.Running = pod.Status.Phase == "Running"
		info.Eligible = info.Ready && now.Sub(pod.Metadata.CreationTimestamp) >= 5*time.Minute
		for _, container := range pod.Spec.Containers {
			info.Containers = append(info.Containers, container.Name)
		}
		owner, ok := pod.Metadata.controller()
		switch {
		case !ok:
			info.Kind, info.Workload = "bare", "bare/"+info.Key
		case owner.Kind == "ReplicaSet":
			rs, found := replicaSets[owner.UID]
			info.Version = owner.Name
			if found {
				if top, ok := rs.Metadata.controller(); ok {
					info.Kind = top.Kind
					if top.Kind != "Deployment" {
						info.Kind = "other"
					}
					info.Workload = top.Kind + "/" + pod.Metadata.Namespace + "/" + top.Name
				} else {
					info.Kind, info.Workload = "ReplicaSet", "ReplicaSet/"+pod.Metadata.Namespace+"/"+owner.Name
				}
			} else {
				info.Kind, info.Workload = "ReplicaSet", "ReplicaSet/"+pod.Metadata.Namespace+"/"+owner.Name
			}
		case owner.Kind == "StatefulSet" || owner.Kind == "DaemonSet":
			info.Kind = owner.Kind
			info.Workload = owner.Kind + "/" + pod.Metadata.Namespace + "/" + owner.Name
			info.Version = pod.Metadata.Labels["controller-revision-hash"]
		case owner.Kind == "Job":
			job, found := jobs[owner.UID]
			info.Version = owner.Name
			if top, ok := job.Metadata.controller(); found && ok && top.Kind == "CronJob" {
				info.Kind, info.Workload = "CronJob", "CronJob/"+pod.Metadata.Namespace+"/"+top.Name
			} else {
				info.Kind, info.Workload = "Job", "Job/"+pod.Metadata.Namespace+"/"+owner.Name
			}
		default:
			info.Kind, info.Workload = "other", owner.Kind+"/"+pod.Metadata.Namespace+"/"+owner.Name
			info.Version = pod.Metadata.Labels["controller-revision-hash"]
		}
		inventory.PodInfo[info.Key] = info
		if info.Eligible && info.Version != "" && info.Kind != "Job" && info.Kind != "CronJob" {
			key := info.Workload + "\x00" + info.Version
			inventory.Groups[key] = append(inventory.Groups[key], info)
		}
	}
}

// ---------------------------------------------------------------- statistics

func sampleMedian(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := slices.Clone(values)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func sampleMAD(values []float64, median float64) float64 {
	deviations := make([]float64, len(values))
	for index, value := range values {
		deviations[index] = math.Abs(value - median)
	}
	return sampleMedian(deviations)
}

func samplePercentile(values []float64, percentile float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := slices.Clone(values)
	sort.Float64s(sorted)
	rank := percentile / 100 * float64(len(sorted)-1)
	low := int(math.Floor(rank))
	high := int(math.Ceil(rank))
	if low == high {
		return sorted[low]
	}
	return sorted[low] + (sorted[high]-sorted[low])*(rank-float64(low))
}

func sampleQuantity(value string) (float64, bool) {
	if value == "" {
		return 0, false
	}
	number, err := ParseQuantity(value)
	if err != nil || number == nil {
		return 0, false
	}
	result, _ := number.Float64()
	return result, !math.IsInf(result, 0) && !math.IsNaN(result)
}

func sampleDist(values []float64) map[string]float64 {
	return map[string]float64{"n": float64(len(values)), "p50": samplePercentile(values, 50), "p90": samplePercentile(values, 90), "p99": samplePercentile(values, 99), "max": samplePercentile(values, 100)}
}

func sampleShare(part, total int) float64 {
	if total == 0 {
		return math.NaN()
	}
	return float64(part) / float64(total)
}

// ---------------------------------------------------------------- log levels

var (
	sampleTimestampPrefix = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d) `)
	sampleLogfmtLevel     = regexp.MustCompile(`(?i)(?:^|[\s,{])"?(?:level|lvl|severity|loglevel|log\.level)"?\s*[=:]\s*"?([a-zA-Z]+)`)
	sampleKlog            = regexp.MustCompile(`^([IWEF])\d{4} \d\d:\d\d:\d\d\.\d+`)
	sampleUpperToken      = regexp.MustCompile(`(?:^|[^A-Za-z])(ERROR|ERRO|EROR|ERR|FATAL|PANIC|CRITICAL|CRIT|WARN|WARNING|WRN|INFO|INF|DEBUG|DBG|TRACE|NOTICE)(?:[^A-Za-z]|$)`)
	sampleBracketToken    = regexp.MustCompile(`(?i)[\[(<|]\s*(error|err|fatal|panic|critical|crit|warn|warning|info|debug|trace|notice)\s*[\])>|]`)
	sampleException       = regexp.MustCompile(`(Traceback \(most recent call last\)|^\s+at [\w$.<>/]+\(|^goroutine \d+ \[|^panic: |^\s+File "[^"]+", line \d+|\b[A-Z][A-Za-z0-9]*(Exception|Error): |^Caused by: |^\s+\.\.\. \d+ more$)`)
	sampleLooseError      = regexp.MustCompile(`(?i)\berror\b`)
	sampleLeadingStamp    = regexp.MustCompile(`^\[?(\d{4}[-/]\d\d[-/]\d\d[T ])?\d\d:\d\d:\d\d([.,:]\d+)?(Z|[+-]\d\d:?\d\d)?\]?[\s|,]*`)
)

func sampleNormalizeLevel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error", "err", "erro", "eror", "e", "alert", "emerg", "emergency", "severe":
		return "error"
	case "fatal", "f", "critical", "crit", "panic", "dpanic":
		return "fatal"
	case "warn", "warning", "wrn", "w":
		return "warn"
	case "info", "inf", "information", "informational", "notice", "i":
		return "info"
	case "debug", "dbg", "trace", "d", "d1", "d2", "d3", "d4", "d5", "verbose", "fine", "finer", "finest":
		return "debug"
	}
	return ""
}

func sampleNumericLevel(value float64) string {
	switch {
	case value >= 60:
		return "fatal"
	case value >= 50:
		return "error"
	case value >= 40:
		return "warn"
	case value >= 30:
		return "info"
	case value > 0:
		return "debug"
	}
	return ""
}

// sampleDetectLevel applies the §6.1 reading of a line: a level field (JSON or
// logfmt), a klog prefix, or a level token near the start of the line.
// It returns the level, the method that found it, and whether the line has
// an exception or stack-trace marker.
func sampleDetectLevel(text string) (level, method string, json bool, exception bool) {
	exception = sampleException.MatchString(text)
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var object map[string]any
		if jsonErr := sampleJSONUnmarshal(trimmed, &object); jsonErr == nil {
			json = true
			for key, raw := range object {
				switch strings.ToLower(key) {
				case "level", "lvl", "severity", "loglevel", "log.level", "levelname", "@l", "level_name", "severity_text", "log_level", "severitytext", "s":
					switch typed := raw.(type) {
					case string:
						if normalized := sampleNormalizeLevel(typed); normalized != "" {
							return normalized, "json-field", json, exception
						}
					case float64:
						if normalized := sampleNumericLevel(typed); normalized != "" {
							return normalized, "json-numeric", json, exception
						}
					}
				}
			}
			if nested, ok := object["log"].(map[string]any); ok {
				if typed, ok := nested["level"].(string); ok {
					if normalized := sampleNormalizeLevel(typed); normalized != "" {
						return normalized, "json-field", json, exception
					}
				}
			}
		}
	}
	if match := sampleKlog.FindStringSubmatch(text); match != nil {
		return sampleNormalizeLevel(match[1]), "klog", json, exception
	}
	if match := sampleLogfmtLevel.FindStringSubmatch(text); match != nil {
		if normalized := sampleNormalizeLevel(match[1]); normalized != "" {
			return normalized, "field", json, exception
		}
	}
	// First word after an optional application timestamp (zap console,
	// logback, istio): "2026-11-21T08:44:15Z\tinfo\t...", "[ts] [ERROR ] ...".
	if rest := strings.TrimSpace(text[len(sampleLeadingStamp.FindString(text)):]); rest != "" {
		word := strings.Trim(strings.Fields(rest)[0], "[]():|,")
		if len(word) > 1 {
			if normalized := sampleNormalizeLevel(word); normalized != "" {
				return normalized, "first-word", json, exception
			}
		}
	}
	prefix := text
	if len(prefix) > 120 {
		prefix = prefix[:120]
	}
	if match := sampleBracketToken.FindStringSubmatch(prefix); match != nil {
		if normalized := sampleNormalizeLevel(match[1]); normalized != "" {
			return normalized, "word", json, exception
		}
	}
	if match := sampleUpperToken.FindStringSubmatch(prefix); match != nil {
		if normalized := sampleNormalizeLevel(match[1]); normalized != "" {
			return normalized, "word", json, exception
		}
	}
	return "", "", json, exception
}

func sampleJSONUnmarshal(text string, output any) error {
	return json.Unmarshal([]byte(text), output)
}

// ---------------------------------------------------------------- logs

type sampleLogTarget struct {
	Namespace, Pod, Container, Workload, Kind string
	ProbeBytes60s                             int
	ProbeLines60s                             int
	ProbeTruncated                            bool
	ProbeError                                string
	Class                                     string
	Analysis                                  *sampleLogResult `json:"-"`
}

type sampleLogResult struct {
	Target          sampleLogTarget  `json:"target"`
	FollowedSeconds float64          `json:"followed_seconds"`
	Lines           int              `json:"lines"`
	Bytes           int64            `json:"bytes"`
	LinesPerSecond  float64          `json:"lines_per_second"`
	BytesPerSecond  float64          `json:"bytes_per_second"`
	MaxLineBytes    int              `json:"max_line_bytes"`
	JSONLines       int              `json:"json_lines"`
	Recognized      int              `json:"recognized"`
	ByMethod        map[string]int   `json:"by_method"`
	ByLevel         map[string]int   `json:"by_level"`
	ErrorByRule     int              `json:"error_by_rule"`
	ExceptionMarker int              `json:"exception_marker"`
	ProductLevelErr int              `json:"product_breaklogs_error"`
	LooseErrorWord  int              `json:"loose_error_word"`
	Capped          bool             `json:"capped_4mib"`
	Opens           int              `json:"opens"`
	Closes          []string         `json:"closes"`
	OpenErrors      []string         `json:"open_errors"`
	MaxGapSeconds   float64          `json:"max_gap_seconds"`
	GapsOver2Min    int              `json:"gaps_over_2min"`
	MaxLagSeconds   float64          `json:"max_timestamp_lag_seconds"`
	Samples         []map[string]any `json:"-"`
	lastLine        time.Time
	firstOpen       time.Time
}

func (probe *sampleProbe) logPath(target sampleLogTarget, query url.Values) string {
	return "/api/v1/namespaces/" + url.PathEscape(target.Namespace) + "/pods/" + url.PathEscape(target.Pod) + "/log?" + query.Encode()
}

func (probe *sampleProbe) probeLogRate(target *sampleLogTarget) {
	query := url.Values{"container": {target.Container}, "sinceSeconds": {"60"}, "limitBytes": {strconv.Itoa(1 << 20)}}
	data, truncated, latency, err := probe.get(probe.ctx, "pods/log probe", probe.logPath(*target, query), "*/*", 1<<20)
	call := sampleCall{Name: "pods/log sinceSeconds=60", Page: 1, LatencyMS: float64(latency.Microseconds()) / 1000, Bytes: len(data)}
	if err != nil {
		call.Error = probe.scrub(err.Error())
		target.ProbeError = call.Error
	}
	probe.record(call)
	target.ProbeBytes60s = len(data)
	target.ProbeTruncated = truncated
	target.ProbeLines60s = strings.Count(string(data), "\n")
	analysis := &sampleLogResult{ByMethod: map[string]int{}, ByLevel: map[string]int{}, lastLine: time.Now()}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line != "" {
			probe.observeLine(analysis, []byte(line), len(line))
		}
	}
	target.Analysis = analysis
}

func (probe *sampleProbe) follow(ctx context.Context, target sampleLogTarget, maxBytes int64) *sampleLogResult {
	result := &sampleLogResult{Target: target, ByMethod: map[string]int{}, ByLevel: map[string]int{}}
	started := time.Now()
	result.firstOpen = started
	result.lastLine = started
	reconnects := 0
	for ctx.Err() == nil && !result.Capped {
		query := url.Values{"container": {target.Container}, "follow": {"true"}, "timestamps": {"true"}, "sinceSeconds": {"1"}}
		body, err := probe.client.openPodLogStream(ctx, probe.logPath(target, query), false)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			result.OpenErrors = append(result.OpenErrors, probe.scrub(err.Error()))
			reconnects++
			if reconnects > 5 {
				break
			}
			sampleSleep(ctx, 5*time.Second)
			continue
		}
		result.Opens++
		opened := time.Now()
		closeReason := probe.readLogStream(ctx, body, result, maxBytes)
		body.Close()
		if ctx.Err() != nil {
			break
		}
		result.Closes = append(result.Closes, fmt.Sprintf("%s after %.0fs", closeReason, time.Since(opened).Seconds()))
		if result.Capped {
			break
		}
		reconnects++
		if reconnects > 5 {
			break
		}
		sampleSleep(ctx, 2*time.Second)
	}
	gap := time.Since(result.lastLine).Seconds()
	if gap > result.MaxGapSeconds {
		result.MaxGapSeconds = gap
	}
	result.FollowedSeconds = time.Since(started).Seconds()
	if result.FollowedSeconds > 0 {
		result.LinesPerSecond = float64(result.Lines) / result.FollowedSeconds
		result.BytesPerSecond = float64(result.Bytes) / result.FollowedSeconds
	}
	return result
}

func (probe *sampleProbe) readLogStream(ctx context.Context, body io.Reader, result *sampleLogResult, maxBytes int64) string {
	reader := bufio.NewReaderSize(body, 64<<10)
	var line []byte
	length := 0
	for {
		fragment, isPrefix, err := reader.ReadLine()
		if len(fragment) > 0 {
			length += len(fragment)
			if len(line) < 16<<10 {
				room := (16 << 10) - len(line)
				if room > len(fragment) {
					room = len(fragment)
				}
				line = append(line, fragment[:room]...)
			}
		}
		if err == nil && isPrefix {
			continue
		}
		if err == nil || (len(line) > 0 && length > 0) {
			probe.observeLine(result, line, length)
			result.Bytes += int64(length + 1)
			line = line[:0]
			length = 0
			if result.Bytes >= maxBytes {
				result.Capped = true
				return "cap"
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return "deadline"
			}
			if errors.Is(err, io.EOF) {
				return "EOF"
			}
			return "error: " + probe.scrub(err.Error())
		}
	}
}

func (probe *sampleProbe) observeLine(result *sampleLogResult, raw []byte, length int) {
	now := time.Now()
	gap := now.Sub(result.lastLine).Seconds()
	if gap > result.MaxGapSeconds {
		result.MaxGapSeconds = gap
	}
	if gap >= 120 {
		result.GapsOver2Min++
	}
	result.lastLine = now
	result.Lines++
	if length > result.MaxLineBytes {
		result.MaxLineBytes = length
	}
	text := string(raw)
	if prefix := sampleTimestampPrefix.FindString(text); prefix != "" {
		if stamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(prefix)); err == nil {
			lag := now.Sub(stamp).Seconds()
			if lag > result.MaxLagSeconds {
				result.MaxLagSeconds = lag
			}
		}
		text = text[len(prefix):]
	}
	level, method, isJSON, exception := sampleDetectLevel(text)
	if isJSON {
		result.JSONLines++
	}
	if level != "" {
		result.Recognized++
		result.ByMethod[method]++
		result.ByLevel[level]++
	} else {
		result.ByLevel["none"]++
	}
	if exception {
		result.ExceptionMarker++
	}
	if level == "error" || level == "fatal" || exception {
		result.ErrorByRule++
	}
	if breaklogs.Level(text) == "error" {
		result.ProductLevelErr++
	}
	if sampleLooseError.MatchString(text) {
		result.LooseErrorWord++
	}
	if len(result.Samples) < 50 {
		sample := text
		if len(sample) > 1024 {
			sample = sample[:1024] + "…"
		}
		result.Samples = append(result.Samples, map[string]any{"line": probe.scrub(sample), "bytes": length, "level": level, "method": method, "exception": exception})
	}
}

func sampleSleep(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// ---------------------------------------------------------------- events watch

type sampleWatchResult struct {
	Opens       int            `json:"opens"`
	Closes      []string       `json:"closes"`
	ByType      map[string]int `json:"by_type"`
	ByReason    map[string]int `json:"by_reason_added_or_modified"`
	Errors      []string       `json:"errors"`
	MaxLineSize int            `json:"max_line_bytes"`
	Gone        bool           `json:"gone_410"`
	Seconds     float64        `json:"seconds"`
}

func (probe *sampleProbe) watchEvents(ctx context.Context, resourceVersion string) *sampleWatchResult {
	result := &sampleWatchResult{ByType: map[string]int{}, ByReason: map[string]int{}}
	started := time.Now()
	defer func() { result.Seconds = time.Since(started).Seconds() }()
	attempts := 0
	for ctx.Err() == nil && attempts < 6 {
		attempts++
		query := url.Values{"watch": {"true"}, "fieldSelector": {"type=Warning"}, "allowWatchBookmarks": {"true"}}
		if resourceVersion != "" {
			query.Set("resourceVersion", resourceVersion)
		}
		body, err := probe.client.openPodLogStream(ctx, "/api/v1/events?"+query.Encode(), false)
		if err != nil {
			if ctx.Err() != nil {
				return result
			}
			result.Errors = append(result.Errors, probe.scrub(err.Error()))
			sampleSleep(ctx, 5*time.Second)
			continue
		}
		result.Opens++
		opened := time.Now()
		reader := bufio.NewReaderSize(body, 1<<20)
		reason := "EOF"
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > result.MaxLineSize {
				result.MaxLineSize = len(line)
			}
			if len(strings.TrimSpace(string(line))) > 0 {
				var event struct {
					Type   string `json:"type"`
					Object struct {
						Metadata struct {
							ResourceVersion string `json:"resourceVersion"`
						} `json:"metadata"`
						Reason string `json:"reason"`
						Code   int    `json:"code"`
					} `json:"object"`
				}
				if json.Unmarshal(line, &event) == nil {
					result.ByType[event.Type]++
					if event.Type == "ADDED" || event.Type == "MODIFIED" {
						result.ByReason[event.Object.Reason]++
					}
					if event.Type == "ERROR" && event.Object.Code == 410 {
						result.Gone = true
					}
					if event.Object.Metadata.ResourceVersion != "" && event.Type != "ERROR" {
						resourceVersion = event.Object.Metadata.ResourceVersion
					}
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					reason = "deadline"
				} else if !errors.Is(err, io.EOF) {
					reason = "error: " + probe.scrub(err.Error())
				}
				break
			}
		}
		body.Close()
		if ctx.Err() != nil {
			return result
		}
		result.Closes = append(result.Closes, fmt.Sprintf("%s after %.0fs", reason, time.Since(opened).Seconds()))
		if result.Gone {
			return result
		}
		sampleSleep(ctx, 2*time.Second)
	}
	return result
}

// ---------------------------------------------------------------- test

func TestLiveClusterSampleProbe(t *testing.T) {
	connectorsPath := os.Getenv("KUBE_SAMPLE_CONNECTORS")
	if connectorsPath == "" {
		t.Skip("KUBE_SAMPLE_CONNECTORS is not set; this probe runs only against an authorised real cluster")
	}
	dataDir, resultsDir := os.Getenv("HARNESS_DATA_DIR"), os.Getenv("HARNESS_RESULTS_DIR")
	if dataDir == "" || resultsDir == "" {
		t.Skip("HARNESS_DATA_DIR and HARNESS_RESULTS_DIR must name the output directories")
	}
	window := sampleEnvDuration("KUBE_SAMPLE_WINDOW", 10*time.Minute)
	interval := sampleEnvDuration("KUBE_SAMPLE_INTERVAL", 60*time.Second)
	logWindow := sampleEnvDuration("KUBE_SAMPLE_LOG_WINDOW", 5*time.Minute)
	logPods := sampleEnvInt("KUBE_SAMPLE_LOG_PODS", 8)
	logCandidates := sampleEnvInt("KUBE_SAMPLE_LOG_CANDIDATES", 60)

	absolute, err := filepath.Abs(connectorsPath)
	if err != nil {
		t.Fatalf("connectors path: %v", err)
	}
	configDir := filepath.Join(dataDir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("# live-cluster sample probe: built-in defaults plus the sibling connectors.yaml\n"), 0o600); err != nil {
		t.Fatalf("config file: %v", err)
	}
	link := filepath.Join(configDir, "connectors.yaml")
	_ = os.Remove(link)
	if err := os.Symlink(absolute, link); err != nil {
		t.Fatalf("connectors link: %v", err)
	}
	if err := config.LoadConfig(filepath.Join(configDir, "config.yaml")); err != nil {
		t.Fatalf("load config (message withheld if it may carry values): %T", err)
	}
	connector := config.GetConfig().Connectors.Kubernetes
	redactor, redactorErrs := redaction.NewRedactor(false, nil)
	if len(redactorErrs) > 0 {
		t.Fatalf("redactor: %d rule errors", len(redactorErrs))
	}

	results := map[string]any{}
	report := &strings.Builder{}
	for index, entry := range connector.Resolved() {
		cfg := entry.KubernetesToolConfig
		if cfg.Auth.Mode != "eks" || cfg.Actions.Enable {
			t.Fatalf("cluster %d: probe refuses anything but auth.mode eks with actions disabled", index)
		}
		clusterName := cfg.Auth.EKS.ClusterName
		t.Logf("cluster %d: name=%s region=%s mode=%s", index, clusterName, cfg.Auth.EKS.Region, cfg.Auth.Mode)
		probe := &sampleProbe{t: t, cluster: clusterName, redactor: redactor, dataDir: filepath.Join(dataDir, fmt.Sprintf("cluster-%d", index))}
		if parsed, err := url.Parse(cfg.Endpoint); err == nil {
			probe.hide = []string{cfg.Endpoint, parsed.Host, parsed.Hostname()}
		}
		if err := os.MkdirAll(probe.dataDir, 0o700); err != nil {
			t.Fatalf("data dir: %v", err)
		}
		clusterResult := probe.run(cfg, window, interval, logWindow, logPods, logCandidates, report)
		results[clusterName] = clusterResult
		_ = os.WriteFile(filepath.Join(probe.dataDir, "probe.log"), []byte(strings.Join(probe.logLines, "\n")+"\n"), 0o600)
	}
	encoded, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(filepath.Join(resultsDir, "live-cluster-sample.json"), []byte(redactor.Scrub(string(encoded))), 0o600); err != nil {
		t.Fatalf("write results: %v", err)
	}
	if err := os.WriteFile(filepath.Join(resultsDir, "live-cluster-sample.md"), []byte(redactor.Scrub(report.String())), 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}
}

func sampleEnvDuration(name string, fallback time.Duration) time.Duration {
	if value, err := time.ParseDuration(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}

func sampleEnvInt(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}

func (probe *sampleProbe) writeData(name string, value any) {
	var text string
	switch typed := value.(type) {
	case []byte:
		text = string(typed)
	default:
		encoded, _ := json.MarshalIndent(value, "", "  ")
		text = string(encoded)
	}
	_ = os.WriteFile(filepath.Join(probe.dataDir, name), []byte(probe.scrub(text)), 0o600)
}

type sampleSample struct {
	At           time.Time      `json:"at"`
	LatencyMS    float64        `json:"latency_ms"`
	Bytes        int            `json:"bytes"`
	Items        int            `json:"items"`
	Pages        int            `json:"pages"`
	ItemsNull    bool           `json:"items_null"`
	Error        string         `json:"error,omitempty"`
	RunningNoMet int            `json:"running_pods_missing_from_metrics"`
	Running      int            `json:"running_pods"`
	EmptyItems   int            `json:"items_with_no_containers"`
	MissingCont  int            `json:"spec_containers_missing_usage"`
	Containers   int            `json:"spec_containers_of_listed_pods"`
	ZeroCPU      int            `json:"containers_zero_cpu"`
	UnknownPods  int            `json:"items_not_in_inventory"`
	StaleItems   int            `json:"items_same_timestamp_as_previous"`
	MaxAgeS      float64        `json:"max_item_timestamp_age_seconds"`
	Windows      map[string]int `json:"windows"`
}

type sampleGroupTrack struct {
	Kind        string
	CPUSpreads  []float64
	MemSpreads  []float64
	CPUMedians  []float64
	Sizes       []int
	Deviating30 int // samples where more than 30% of the group deviated
}

func (probe *sampleProbe) run(cfg config.KubernetesToolConfig, window, interval, logWindow time.Duration, logPods, logCandidates int, report *strings.Builder) map[string]any {
	t := probe.t
	commandStarted := time.Now()
	timeout, _ := time.ParseDuration(cfg.Timeout)
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	auth := cfg.Auth
	clientConfig, err := ResolveAuthentication(AuthOptions{
		Mode: auth.Mode, Endpoint: cfg.Endpoint, CAFile: cfg.CAFile, CAData: cfg.CAData, ServerName: cfg.ServerName,
		EKSClusterName: auth.EKS.ClusterName, EKSRegion: auth.EKS.Region, EKSRoleARN: auth.EKS.RoleARN, EKSProfile: auth.EKS.Profile,
	})
	if err != nil {
		t.Fatalf("resolve EKS authentication: %s", probe.scrub(err.Error()))
	}
	clientConfig.Timeout = timeout
	clientConfig.MaxBodyBytes = 32 << 20
	clientConfig.AllowLoopback = cfg.AllowLoopback
	clientConfig.AllowPrivateNetworks = cfg.AllowPrivateNetworks
	clientConfig.EndpointCIDRs = cfg.EndpointCIDRs
	client, err := NewClient(clientConfig)
	if err != nil {
		t.Fatalf("new client: %s", probe.scrub(err.Error()))
	}
	clusterID := strings.TrimSpace(cfg.ClusterID)
	if clusterID == "" {
		clusterID = "default"
	}
	constructStarted := time.Now()
	service := NewService(client, Scope{OrgID: "default", ClusterID: clusterID, CredentialID: cfg.CredentialID}, 0)
	constructSeconds := time.Since(constructStarted).Seconds()
	probe.client, probe.service = client, service
	base, cancelBase := service.credentialContext(context.Background())
	defer cancelBase()
	probe.ctx = base
	validateStarted := time.Now()
	if err := service.validateCredential(base); err != nil {
		t.Fatalf("credential validation failed: %s", probe.scrub(err.Error()))
	}
	probe.logf("auth ok: service construct %.2fs, validate %.2fs", constructSeconds, time.Since(validateStarted).Seconds())

	// ---- inventory t0
	inventory := probe.inventory(true)
	inventory.index(time.Now())
	probe.logf("inventory: pods=%d rs=%d deploy=%d sts=%d ds=%d jobs=%d cronjobs=%d revisions=%d nodes=%d", len(inventory.Pods), len(inventory.ReplicaSets), len(inventory.Deployments), len(inventory.StatefulSet), len(inventory.DaemonSets), len(inventory.Jobs), len(inventory.CronJobs), len(inventory.Revisions), inventory.Nodes)

	// ---- events t0 + watch
	eventRaw, eventStat, eventPage, eventErr := probe.list("events (Warning, core/v1)", "/api/v1/events?fieldSelector="+url.QueryEscape("type=Warning"), 500)
	if eventErr != nil {
		probe.logf("warning events list failed: %v", eventErr)
	}
	probe.writeData("events-warning-core-v1-first-page.json", eventPage)
	_, newEventStat, newEventPage, newEventErr := probe.list("events (Warning, events.k8s.io/v1)", "/apis/events.k8s.io/v1/events?fieldSelector="+url.QueryEscape("type=Warning"), 500)
	if newEventErr != nil {
		probe.logf("events.k8s.io list failed: %v", newEventErr)
	}
	probe.writeData("events-warning-events-k8s-io-first-page.json", newEventPage)
	events0, _ := sampleDecode[sampleEvent](eventRaw)
	eventShape := sampleEventShape(events0)
	eventCounts := map[string]int64{}
	eventReason := map[string]string{}
	for _, event := range events0 {
		eventCounts[event.Metadata.UID] = event.occurrences()
		eventReason[event.Metadata.UID] = event.Reason
	}
	occurrencesByReason := map[string]int64{}
	newEventsByReason := map[string]int{}

	windowStart := time.Now()
	ctx, cancel := context.WithTimeout(base, window+2*time.Minute)
	defer cancel()
	var background sync.WaitGroup
	var watch *sampleWatchResult
	background.Add(1)
	go func() {
		defer background.Done()
		watchCtx, stop := context.WithTimeout(ctx, window)
		defer stop()
		watch = probe.watchEvents(watchCtx, sampleListResourceVersion(eventPage))
	}()

	// ---- log targets
	targets := probe.logTargets(inventory, logCandidates)
	candidateRates := []float64{}
	for index := range targets {
		probe.probeLogRate(&targets[index])
		if targets[index].ProbeError == "" {
			candidateRates = append(candidateRates, float64(targets[index].ProbeBytes60s)/60)
		}
	}
	chosen := sampleChooseLogTargets(targets, logPods)
	logResults := make([]*sampleLogResult, len(chosen))
	for index, target := range chosen {
		background.Add(1)
		go func() {
			defer background.Done()
			followCtx, stop := context.WithTimeout(ctx, logWindow)
			defer stop()
			logResults[index] = probe.follow(followCtx, target, 4<<20)
		}()
	}
	probe.logf("log follow: %d candidates probed, %d followed for %s", len(targets), len(chosen), logWindow)

	// ---- metrics sampling
	samples := []sampleSample{}
	tracks := map[string]*sampleGroupTrack{}
	consecutive := map[string]int{}
	flaggedPods := map[string]bool{}
	previousStamp := map[string]time.Time{}
	samplesTotal := int(window/interval) + 1
	rollouts := sampleRolloutTracker{}
	rollouts.observe(inventory)
	for sampleIndex := 0; sampleIndex < samplesTotal; sampleIndex++ {
		due := windowStart.Add(time.Duration(sampleIndex) * interval)
		sampleSleep(ctx, time.Until(due))
		if ctx.Err() != nil {
			probe.logf("sampling stopped early at sample %d: %v", sampleIndex, ctx.Err())
			break
		}
		if sampleIndex > 0 && sampleIndex%5 == 0 {
			refreshed := probe.inventory(sampleIndex == samplesTotal-1)
			refreshed.index(time.Now())
			if len(refreshed.Pods) > 0 {
				if sampleIndex != samplesTotal-1 {
					refreshed.Deployments, refreshed.StatefulSet, refreshed.DaemonSets, refreshed.Revisions = inventory.Deployments, inventory.StatefulSet, inventory.DaemonSets, inventory.Revisions
				}
				rollouts.observe(refreshed)
				inventory = refreshed
			}
		}
		sample := probe.metricsSample(inventory, previousStamp, sampleIndex == 0)
		samples = append(samples, sample.sampleSample)
		probe.spreads(inventory, sample.byPod, tracks, consecutive, flaggedPods)
		// events delta
		raw, _, _, err := probe.list("events (Warning, core/v1)", "/api/v1/events?fieldSelector="+url.QueryEscape("type=Warning"), 500)
		if err == nil {
			current, _ := sampleDecode[sampleEvent](raw)
			for _, event := range current {
				now := event.occurrences()
				before, seen := eventCounts[event.Metadata.UID]
				switch {
				case !seen:
					occurrencesByReason[event.Reason] += now
					newEventsByReason[event.Reason]++
				case now > before:
					occurrencesByReason[event.Reason] += now - before
				}
				eventCounts[event.Metadata.UID] = now
				eventReason[event.Metadata.UID] = event.Reason
			}
		}
		probe.logf("sample %d/%d: metrics items=%d bytes=%d latency=%.0fms", sampleIndex+1, samplesTotal, sample.Items, sample.Bytes, sample.LatencyMS)
	}
	windowSeconds := time.Since(windowStart).Seconds()
	background.Wait()

	// ---- assemble
	result := map[string]any{}
	shape := sampleInventoryShape(inventory, probe)
	result["inventory"] = shape
	result["metrics_samples"] = samples
	spread := sampleSpreadSummary(tracks)
	spread["pods_flagged_3_ticks"] = len(flaggedPods)
	result["spread"] = spread
	totalOccurrences := int64(0)
	for _, value := range occurrencesByReason {
		totalOccurrences += value
	}
	result["events"] = map[string]any{
		"initial_list": eventStat, "events_k8s_io_list": newEventStat, "shape_initial": eventShape,
		"occurrences_by_reason": occurrencesByReason, "new_objects_by_reason": newEventsByReason,
		"occurrences_total": totalOccurrences, "window_seconds": windowSeconds,
		"per_minute": float64(totalOccurrences) / (windowSeconds / 60), "watch": watch,
	}
	result["rollouts"] = rollouts.summary(inventory, time.Now())
	logSummary := sampleLogSummary(logResults)
	logSummary["candidate_bytes_per_second"] = sampleDist(candidateRates)
	logSummary["candidates"] = len(targets)
	probed := []*sampleLogResult{}
	for _, target := range targets {
		if target.Analysis != nil && target.Analysis.Lines > 0 {
			probed = append(probed, target.Analysis)
		}
	}
	logSummary["probe_pass"] = sampleLogSummary(probed)
	logSummary["probe_pass_workloads_mostly_recognized"] = sampleMostlyRecognized(probed)
	probeSamples := []map[string]any{}
	for _, target := range targets {
		if target.Analysis != nil && target.Analysis.Lines > 0 {
			probeSamples = append(probeSamples, map[string]any{"workload": target.Workload, "result": target.Analysis, "samples": target.Analysis.Samples})
		}
	}
	probe.writeData("log-probe-pass.json", probeSamples)
	result["logs"] = logSummary
	result["calls"] = sampleCallSummary(probe.calls)
	result["seconds"] = time.Since(commandStarted).Seconds()

	for index, logResult := range logResults {
		if logResult == nil {
			continue
		}
		probe.writeData(fmt.Sprintf("log-samples-%d.json", index), map[string]any{"target": logResult.Target, "result": logResult, "samples": logResult.Samples})
	}
	probe.writeData("calls.json", probe.calls)
	sampleRenderReport(report, probe.cluster, result, logResults, samples)
	return result
}

// ---------------------------------------------------------------- inventory lists

func (probe *sampleProbe) inventory(full bool) *sampleInventory {
	inventory := &sampleInventory{At: time.Now()}
	listInto := func(name, apiPath string) []json.RawMessage {
		raw, stat, _, err := probe.list(name, apiPath, 500)
		if err != nil {
			probe.logf("list %s failed after %d items: %v", name, stat.Items, err)
		}
		return raw
	}
	pods, bad := sampleDecode[samplePod](listInto("pods", "/api/v1/pods"))
	if bad > 0 {
		probe.logf("pods: %d items did not decode", bad)
	}
	inventory.Pods = pods
	inventory.ReplicaSets, _ = sampleDecode[sampleWorkload](listInto("replicasets", "/apis/apps/v1/replicasets"))
	if !full {
		return inventory
	}
	inventory.Deployments, _ = sampleDecode[sampleWorkload](listInto("deployments", "/apis/apps/v1/deployments"))
	inventory.StatefulSet, _ = sampleDecode[sampleWorkload](listInto("statefulsets", "/apis/apps/v1/statefulsets"))
	inventory.DaemonSets, _ = sampleDecode[sampleWorkload](listInto("daemonsets", "/apis/apps/v1/daemonsets"))
	inventory.Jobs, _ = sampleDecode[sampleWorkload](listInto("jobs", "/apis/batch/v1/jobs"))
	inventory.CronJobs, _ = sampleDecode[sampleWorkload](listInto("cronjobs", "/apis/batch/v1/cronjobs"))
	inventory.Revisions, _ = sampleDecode[sampleWorkload](listInto("controllerrevisions", "/apis/apps/v1/controllerrevisions"))
	var nodes []json.RawMessage
	nodes = listInto("nodes (metadata)", "/api/v1/nodes")
	inventory.Nodes = len(nodes)
	return inventory
}

// ---------------------------------------------------------------- metrics

type sampleMetricsSample struct {
	sampleSample
	byPod map[string][2]float64 // ns/name -> cpu millicores, memory bytes
}

func (probe *sampleProbe) metricsSample(inventory *sampleInventory, previous map[string]time.Time, keepRaw bool) sampleMetricsSample {
	out := sampleMetricsSample{byPod: map[string][2]float64{}}
	out.At = time.Now().UTC()
	out.Windows = map[string]int{}
	raw, stat, page, err := probe.list("metrics.k8s.io pods", "/apis/metrics.k8s.io/v1beta1/pods", 500)
	out.LatencyMS, out.Bytes, out.Items, out.Pages, out.ItemsNull = stat.LatencyMS, stat.Bytes, stat.Items, stat.Pages, stat.ItemsNull
	if keepRaw {
		probe.writeData("metrics-k8s-io-pods-first-page.json", page)
	}
	if err != nil {
		out.Error = probe.scrub(err.Error())
		return out
	}
	items, _ := sampleDecode[samplePodMetric](raw)
	listed := map[string]samplePodMetric{}
	for _, item := range items {
		key := item.Metadata.Namespace + "/" + item.Metadata.Name
		listed[key] = item
		if seconds, err := time.ParseDuration(item.Window); err == nil {
			bucket := "<10s"
			switch {
			case seconds >= 40*time.Second:
				bucket = ">=40s"
			case seconds >= 21*time.Second:
				bucket = "21-39s"
			case seconds >= 10*time.Second:
				bucket = "10-20s"
			}
			out.Windows[bucket]++
		} else {
			out.Windows["unparsed"]++
		}
		if age := out.At.Sub(item.Timestamp).Seconds(); age > out.MaxAgeS {
			out.MaxAgeS = age
		}
		if before, ok := previous[key]; ok && before.Equal(item.Timestamp) {
			out.StaleItems++
		}
		previous[key] = item.Timestamp
		if len(item.Containers) == 0 {
			out.EmptyItems++
		}
		info, known := inventory.PodInfo[key]
		if !known {
			out.UnknownPods++
		}
		cpu, memory := 0.0, 0.0
		usage := map[string]bool{}
		complete := true
		for _, container := range item.Containers {
			usage[container.Name] = true
			c, okC := sampleQuantity(container.Usage["cpu"])
			m, okM := sampleQuantity(container.Usage["memory"])
			if !okC || !okM {
				complete = false
				continue
			}
			if c == 0 {
				out.ZeroCPU++
			}
			cpu += c * 1000
			memory += m
		}
		if known {
			for _, name := range info.Containers {
				out.Containers++
				if !usage[name] {
					out.MissingCont++
					complete = false
				}
			}
		}
		if complete && len(item.Containers) > 0 {
			out.byPod[key] = [2]float64{cpu, memory}
		}
	}
	for key, info := range inventory.PodInfo {
		if info.Running {
			out.Running++
			if _, ok := listed[key]; !ok {
				out.RunningNoMet++
			}
		}
	}
	return out
}

func (probe *sampleProbe) spreads(inventory *sampleInventory, byPod map[string][2]float64, tracks map[string]*sampleGroupTrack, consecutive map[string]int, flagged map[string]bool) {
	for key, members := range inventory.Groups {
		if len(members) < 4 {
			continue
		}
		cpus, mems := []float64{}, []float64{}
		names := []string{}
		for _, member := range members {
			value, ok := byPod[member.Key]
			if !ok {
				continue
			}
			cpus = append(cpus, value[0])
			mems = append(mems, value[1])
			names = append(names, member.Key)
		}
		if len(cpus) < 4 {
			continue
		}
		workload := strings.SplitN(key, "\x00", 2)[0]
		track := tracks[workload]
		if track == nil {
			track = &sampleGroupTrack{Kind: members[0].Kind}
			tracks[workload] = track
		}
		deviating := 0
		for signalIndex, values := range [][]float64{cpus, mems} {
			median := sampleMedian(values)
			mad := sampleMAD(values, median)
			if median > 0 {
				if signalIndex == 0 {
					track.CPUSpreads = append(track.CPUSpreads, mad/median)
					track.CPUMedians = append(track.CPUMedians, median)
				} else {
					track.MemSpreads = append(track.MemSpreads, mad/median)
				}
			}
			floor := 10.0
			if signalIndex == 1 {
				floor = 16 << 20
			}
			scale := math.Max(mad, math.Max(0.05*median, floor))
			for valueIndex, value := range values {
				score := 0.6745 * (value - median) / scale
				podKey := names[valueIndex] + "#" + strconv.Itoa(signalIndex)
				if math.Abs(score) >= 3.5 && median > 0 && math.Abs(value-median) >= 0.3*median {
					consecutive[podKey]++
					if signalIndex == 0 {
						deviating++
					}
					if consecutive[podKey] >= 3 {
						flagged[podKey] = true
					}
				} else {
					consecutive[podKey] = 0
				}
			}
		}
		if float64(deviating) > 0.3*float64(len(cpus)) {
			track.Deviating30++
		}
		track.Sizes = append(track.Sizes, len(cpus))
	}
}

func sampleSpreadSummary(tracks map[string]*sampleGroupTrack) map[string]any {
	type acc struct{ cpu, mem []float64 }
	byKind := map[string]*acc{"all": {}}
	idleCPU := 0
	deviating := 0
	for _, track := range tracks {
		for _, kind := range []string{"all", track.Kind} {
			if byKind[kind] == nil {
				byKind[kind] = &acc{}
			}
			if len(track.CPUSpreads) > 0 {
				byKind[kind].cpu = append(byKind[kind].cpu, sampleMean(track.CPUSpreads))
			}
			if len(track.MemSpreads) > 0 {
				byKind[kind].mem = append(byKind[kind].mem, sampleMean(track.MemSpreads))
			}
		}
		if len(track.CPUMedians) > 0 && sampleMedian(track.CPUMedians) < 10 {
			idleCPU++
		}
		if track.Deviating30 > 0 {
			deviating++
		}
	}
	out := map[string]any{"workloads_scored": len(tracks), "workloads_median_cpu_below_10m": idleCPU, "workloads_with_a_sample_over_30pct_deviating": deviating}
	for kind, values := range byKind {
		above := func(list []float64) int {
			count := 0
			for _, value := range list {
				if value > 0.25 {
					count++
				}
			}
			return count
		}
		out[kind] = map[string]any{
			"cpu": sampleDist(values.cpu), "cpu_above_25pct": above(values.cpu), "cpu_share_above_25pct": sampleShare(above(values.cpu), len(values.cpu)),
			"memory": sampleDist(values.mem), "memory_above_25pct": above(values.mem), "memory_share_above_25pct": sampleShare(above(values.mem), len(values.mem)),
		}
	}
	return out
}

func sampleMean(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

// ---------------------------------------------------------------- inventory shape

func sampleInventoryShape(inventory *sampleInventory, probe *sampleProbe) map[string]any {
	workloadsByKind := map[string]map[string]bool{}
	podsByKind := map[string]int{}
	readyByWorkload := map[string]int{}
	kindOf := map[string]string{}
	versionsByWorkload := map[string]map[string]bool{}
	identity := map[string][2]int{} // kind -> [pods with version identity, pods]
	for _, info := range inventory.PodInfo {
		if workloadsByKind[info.Kind] == nil {
			workloadsByKind[info.Kind] = map[string]bool{}
		}
		workloadsByKind[info.Kind][info.Workload] = true
		podsByKind[info.Kind]++
		kindOf[info.Workload] = info.Kind
		if info.Eligible {
			readyByWorkload[info.Workload]++
		}
		if versionsByWorkload[info.Workload] == nil {
			versionsByWorkload[info.Workload] = map[string]bool{}
		}
		if info.Version != "" {
			versionsByWorkload[info.Workload][info.Version] = true
		}
		pair := identity[info.Kind]
		pair[1]++
		if info.Version != "" {
			pair[0]++
		}
		identity[info.Kind] = pair
	}
	// version label presence per kind
	labelPresence := map[string]map[string]int{}
	for _, pod := range inventory.Pods {
		info := inventory.PodInfo[pod.Metadata.Namespace+"/"+pod.Metadata.Name]
		if info == nil {
			continue
		}
		if labelPresence[info.Kind] == nil {
			labelPresence[info.Kind] = map[string]int{}
		}
		for _, label := range []string{"pod-template-hash", "controller-revision-hash", "batch.kubernetes.io/job-name", "app.kubernetes.io/version", "version"} {
			if _, ok := pod.Metadata.Labels[label]; ok {
				labelPresence[info.Kind][label]++
			}
		}
	}
	// layer 2 coverage
	eligibleWorkloads := map[string]bool{}
	eligiblePods := 0
	for key, members := range inventory.Groups {
		if len(members) >= 4 {
			eligibleWorkloads[strings.SplitN(key, "\x00", 2)[0]] = true
			eligiblePods += len(members)
		}
	}
	coverageByKind := map[string][2]int{} // kind -> [eligible workloads, workloads]
	for workload, kind := range kindOf {
		pair := coverageByKind[kind]
		pair[1]++
		if eligibleWorkloads[workload] {
			pair[0]++
		}
		coverageByKind[kind] = pair
	}
	buckets := map[string]int{}
	bucketPods := map[string]int{}
	for workload, kind := range kindOf {
		if kind == "Job" || kind == "CronJob" {
			continue
		}
		count := readyByWorkload[workload]
		bucket := ""
		switch {
		case count == 0:
			bucket = "0"
		case count == 1:
			bucket = "1"
		case count == 2:
			bucket = "2"
		case count == 3:
			bucket = "3"
		case count <= 5:
			bucket = "4-5"
		case count <= 10:
			bucket = "6-10"
		case count <= 20:
			bucket = "11-20"
		default:
			bucket = "21+"
		}
		buckets[bucket]++
		bucketPods[bucket] += count
	}
	multiVersion := 0
	for workload, versions := range versionsByWorkload {
		if len(versions) > 1 && kindOf[workload] != "Job" && kindOf[workload] != "CronJob" {
			multiVersion++
		}
	}
	// limits and sidecars
	containers, memLimit, cpuLimit, memRequest, cpuRequest := 0, 0, 0, 0, 0
	sidecars := map[string]int{}
	nativeSidecars := 0
	initContainers := 0
	running := 0
	for _, pod := range inventory.Pods {
		if pod.Status.Phase == "Running" {
			running++
		}
		extra := len(pod.Spec.Containers) - 1
		for _, container := range pod.Spec.InitContainers {
			initContainers++
			if container.RestartPolicy == "Always" {
				extra++
				nativeSidecars++
			}
		}
		switch {
		case extra <= 0:
			sidecars["0"]++
		case extra == 1:
			sidecars["1"]++
		case extra == 2:
			sidecars["2"]++
		default:
			sidecars["3+"]++
		}
		for _, container := range pod.Spec.Containers {
			containers++
			if container.Resources.Limits["memory"] != "" {
				memLimit++
			}
			if container.Resources.Limits["cpu"] != "" {
				cpuLimit++
			}
			if container.Resources.Requests["memory"] != "" {
				memRequest++
			}
			if container.Resources.Requests["cpu"] != "" {
				cpuRequest++
			}
		}
	}
	// samples of pods per kind (sanitized typed copies) and workloads
	samples := map[string][]samplePod{}
	for _, pod := range inventory.Pods {
		info := inventory.PodInfo[pod.Metadata.Namespace+"/"+pod.Metadata.Name]
		if info != nil && len(samples[info.Kind]) < 2 {
			copyPod := pod
			copyPod.Metadata.Annotations = nil
			samples[info.Kind] = append(samples[info.Kind], copyPod)
		}
	}
	probe.writeData("pods-by-kind-sample.json", samples)
	workloadSamples := map[string][]sampleWorkload{}
	for kind, list := range map[string][]sampleWorkload{"Deployment": inventory.Deployments, "StatefulSet": inventory.StatefulSet, "DaemonSet": inventory.DaemonSets, "Job": inventory.Jobs, "CronJob": inventory.CronJobs, "ReplicaSet": inventory.ReplicaSets} {
		for index := 0; index < len(list) && index < 2; index++ {
			workloadSamples[kind] = append(workloadSamples[kind], list[index].sanitized())
		}
	}
	probe.writeData("workloads-by-kind-sample.json", workloadSamples)
	workloads := 0
	countsByKind := map[string]int{}
	for kind, set := range workloadsByKind {
		countsByKind[kind] = len(set)
		workloads += len(set)
	}
	return map[string]any{
		"pods": len(inventory.Pods), "running_pods": running, "nodes": inventory.Nodes,
		"objects":                     map[string]int{"deployments": len(inventory.Deployments), "statefulsets": len(inventory.StatefulSet), "daemonsets": len(inventory.DaemonSets), "jobs": len(inventory.Jobs), "cronjobs": len(inventory.CronJobs), "replicasets": len(inventory.ReplicaSets), "controllerrevisions": len(inventory.Revisions)},
		"workloads_with_pods_by_kind": countsByKind, "pods_by_kind": podsByKind, "workloads_with_pods": workloads,
		"layer2_workloads": len(eligibleWorkloads), "layer2_pods": eligiblePods, "layer2_pod_share": sampleShare(eligiblePods, len(inventory.Pods)),
		"layer2_by_kind": coverageByKind, "ready_replica_buckets_workloads": buckets, "ready_replica_buckets_pods": bucketPods,
		"workloads_with_two_versions_at_t0": multiVersion,
		"containers":                        containers, "memory_limit": memLimit, "cpu_limit": cpuLimit, "memory_request": memRequest, "cpu_request": cpuRequest,
		"memory_limit_share": sampleShare(memLimit, containers), "cpu_limit_share": sampleShare(cpuLimit, containers),
		"memory_request_share": sampleShare(memRequest, containers), "cpu_request_share": sampleShare(cpuRequest, containers),
		"sidecars_per_pod": sidecars, "native_sidecars": nativeSidecars, "init_containers": initContainers,
		"version_identity_by_kind": identity, "version_labels_by_kind": labelPresence,
	}
}

// ---------------------------------------------------------------- rollouts

type sampleRolloutTracker struct {
	firstRS        map[string]bool
	newRS          []string
	deployRevision map[string]string
	stsRevision    map[string]string
	dsGeneration   map[string]int64
	changed        map[string]string
	observations   int
}

func (tracker *sampleRolloutTracker) observe(inventory *sampleInventory) {
	tracker.observations++
	if tracker.firstRS == nil {
		tracker.firstRS, tracker.deployRevision, tracker.stsRevision, tracker.dsGeneration, tracker.changed = map[string]bool{}, map[string]string{}, map[string]string{}, map[string]int64{}, map[string]string{}
		for _, rs := range inventory.ReplicaSets {
			tracker.firstRS[rs.Metadata.UID] = true
		}
		for _, deployment := range inventory.Deployments {
			tracker.deployRevision[deployment.Metadata.UID] = deployment.Metadata.Annotations["deployment.kubernetes.io/revision"]
		}
		for _, sts := range inventory.StatefulSet {
			tracker.stsRevision[sts.Metadata.UID] = sts.Status.UpdateRevision
		}
		for _, ds := range inventory.DaemonSets {
			tracker.dsGeneration[ds.Metadata.UID] = ds.Metadata.Generation
		}
		return
	}
	for _, rs := range inventory.ReplicaSets {
		if !tracker.firstRS[rs.Metadata.UID] {
			tracker.firstRS[rs.Metadata.UID] = true
			tracker.newRS = append(tracker.newRS, rs.Metadata.Namespace+"/"+rs.Metadata.Name)
		}
	}
	for _, deployment := range inventory.Deployments {
		revision := deployment.Metadata.Annotations["deployment.kubernetes.io/revision"]
		if before, ok := tracker.deployRevision[deployment.Metadata.UID]; ok && before != revision {
			tracker.changed["Deployment/"+deployment.Metadata.Namespace+"/"+deployment.Metadata.Name] = before + "->" + revision
		}
		tracker.deployRevision[deployment.Metadata.UID] = revision
	}
	for _, sts := range inventory.StatefulSet {
		if before, ok := tracker.stsRevision[sts.Metadata.UID]; ok && before != sts.Status.UpdateRevision {
			tracker.changed["StatefulSet/"+sts.Metadata.Namespace+"/"+sts.Metadata.Name] = "updateRevision changed"
		}
		tracker.stsRevision[sts.Metadata.UID] = sts.Status.UpdateRevision
	}
	for _, ds := range inventory.DaemonSets {
		if before, ok := tracker.dsGeneration[ds.Metadata.UID]; ok && before != ds.Metadata.Generation {
			tracker.changed["DaemonSet/"+ds.Metadata.Namespace+"/"+ds.Metadata.Name] = "generation changed"
		}
		tracker.dsGeneration[ds.Metadata.UID] = ds.Metadata.Generation
	}
}

func (tracker *sampleRolloutTracker) summary(inventory *sampleInventory, now time.Time) map[string]any {
	// long horizon from creation times: Deployment ReplicaSets and
	// StatefulSet/DaemonSet ControllerRevisions.
	horizon := func(created time.Time) string {
		age := now.Sub(created)
		switch {
		case age <= time.Hour:
			return "1h"
		case age <= 24*time.Hour:
			return "24h"
		case age <= 7*24*time.Hour:
			return "7d"
		case age <= 30*24*time.Hour:
			return "30d"
		}
		return "older"
	}
	rsByAge := map[string]int{}
	newestByDeployment := map[string]time.Time{}
	for _, rs := range inventory.ReplicaSets {
		owner, ok := rs.Metadata.controller()
		if !ok || owner.Kind != "Deployment" {
			continue
		}
		rsByAge[horizon(rs.Metadata.CreationTimestamp)]++
		key := rs.Metadata.Namespace + "/" + owner.Name
		if rs.Metadata.CreationTimestamp.After(newestByDeployment[key]) {
			newestByDeployment[key] = rs.Metadata.CreationTimestamp
		}
	}
	sinceLast := []float64{}
	lastByAge := map[string]int{}
	for _, created := range newestByDeployment {
		sinceLast = append(sinceLast, now.Sub(created).Hours())
		lastByAge[horizon(created)]++
	}
	revByAge := map[string]int{}
	for _, revision := range inventory.Revisions {
		owner, ok := revision.Metadata.controller()
		if !ok || (owner.Kind != "StatefulSet" && owner.Kind != "DaemonSet") {
			continue
		}
		revByAge[owner.Kind+" "+horizon(revision.Metadata.CreationTimestamp)]++
	}
	inProgress := 0
	for _, deployment := range inventory.Deployments {
		if deployment.Spec.Replicas != nil && (deployment.Status.UpdatedReplicas < *deployment.Spec.Replicas || deployment.Status.Replicas > *deployment.Spec.Replicas) {
			inProgress++
		}
	}
	strategies := map[string]int{}
	for _, deployment := range inventory.Deployments {
		strategies["Deployment "+deployment.Spec.Strategy.Type]++
	}
	for _, sts := range inventory.StatefulSet {
		strategies["StatefulSet "+sts.Spec.UpdateStrategy.Type]++
	}
	return map[string]any{
		"window_new_replicasets": len(tracker.newRS), "window_revision_changes": len(tracker.changed), "window_changed": tracker.changed,
		"inventory_observations":        tracker.observations,
		"deployment_replicasets_by_age": rsByAge, "deployments_last_rollout_by_age": lastByAge,
		"deployment_hours_since_last_rollout": sampleDist(sinceLast),
		"sts_ds_controllerrevisions_by_age":   revByAge, "deployments_not_settled_at_end": inProgress, "strategies": strategies,
	}
}

// ---------------------------------------------------------------- events shape

func sampleEventShape(events []sampleEvent) map[string]any {
	countGreater1, series, eventTime, lastNull, firstNull := 0, 0, 0, 0, 0
	byReason := map[string]int{}
	byKind := map[string]int{}
	counts := []float64{}
	for _, event := range events {
		if event.Count > 1 {
			countGreater1++
		}
		if event.Series != nil {
			series++
		}
		if len(event.EventTime) > 0 && string(event.EventTime) != "null" {
			eventTime++
		}
		if event.LastTimestamp == nil {
			lastNull++
		}
		if event.FirstTimestamp == nil {
			firstNull++
		}
		byReason[event.Reason]++
		byKind[event.InvolvedObject.Kind]++
		counts = append(counts, float64(event.occurrences()))
	}
	return map[string]any{"objects": len(events), "count_gt_1": countGreater1, "series_set": series, "event_time_set": eventTime, "last_timestamp_null": lastNull, "first_timestamp_null": firstNull, "by_reason": byReason, "by_involved_kind": byKind, "occurrences_per_object": sampleDist(counts)}
}

// ---------------------------------------------------------------- log targets

func (probe *sampleProbe) logTargets(inventory *sampleInventory, limit int) []sampleLogTarget {
	perWorkload := map[string]bool{}
	byNamespace := map[string][]sampleLogTarget{}
	namespaces := []string{}
	pods := slices.Clone(inventory.Pods)
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].Metadata.Namespace+"/"+pods[i].Metadata.Name < pods[j].Metadata.Namespace+"/"+pods[j].Metadata.Name
	})
	for _, pod := range pods {
		info := inventory.PodInfo[pod.Metadata.Namespace+"/"+pod.Metadata.Name]
		if info == nil || !info.Running || !info.Ready || info.Kind == "Job" || info.Kind == "CronJob" || len(pod.Spec.Containers) == 0 {
			continue
		}
		if perWorkload[info.Workload] {
			continue
		}
		perWorkload[info.Workload] = true
		container := pod.Spec.Containers[0].Name
		if preferred := pod.Metadata.Annotations["kubectl.kubernetes.io/default-container"]; preferred != "" {
			container = preferred
		}
		if _, ok := byNamespace[info.Namespace]; !ok {
			namespaces = append(namespaces, info.Namespace)
		}
		byNamespace[info.Namespace] = append(byNamespace[info.Namespace], sampleLogTarget{Namespace: info.Namespace, Pod: info.Name, Container: container, Workload: info.Workload, Kind: info.Kind})
	}
	targets := []sampleLogTarget{}
	for round := 0; round < 10 && len(targets) < limit; round++ {
		for _, namespace := range namespaces {
			if round < len(byNamespace[namespace]) && len(targets) < limit {
				targets = append(targets, byNamespace[namespace][round])
			}
		}
	}
	return targets
}

func sampleChooseLogTargets(targets []sampleLogTarget, count int) []sampleLogTarget {
	usable := []sampleLogTarget{}
	for _, target := range targets {
		if target.ProbeError == "" {
			usable = append(usable, target)
		}
	}
	sort.SliceStable(usable, func(i, j int) bool { return usable[i].ProbeBytes60s > usable[j].ProbeBytes60s })
	perNamespace := map[string]int{}
	chosen := []sampleLogTarget{}
	taken := map[string]bool{}
	take := func(target sampleLogTarget, class string) bool {
		key := target.Namespace + "/" + target.Pod
		if taken[key] || perNamespace[target.Namespace] >= 2 {
			return false
		}
		target.Class = class
		taken[key] = true
		perNamespace[target.Namespace]++
		chosen = append(chosen, target)
		return true
	}
	chatty := count / 2
	for _, target := range usable {
		if len(chosen) >= chatty || target.ProbeBytes60s == 0 {
			break
		}
		take(target, "chatty")
	}
	for index := len(usable) - 1; index >= 0 && len(chosen) < count; index-- {
		take(usable[index], "quiet")
	}
	return chosen
}

func sampleLogSummary(results []*sampleLogResult) map[string]any {
	lines, recognized, errorsByRule, exceptions, product, loose, jsonLines := 0, 0, 0, 0, 0, 0, 0
	lineRates, byteRates := []float64{}, []float64{}
	maxLine := 0
	closes, opens, capped := 0, 0, 0
	byMethod := map[string]int{}
	byLevel := map[string]int{}
	for _, result := range results {
		if result == nil {
			continue
		}
		lines += result.Lines
		recognized += result.Recognized
		errorsByRule += result.ErrorByRule
		exceptions += result.ExceptionMarker
		product += result.ProductLevelErr
		loose += result.LooseErrorWord
		jsonLines += result.JSONLines
		lineRates = append(lineRates, result.LinesPerSecond)
		byteRates = append(byteRates, result.BytesPerSecond)
		maxLine = max(maxLine, result.MaxLineBytes)
		closes += len(result.Closes)
		opens += result.Opens
		if result.Capped {
			capped++
		}
		for key, value := range result.ByMethod {
			byMethod[key] += value
		}
		for key, value := range result.ByLevel {
			byLevel[key] += value
		}
	}
	return map[string]any{
		"followed": len(results), "lines": lines, "recognized_share": sampleShare(recognized, lines), "error_by_rule_share": sampleShare(errorsByRule, lines),
		"exception_marker_share": sampleShare(exceptions, lines), "product_breaklogs_error_share": sampleShare(product, lines), "loose_error_word_share": sampleShare(loose, lines),
		"json_share": sampleShare(jsonLines, lines), "lines_per_second": sampleDist(lineRates), "bytes_per_second": sampleDist(byteRates),
		"max_line_bytes": maxLine, "opens": opens, "closes": closes, "capped": capped, "by_method": byMethod, "by_level": byLevel,
	}
}

// ---------------------------------------------------------------- calls

func sampleCallSummary(calls []sampleCall) map[string]any {
	type acc struct {
		latency []float64
		bytes   []float64
		items   []float64
		errors  int
		pages   int
	}
	byName := map[string]*acc{}
	for _, call := range calls {
		entry := byName[call.Name]
		if entry == nil {
			entry = &acc{}
			byName[call.Name] = entry
		}
		entry.pages++
		if call.Error != "" {
			entry.errors++
			continue
		}
		entry.latency = append(entry.latency, call.LatencyMS)
		entry.bytes = append(entry.bytes, float64(call.Bytes))
		entry.items = append(entry.items, float64(call.Items))
	}
	out := map[string]any{}
	for name, entry := range byName {
		out[name] = map[string]any{"requests": entry.pages, "errors": entry.errors, "latency_ms": sampleDist(entry.latency), "bytes": sampleDist(entry.bytes), "items": sampleDist(entry.items)}
	}
	return out
}

// ---------------------------------------------------------------- report

func sampleF(value any) string {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) {
			return "n/a"
		}
		if typed == math.Trunc(typed) && math.Abs(typed) < 1e12 {
			return strconv.FormatFloat(typed, 'f', 0, 64)
		}
		return strconv.FormatFloat(typed, 'f', 3, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	}
	return fmt.Sprint(value)
}

func samplePct(value float64) string {
	if math.IsNaN(value) {
		return "n/a"
	}
	return strconv.FormatFloat(value*100, 'f', 1, 64) + "%"
}

func sampleDistRow(label string, dist map[string]float64) string {
	return fmt.Sprintf("| %s | %s | %s | %s | %s | %s |\n", label, sampleF(dist["n"]), sampleF(dist["p50"]), sampleF(dist["p90"]), sampleF(dist["p99"]), sampleF(dist["max"]))
}

func sampleSortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sampleRenderReport(report *strings.Builder, cluster string, result map[string]any, logs []*sampleLogResult, samples []sampleSample) {
	fmt.Fprintf(report, "## Cluster `%s`\n\n", cluster)
	shape := result["inventory"].(map[string]any)
	fmt.Fprintf(report, "### 1. Inventory shape\n\n| Measure | Value |\n|---|---|\n")
	fmt.Fprintf(report, "| Nodes | %v |\n| Pods (Running) | %v (%v) |\n| Objects | %v |\n| Workloads with Pods, by kind | %v |\n| Pods by kind | %v |\n",
		shape["nodes"], shape["pods"], shape["running_pods"], shape["objects"], shape["workloads_with_pods_by_kind"], shape["pods_by_kind"])
	fmt.Fprintf(report, "| **Layer 2: workloads with a group of >=4 Ready (>5 min) Pods of one version** | **%v of %v workloads with Pods** |\n", shape["layer2_workloads"], shape["workloads_with_pods"])
	fmt.Fprintf(report, "| **Layer 2: Pods in such groups** | **%v (%s of all Pods)** |\n", shape["layer2_pods"], samplePct(shape["layer2_pod_share"].(float64)))
	fmt.Fprintf(report, "| Layer 2 by kind [eligible, workloads] | %v |\n", shape["layer2_by_kind"])
	fmt.Fprintf(report, "| Ready-replica buckets (workloads, excl. Job/CronJob) | %v |\n| Ready-replica buckets (Pods) | %v |\n", shape["ready_replica_buckets_workloads"], shape["ready_replica_buckets_pods"])
	fmt.Fprintf(report, "| Workloads with two versions at t0 | %v |\n", shape["workloads_with_two_versions_at_t0"])
	fmt.Fprintf(report, "| Containers | %v |\n| Memory limit set | %v (%s) |\n| CPU limit set | %v (%s) |\n| Memory request set | %s |\n| CPU request set | %s |\n",
		shape["containers"], shape["memory_limit"], samplePct(shape["memory_limit_share"].(float64)), shape["cpu_limit"], samplePct(shape["cpu_limit_share"].(float64)), samplePct(shape["memory_request_share"].(float64)), samplePct(shape["cpu_request_share"].(float64)))
	fmt.Fprintf(report, "| Extra containers per Pod (sidecars incl. native) | %v; native sidecars %v; init containers %v |\n", shape["sidecars_per_pod"], shape["native_sidecars"], shape["init_containers"])
	fmt.Fprintf(report, "| Version identity [Pods with version key, Pods] by kind | %v |\n| Version labels by kind | %v |\n\n", shape["version_identity_by_kind"], shape["version_labels_by_kind"])

	fmt.Fprintf(report, "### 2. metrics.k8s.io Pod list (%d samples)\n\n| # | Latency ms | Bytes | Items | Pages | items null | Running not listed | Items w/o containers | Spec containers w/o usage | Zero-CPU containers | Not in inventory | Same timestamp as previous | Max item age s | Windows | Error |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n", len(samples))
	for index, sample := range samples {
		fmt.Fprintf(report, "| %d | %.0f | %d | %d | %d | %v | %d/%d | %d | %d/%d | %d | %d | %d | %.0f | %v | %s |\n", index+1, sample.LatencyMS, sample.Bytes, sample.Items, sample.Pages, sample.ItemsNull, sample.RunningNoMet, sample.Running, sample.EmptyItems, sample.MissingCont, sample.Containers, sample.ZeroCPU, sample.UnknownPods, sample.StaleItems, sample.MaxAgeS, sample.Windows, sample.Error)
	}
	spread := result["spread"].(map[string]any)
	fmt.Fprintf(report, "\n**Sibling spread (MAD / median, mean over samples per workload, groups of >=4 same-version Ready Pods)** — workloads scored: %v; median CPU < 10m: %v; with a sample where >30%% of the group deviated: %v; Pods that would be flagged (3.5 score, 30%%, 3 ticks): %v\n\n",
		spread["workloads_scored"], spread["workloads_median_cpu_below_10m"], spread["workloads_with_a_sample_over_30pct_deviating"], spread["pods_flagged_3_ticks"])
	fmt.Fprintf(report, "| Kind / signal | n | p50 | p90 | p99 | max | Share > 25%% (not comparable) |\n|---|---|---|---|---|---|---|\n")
	for _, kind := range sampleSortedKeys(spread) {
		entry, ok := spread[kind].(map[string]any)
		if !ok {
			continue
		}
		for _, signal := range []string{"cpu", "memory"} {
			dist := entry[signal].(map[string]float64)
			row := strings.TrimSuffix(sampleDistRow(kind+" "+signal, dist), "|\n")
			fmt.Fprintf(report, "%s| %s |\n", row, samplePct(entry[signal+"_share_above_25pct"].(float64)))
		}
	}

	events := result["events"].(map[string]any)
	fmt.Fprintf(report, "\n### 3. Warning events\n\n| Measure | Value |\n|---|---|\n| Occurrences in window (list deltas) | %v over %.0f s = **%.2f / min** |\n| New event objects by reason | %v |\n| Occurrences by reason | %v |\n| Initial list shape | %v |\n| Watch | %+v |\n\n",
		events["occurrences_total"], events["window_seconds"], events["per_minute"], events["new_objects_by_reason"], events["occurrences_by_reason"], events["shape_initial"], events["watch"])

	rollouts := result["rollouts"].(map[string]any)
	fmt.Fprintf(report, "### 4. Rollouts\n\n| Measure | Value |\n|---|---|\n")
	for _, key := range sampleSortedKeys(rollouts) {
		fmt.Fprintf(report, "| %s | %v |\n", key, rollouts[key])
	}

	logSummary := result["logs"].(map[string]any)
	fmt.Fprintf(report, "\n### 5. Pod logs (%v followed of %v probed)\n\n| Class | Kind | lines/s | bytes/s | lines | max line B | recognized | error by §6.1 rule | exception marker | product breaklogs error | loose `error` word | JSON | opens | closes | capped | max gap s | open errors |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n", logSummary["followed"], logSummary["candidates"])
	for _, item := range logs {
		if item == nil {
			continue
		}
		fmt.Fprintf(report, "| %s | %s | %.2f | %.0f | %d | %d | %s | %s | %s | %s | %s | %s | %d | %v | %v | %.0f | %d |\n", item.Target.Class, item.Target.Kind, item.LinesPerSecond, item.BytesPerSecond, item.Lines, item.MaxLineBytes,
			samplePct(sampleShare(item.Recognized, item.Lines)), samplePct(sampleShare(item.ErrorByRule, item.Lines)), samplePct(sampleShare(item.ExceptionMarker, item.Lines)), samplePct(sampleShare(item.ProductLevelErr, item.Lines)), samplePct(sampleShare(item.LooseErrorWord, item.Lines)), samplePct(sampleShare(item.JSONLines, item.Lines)),
			item.Opens, item.Closes, item.Capped, item.MaxGapSeconds, len(item.OpenErrors))
	}
	fmt.Fprintf(report, "\n| Aggregate | Value |\n|---|---|\n| Lines | %v |\n| Recognized level | %s |\n| Error by §6.1 rule | %s |\n| Exception marker | %s |\n| Product `breaklogs.Level` error | %s |\n| Loose `error` word | %s |\n| JSON lines | %s |\n| By method | %v |\n| By level | %v |\n| Max line bytes | %v |\n\n",
		logSummary["lines"], samplePct(logSummary["recognized_share"].(float64)), samplePct(logSummary["error_by_rule_share"].(float64)), samplePct(logSummary["exception_marker_share"].(float64)), samplePct(logSummary["product_breaklogs_error_share"].(float64)), samplePct(logSummary["loose_error_word_share"].(float64)), samplePct(logSummary["json_share"].(float64)), logSummary["by_method"], logSummary["by_level"], logSummary["max_line_bytes"])
	fmt.Fprintf(report, "| Distribution | n | p50 | p90 | p99 | max |\n|---|---|---|---|---|---|\n")
	report.WriteString(sampleDistRow("followed lines/s", logSummary["lines_per_second"].(map[string]float64)))
	report.WriteString(sampleDistRow("followed bytes/s", logSummary["bytes_per_second"].(map[string]float64)))
	report.WriteString(sampleDistRow("probed candidates bytes/s (last 60 s, one Pod per workload)", logSummary["candidate_bytes_per_second"].(map[string]float64)))

	if pass, ok := logSummary["probe_pass"].(map[string]any); ok {
		fmt.Fprintf(report, "\n**Probe pass** (last 60 s of every probed candidate, one Pod per workload): lines %v; recognized %s; error by rule %s; exception marker %s; product breaklogs error %s; loose `error` word %s; JSON %s; by method %v; by level %v; workloads %v\n", pass["lines"], samplePct(pass["recognized_share"].(float64)), samplePct(pass["error_by_rule_share"].(float64)), samplePct(pass["exception_marker_share"].(float64)), samplePct(pass["product_breaklogs_error_share"].(float64)), samplePct(pass["loose_error_word_share"].(float64)), samplePct(pass["json_share"].(float64)), pass["by_method"], pass["by_level"], logSummary["probe_pass_workloads_mostly_recognized"])
	}
	calls := result["calls"].(map[string]any)
	fmt.Fprintf(report, "\n### 6. API call latencies\n\n| Call | Requests | Errors | latency p50 ms | p90 | max | bytes p50 | bytes max | items max |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, name := range sampleSortedKeys(calls) {
		entry := calls[name].(map[string]any)
		latency := entry["latency_ms"].(map[string]float64)
		bytes := entry["bytes"].(map[string]float64)
		items := entry["items"].(map[string]float64)
		fmt.Fprintf(report, "| %s | %v | %v | %s | %s | %s | %s | %s | %s |\n", name, entry["requests"], entry["errors"], sampleF(latency["p50"]), sampleF(latency["p90"]), sampleF(latency["max"]), sampleF(bytes["p50"]), sampleF(bytes["max"]), sampleF(items["max"]))
	}
	fmt.Fprintf(report, "\nProbe wall time for this cluster: %.0f s\n\n", result["seconds"])
}

// sampleMostlyRecognized counts the probed workloads whose lines have a
// recognised level at least 80% of the time.
func sampleMostlyRecognized(results []*sampleLogResult) map[string]int {
	out := map[string]int{"workloads_with_lines": len(results)}
	for _, result := range results {
		if sampleShare(result.Recognized, result.Lines) >= 0.8 {
			out["recognized_80pct"]++
		}
		if result.ErrorByRule > 0 {
			out["with_error_lines"]++
		}
		if result.JSONLines*2 > result.Lines {
			out["mostly_json"]++
		}
	}
	return out
}
