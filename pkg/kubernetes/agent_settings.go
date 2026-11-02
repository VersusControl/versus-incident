package kubernetes

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func (service *Service) SetBreakLogSources(resolve func(string) []breaklogs.Source) {
	if service == nil || service.changes == nil {
		return
	}
	service.changes.mu.Lock()
	service.logSources = resolve
	service.changes.mu.Unlock()
	clearLinkedCache()
}

func (service *Service) breakLogSourceResolver() func(string) []breaklogs.Source {
	if service == nil || service.changes == nil {
		return nil
	}
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	return service.logSources
}

func (service *Service) logSettingsStore() (storage.Provider, string) {
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	return service.changes.provider, breaklogs.SettingsKey(service.scope.OrgID, service.scope.ClusterID)
}

func (service *Service) configuredLogSources() []breaklogs.Source {
	resolve := service.breakLogSourceResolver()
	if resolve == nil {
		return []breaklogs.Source{}
	}
	return resolve(service.scope.OrgID)
}

type TraceBackend struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
}
type AgentDetected struct {
	MetricsServer bool          `json:"metrics_server"`
	Traces        *TraceBackend `json:"traces"`
	Workloads     int           `json:"workloads"`
	Namespaces    int           `json:"namespaces"`
}
type AgentSettings struct {
	breaklogs.Settings
	Sources             []breaklogs.Source `json:"sources"`
	Detected            AgentDetected      `json:"detected"`
	LinkedSourceMissing bool               `json:"linked_source_missing"`
	CanWrite            bool               `json:"can_write"`
}

func DetectAgentBackends(records []kubeindex.Record) AgentDetected {
	result := AgentDetected{}
	namespaces := map[string]bool{}
	backends := []TraceBackend{}
	for _, record := range records {
		if record.Kind == "Deployment" || record.Kind == "StatefulSet" {
			result.Workloads++
			namespaces[record.Namespace] = true
		}
		if record.Kind != "Service" && record.Kind != "Deployment" {
			continue
		}
		for _, name := range []string{record.Name, record.Labels["app.kubernetes.io/name"]} {
			if name == "metrics-server" {
				result.MetricsServer = true
			}
			switch name {
			case "jaeger", "jaeger-query", "tempo", "opentelemetry-collector", "otel-collector", "zipkin":
				backends = append(backends, TraceBackend{Kind: name, Namespace: record.Namespace})
			}
		}
	}
	result.Namespaces = len(namespaces)
	sort.Slice(backends, func(left, right int) bool {
		if backends[left].Namespace != backends[right].Namespace {
			return backends[left].Namespace < backends[right].Namespace
		}
		return backends[left].Kind < backends[right].Kind
	})
	if len(backends) > 0 {
		result.Traces = &backends[0]
	}
	return result
}

func (service *Service) AgentSettings(ctx context.Context) (AgentSettings, error) {
	if err := service.validateCredential(ctx); err != nil {
		return AgentSettings{}, err
	}
	provider, key := service.logSettingsStore()
	state, err := breaklogs.LoadSettings(provider, key)
	if err != nil {
		return AgentSettings{}, err
	}
	snapshot, _, err := service.IndexSnapshot(ctx, "Service", "Deployment", "StatefulSet")
	if err != nil {
		return AgentSettings{}, err
	}
	result := AgentSettings{Settings: state.Settings, Sources: service.configuredLogSources(), Detected: DetectAgentBackends(snapshot.Records), LinkedSourceMissing: state.Settings.BreakLogs.LinkedSource != ""}
	if result.BreakLogs.Fields == nil {
		result.BreakLogs.Fields = map[string]breaklogs.Fields{}
	}
	for _, kind := range []string{"loki", "elasticsearch", "cloudwatchlogs", "graylog", "splunk", "signoz"} {
		fields, normalizeErr := breaklogs.NormalizeFields(kind, result.BreakLogs.Fields[kind])
		if normalizeErr != nil {
			return AgentSettings{}, ErrInvalidArguments
		}
		result.BreakLogs.Fields[kind] = fields
	}
	usage, usageErr := service.Usage(ctx, "", 1)
	result.Detected.MetricsServer = usageErr == nil && (usage.Availability == "available" || usage.Availability == "stale" || usage.Availability == "partial")
	for position := range result.Sources {
		source := &result.Sources[position]
		source.Health = "not_checked"
		if health, ok := state.Health[source.Name]; ok {
			source.Health = health.State
			at := health.At
			source.CheckedAt = &at
		}
		if source.Name == result.BreakLogs.LinkedSource {
			result.LinkedSourceMissing = false
		}
	}
	return result, service.validateCredential(ctx)
}

func (service *Service) SaveAgentSettings(ctx context.Context, settings breaklogs.Settings) error {
	if err := service.validateCredential(ctx); err != nil {
		return err
	}
	if settings.BreakLogs.Fields == nil {
		settings.BreakLogs.Fields = map[string]breaklogs.Fields{}
	}
	if len(settings.Learning.Scope.Include) > 100 || len(settings.Learning.Scope.Exclude) > 100 || len(settings.BreakLogs.Fields) > 6 || len(settings.BreakLogs.LinkedSource) > 128 {
		return ErrInvalidArguments
	}
	if settings.BreakLogs.IngestDelay != nil && (*settings.BreakLogs.IngestDelay < 0 || *settings.BreakLogs.IngestDelay > 3600) {
		return ErrInvalidArguments
	}
	for _, namespace := range append(append([]string(nil), settings.Learning.Scope.Include...), settings.Learning.Scope.Exclude...) {
		if !validPodLogName(namespace, 63, false) {
			return ErrInvalidArguments
		}
	}
	for kind, fields := range settings.BreakLogs.Fields {
		normalized, err := breaklogs.NormalizeFields(kind, fields)
		if err != nil {
			return ErrInvalidArguments
		}
		settings.BreakLogs.Fields[kind] = normalized
	}
	if settings.BreakLogs.LinkedSource != "" {
		found := false
		for _, source := range service.configuredLogSources() {
			if source.Name == settings.BreakLogs.LinkedSource {
				found = true
				fields, err := breaklogs.NormalizeFields(source.Type, settings.BreakLogs.Fields[source.Type])
				if err != nil {
					return ErrInvalidArguments
				}
				if _, err := source.Build(fields); err != nil {
					return ErrInvalidArguments
				}
				settings.BreakLogs.Fields[source.Type] = fields
			}
		}
		if !found {
			return ErrInvalidArguments
		}
	}
	if settings.Learning.Scope.Include == nil {
		settings.Learning.Scope.Include = []string{}
	}
	if settings.Learning.Scope.Exclude == nil {
		settings.Learning.Scope.Exclude = []string{}
	}
	if settings.BreakLogs.Fields == nil {
		settings.BreakLogs.Fields = map[string]breaklogs.Fields{}
	}
	provider, key := service.logSettingsStore()
	clearLinkedCache()
	return service.commitCredential(ctx, func() error {
		return breaklogs.UpdateSettings(provider, key, func(state *breaklogs.SettingsState) error {
			settings.Alerts = state.Settings.Alerts
			state.Settings = settings
			return nil
		})
	})
}

type TestLinkRequest struct {
	Source string           `json:"source"`
	Fields breaklogs.Fields `json:"fields"`
}
type TestLinkResult struct {
	Lines      int    `json:"lines"`
	Namespace  string `json:"namespace"`
	Workload   string `json:"workload"`
	DurationMS int64  `json:"duration_ms"`
	Preview    string `json:"preview"`
	Truncated  bool   `json:"truncated"`
}

func (service *Service) TestLogLink(ctx context.Context, request TestLinkRequest) (TestLinkResult, error) {
	if err := service.validateCredential(ctx); err != nil {
		return TestLinkResult{}, err
	}
	var selected *breaklogs.Source
	for _, source := range service.configuredLogSources() {
		if source.Name == request.Source {
			copy := source
			selected = &copy
			break
		}
	}
	if selected == nil {
		return TestLinkResult{}, errors.New("not_found")
	}
	fields, err := breaklogs.NormalizeFields(selected.Type, request.Fields)
	if err != nil {
		return TestLinkResult{}, errors.New("invalid_mapping")
	}
	reader, err := selected.Build(fields)
	if err != nil {
		return TestLinkResult{}, errors.New("invalid_mapping")
	}
	snapshot, _, err := service.IndexSnapshot(ctx, "Pod", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob")
	if err != nil {
		return TestLinkResult{}, err
	}
	now := time.Now().UTC()
	roots := kubebreaks.Classify(snapshot.Records, now)
	var root kubeindex.Record
	if len(roots) > 0 {
		root = kubeindex.Record{Name: roots[0].Root.Name, UID: roots[0].Root.UID, Namespace: roots[0].Root.Namespace}
	} else {
		for _, record := range snapshot.Records {
			if record.Kind == "Deployment" || record.Kind == "StatefulSet" {
				if root.UID == "" || record.Replicas > root.Replicas || record.Replicas == root.Replicas && record.UID < root.UID {
					root = record
				}
			}
		}
	}
	if root.UID == "" {
		return TestLinkResult{}, errors.New("not_found")
	}
	pods := breakLogPods(snapshot.Records, root.Namespace, root.UID)
	query := breaklogs.LinkedQuery{Namespace: root.Namespace, PodPrefix: root.Name, From: now.Add(-15 * time.Minute), To: now, Limit: 500}
	for _, pod := range pods {
		if len(query.Pods) == 5 {
			break
		}
		query.Pods = append(query.Pods, pod.Name)
	}
	started := time.Now()
	readContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := reader.Read(readContext, query)
	elapsed := time.Since(started)
	if healthErr := service.recordLogHealth(ctx, selected.Name, started, err); healthErr != nil {
		return TestLinkResult{}, healthErr
	}
	if err != nil {
		return TestLinkResult{}, err
	}
	preview := reader.Preview(query)
	if service.scrubber != nil {
		preview = service.scrubber.Scrub(preview)
	}
	return TestLinkResult{Lines: len(result.Lines), Namespace: root.Namespace, Workload: root.Name, DurationMS: elapsed.Milliseconds(), Preview: preview, Truncated: result.Truncated}, service.validateCredential(ctx)
}

func (service *Service) recordLogHealth(ctx context.Context, name string, started time.Time, readErr error) error {
	health := "healthy"
	if time.Since(started) > 3*time.Second {
		health = "slow"
	}
	if readErr != nil {
		health = "failing"
	}
	provider, key := service.logSettingsStore()
	return service.commitCredential(ctx, func() error {
		return breaklogs.UpdateSettings(provider, key, func(state *breaklogs.SettingsState) error {
			now := time.Now().UTC()
			if previous, ok := state.Health[name]; ok && previous.State == health && now.Sub(previous.At) < time.Minute {
				return nil
			}
			if state.Health == nil {
				state.Health = map[string]breaklogs.Health{}
			}
			configured := map[string]bool{}
			for _, source := range service.configuredLogSources() {
				configured[source.Name] = true
			}
			for source := range state.Health {
				if !configured[source] {
					delete(state.Health, source)
				}
			}
			state.Health[strings.Clone(name)] = breaklogs.Health{State: health, At: now}
			return nil
		})
	})
}
