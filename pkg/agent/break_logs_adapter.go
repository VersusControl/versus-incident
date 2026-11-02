package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/cloudwatchlogs"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/elasticsearch"
	"github.com/VersusControl/versus-incident/pkg/graylog"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/loki"
	"github.com/VersusControl/versus-incident/pkg/signoz"
	"github.com/VersusControl/versus-incident/pkg/splunk"
	"github.com/aws/smithy-go"
)

type breakLogAdapter struct {
	source   config.AgentSourceConfig
	fields   breaklogs.Fields
	scrubber core.Scrubber
	cloudAPI cloudwatchlogs.API
}

func ConfigureKubernetesBreakLogs(registry *kubernetes.ServiceRegistry, sources []config.AgentSourceConfig, scrubber core.Scrubber, org string) {
	registry.Each(func(service *kubernetes.Service) {
		service.SetBreakLogSources(func(requestOrg string) []breaklogs.Source {
			if requestOrg != org {
				return []breaklogs.Source{}
			}
			return BuildBreakLogSources(sources, scrubber, service.Scope().ClusterID)
		})
	})
}

func BuildBreakLogSources(sources []config.AgentSourceConfig, scrubber core.Scrubber, cluster string) []breaklogs.Source {
	counts := map[string]int{}
	for _, source := range sources {
		if source.Enable {
			counts[source.Name]++
		}
	}
	result := []breaklogs.Source{}
	for _, source := range sources {
		if !source.Enable || source.Name == "" || len(source.Name) > 128 || counts[source.Name] != 1 || source.KubernetesCluster != "" && source.KubernetesCluster != cluster || breaklogs.DefaultFields(source.Type).Namespace == "" {
			continue
		}
		copy := source
		// Include pointer-valued immutable TLS trust configuration as well as
		// serialized settings. Only the digest, never credentials, leaves here.
		digest := sha256.Sum256([]byte(fmt.Sprintf("%#v", copy)))
		cacheKey := hex.EncodeToString(digest[:])
		scope := ""
		switch source.Type {
		case "loki":
			scope = source.Loki.Query
		case "elasticsearch":
			scope = source.Elasticsearch.Index
		case "cloudwatchlogs":
			scope = source.CloudWatchLogs.LogGroupName
		case "graylog":
			scope = source.Graylog.StreamID
		case "splunk":
			scope = source.Splunk.Search
		case "signoz":
			scope = source.Signoz.Query
		}
		if scrubber != nil {
			scope = (&breakLogAdapter{source: copy, scrubber: scrubber}).safe(scope)
		}
		if len(scope) > 1024 {
			scope = strings.ToValidUTF8(scope[:1024], "")
		}
		result = append(result, breaklogs.Source{CacheKey: cacheKey, Name: source.Name, Type: source.Type, Scope: scope, Health: "not_checked", Build: func(fields breaklogs.Fields) (breaklogs.LinkedReader, error) {
			normalized, err := breaklogs.NormalizeFields(copy.Type, fields)
			if err != nil || scrubber == nil || copy.Type == "loki" && !linkedLokiSelector.MatchString(strings.TrimSpace(copy.Loki.Query)) {
				return nil, errors.New("invalid_mapping")
			}
			return &breakLogAdapter{source: copy, fields: normalized, scrubber: scrubber}, nil
		}})
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Type != result[right].Type {
			return result[left].Type < result[right].Type
		}
		return result[left].Name < result[right].Name
	})
	return result
}

type linkedReadPlan struct {
	Type  string    `json:"type"`
	Scope string    `json:"scope"`
	Query any       `json:"query"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Limit int       `json:"limit"`
}

func (adapter *breakLogAdapter) plan(query breaklogs.LinkedQuery, pod string) linkedReadPlan {
	fields := adapter.fields
	filters := map[string]string{fields.Namespace: query.Namespace, fields.Pod: pod}
	if pod == "" {
		delete(filters, fields.Pod)
	}
	if query.Container != "" {
		filters[fields.Container] = query.Container
	}
	keys := []string{}
	for key := range filters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	multiple := pod == "" && len(query.Pods) > 0 && query.Pods[0] != ""
	plan := linkedReadPlan{Type: adapter.source.Type, From: query.From, To: query.To, Limit: min(50, query.Limit)}
	switch adapter.source.Type {
	case "loki":
		selector := strings.TrimSpace(adapter.source.Loki.Query)
		if strings.HasSuffix(selector, "}") {
			selector = strings.TrimSuffix(selector, "}")
			for _, key := range keys {
				selector += "," + key + "=" + strconv.Quote(filters[key])
			}
			if pod == "" {
				pattern := regexp.QuoteMeta(query.PodPrefix) + "-.*"
				if multiple {
					names := []string{}
					for _, name := range query.Pods {
						names = append(names, regexp.QuoteMeta(name))
					}
					pattern = strings.Join(names, "|")
				}
				selector += "," + fields.Pod + "=~" + strconv.Quote(pattern)
			}
			selector += "}"
		}
		plan.Scope = adapter.source.Loki.Query
		plan.Query = selector
	case "elasticsearch":
		clauses := []any{}
		for _, key := range keys {
			clauses = append(clauses, map[string]any{"term": map[string]string{key: filters[key]}})
		}
		if pod == "" {
			if multiple {
				clauses = append(clauses, map[string]any{"terms": map[string]any{fields.Pod: query.Pods}})
			} else {
				clauses = append(clauses, map[string]any{"prefix": map[string]string{fields.Pod: query.PodPrefix + "-"}})
			}
		}
		clauses = append(clauses, map[string]any{"range": map[string]any{fields.Time: map[string]string{"gte": query.From.Format(time.RFC3339Nano), "lte": query.To.Format(time.RFC3339Nano)}}})
		plan.Scope = adapter.source.Elasticsearch.Index
		plan.Query = map[string]any{"query": map[string]any{"bool": map[string]any{"filter": clauses}}, "sort": []any{map[string]string{fields.Time: "asc"}}}
	case "graylog":
		parts := []string{}
		for _, key := range keys {
			parts = append(parts, key+":"+strconv.Quote(filters[key]))
		}
		if pod == "" {
			if multiple {
				names := []string{}
				for _, name := range query.Pods {
					names = append(names, fields.Pod+":"+strconv.Quote(name))
				}
				parts = append(parts, "("+strings.Join(names, " OR ")+")")
			} else {
				parts = append(parts, fields.Pod+":"+strings.ReplaceAll(query.PodPrefix+"-", "-", `\-`)+"*")
			}
		}
		if scope := adapter.source.Graylog.Query; scope != "" && scope != "*" {
			configured := []string{}
			for _, part := range strings.Split(scope, " AND ") {
				key, value, ok := strings.Cut(part, ":")
				if ok {
					configured = append(configured, key+":"+strconv.Quote(value))
				}
			}
			parts = append(configured, parts...)
		}
		plan.Scope = adapter.source.Graylog.StreamID
		plan.Query = strings.Join(parts, " AND ")
	case "splunk":
		parts := []string{strings.TrimPrefix(adapter.source.Splunk.Search, "search ")}
		for _, key := range keys {
			parts = append(parts, key+"="+filters[key])
		}
		if pod == "" {
			if multiple {
				names := []string{}
				for _, name := range query.Pods {
					names = append(names, fields.Pod+"="+strconv.Quote(name))
				}
				parts = append(parts, "("+strings.Join(names, " OR ")+")")
			} else {
				parts = append(parts, fields.Pod+"="+query.PodPrefix+"-*")
			}
		}
		plan.Scope = adapter.source.Splunk.Search
		plan.Query = "search " + strings.Join(parts, " AND ")
	case "signoz":
		parts := []string{}
		if adapter.source.Signoz.Query != "" {
			parts = append(parts, "("+adapter.source.Signoz.Query+")")
		}
		for _, key := range keys {
			parts = append(parts, key+" = '"+strings.ReplaceAll(strings.ReplaceAll(filters[key], "\\", "\\\\"), "'", "\\'")+"'")
		}
		if pod == "" {
			if multiple {
				names := []string{}
				for _, name := range query.Pods {
					names = append(names, fields.Pod+" = '"+strings.ReplaceAll(strings.ReplaceAll(name, "\\", "\\\\"), "'", "\\'")+"'")
				}
				parts = append(parts, "("+strings.Join(names, " OR ")+")")
			} else {
				parts = append(parts, fields.Pod+" LIKE '"+strings.ReplaceAll(strings.ReplaceAll(query.PodPrefix, "_", "\\_"), "%", "\\%")+"-%'")
			}
		}
		plan.Scope = adapter.source.Signoz.Query
		plan.Query = "(" + strings.Join(parts, " AND ") + ")"
	case "cloudwatchlogs":
		plan.Scope = adapter.source.CloudWatchLogs.LogGroupName
		plan.Query = map[string]any{"operation": "FilterLogEvents", "filter_pattern": adapter.cloudPattern(query, pod), "stream_prefix": adapter.source.CloudWatchLogs.LogStreamPrefix, "match_json_fields": filters, "pods": query.Pods, "pod_prefix": query.PodPrefix, "message_field": fields.Message}
	}
	return plan
}

func (adapter *breakLogAdapter) Preview(query breaklogs.LinkedQuery) string {
	plans := []linkedReadPlan{}
	pods := query.Pods
	if len(pods) == 0 {
		pods = []string{""}
	} else if len(pods) > 1 {
		pods = []string{""}
	}
	for _, pod := range pods {
		plan := adapter.plan(query, pod)
		if adapter.source.Type == "elasticsearch" {
			cfg := adapter.source.Elasticsearch
			fields := adapter.fields
			cfg.TimeField = fields.Time
			cfg.MessageField = fields.Message
			cfg.ExtraFields = append(append([]string(nil), cfg.ExtraFields...), fields.Namespace, fields.Pod, fields.Container)
			if service, err := elasticsearch.NewService(cfg); err == nil {
				body, _ := json.Marshal(plan.Query)
				if preview, err := service.PreviewSearch(elasticsearch.SearchOptions{QueryBody: body, Fields: []string{fields.Time, fields.Message, fields.Container, fields.Pod}, Limit: plan.Limit}); err == nil {
					plan.Query = preview
				}
			}
		}
		plans = append(plans, plan)
	}
	encoded, _ := json.Marshal(plans)
	return adapter.safe(string(encoded))
}

var linkedValue = regexp.MustCompile(`^[A-Za-z0-9_.@/-]{1,253}$`)
var linkedLokiSelector = regexp.MustCompile(`^\{\s*[A-Za-z_][A-Za-z0-9_]*\s*(?:=~|!~|!=|=)\s*"(?:[^"\\]|\\.)*"(?:\s*,\s*[A-Za-z_][A-Za-z0-9_]*\s*(?:=~|!~|!=|=)\s*"(?:[^"\\]|\\.)*")*\s*\}$`)

func (adapter *breakLogAdapter) Read(ctx context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
	result := breaklogs.Result{Lines: []breaklogs.LinkedLine{}}
	if err := query.Validate(); err != nil {
		return result, errors.New("invalid_mapping")
	}
	if adapter.source.Type == "loki" && !linkedLokiSelector.MatchString(strings.TrimSpace(adapter.source.Loki.Query)) {
		return result, errors.New("invalid_mapping")
	}
	if !linkedValue.MatchString(query.Namespace) || query.Container != "" && !linkedValue.MatchString(query.Container) {
		return result, errors.New("invalid_mapping")
	}
	if len(query.Pods) == 0 {
		if !linkedValue.MatchString(query.PodPrefix) {
			return result, errors.New("invalid_mapping")
		}
		query.Pods = []string{""}
	}
	for _, pod := range query.Pods {
		if pod != "" && !linkedValue.MatchString(pod) {
			return result, errors.New("invalid_mapping")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	bytes := 0
	readPods := query.Pods
	if len(query.Pods) > 1 {
		readPods = []string{""}
	}
	for _, pod := range readPods {
		if len(result.Lines) >= query.Limit {
			result.Truncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			return result, errors.New("timeout")
		}
		if !breaklogs.AdmitLinkedCall(ctx) {
			result.Truncated = true
			break
		}
		plan := adapter.plan(query, pod)
		plan.Limit = min(50, query.Limit-len(result.Lines))
		rows, truncated, err := adapter.readPlan(ctx, query, pod, plan)
		if err != nil {
			return result, safeLinkedError(ctx, err)
		}
		result.Truncated = result.Truncated || truncated
		for _, row := range rows {
			if row.At.IsZero() {
				result.Truncated = true
				continue
			}
			if row.At.Before(query.From) || row.At.After(query.To) {
				continue
			}
			selected := pod != "" && row.Pod == pod
			if pod == "" {
				for _, name := range query.Pods {
					selected = selected || name != "" && name == row.Pod
				}
				if query.Pods[0] == "" {
					selected = strings.HasPrefix(row.Pod, query.PodPrefix+"-")
				}
			}
			if row.Pod == "" || !linkedValue.MatchString(row.Pod) || !selected {
				result.Truncated = true
				continue
			}
			row.Text = adapter.safe(row.Text)
			if len(row.Text) > 4096 {
				row.Text = strings.ToValidUTF8(row.Text[:4096], "")
				result.Truncated = true
			}
			bytes += len(row.Text)
			if bytes > 1<<20 || len(result.Lines) == query.Limit {
				result.Truncated = true
				break
			}
			result.Lines = append(result.Lines, row)
		}
	}
	return result, nil
}

func safeLinkedError(ctx context.Context, err error) error {
	safe := func(class string) error { return &breaklogs.ReadError{Class: class} }
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return safe("timeout")
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return safe("timeout")
	}
	var upstream smithy.APIError
	if errors.As(err, &upstream) {
		switch strings.ToLower(upstream.ErrorCode()) {
		case "accessdenied", "accessdeniedexception", "unauthorizedexception", "unrecognizedclientexception", "invalidsignatureexception":
			return safe("unauthorized")
		case "resourcenotfoundexception", "notfoundexception":
			return safe("not_found")
		case "requesttimeout", "requesttimeoutexception":
			return safe("timeout")
		default:
			return safe("unavailable")
		}
	}
	value := strings.ToLower(err.Error())
	if errors.Is(err, elasticsearch.ErrForbidden) || errors.Is(err, elasticsearch.ErrUnauthorized) || strings.Contains(value, "401") || strings.Contains(value, "403") || strings.Contains(value, "accessdenied") || strings.Contains(value, "unauthorized") {
		return safe("unauthorized")
	}
	if errors.Is(err, elasticsearch.ErrNotFound) || strings.Contains(value, "404") || strings.Contains(value, "notfound") {
		return safe("not_found")
	}
	if errors.Is(err, graylog.ErrInvalidArgument) || errors.Is(err, graylog.ErrInvalidConfig) || errors.Is(err, graylog.ErrUnsupportedScope) || errors.Is(err, splunk.ErrInvalidArgument) || errors.Is(err, splunk.ErrInvalidConfig) || errors.Is(err, cloudwatchlogs.ErrScope) || errors.Is(err, loki.ErrInvalidConfig) || errors.Is(err, loki.ErrInvalidArgument) || err.Error() == "invalid_mapping" {
		return safe("invalid_mapping")
	}
	return safe("unavailable")
}

// CloudWatch evaluates the exact Pod alternatives server-side, so unrelated
// streams cannot consume the bounded pages before verified Pods are queried.
func (adapter *breakLogAdapter) cloudPattern(query breaklogs.LinkedQuery, pod string) string {
	parts := []string{"$." + adapter.fields.Namespace + " = " + strconv.Quote(query.Namespace)}
	if query.Container != "" {
		parts = append(parts, "$."+adapter.fields.Container+" = "+strconv.Quote(query.Container))
	}
	names := query.Pods
	if pod != "" {
		names = []string{pod}
	}
	if len(names) > 0 && names[0] != "" {
		matches := []string{}
		for _, name := range names {
			matches = append(matches, "$."+adapter.fields.Pod+" = "+strconv.Quote(name))
		}
		parts = append(parts, "("+strings.Join(matches, " || ")+")")
	}
	identity := "{ " + strings.Join(parts, " && ") + " }"
	scope := strings.TrimSpace(adapter.source.CloudWatchLogs.FilterPattern)
	if scope == "" {
		return identity
	}
	// JSON filter patterns can be combined without broadening the configured
	// scope. Preserve legacy non-JSON scopes as one bounded scope sample with
	// exact identity filtering after retrieval; unrelated rows may exhaust its
	// native pages, in which case coverage stays explicitly truncated.
	if strings.HasPrefix(scope, "{") && strings.HasSuffix(scope, "}") {
		return "{ (" + strings.TrimSpace(scope[1:len(scope)-1]) + ") && (" + strings.Join(parts, " && ") + ") }"
	}
	return scope
}

func (adapter *breakLogAdapter) readPlan(ctx context.Context, query breaklogs.LinkedQuery, pod string, plan linkedReadPlan) ([]breaklogs.LinkedLine, bool, error) {
	fields := adapter.fields
	filters := map[string]string{fields.Namespace: query.Namespace, fields.Pod: pod}
	prefixes := map[string]string{}
	oneOf := map[string][]string{}
	if pod == "" {
		delete(filters, fields.Pod)
		if query.Pods[0] != "" {
			oneOf[fields.Pod] = append([]string(nil), query.Pods...)
		} else {
			prefixes[fields.Pod] = query.PodPrefix
		}
	}
	if query.Container != "" {
		filters[fields.Container] = query.Container
	}
	rows := []breaklogs.LinkedLine{}
	invalidRows := false
	add := func(at time.Time, text, container string, foundPod ...string) {
		name := pod
		if len(foundPod) > 0 {
			name = foundPod[0]
		}
		if !linkedValue.MatchString(container) || !linkedValue.MatchString(name) {
			invalidRows = true
			return
		}
		rows = append(rows, breaklogs.LinkedLine{At: at, Text: text, Pod: name, Container: container})
	}
	switch adapter.source.Type {
	case "loki":
		cfg := adapter.source.Loki
		cfg.Query = plan.Query.(string)
		service, err := loki.NewService(cfg, adapter.scrubber)
		if err != nil {
			return nil, false, err
		}
		// A bounded linked excerpt must contribute the head missing from the
		// kubelet's retained tail, rather than spend its budget on duplicates.
		result, err := service.ReadWindow(ctx, loki.ReadRequest{Limit: plan.Limit, Forward: true}, query.From, query.To)
		for _, row := range result.Records {
			if row.Labels[fields.Namespace] != query.Namespace || row.Labels[fields.Pod] == "" || query.Container != "" && row.Labels[fields.Container] != query.Container {
				invalidRows = true
				continue
			}
			add(row.Timestamp, row.Message, row.Labels[fields.Container], row.Labels[fields.Pod])
		}
		return rows, result.Truncated || invalidRows, err
	case "elasticsearch":
		cfg := adapter.source.Elasticsearch
		// Native address failover is bounded independently of planner calls.
		// Other adapters use one request (Loki/SigNoz), three native pages
		// (CloudWatch/Graylog), or six lifecycle requests (Splunk) per query.
		cfg.Addresses = append([]string(nil), cfg.Addresses[:min(3, len(cfg.Addresses))]...)
		cfg.TimeField = fields.Time
		cfg.MessageField = fields.Message
		cfg.ExtraFields = append(append([]string(nil), cfg.ExtraFields...), fields.Namespace, fields.Pod, fields.Container)
		service, err := elasticsearch.NewService(cfg)
		if err != nil {
			return nil, false, err
		}
		service.SetScrubber(adapter.scrubber)
		body, _ := json.Marshal(plan.Query)
		result, err := service.SearchWithPrefix(ctx, elasticsearch.SearchOptions{QueryBody: body, Fields: []string{fields.Time, fields.Message, fields.Container, fields.Pod}, Limit: plan.Limit})
		for _, row := range result.Hits {
			message := mappedValue(row.Fields, fields.Message)
			if message == nil {
				invalidRows = true
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(mappedValue(row.Fields, fields.Time)))
			add(at, fmt.Sprint(message), mappedString(row.Fields, fields.Container), mappedString(row.Fields, fields.Pod))
		}
		return rows, result.Truncated || result.TimedOut || invalidRows, err
	case "graylog":
		cfg := adapter.source.Graylog
		cfg.Fields = append(append([]string(nil), cfg.Fields...), fields.Namespace, fields.Pod, fields.Container)
		service, err := graylog.NewService(cfg, adapter.scrubber)
		if err != nil {
			return nil, false, err
		}
		result, err := service.ReadWindow(ctx, graylog.ReadRequest{Filters: filters, OneOfFilters: oneOf, PrefixFilters: prefixes, MaxRequests: 3, StringFields: []string{fields.Namespace, fields.Pod, fields.Container}, Limit: plan.Limit}, query.From, query.To)
		for _, row := range result.Records {
			at, _ := time.Parse(time.RFC3339Nano, row.Timestamp)
			add(at, row.Message, row.Fields[fields.Container], row.Fields[fields.Pod])
		}
		return rows, result.Truncated || result.Partial || invalidRows, err
	case "splunk":
		service, err := splunk.NewService(adapter.source.Splunk, adapter.scrubber)
		if err != nil {
			return nil, false, err
		}
		var result splunk.ReadResult
		if len(oneOf) > 0 {
			result, err = service.ReadWindowWithOneOfFields(ctx, filters, oneOf, query.From, query.To, plan.Limit, []string{fields.Container})
		} else {
			result, err = service.ReadWindowWithStringFields(ctx, filters, query.From, query.To, plan.Limit, []string{fields.Container}, prefixes)
		}
		for _, row := range result.Records {
			if row.Fields[fields.Namespace] != query.Namespace || query.Container != "" && row.Fields[fields.Container] != query.Container {
				invalidRows = true
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, row.Timestamp)
			if at.IsZero() {
				if seconds, parseErr := strconv.ParseFloat(row.Timestamp, 64); parseErr == nil {
					at = time.Unix(0, int64(seconds*1e9)).UTC()
				}
			}
			add(at, row.Message, row.Fields[fields.Container], row.Fields[fields.Pod])
		}
		return rows, result.Truncated || result.Partial || invalidRows, err
	case "signoz":
		cfg := adapter.source.Signoz
		service, err := signoz.NewService(signoz.Config{Address: cfg.Address, APIKey: cfg.APIKey, InsecureSkipVerify: cfg.InsecureSkipVerify, AllowLoopback: cfg.AllowLoopback, AllowPrivate: cfg.AllowPrivateNetworks, RootCAs: cfg.RootCAs, ScopeFilter: strings.TrimSuffix(strings.TrimPrefix(plan.Query.(string), "("), ")")}, signoz.Policy{Timeout: 10 * time.Second, MaximumBytes: 512 << 10, MaximumWindow: 2 * time.Hour, MaximumRows: plan.Limit})
		if err != nil {
			return nil, false, err
		}
		service.SetScrubber(adapter.scrubber)
		result, err := service.SearchLogs(ctx, signoz.SearchRequest{Start: query.From, End: query.To, Limit: plan.Limit})
		var walk func(any, time.Time)
		walk = func(value any, inherited time.Time) {
			switch item := value.(type) {
			case map[string]any:
				at := linkedTimestamp(mappedValue(item, "timestamp"))
				if at.IsZero() {
					at = inherited
				}
				if body := mappedValue(item, "body"); body != nil {
					add(at, fmt.Sprint(body), mappedString(item, fields.Container), mappedString(item, fields.Pod))
					return
				}
				for _, child := range item {
					walk(child, at)
				}
			case []any:
				for _, child := range item {
					walk(child, inherited)
				}
			}
		}
		walk(result.Data, time.Time{})
		return rows, result.Truncated || invalidRows, err
	case "cloudwatchlogs":
		cfg := adapter.source.CloudWatchLogs
		cfg.FilterPattern = adapter.cloudPattern(query, pod)
		if cfg.FilterPattern == "" {
			return nil, false, errors.New("invalid_mapping")
		}
		api := adapter.cloudAPI
		var err error
		if api == nil {
			api, err = cloudwatchlogs.NewToolAPI(ctx, cfg.Region)
			if err != nil {
				return nil, false, err
			}
		}
		reader, err := cloudwatchlogs.NewReader(api, cloudwatchlogs.Scope{Region: cfg.Region, LogGroupName: cfg.LogGroupName, LogStreamPrefix: cfg.LogStreamPrefix, FilterPattern: cfg.FilterPattern}, adapter.scrubber)
		if err != nil {
			return nil, false, err
		}
		result, err := reader.Read(ctx, cloudwatchlogs.Request{Start: query.From, End: query.To, Limit: plan.Limit})
		for _, row := range result.Records {
			var object map[string]any
			if json.Unmarshal([]byte(row.Message), &object) != nil {
				invalidRows = true
				continue
			}
			matches := true
			for key, value := range filters {
				if mappedString(object, key) != value {
					matches = false
					break
				}
			}
			foundPod := mappedString(object, fields.Pod)
			if pod == "" && len(oneOf) == 0 && !strings.HasPrefix(foundPod, query.PodPrefix+"-") {
				matches = false
			}
			if matches {
				message := mappedValue(object, fields.Message)
				if message == nil {
					invalidRows = true
					continue
				}
				add(linkedTimestamp(row.Timestamp), fmt.Sprint(message), mappedString(object, fields.Container), foundPod)
			}
		}
		return rows, result.Truncated || invalidRows, err
	}
	return nil, false, errors.New("invalid_mapping")
}

func mappedString(object map[string]any, key string) string {
	value, _ := mappedValue(object, key).(string)
	return value
}

func mappedValue(object map[string]any, key string) any {
	if value, ok := object[key]; ok {
		return value
	}
	for _, container := range []string{"resources_string", "attributes_string", "scope_string"} {
		if attributes, ok := object[container].(map[string]any); ok {
			if value, exists := attributes[key]; exists {
				return value
			}
		}
	}
	parts := strings.SplitN(key, ".", 2)
	if len(parts) == 2 {
		if nested, ok := object[parts[0]].(map[string]any); ok {
			return mappedValue(nested, parts[1])
		}
	}
	return nil
}

func linkedTimestamp(value any) time.Time {
	if text, ok := value.(string); ok {
		if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return at
		}
		if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
			return linkedTimestamp(float64(parsed))
		}
	}
	if number, ok := value.(float64); ok {
		switch {
		case number > 1e17:
			return time.Unix(0, int64(number)).UTC()
		case number > 1e14:
			return time.UnixMicro(int64(number)).UTC()
		case number > 1e11:
			return time.UnixMilli(int64(number)).UTC()
		default:
			return time.Unix(int64(number), 0).UTC()
		}
	}
	return time.Time{}
}

func (adapter *breakLogAdapter) safe(value string) string {
	value = adapter.scrubber.Scrub(value)
	source := adapter.source
	for _, secret := range []string{source.Loki.BearerToken, source.Loki.Password, source.Elasticsearch.Password, source.Elasticsearch.APIKey, source.Graylog.APIToken, source.Graylog.Password, source.Splunk.Token, source.Splunk.Password, source.Signoz.APIKey} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}
