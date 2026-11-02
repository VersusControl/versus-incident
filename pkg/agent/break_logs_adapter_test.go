package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/signoz"
	awslogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/smithy-go"
)

func TestLinkedUpstreamErrorsRetainOnlySafeClass(t *testing.T) {
	for _, test := range []struct {
		err   error
		class string
	}{
		{context.DeadlineExceeded, "timeout"},
		{&smithy.GenericAPIError{Code: "AccessDeniedException", Message: "secret upstream body"}, "unauthorized"},
		{&smithy.GenericAPIError{Code: "ServiceUnavailableException", Message: "secret upstream body"}, "unavailable"},
		{&smithy.GenericAPIError{Code: "ResourceNotFoundException", Message: "secret upstream body"}, "not_found"},
		{&signoz.StatusError{StatusCode: 503}, "unavailable"},
		{errors.New("transport failure secret"), "unavailable"},
		{errors.New("invalid_mapping"), "invalid_mapping"},
	} {
		err := safeLinkedError(context.Background(), test.err)
		var typed *breaklogs.ReadError
		if !errors.As(err, &typed) || typed.Class != test.class || err.Error() != test.class {
			t.Fatalf("unsafe or incorrect upstream error: %T %v", err, err)
		}
	}
}

type linkedCanaryScrubber struct{}

func TestLinkedMultiPodSelectorsReadEveryIdentityInOneWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, kind := range []string{"loki", "elasticsearch", "signoz"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			var actual string
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				if kind == "loki" {
					actual = request.URL.Query().Get("query")
					streams := []any{}
					for i := 0; i < 4; i++ {
						streams = append(streams, map[string]any{"stream": map[string]string{"namespace": "shop", "pod": fmt.Sprintf("api-%d", i), "container": "app"}, "values": [][]string{{strconv.FormatInt(now.Add(-time.Minute).UnixNano(), 10), "LINKED_CANARY"}}})
					}
					json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": streams}})
					return
				}
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				raw, _ := json.Marshal(body)
				actual = string(raw)
				rows := []any{}
				for i := 0; i < 4; i++ {
					name := fmt.Sprintf("api-%d", i)
					if kind == "elasticsearch" {
						rows = append(rows, map[string]any{"_source": map[string]any{"@timestamp": now.Add(-time.Minute).Format(time.RFC3339Nano), "message": "LINKED_CANARY", "kubernetes": map[string]string{"namespace_name": "shop", "pod_name": name, "container_name": "app"}}})
					} else {
						rows = append(rows, map[string]any{"timestamp": now.Add(-time.Minute).Format(time.RFC3339Nano), "data": map[string]any{"body": "LINKED_CANARY", "resources_string": map[string]string{"k8s.namespace.name": "shop", "k8s.pod.name": name, "k8s.container.name": "app"}}})
					}
				}
				if kind == "elasticsearch" {
					json.NewEncoder(writer).Encode(map[string]any{"hits": map[string]any{"total": map[string]any{"value": 4, "relation": "eq"}, "hits": rows}})
				} else {
					json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"data": map[string]any{"results": []any{map[string]any{"queryName": "A", "rows": rows}}}}})
				}
			}))
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			source := config.AgentSourceConfig{Type: kind}
			source.Loki = config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api"}`, InsecureSkipVerify: true}
			source.Elasticsearch = config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, Index: "logs-*", AllowLoopback: true, InsecureSkipVerify: true}
			source.Signoz = config.AgentSignozSourceConfig{Address: server.URL, APIKey: "test-key", AllowLoopback: true, RootCAs: roots, Query: "environment = 'prod'"}
			adapter := &breakLogAdapter{source: source, fields: breaklogs.DefaultFields(kind), scrubber: linkedCanaryScrubber{}}
			query := breaklogs.LinkedQuery{Namespace: "shop", Pods: []string{"api-0", "api-1", "api-2", "api-3"}, From: now.Add(-5 * time.Minute), To: now, Limit: 2000}
			result, err := breaklogs.ReadWindow(t.Context(), adapter, query)
			if err != nil || calls != 1 || result.Truncated || len(result.Lines) != 4 || len(result.Complete) != 1 {
				t.Fatal("multi-Pod batch starved identities", calls, result, err)
			}
			for _, name := range query.Pods {
				if !strings.Contains(actual, name) {
					t.Fatal("selector omitted verified Pod", name, actual)
				}
			}
			for _, line := range result.Lines {
				if line.Text != "[redacted]" {
					t.Fatal("batch bypassed redaction")
				}
			}
		})
	}
}

func TestLinkedBatchedAdapterSharesWholeQueryBudget(t *testing.T) {
	now := time.Now().UTC()
	api := &linkedCloudAPI{t: t, at: now.Add(-time.Minute), saturated: true}
	adapter := &breakLogAdapter{source: config.AgentSourceConfig{Type: "cloudwatchlogs", CloudWatchLogs: config.AgentCloudWatchLogsSourceConfig{Region: "us-east-1", LogGroupName: "/configured/group", FilterPattern: "configured"}}, fields: breaklogs.DefaultFields("cloudwatchlogs"), scrubber: linkedCanaryScrubber{}, cloudAPI: api}
	query := breaklogs.LinkedQuery{Namespace: "shop", Pods: []string{"api-1", "api-2", "api-3", "api-4"}, From: now.Add(-5 * time.Minute), To: now, Limit: 2000}
	result, err := breaklogs.ReadWindow(t.Context(), adapter, query)
	if err != nil || api.calls != 3 || !result.Truncated || len(result.Complete) != 0 || len(result.Lines) > 6000 {
		t.Fatal("fallback exceeded whole-query budget or hid starvation", api.calls, result, err)
	}
}

func (linkedCanaryScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, "LINKED_CANARY", "[redacted]")
}

type linkedCloudAPI struct {
	t                *testing.T
	at               time.Time
	calls            int
	missingContainer bool
	saturated        bool
}

func (api *linkedCloudAPI) FilterLogEvents(ctx context.Context, input *awslogs.FilterLogEventsInput, _ ...func(*awslogs.Options)) (*awslogs.FilterLogEventsOutput, error) {
	api.calls++
	if input.LogGroupName == nil || *input.LogGroupName != "/configured/group" || input.FilterPattern == nil || *input.FilterPattern != "configured" || input.StartTime == nil || input.EndTime == nil {
		api.t.Fatal("lost CloudWatch scope/window")
	}
	message := `{"kubernetes":{"namespace_name":"shop","pod_name":"api-1","container_name":"app"},"log":"LINKED_CANARY"}`
	if api.missingContainer {
		message = strings.ReplaceAll(message, `"container_name":"app"`, `"container_name":null`)
	}
	stream := "configured-stream"
	at := api.at.UnixMilli()
	events := []types.FilteredLogEvent{{Timestamp: &at, Message: &message, LogStreamName: &stream}}
	if api.saturated {
		events = nil
		for i := 0; i < 50; i++ {
			events = append(events, types.FilteredLogEvent{Timestamp: &at, Message: &message, LogStreamName: &stream})
		}
	}
	return &awslogs.FilterLogEventsOutput{Events: events}, nil
}

func TestBreakLogAdaptersAllSixHistoricalScopedContracts(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	at := now.Add(-time.Minute)
	for _, kind := range []string{"loki", "elasticsearch", "cloudwatchlogs", "graylog", "splunk", "signoz"} {
		t.Run(kind, func(t *testing.T) {
			requests := 0
			actualQuery := ""
			var esBody map[string]any
			missingContainer := false
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if missingContainer {
					recorded := httptest.NewRecorder()
					original := writer
					writer = recorded
					defer func() {
						body := recorded.Body.String()
						for _, key := range []string{"container_name", "k8s.container.name", "kubernetes_container_name", "mapped_container"} {
							body = strings.ReplaceAll(body, `"`+key+`":"app"`, `"`+key+`":null`)
						}
						fmt.Fprint(original, body)
					}()
				}
				requests++
				switch kind {
				case "loki":
					actualQuery = request.URL.Query().Get("query")
					if request.URL.Query().Get("start") != strconv.FormatInt(now.Add(-15*time.Minute).UnixNano(), 10) || request.URL.Query().Get("end") != strconv.FormatInt(now.UnixNano(), 10) {
						t.Error("Loki window")
					}
					fmt.Fprintf(writer, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"namespace":"shop","pod":"api-1","container":"app"},"values":[["%d","LINKED_CANARY"]]}]}}`, at.UnixNano())
				case "elasticsearch":
					if request.URL.Path != "/logs-*/_search" {
						t.Error("ES index scope", request.URL.Path)
					}
					if json.NewDecoder(request.Body).Decode(&esBody) != nil {
						t.Error("ES body")
					}
					fmt.Fprintf(writer, `{"hits":{"total":{"value":1,"relation":"eq"},"hits":[{"_source":{"@timestamp":%q,"message":"LINKED_CANARY","kubernetes":{"namespace_name":"shop","pod_name":"api-1","container_name":"app"}}}]}}`, at.Format(time.RFC3339Nano))
				case "graylog":
					actualQuery = request.URL.Query().Get("query")
					if request.URL.Query().Get("filter") != "streams:configured-stream" || request.URL.Query().Get("to") != now.Format("2006-01-02T15:04:05.000Z") {
						t.Error("Graylog scope/window")
					}
					fmt.Fprintf(writer, `{"total_results":1,"messages":[{"message":{"_id":"native-message-id","timestamp":%q,"message":"LINKED_CANARY","kubernetes_namespace_name":"shop","kubernetes_pod_name":"api-1","kubernetes_container_name":"app"}}]}`, at.Format(time.RFC3339Nano))
				case "splunk":
					switch {
					case request.Method == http.MethodPost:
						if request.ParseForm() != nil {
							t.Error("Splunk form")
						}
						actualQuery = request.Form.Get("search")
						if request.Form.Get("earliest_time") != strconv.FormatInt(now.Add(-15*time.Minute).Unix(), 10) || request.Form.Get("latest_time") != strconv.FormatInt(now.Unix(), 10) {
							t.Error("Splunk window")
						}
						fmt.Fprint(writer, `{"sid":"safe-job"}`)
					case request.Method == http.MethodDelete:
						fmt.Fprint(writer, `{}`)
					case strings.Contains(request.URL.Path, "/results"):
						row := map[string]string{"index": "main", "_time": at.Format(time.RFC3339Nano), "_raw": "LINKED_CANARY", "namespace": "shop", "pod": "api-1", "mapped_container": "app"}
						projected := map[string]string{}
						for _, field := range request.URL.Query()["f"] {
							if value, ok := row[field]; ok {
								projected[field] = value
							}
						}
						json.NewEncoder(writer).Encode(map[string]any{"results": []any{projected}})
					default:
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
					}
				case "signoz":
					var body map[string]any
					if json.NewDecoder(request.Body).Decode(&body) != nil {
						t.Error("SigNoz body")
					}
					spec := body["compositeQuery"].(map[string]any)["queries"].([]any)[0].(map[string]any)["spec"].(map[string]any)
					actualQuery = spec["filter"].(map[string]any)["expression"].(string)
					fmt.Fprintf(writer, `{"status":"success","data":{"data":{"results":[{"queryName":"A","rows":[{"timestamp":%q,"data":{"body":"LINKED_CANARY","resources_string":{"k8s.namespace.name":"shop","k8s.pod.name":"api-1","k8s.container.name":"app"}}}]}]}}}`, at.Format(time.RFC3339Nano))
				default:
					t.Error("unexpected HTTP provider")
				}
			}))
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			source := config.AgentSourceConfig{Name: "configured", Type: kind, Enable: true}
			source.Loki = config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api"}`, InsecureSkipVerify: true}
			source.Elasticsearch = config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, AllowLoopback: true, InsecureSkipVerify: true, Index: "logs-*"}
			source.Graylog = config.AgentGraylogSourceConfig{Address: server.URL, Query: "environment:prod", StreamID: "configured-stream", InsecureSkipVerify: true}
			source.Splunk = config.AgentSplunkSourceConfig{Address: server.URL, Search: "search index=main AND sourcetype=api", InsecureSkipVerify: true}
			source.Signoz = config.AgentSignozSourceConfig{Address: server.URL, APIKey: "configured-secret-value", AllowLoopback: true, RootCAs: roots, Query: "environment = 'prod'"}
			source.CloudWatchLogs = config.AgentCloudWatchLogsSourceConfig{Region: "us-east-1", LogGroupName: "/configured/group", FilterPattern: "configured"}
			built := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")
			if len(built) != 1 {
				t.Fatal("source absent")
			}
			fields := breaklogs.Fields{}
			if kind == "splunk" {
				fields.Container = "mapped_container"
			}
			reader, err := built[0].Build(fields)
			if err != nil {
				t.Fatal(err)
			}
			adapter := reader.(*breakLogAdapter)
			cloud := &linkedCloudAPI{t: t, at: at}
			adapter.cloudAPI = cloud
			query := breaklogs.LinkedQuery{Namespace: "shop", PodPrefix: "api", Pods: []string{"api-1"}, From: now.Add(-15 * time.Minute), To: now, Limit: 500}
			preview := adapter.Preview(query)
			result, err := reader.Read(t.Context(), query)
			if err != nil || len(result.Lines) != 1 || result.Lines[0].Text != "[redacted]" || result.Lines[0].Pod != "api-1" || result.Lines[0].Container != "app" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if kind == "graylog" && result.Truncated {
				t.Fatal("native metadata truncated complete Graylog read")
			}
			if strings.Contains(preview, "configured-secret-value") || strings.Contains(fmt.Sprint(result), "LINKED_CANARY") {
				t.Fatal("secret escaped scrub")
			}
			if kind == "cloudwatchlogs" {
				if cloud.calls != 1 {
					t.Fatal("no real bounded AWS reader call")
				}
			} else if requests == 0 {
				t.Fatal("fake provider")
			}
			plan := adapter.plan(query, "api-1")
			if kind == "loki" || kind == "graylog" || kind == "splunk" || kind == "signoz" {
				if actualQuery != plan.Query.(string) {
					t.Fatalf("preview query=%q executed=%q", plan.Query, actualQuery)
				}
			}
			if kind == "elasticsearch" {
				expected := plan.Query.(map[string]any)["query"]
				encoded, _ := json.Marshal(expected)
				var decoded any
				json.Unmarshal(encoded, &decoded)
				if !reflect.DeepEqual(esBody["query"], decoded) {
					t.Errorf("ES preview mismatch: %v != %v", esBody["query"], decoded)
				}
			}
			query.Pods = nil
			result, err = reader.Read(t.Context(), query)
			if err != nil || len(result.Lines) != 1 {
				t.Fatalf("prefix result=%+v err=%v", result, err)
			}
			if kind == "elasticsearch" || kind == "signoz" || kind == "cloudwatchlogs" || kind == "graylog" || kind == "splunk" {
				missingContainer = true
				cloud.missingContainer = true
				result, err = reader.Read(t.Context(), query)
				if err != nil || len(result.Lines) != 0 || !result.Truncated {
					t.Fatalf("missing container invented identity: %+v err=%v", result, err)
				}
			}
		})
	}
}

func TestBreakLogSplunkJobProjectionWithoutContainerFilter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, mapping := range []struct {
		name   string
		fields breaklogs.Fields
	}{
		{name: "default", fields: breaklogs.Fields{Namespace: "namespace", Pod: "pod", Container: "container_name"}},
		{name: "custom", fields: breaklogs.Fields{Namespace: "k8s_ns", Pod: "k8s_pod", Container: "k8s_ctr"}},
		{name: "dotted", fields: breaklogs.Fields{Namespace: "k8s.namespace", Pod: "k8s.pod", Container: "k8s.container"}},
	} {
		t.Run(mapping.name, func(t *testing.T) {
			fields := mapping.fields
			var retained map[string]bool
			invalidIdentity := ""
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodPost:
					if err := request.ParseForm(); err != nil {
						t.Error(err)
					}
					search := request.Form.Get("search")
					if !strings.Contains(search, "index=main AND sourcetype=_json") || !strings.Contains(search, fields.Namespace+"=shop") || !strings.Contains(search, fields.Pod+"=api-") || strings.Contains(search, fields.Container+"=") {
						t.Errorf("unexpected native search: %s", search)
					}
					wantedFields := map[string]bool{
						"index": true, "_time": true, "_raw": true,
						"source": true, "sourcetype": true, "host": true, "service": true,
						"severity": true, "level": true, "environment": true,
						fields.Namespace: true, fields.Pod: true, fields.Container: true,
					}
					retained = map[string]bool{}
					for _, field := range request.Form["rf"] {
						if strings.Contains(field, ",") || !wantedFields[field] || retained[field] {
							t.Errorf("unexpected repeated projection field: %q", field)
							continue
						}
						retained[field] = true
					}
					if !reflect.DeepEqual(retained, wantedFields) {
						t.Errorf("job projection=%v want=%v", retained, wantedFields)
					}
					fmt.Fprint(writer, `{"sid":"projection-job"}`)
				case request.Method == http.MethodDelete:
					fmt.Fprint(writer, `{}`)
				case strings.HasSuffix(request.URL.Path, "/results"):
					rows := []any{}
					for position := 0; position < 5; position++ {
						row := map[string]any{"index": "main", "_time": now.Add(-time.Duration(position+1) * time.Second).Format(time.RFC3339Nano), "_raw": "LINKED_CANARY", fields.Namespace: "shop", fields.Pod: "api-1", fields.Container: "app"}
						if position == 4 && invalidIdentity != "" {
							row[invalidIdentity] = nil
							if invalidIdentity == fields.Pod {
								row[invalidIdentity] = "bad identity"
							}
						}
						projected := map[string]any{}
						for _, field := range request.URL.Query()["f"] {
							if value, exists := row[field]; exists && retained[field] {
								projected[field] = value
							}
						}
						rows = append(rows, projected)
					}
					json.NewEncoder(writer).Encode(map[string]any{"results": rows})
				default:
					fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
				}
			}))
			defer server.Close()
			source := config.AgentSourceConfig{Name: "configured", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: server.URL, Search: "search index=main AND sourcetype=_json"}}
			configured := fields
			if mapping.name == "default" {
				configured = breaklogs.Fields{}
			}
			reader, err := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")[0].Build(configured)
			if err != nil {
				t.Fatal(err)
			}
			for _, selection := range []string{"exact", "prefix"} {
				t.Run(selection, func(t *testing.T) {
					query := breaklogs.LinkedQuery{Namespace: "shop", PodPrefix: "api", From: now.Add(-15 * time.Minute), To: now, Limit: 50}
					if selection == "exact" {
						query.Pods = []string{"api-1"}
					}
					for _, invalid := range []string{"", fields.Pod, fields.Container} {
						invalidIdentity = invalid
						result, err := reader.Read(t.Context(), query)
						wanted := 5
						if invalid != "" {
							wanted = 4
						}
						if err != nil || len(result.Lines) != wanted || result.Truncated != (invalid != "") {
							t.Fatalf("invalid=%q lines=%d truncated=%v err=%v", invalid, len(result.Lines), result.Truncated, err)
						}
						for _, line := range result.Lines {
							if line.Pod != "api-1" || line.Container != "app" || line.Text != "[redacted]" {
								t.Fatalf("lost actual identity: %+v", line)
							}
						}
					}
				})
			}
		})
	}
}

func TestBreakLogSplunkNativeRawIdentityTypes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, field := range []string{"namespace", "pod", "container_name"} {
		for _, test := range []struct {
			name          string
			value         any
			missing       bool
			missingRaw    bool
			projectedOnly bool
			mapped        bool
		}{
			{name: "missing_projection", missing: true},
			{name: "missing_raw", missingRaw: true},
			{name: "projected_only", projectedOnly: true},
			{name: "mapped_projected_only", projectedOnly: true, mapped: true},
			{name: "null"},
			{name: "numeric", value: 42},
			{name: "boolean", value: true},
			{name: "object", value: map[string]any{"name": "app"}},
			{name: "array", value: []any{"app"}},
			{name: "empty", value: ""},
			{name: "oversized", value: strings.Repeat("a", 254)},
			{name: "valid", value: map[string]string{"namespace": "shop", "pod": "api-1", "container_name": "app"}[field]},
		} {
			t.Run(field+"/"+test.name, func(t *testing.T) {
				containerField := "container_name"
				if test.mapped {
					containerField = "mapped_container"
				}
				identityField := field
				if field == "container_name" {
					identityField = containerField
				}
				rows := []any{}
				for position, container := range []string{"app", "null", "42", "bad"} {
					raw := map[string]any{"namespace": "shop", "pod": "api-1", "container_name": container, "message": "LINKED_CANARY"}
					row := map[string]any{"index": "main", "_time": now.Add(-time.Minute).Format(time.RFC3339Nano), "namespace": "shop", "pod": "api-1", containerField: container}
					if position == 3 {
						if test.missing {
							delete(row, identityField)
						} else if test.missingRaw {
							delete(raw, field)
						} else if test.projectedOnly {
							raw = map[string]any{"message": "safe"}
						} else {
							raw[field] = test.value
							if text, ok := test.value.(string); ok {
								row[identityField] = text
								if field == "container_name" {
									container = text
								}
							} else {
								encoded, _ := json.Marshal(test.value)
								row[identityField] = string(encoded)
							}
						}
					}
					encoded, _ := json.Marshal(raw)
					row["_raw"] = string(encoded)
					rows = append(rows, row)
				}
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					switch {
					case request.Method == http.MethodPost:
						fmt.Fprint(writer, `{"sid":"native-types"}`)
					case request.Method == http.MethodDelete:
						fmt.Fprint(writer, `{}`)
					case strings.HasSuffix(request.URL.Path, "/results"):
						json.NewEncoder(writer).Encode(map[string]any{"results": rows})
					default:
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
					}
				}))
				defer server.Close()
				source := config.AgentSourceConfig{Name: "configured", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: server.URL, Search: "search index=main AND sourcetype=_json"}}
				reader, err := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")[0].Build(breaklogs.Fields{Container: containerField})
				if err != nil {
					t.Fatal(err)
				}
				for _, selection := range []string{"exact", "prefix"} {
					t.Run(selection, func(t *testing.T) {
						query := breaklogs.LinkedQuery{Namespace: "shop", PodPrefix: "api", From: now.Add(-15 * time.Minute), To: now, Limit: 50}
						if selection == "exact" {
							query.Pods = []string{"api-1"}
						}
						result, err := reader.Read(t.Context(), query)
						wanted := 3
						valid := test.name == "valid" || test.missingRaw || test.projectedOnly
						if valid {
							wanted = 4
						}
						if err != nil || len(result.Lines) != wanted || result.Truncated != !valid {
							t.Fatalf("native mixed identities: lines=%d truncated=%v err=%v", len(result.Lines), result.Truncated, err)
						}
						if strings.Contains(fmt.Sprint(result), "LINKED_CANARY") {
							t.Fatal("canary escaped scrub")
						}
						for position, line := range result.Lines {
							lastContainer := "bad"
							if field == "container_name" && test.name == "valid" {
								lastContainer = "app"
							}
							if line.Pod != "api-1" || line.Container != []string{"app", "null", "42", lastContainer}[position] {
								t.Fatalf("valid identity lost: %+v", line)
							}
							var raw map[string]any
							if err := json.Unmarshal([]byte(line.Text), &raw); err != nil {
								t.Fatalf("invalid scrubbed JSON: %v", err)
							}
							if position == 3 && test.projectedOnly {
								if len(raw) != 1 || raw["message"] != "safe" {
									t.Fatalf("projected identity changed raw message: %+v", raw)
								}
								continue
							}
							if (position != 3 || !test.missingRaw || field != "namespace") && raw["namespace"] != "shop" || (position != 3 || !test.missingRaw || field != "pod") && raw["pod"] != line.Pod || (position != 3 || !test.missingRaw || field != "container_name") && raw["container_name"] != line.Container || raw["message"] != "[redacted]" {
								t.Fatalf("scrubbed row lost identity or redaction: %+v", raw)
							}
						}
					})
				}
			})
		}
	}
}

func TestBreakLogAdaptersMixedInvalidIdentitiesRemainTruncated(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, kind := range []string{"graylog", "splunk"} {
		t.Run(kind, func(t *testing.T) {
			for _, identity := range []string{"pod", "container"} {
				for _, invalid := range []struct {
					name    string
					value   any
					missing bool
				}{
					{name: "missing", missing: true},
					{name: "null"},
					{name: "numeric", value: 42},
					{name: "boolean", value: true},
					{name: "malformed", value: "bad identity"},
				} {
					t.Run(identity+"/"+invalid.name, func(t *testing.T) {
						podField, containerField := "kubernetes_pod_name", "kubernetes_container_name"
						good := map[string]any{"timestamp": now.Add(-time.Minute).Format(time.RFC3339Nano), "message": "LINKED_CANARY", "kubernetes_namespace_name": "shop", podField: "api-1", containerField: "app"}
						if kind == "splunk" {
							podField, containerField = "pod", "mapped_container"
							good = map[string]any{"index": "main", "_time": now.Add(-time.Minute).Format(time.RFC3339Nano), "_raw": "LINKED_CANARY", "namespace": "shop", podField: "api-1", containerField: "app"}
						}
						bad := make(map[string]any, len(good))
						for key, value := range good {
							bad[key] = value
						}
						field := podField
						if identity == "container" {
							field = containerField
						}
						if invalid.missing {
							delete(bad, field)
						} else {
							bad[field] = invalid.value
						}
						server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
							if kind == "graylog" {
								json.NewEncoder(writer).Encode(map[string]any{"total_results": 2, "messages": []any{map[string]any{"message": good}, map[string]any{"message": bad}}})
								return
							}
							switch {
							case request.Method == http.MethodPost:
								fmt.Fprint(writer, `{"sid":"safe-job"}`)
							case request.Method == http.MethodDelete:
								fmt.Fprint(writer, `{}`)
							case strings.Contains(request.URL.Path, "/results"):
								json.NewEncoder(writer).Encode(map[string]any{"results": []any{good, bad}})
							default:
								fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
							}
						}))
						defer server.Close()
						source := config.AgentSourceConfig{Name: "configured", Type: kind, Enable: true}
						source.Graylog = config.AgentGraylogSourceConfig{Address: server.URL, Query: "environment:prod", StreamID: "configured-stream", InsecureSkipVerify: true}
						source.Splunk = config.AgentSplunkSourceConfig{Address: server.URL, Search: "search index=main AND sourcetype=api", InsecureSkipVerify: true}
						fields := breaklogs.Fields{}
						if kind == "splunk" {
							fields.Container = containerField
						}
						reader, err := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")[0].Build(fields)
						if err != nil {
							t.Fatal(err)
						}
						for _, selection := range []string{"exact", "prefix"} {
							t.Run(selection, func(t *testing.T) {
								query := breaklogs.LinkedQuery{Namespace: "shop", PodPrefix: "api", From: now.Add(-15 * time.Minute), To: now, Limit: 500}
								if selection == "exact" {
									query.Pods = []string{"api-1"}
									query.Container = "app"
								}
								result, err := reader.Read(t.Context(), query)
								if err != nil || !result.Truncated || len(result.Lines) != 1 || result.Lines[0].Pod != "api-1" || result.Lines[0].Container != "app" || result.Lines[0].Text != "[redacted]" {
									t.Fatalf("mixed identity result=%+v err=%v", result, err)
								}
							})
						}
					})
				}
			}
		})
	}
}

func TestBreakLogAdaptersAllSixRejectUnsafeReadsBeforeEgress(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []string{"loki", "elasticsearch", "cloudwatchlogs", "graylog", "splunk", "signoz"} {
		t.Run(kind, func(t *testing.T) {
			source := config.AgentSourceConfig{Name: "configured", Type: kind, Enable: true}
			source.Loki.Query = `{app="api"}`
			built := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")
			if len(built) != 1 {
				t.Fatal("source absent")
			}
			if _, err := built[0].Build(breaklogs.Fields{Namespace: `namespace" OR *`}); err == nil {
				t.Fatal("unsafe mapping accepted")
			}
			reader, err := built[0].Build(breaklogs.Fields{})
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name   string
				mutate func(*breaklogs.LinkedQuery)
			}{
				{"empty namespace", func(query *breaklogs.LinkedQuery) { query.Namespace = "" }},
				{"namespace injection", func(query *breaklogs.LinkedQuery) { query.Namespace = `shop" OR *` }},
				{"pod injection", func(query *breaklogs.LinkedQuery) { query.Pods = []string{`api" OR *`} }},
				{"container injection", func(query *breaklogs.LinkedQuery) { query.Container = `app" OR *` }},
				{"unscoped prefix", func(query *breaklogs.LinkedQuery) { query.Pods = nil }},
				{"prefix injection", func(query *breaklogs.LinkedQuery) { query.Pods = nil; query.PodPrefix = "api*" }},
				{"reversed window", func(query *breaklogs.LinkedQuery) { query.From = query.To.Add(time.Second) }},
				{"oversized window", func(query *breaklogs.LinkedQuery) { query.From = query.To.Add(-2*time.Hour - time.Second) }},
				{"oversized limit", func(query *breaklogs.LinkedQuery) { query.Limit = 2001 }},
			} {
				t.Run(test.name, func(t *testing.T) {
					query := breaklogs.LinkedQuery{Namespace: "shop", Pods: []string{"api-1"}, From: now.Add(-time.Minute), To: now, Limit: 50}
					test.mutate(&query)
					result, err := reader.Read(t.Context(), query)
					if err == nil || err.Error() != "invalid_mapping" || len(result.Lines) != 0 {
						t.Fatalf("unsafe read: result=%+v err=%v", result, err)
					}
				})
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			result, err := reader.Read(ctx, breaklogs.LinkedQuery{Namespace: "shop", Pods: []string{"api-1"}, From: now.Add(-time.Minute), To: now, Limit: 50})
			if err == nil || err.Error() != "timeout" || len(result.Lines) != 0 {
				t.Fatalf("canceled read: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestBreakLogAdaptersRejectMappingsBoundsAndCanceledReads(t *testing.T) {
	source := config.AgentSourceConfig{Name: "logs", Type: "loki", Enable: true, Loki: config.AgentLokiSourceConfig{Address: "http://127.0.0.1:1", Query: `{app="api"}`}}
	built := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")
	if _, err := built[0].Build(breaklogs.Fields{Pod: `pod" or *`}); err == nil {
		t.Fatal("mapping injection")
	}
	reader, _ := built[0].Build(breaklogs.Fields{})
	now := time.Now().UTC()
	query := breaklogs.LinkedQuery{Namespace: "shop", Pods: []string{"api-1"}, From: now.Add(-time.Minute), To: now, Limit: 50}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.Read(ctx, query); err == nil || err.Error() != "timeout" {
		t.Fatal("canceled", err)
	}
	query.Pods = []string{`api" OR *`}
	if _, err := reader.Read(t.Context(), query); err == nil || err.Error() != "invalid_mapping" {
		t.Fatal("value injection", err)
	}
	query.Pods = []string{"api-1"}
	query.Limit = 2001
	if _, err := reader.Read(t.Context(), query); err == nil {
		t.Fatal("bound")
	}
	if len(BuildBreakLogSources([]config.AgentSourceConfig{source, source}, linkedCanaryScrubber{}, "cluster")) != 0 {
		t.Fatal("duplicate source")
	}
	source.Enable = false
	if len(BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")) != 0 {
		t.Fatal("disabled source")
	}
}

func TestBreakLogElasticsearchSafeStatusClasses(t *testing.T) {
	for _, test := range []struct {
		status int
		class  string
	}{{http.StatusNotFound, "not_found"}, {http.StatusForbidden, "unauthorized"}, {http.StatusUnauthorized, "unauthorized"}} {
		t.Run(test.class+strconv.Itoa(test.status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(test.status)
				fmt.Fprint(writer, `{"error":{"type":"LINKED_CANARY"}}`)
			}))
			defer server.Close()
			source := config.AgentSourceConfig{Name: "logs", Type: "elasticsearch", Enable: true, Elasticsearch: config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, Index: "missing", AllowLoopback: true, InsecureSkipVerify: true}}
			reader, err := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")[0].Build(breaklogs.Fields{})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			result, err := reader.Read(t.Context(), breaklogs.LinkedQuery{Namespace: "shop", Pods: []string{"api-1"}, From: now.Add(-time.Minute), To: now, Limit: 50})
			if err == nil || err.Error() != test.class || len(result.Lines) != 0 {
				t.Fatalf("status=%d result=%+v err=%v", test.status, result, err)
			}
		})
	}
}

func TestBreakLogLokiRejectsPipelineAndCrossNamespaceRows(t *testing.T) {
	now := time.Now().UTC()
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		query := request.URL.Query().Get("query")
		if !strings.Contains(query, `namespace="shop"`) || !strings.Contains(query, `pod="api-1"`) {
			t.Errorf("unscoped query %q", query)
		}
		fmt.Fprintf(writer, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"namespace":"other","pod":"api-1","container":"app"},"values":[["%d","foreign"]]}]}}`, now.Add(-time.Second).UnixNano())
	}))
	defer server.Close()
	source := config.AgentSourceConfig{Name: "logs", Type: "loki", Enable: true, Loki: config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="x"} | json`, InsecureSkipVerify: true}}
	if _, err := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")[0].Build(breaklogs.Fields{}); err == nil || requests != 0 {
		t.Fatal("pipeline accepted or sent upstream", err)
	}
	source.Loki.Query = `{app="x"}`
	reader, err := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, "cluster")[0].Build(breaklogs.Fields{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.Read(t.Context(), breaklogs.LinkedQuery{Namespace: "shop", Pods: []string{"api-1"}, From: now.Add(-time.Minute), To: now, Limit: 50})
	if err != nil || len(result.Lines) != 0 || !result.Truncated || requests != 1 {
		t.Fatalf("cross-namespace result=%+v err=%v requests=%d", result, err, requests)
	}
}
