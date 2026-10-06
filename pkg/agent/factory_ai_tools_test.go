package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	cloudwatchlogtools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/cloudwatchlogs"
	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	elasticsearchtools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/elasticsearch"
	graylogtools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/graylog"
	lokitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/loki"
	signoztools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/signoz"
	splunktools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/splunk"
	versustools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/versus"
	"github.com/VersusControl/versus-incident/pkg/agent/ledger"
	"github.com/VersusControl/versus-incident/pkg/baseline"
	cloudwatchlogapp "github.com/VersusControl/versus-incident/pkg/cloudwatchlogs"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	graylogapp "github.com/VersusControl/versus-incident/pkg/graylog"
	lokiapp "github.com/VersusControl/versus-incident/pkg/loki"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	signozapp "github.com/VersusControl/versus-incident/pkg/signoz"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

type cloudWatchLogRoutingAPI struct{ calls int }

func (api *cloudWatchLogRoutingAPI) FilterLogEvents(context.Context, *cloudwatchlogs.FilterLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	api.calls++
	return &cloudwatchlogs.FilterLogEventsOutput{}, nil
}

type registrationHealth struct{}

func (registrationHealth) DetectionHealth(tenancy.OrgScope) versustools.DetectionHealthSnapshot {
	return versustools.DetectionHealthSnapshot{Observation: "unknown"}
}

type factoryBaselineProviderFunc func(context.Context, core.BaselineRequest) (core.BaselineResult, error)

func (provider factoryBaselineProviderFunc) DescribeBaselines(ctx context.Context, request core.BaselineRequest) (core.BaselineResult, error) {
	return provider(ctx, request)
}

func TestBuildAnalyzeToolsPreservesBaseOrder(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	tools := buildAnalyzeTools(store, tenancy.DefaultOrgScope(), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil)
	want := []string{"get_incident", "get_pattern", "get_service", "get_system_overview", "list_services", "list_capabilities", "get_alert_decision", "search_incidents", "list_patterns", "list_analyses"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	seen := make(map[string]bool, len(tools))
	for index, name := range want {
		if tools[index].Name() != name {
			t.Fatalf("tool[%d] = %q, want %q", index, tools[index].Name(), name)
		}
		if seen[name] {
			t.Fatalf("duplicate tool name %q", name)
		}
		seen[name] = true
	}
}

func TestBuildAnalyzeToolsRegistersAllDiscoveryTools(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	tools := buildAnalyzeTools(store, tenancy.DefaultOrgScope(), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, registrationHealth{})
	want := []string{"get_incident", "get_pattern", "get_service", "get_system_overview", "list_services", "get_detection_health", "list_capabilities", "get_alert_decision", "search_incidents", "list_patterns", "list_analyses"}

	registered := make(map[string]bool, len(tools))
	for _, tool := range tools {
		registered[tool.Name()] = true
	}
	for _, name := range want {
		if !registered[name] {
			t.Fatalf("discovery tool %q is not registered", name)
		}
	}
}

func TestBuildAnalyzeToolsRuntimeCatalogContract(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	for _, tool := range buildAnalyzeTools(store, tenancy.DefaultOrgScope(), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, registrationHealth{}) {
		if _, ok := aitools.Lookup(tool.Name()); !ok {
			t.Errorf("runtime tool %q is absent from availability catalog", tool.Name())
		}
	}
}

func TestProposalToolIsAddedOnlyToChatRuntime(t *testing.T) {
	provider := storage.NewMemory()
	service, err := act.NewService(provider, "org-a", ledger.NewBlobWriter(provider, "org-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	analyzeTools := []core.Tool{}
	chatTools := chatRuntimeToolCatalog(analyzeTools, service)
	if len(analyzeTools) != 0 || len(chatTools) != 1 || chatTools[0].Name() != "propose_action" {
		t.Fatalf("analyze tools=%v chat tools=%v", analyzeTools, chatTools)
	}
	if got := chatRuntimeToolCatalog(analyzeTools, nil); len(got) != 0 {
		t.Fatalf("chat tool catalog without an actor = %v", got)
	}
}

func TestKubernetesActionsRejectPodServiceAccountReuse(t *testing.T) {
	_, err := buildKubernetesActionAdapters(config.KubernetesToolConfig{
		Endpoint: "https://cluster.example",
		Actions:  config.KubernetesActionsToolConfig{Enable: true, Auth: config.KubernetesAuthConfig{Mode: "in_cluster"}},
	})
	if err == nil || !strings.Contains(err.Error(), "separate actor credential") {
		t.Fatalf("in-cluster actor auth error=%v", err)
	}
}

func TestBuildSigNozToolSourcesUsesOnlyOSSLogType(t *testing.T) {
	sources, errs := buildSigNozToolSources([]config.AgentSourceConfig{
		{Name: "logs", Type: "signoz", Enable: true, Signoz: config.AgentSignozSourceConfig{Address: "https://signoz.example", APIKey: "test-api-key"}},
		{Name: "metrics", Type: "signoz_metrics", Enable: true, Options: map[string]any{"address": "https://signoz.example", "api_key": "key"}},
		{Name: "disabled", Type: "signoz", Enable: false},
	}, nil)
	if len(errs) != 0 || len(sources) != 1 || sources[0].Name != "logs" {
		t.Fatalf("sources=%v errors=%v", sources, errs)
	}
}

func TestLokiAndSigNozLogCapabilitiesRouteForChatAndAnalyze(t *testing.T) {
	cloudAPI := &cloudWatchLogRoutingAPI{}
	cloudReader, err := cloudwatchlogapp.NewReader(cloudAPI, cloudwatchlogapp.Scope{Region: "us-east-1", LogGroupName: "/prod"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lokiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("unexpected Loki path: %s", request.URL.Path)
		}
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer lokiServer.Close()
	lokiSources, errs := buildLokiToolSources([]config.AgentSourceConfig{
		{Name: "loki-a", Type: "loki", Enable: true, Loki: config.AgentLokiSourceConfig{Address: lokiServer.URL, Query: `{service="api"}`}},
		{Name: "off", Type: "loki", Enable: false},
	}, nil)
	if len(errs) != 0 || len(lokiSources) != 1 {
		t.Fatalf("Loki sources=%v errors=%v", lokiSources, errs)
	}
	graylogServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("filter") != "streams:stream-1" {
			t.Errorf("Graylog request missing stream scope: %s", request.URL.RawQuery)
		}
		fmt.Fprint(writer, `{"total_results":0,"messages":[]}`)
	}))
	defer graylogServer.Close()
	graylogSources, graylogErrs := buildGraylogToolSources([]config.AgentSourceConfig{
		{Name: "graylog-a", Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: graylogServer.URL, Query: "service:api", StreamID: "stream-1"}},
		{Name: "graylog-unsupported", Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: graylogServer.URL, Query: "service:api OR service:worker"}},
		{Name: "graylog-off", Type: "graylog", Enable: false},
	}, nil)
	if len(graylogSources) != 1 || len(graylogErrs) != 1 {
		t.Fatalf("Graylog sources=%v errors=%v", graylogSources, graylogErrs)
	}
	splunkServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost:
			fmt.Fprint(writer, `{"sid":"123.456"}`)
		case request.Method == http.MethodDelete:
			writer.WriteHeader(http.StatusOK)
		case strings.HasSuffix(request.URL.Path, "/results"):
			fmt.Fprint(writer, `{"results":[]}`)
		default:
			fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
		}
	}))
	defer splunkServer.Close()
	splunkSources, splunkErrs := buildSplunkToolSources([]config.AgentSourceConfig{
		{Name: "splunk-a", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: splunkServer.URL, Search: "index=main"}},
		{Name: "splunk-unsupported", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: splunkServer.URL, Search: "index=main | stats count"}},
		{Name: "splunk-off", Type: "splunk", Enable: false},
	}, nil)
	if len(splunkSources) != 1 || len(splunkErrs) != 1 {
		t.Fatalf("Splunk sources=%v errors=%v", splunkSources, splunkErrs)
	}
	signozServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"data":{"data":{"results":[]}}}`)
	}))
	defer signozServer.Close()
	signozService, err := signozapp.NewService(signozapp.Config{Address: signozServer.URL, APIKey: "test-api-key", AllowLoopback: true}, signozapp.ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	runtime := combineLogTools(signoztools.New([]signoztools.Source{{Name: "signoz-a", Kind: signozapp.SignalLogs, Service: signozService}}), lokitools.New(lokiSources), graylogtools.New(graylogSources), splunktools.New(splunkSources), cloudwatchlogtools.New([]cloudwatchlogtools.Source{{Name: "cloud-a", Reader: cloudReader}}))
	if len(runtime) != 2 {
		t.Fatalf("duplicate log tools: %v", toolNamesForTest(runtime))
	}
	for _, candidate := range runtime {
		want := map[string]string{
			"discover_log_fields": "Discover bounded log fields within the selected source's supported scope; available arguments depend on the source.",
			"read_log_records":    "Read bounded log records from the selected source; available filters depend on the source.",
		}[candidate.Name()]
		if candidate.Description() != want {
			t.Errorf("%s description = %q, want %q", candidate.Name(), candidate.Description(), want)
		}
	}
	var read core.Tool
	for _, candidate := range runtime {
		if candidate.Name() == "read_log_records" {
			read = candidate
		}
	}
	if read == nil || !slices.Equal(read.(core.SourceRoutedTool).SourceNames(), []string{"cloud-a", "graylog-a", "loki-a", "signoz-a", "splunk-a"}) {
		t.Fatalf("route = %v", read)
	}
	if required, ok := read.ArgsSchema()["required"].([]string); ok && slices.Contains(required, "service") {
		t.Fatalf("Loki label-only reads blocked by merged schema: %v", required)
	}
	for _, candidate := range runtime {
		properties := candidate.ArgsSchema()["properties"].(map[string]any)
		offset := properties["offset"].(map[string]any)
		wantMaximum := any(lokiapp.MaximumOffset)
		if candidate.Name() == "read_log_records" {
			wantMaximum = signozapp.MaximumOffset
		}
		if offset["minimum"] != 0 || offset["maximum"] != wantMaximum || offset["description"] != "Interpretation and valid range depend on the selected source." {
			t.Errorf("%s merged offset = %v", candidate.Name(), offset)
		}
		search := properties["search"].(map[string]any)
		if search["description"] != "Interpretation and valid range depend on the selected source." {
			t.Errorf("%s merged search = %v", candidate.Name(), search)
		}
	}
	snapshot := aitools.BindRuntimeCapabilities(aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{"logs": {Configured: true}}}, runtime)
	if got := snapshot.Capabilities["read_log_records"].Count; got != 5 {
		t.Fatalf("source count = %d", got)
	}
	manager := aitools.NewManager(storage.NewMemory())
	allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	for _, kind := range []aitools.AgentKind{aitools.AgentAnalyze, aitools.AgentChat} {
		filtered, err := manager.Filter(tenancy.DefaultOrgScope(), kind, runtime, snapshot)
		if err != nil || len(filtered) != 2 {
			t.Fatalf("%s: %v, %v", kind, toolNamesForTest(filtered), err)
		}
		for _, candidate := range filtered {
			if candidate.Name() == "read_log_records" {
				read = candidate
			}
		}
		if read.(core.ContextAuthorizedTool).AuthorizedTool(context.Background()) != nil {
			t.Fatal("catalog permission bypass")
		}
		for _, source := range []string{"cloud-a", "graylog-a", "loki-a", "signoz-a", "splunk-a"} {
			arguments := `{"source":"` + source + `","service":"api"}`
			switch source {
			case "graylog-a":
				arguments = `{"source":"graylog-a","filters":{"service":"api"}}`
			case "splunk-a":
				arguments = `{"source":"splunk-a","filters":{"host":"node"}}`
			case "cloud-a":
				arguments = `{"source":"cloud-a"}`
			}
			result, err := read.Invoke(allowed, json.RawMessage(arguments))
			if err != nil || result.Data["source"] != source {
				t.Fatalf("%s: %+v, %v", source, result, err)
			}
		}
		if cloudAPI.calls != 1 {
			t.Fatalf("CloudWatch calls after %s = %d, want 1", kind, cloudAPI.calls)
		}
		cloudAPI.calls = 0
		for _, args := range []string{`{"service":"api"}`, `{"source":"absent","service":"api"}`, `{"source":"signoz-a","labels":{"service":"api"}}`, `{"source":"loki-a","severity":"critical"}`, `{"source":"graylog-a","labels":{"service":"api"}}`, `{"source":"splunk-a","filters":{"index":"other"}}`} {
			if _, err := read.Invoke(allowed, json.RawMessage(args)); err == nil {
				t.Fatalf("source fallthrough: %s", args)
			}
		}
		for _, args := range []string{`{"source":"graylog-a","offset":100}`, `{"source":"graylog-a","search":"error*"}`} {
			if _, err := read.Invoke(allowed, json.RawMessage(args)); err == nil {
				t.Fatalf("Graylog accepted invalid provider argument: %s", args)
			}
		}
	}
}

func TestSplunkSourceNamesAndUnsupportedScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("source construction must not contact Splunk")
	}))
	defer server.Close()
	secret := "private-splunk-token"
	sources, errs := buildSplunkToolSources([]config.AgentSourceConfig{
		{Name: "safe-source", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main", Token: secret}},
		{Name: "admin-ops", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main", Owner: "admin", App: "ops"}},
		{Name: "private-splunk-token", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}},
		{Name: "invalid-scope", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main | stats count"}},
		{Name: "disabled", Type: "splunk", Enable: false},
	}, nil)
	if len(sources) != 2 || sources[0].Name != "admin-ops" || sources[1].Name != "safe-source" || len(errs) != 2 {
		t.Fatalf("sources=%v errs=%v", sources, errs)
	}
	for _, err := range errs {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("secret in warning: %v", err)
		}
	}
}

func TestUnsupportedSplunkToolScopePreservesStandingSource(t *testing.T) {
	configured := config.AgentSourceConfig{
		Name: "standing", Type: "splunk", Enable: true,
		Splunk: config.AgentSplunkSourceConfig{Address: "http://localhost:8089", Search: "index=main | stats count"},
	}
	standing, buildErrs := BuildSources(config.AgentConfig{Sources: []config.AgentSourceConfig{configured}})
	tools, toolErrs := buildSplunkToolSources([]config.AgentSourceConfig{configured}, nil)
	if len(buildErrs) != 0 || len(standing) != 1 || standing[0].Name() != "splunk:standing" || len(tools) != 0 || len(toolErrs) != 1 {
		t.Fatalf("standing=%v buildErrs=%v tools=%v toolErrs=%v", standing, buildErrs, tools, toolErrs)
	}
}

func TestAmbiguousLogSourceOmitsCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	signozService, err := signozapp.NewService(signozapp.Config{Address: server.URL, APIKey: "test-api-key", AllowLoopback: true}, signozapp.ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	lokiService, err := lokiapp.NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api"}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	combined := combineLogTools(signoztools.New([]signoztools.Source{{Name: "duplicate", Kind: signozapp.SignalLogs, Service: signozService}}), lokitools.New([]lokitools.Source{{Name: "duplicate", Service: lokiService}}))
	if len(combined) != 0 {
		t.Fatalf("ambiguous source exposed capability: %v", toolNamesForTest(combined))
	}
	graylogService, err := graylogapp.NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	combined = combineLogTools(lokitools.New([]lokitools.Source{{Name: "duplicate", Service: lokiService}}), graylogtools.New([]graylogtools.Source{{Name: "duplicate", Service: graylogService}}))
	if len(combined) != 0 {
		t.Fatalf("Graylog duplicate exposed capability: %v", toolNamesForTest(combined))
	}
	graylogSources, errs := buildGraylogToolSources([]config.AgentSourceConfig{
		{Name: "repeated", Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: server.URL}},
		{Name: "repeated", Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: server.URL}},
	}, nil)
	if len(graylogSources) != 0 || len(errs) == 0 {
		t.Fatalf("duplicate Graylog source remained active: %v, %v", graylogSources, errs)
	}
}

type nameRedactor struct{}

func (nameRedactor) Scrub(value string) string {
	return strings.ReplaceAll(value, "sensitive", "[redacted]")
}

func TestGraylogToolSourceNamesStaySafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"total_results":1,"messages":[{"message":{"message":"ok"}}]}`)
	}))
	defer server.Close()
	configFor := func(name string) config.AgentSourceConfig {
		return config.AgentSourceConfig{Name: name, Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: server.URL, APIToken: "token123"}}
	}
	sources, errs := buildGraylogToolSources([]config.AgentSourceConfig{
		configFor("source-token123"), configFor("sensitive-name"),
		configFor("source with spaces"), configFor("graylog-prod_1"),
	}, nameRedactor{})
	if len(sources) != 1 || sources[0].Name != "graylog-prod_1" || len(errs) != 3 {
		t.Fatalf("sources=%v errors=%v", sources, errs)
	}
	for _, err := range errs {
		if strings.Contains(err.Error(), "token123") || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), strings.Repeat("a", 81)) {
			t.Fatalf("source name leaked in error: %v", err)
		}
	}
	tools := graylogtools.New(sources)
	allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	for _, tool := range tools {
		encoded, err := json.Marshal(tool.ArgsSchema())
		if err != nil || strings.Contains(string(encoded), "token123") || strings.Contains(string(encoded), "sensitive") || strings.Contains(string(encoded), strings.Repeat("a", 81)) {
			t.Fatalf("unsafe schema: %s, %v", encoded, err)
		}
		result, err := tool.Invoke(allowed, json.RawMessage(`{"source":"graylog-prod_1"}`))
		if err != nil || result.Data["source"] != "graylog-prod_1" {
			t.Fatalf("valid source result = %+v, %v", result, err)
		}
	}
}

func TestGraylogToolSourceNamesRejectOtherSourceCredentialsAndFragments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"total_results":0,"messages":[]}`)
	}))
	defer server.Close()
	graylog := func(name, token string) config.AgentSourceConfig {
		return config.AgentSourceConfig{Name: name, Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: server.URL, APIToken: token}}
	}
	secrets := []string{
		"otherGraylogToken123", "lokiBearerToken123", "signozApiKey123",
		"elasticApiKey123", "prometheusApiKey123", "tempoAuthorization123",
	}
	sources, errs := buildGraylogToolSources([]config.AgentSourceConfig{
		graylog(secrets[0], "own-token"),
		graylog("other-graylog", secrets[0]),
		graylog("raylogToken1", "ownGraylogToken123"),
		graylog("loki-"+secrets[1], ""),
		graylog("signoz-"+secrets[2], ""),
		graylog("elastic-"+secrets[3], ""),
		graylog("prom-"+secrets[4], ""),
		graylog("tempo-"+secrets[5], ""),
		graylog("standing", ""),
		graylog("api", ""),
		{Type: "loki", Loki: config.AgentLokiSourceConfig{BearerToken: secrets[1]}},
		{Type: "signoz", Signoz: config.AgentSignozSourceConfig{APIKey: secrets[2]}},
		{Type: "elasticsearch", Elasticsearch: config.AgentElasticsearchSourceConfig{APIKey: secrets[3]}},
		{Type: "prometheus", Options: map[string]interface{}{"auth": map[string]interface{}{"api_key": secrets[4]}}},
		{Type: "traces", Options: map[string]interface{}{"headers": map[string]interface{}{"Authorization": secrets[5]}}},
	}, nil)
	if len(sources) != 3 || sources[0].Name != "api" || sources[1].Name != "other-graylog" || sources[2].Name != "standing" || len(errs) != 7 {
		t.Fatalf("accepted names or error count unexpected: %d, %d", len(sources), len(errs))
	}
	for _, constructionErr := range errs {
		for _, secret := range secrets {
			if strings.Contains(constructionErr.Error(), secret) || strings.Contains(constructionErr.Error(), "raylogToken1") {
				t.Fatal("construction error exposed credential material")
			}
		}
	}
	allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	for _, tool := range graylogtools.New(sources) {
		schema, err := json.Marshal(tool.ArgsSchema())
		if err != nil {
			t.Fatal(err)
		}
		result, err := tool.Invoke(allowed, json.RawMessage(`{"source":"standing"}`))
		if err != nil {
			t.Fatal(err)
		}
		encodedResult, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(schema), secret) || strings.Contains(string(encodedResult), secret) || strings.Contains(string(schema), "raylogToken1") || strings.Contains(string(encodedResult), "raylogToken1") {
				t.Fatal("tool schema or result exposed credential material")
			}
		}
	}
}

func TestGraylogToolSourceNamesBoundOptionCredentialScan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"total_results":0,"messages":[]}`)
	}))
	defer server.Close()
	graylog := func(name string) config.AgentSourceConfig {
		return config.AgentSourceConfig{Name: name, Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: server.URL}}
	}
	cycle := map[string]interface{}{}
	cycle["nested"] = []interface{}{cycle}
	deep := interface{}("deepSecret123")
	for range 9 {
		deep = []interface{}{deep}
	}
	wide := make([]interface{}, 1025)
	for index := range wide {
		wide[index] = index
	}
	for _, test := range []struct {
		name    string
		options map[string]interface{}
	}{
		{"cycle", cycle},
		{"depth", map[string]interface{}{"api_key": deep}},
		{"work", map[string]interface{}{"items": wide}},
		{"unsupported", map[string]interface{}{"data": make(chan int)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sources, errs := buildGraylogToolSources([]config.AgentSourceConfig{graylog("safe-source"), {Type: "other", Options: test.options}}, nil)
			if len(sources) != 0 || len(errs) == 0 || len(graylogtools.New(sources)) != 0 {
				t.Fatalf("unsafe options registered Graylog tools: %d sources, %d errors", len(sources), len(errs))
			}
		})
	}
	const fragment = "FragmentXYZ"
	const nestedSecret = "nestedCredential123"
	sources, errs := buildGraylogToolSources([]config.AgentSourceConfig{
		graylog("prod-" + fragment), graylog(nestedSecret), graylog("abc-safe"), graylog("safe-source"),
		{Type: "other", Enable: false, Options: map[string]interface{}{
			"api_key":  "prefix" + fragment + "suffix",
			"password": []interface{}{[]interface{}{nestedSecret}},
			"other":    "xxabcxx",
		}},
	}, nil)
	if len(sources) != 2 || sources[0].Name != "abc-safe" || sources[1].Name != "safe-source" || len(errs) != 2 {
		t.Fatalf("credential name filtering: %d sources, %d errors", len(sources), len(errs))
	}
	allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	for _, tool := range graylogtools.New(sources) {
		schema, err := json.Marshal(tool.ArgsSchema())
		if err != nil {
			t.Fatal(err)
		}
		result, err := tool.Invoke(allowed, json.RawMessage(`{"source":"safe-source"}`))
		if err != nil {
			t.Fatal(err)
		}
		encodedResult, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{fragment, nestedSecret, "prod-" + fragment, "prefix" + fragment + "suffix"} {
			if strings.Contains(string(schema), forbidden) || strings.Contains(string(encodedResult), forbidden) {
				t.Fatal("rejected name exposed in tool schema or result")
			}
		}
	}
	for _, constructionErr := range errs {
		if strings.Contains(constructionErr.Error(), fragment) || strings.Contains(constructionErr.Error(), nestedSecret) {
			t.Fatal("construction error exposed credential material")
		}
	}
}

func TestGraylogToolSourceNamesGlobalWorkBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"total_results":0,"messages":[]}`)
	}))
	defer server.Close()
	const secret = "privateToken123"
	graylog := config.AgentSourceConfig{Name: "safe-source", Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: server.URL, APIToken: secret}}
	other := config.AgentSourceConfig{Name: "ordinary", Type: "loki", Enable: true, Loki: config.AgentLokiSourceConfig{Address: server.URL, Query: `{service="api"}`}}

	tooManySources := append([]config.AgentSourceConfig{graylog, other}, make([]config.AgentSourceConfig, 127)...)
	longName := []config.AgentSourceConfig{graylog, {Name: strings.Repeat("a", 81), Type: "graylog", Enable: true, Graylog: graylog.Graylog}, other}
	oversizedName := []config.AgentSourceConfig{graylog, {Name: strings.Repeat(" ", 2<<20) + "safe-source", Type: "graylog", Enable: true, Graylog: graylog.Graylog}, other}
	largeCredential := []config.AgentSourceConfig{graylog, {Type: "other", Options: map[string]interface{}{"api_key": strings.Repeat("x", 64*1024) + secret}}, other}
	longOptionKey := []config.AgentSourceConfig{graylog, {Type: "other", Options: map[string]interface{}{strings.Repeat("x", 2<<20): "value"}}, other}
	largeOptionScalar := []config.AgentSourceConfig{graylog, {Type: "other", Options: map[string]interface{}{"label": strings.Repeat("x", 2<<20) + secret}}, other}
	cumulativeOptionScalars := []config.AgentSourceConfig{graylog, {Type: "other", Options: map[string]interface{}{"labels": []string{strings.Repeat("x", 40*1024), strings.Repeat("y", 40*1024)}}}, other}
	wideOptions := make([]config.AgentSourceConfig, 8)
	wideOptions[0] = graylog
	for index := 1; index < len(wideOptions); index++ {
		wideOptions[index] = config.AgentSourceConfig{Type: "other", Enable: false, Options: map[string]interface{}{"items": make([]interface{}, 700)}}
	}
	tooManyNames := make([]config.AgentSourceConfig, 65)
	for index := range tooManyNames {
		tooManyNames[index] = graylog
		tooManyNames[index].Name = fmt.Sprintf("graylog-%d", index)
	}
	for _, test := range []struct {
		name    string
		sources []config.AgentSourceConfig
	}{
		{"source count", tooManySources},
		{"name length", longName},
		{"raw source name", oversizedName},
		{"credential bytes", largeCredential},
		{"option key bytes", longOptionKey},
		{"option scalar bytes", largeOptionScalar},
		{"cumulative option scalars", cumulativeOptionScalars},
		{"cumulative option nodes", wideOptions},
		{"eligible names", tooManyNames},
	} {
		t.Run(test.name, func(t *testing.T) {
			sources, errs := buildGraylogToolSources(test.sources, nil)
			if len(sources) != 0 || len(graylogtools.New(sources)) != 0 || len(errs) != 1 {
				t.Fatalf("over-budget Graylog registration: %d sources, %d errors", len(sources), len(errs))
			}
			if errs[0].Error() != "agent: Graylog tool source configuration exceeds safety limits" {
				t.Fatal("budget error exposed configuration details")
			}
		})
	}

	valid, errs := buildGraylogToolSources([]config.AgentSourceConfig{graylog, other}, nil)
	if len(valid) != 1 || len(errs) != 0 {
		t.Fatalf("ordinary Graylog source rejected: %d sources, %d errors", len(valid), len(errs))
	}
	lokiSources, lokiErrs := buildLokiToolSources(tooManySources, nil)
	if len(lokiSources) != 1 || len(lokiErrs) != 0 || len(lokitools.New(lokiSources)) == 0 {
		t.Fatalf("unrelated Loki registration changed: %d sources, %d errors", len(lokiSources), len(lokiErrs))
	}
	allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	for _, tool := range graylogtools.New(valid) {
		schema, err := json.Marshal(tool.ArgsSchema())
		if err != nil || strings.Contains(string(schema), secret) {
			t.Fatalf("Graylog schema exposed credential: %v", err)
		}
		result, err := tool.Invoke(allowed, json.RawMessage(`{"source":"safe-source"}`))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(result)
		if err != nil || strings.Contains(string(encoded), secret) {
			t.Fatalf("Graylog result exposed credential: %v", err)
		}
	}
}

func TestToolRegistrationFiltersChatAndAnalyzeIndependently(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	scope := tenancy.DefaultOrgScope()
	runtime := buildAnalyzeTools(store, scope, newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil)
	manager := aitools.NewManager(store)
	if _, err := manager.SetEnabled(scope, aitools.AgentChat, "get_incident", false); err != nil {
		t.Fatal(err)
	}
	chat, err := manager.Filter(scope, aitools.AgentChat, runtime, aitools.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	analyze, err := manager.Filter(scope, aitools.AgentAnalyze, runtime, aitools.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	registered := func(tools []core.Tool, name string) bool {
		for _, tool := range tools {
			if tool.Name() == name {
				return true
			}
		}
		return false
	}
	if registered(chat, "get_incident") || !registered(analyze, "get_incident") {
		t.Fatalf("independent filtering failed: chat=%v analyze=%v", registered(chat, "get_incident"), registered(analyze, "get_incident"))
	}
}

func TestBaselineToolAvailabilityForChatAndAnalyze(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	scope := tenancy.DefaultOrgScope()
	provider := baseline.NewManager(baseline.Surface{Family: "logs", SourceType: "catalog", Provider: newLogBaselineProvider(catalog, scope, 5, store)})
	runtime := []core.Tool{commontools.DescribeBaseline{Provider: provider, OrgID: scope.Write}}
	snapshot := aitools.BindRuntimeCapabilities(aitools.Snapshot{}, runtime)
	manager := aitools.NewManager(store)
	registered := func(agent aitools.AgentKind) bool {
		filtered, err := manager.Filter(scope, agent, runtime, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return len(filtered) == 1 && filtered[0].Name() == "describe_baseline"
	}
	if !registered(aitools.AgentChat) || !registered(aitools.AgentAnalyze) {
		t.Fatal("baseline tool is not available to both agents")
	}
	if _, err := manager.SetToolsetEnabled(scope, aitools.AgentChat, "describe_baseline", false); err != nil {
		t.Fatal(err)
	}
	if registered(aitools.AgentChat) || !registered(aitools.AgentAnalyze) {
		t.Fatal("baseline toolset policy did not remain agent-specific")
	}
	if got := newLogBaselineProvider(nil, scope, 5, store); got != nil {
		t.Fatalf("nil catalog constructed provider %#v", got)
	}
}

func TestBuildBaselineToolSupportsExtensionOnlyRegistration(t *testing.T) {
	scope := tenancy.NewOrgScope("acme")
	extension := baseline.Surface{Family: "metrics", SourceType: "intelligence", Provider: factoryBaselineProviderFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{Availability: core.HealthReady}, nil
	})}
	tool := buildBaselineTool(nil, []baseline.Surface{extension}, scope)
	if tool == nil || tool.Name() != "describe_baseline" {
		t.Fatalf("extension-only tool = %#v", tool)
	}
	runtime := []core.Tool{tool}
	snapshot := aitools.BindRuntimeCapabilities(aitools.Snapshot{}, runtime)
	manager := aitools.NewManager(storage.NewMemory())
	for _, agentKind := range []aitools.AgentKind{aitools.AgentChat, aitools.AgentAnalyze} {
		filtered, err := manager.Filter(scope, agentKind, runtime, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if len(filtered) != 1 || filtered[0].Name() != "describe_baseline" {
			t.Fatalf("%s tools = %#v", agentKind, filtered)
		}
	}
	if got := buildBaselineTool(nil, nil, scope); got != nil {
		t.Fatalf("no providers constructed tool %#v", got)
	}
}

func TestBuildAIsConstructsBaselineToolFromExtensionWithoutCatalog(t *testing.T) {
	const contributor = "factory-extension-only-test"
	RegisterBaselineProviderContributor(contributor, func(tenancy.OrgScope, []config.AgentSourceConfig) ([]baseline.Surface, []error) {
		return []baseline.Surface{{Family: "metrics", SourceType: "intelligence", Provider: factoryBaselineProviderFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
			return core.BaselineResult{Availability: core.HealthReady}, nil
		})}}, nil
	})
	t.Cleanup(func() { RegisterBaselineProviderContributor(contributor, nil) })

	scope := tenancy.NewOrgScope("acme")
	bundle := BuildAIsForScope(config.AgentConfig{AI: config.AgentAIConfig{Enable: true, Model: "gpt-4o-mini"}}, nil, storage.NewMemory(), scope, nil)
	if bundle.Chat == nil || bundle.Analyze == nil || bundle.ToolSnapshot == nil {
		t.Fatalf("extension-only bundle = %#v", bundle)
	}
	status := bundle.ToolSnapshot(scope).Capabilities["baseline_provider"]
	if !status.Configured || !status.Constructed || !status.Healthy || status.Count != 1 {
		t.Fatalf("baseline capability = %#v", status)
	}
}

func TestBuildAIsExposesToolCatalogWhenAIDisabled(t *testing.T) {
	bundle := BuildAIs(config.AgentConfig{}, nil, storage.NewMemory(), nil)
	if bundle.ToolSettings == nil || bundle.ToolSnapshot == nil {
		t.Fatal("AI-disabled bundle did not expose tool availability")
	}
	resolved := aitools.Resolve(aitools.Requirement{Kind: aitools.RequirementIntegration, Integration: "kubernetes"}, bundle.ToolSnapshot(tenancy.DefaultOrgScope()), true)
	if resolved.State != aitools.StateNeedsIntegration {
		t.Fatalf("kubernetes state = %q", resolved.State)
	}
}

func TestConfiguredToolAvailabilityUsesSourceKinds(t *testing.T) {
	const metricsType = "availability-test-metrics"
	const tracesType = "availability-test-traces"
	signalsources.RegisterKind(metricsType, signalsources.KindMetrics)
	signalsources.RegisterKind(tracesType, signalsources.KindTraces)

	tests := []struct {
		name    string
		sources []config.AgentSourceConfig
		want    map[string]bool
	}{
		{name: "metrics only", sources: []config.AgentSourceConfig{{Enable: true, Type: metricsType}}, want: map[string]bool{"metrics": true}},
		{name: "traces only", sources: []config.AgentSourceConfig{{Enable: true, Type: tracesType}}, want: map[string]bool{"traces": true}},
		{name: "logs only", sources: []config.AgentSourceConfig{{Enable: true, Type: "file"}}, want: map[string]bool{"logs": true}},
		{name: "mixed", sources: []config.AgentSourceConfig{{Enable: true, Type: "file"}, {Enable: true, Type: metricsType}, {Enable: true, Type: tracesType}}, want: map[string]bool{"logs": true, "metrics": true, "traces": true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := configuredToolAvailabilitySnapshot(config.AgentConfig{Sources: test.sources}, nil)
			for _, kind := range []string{"logs", "metrics", "traces"} {
				if got := snapshot.DataSources[kind].Configured; got != test.want[kind] {
					t.Errorf("%s configured = %t, want %t", kind, got, test.want[kind])
				}
				if snapshot.DataSources[kind].Constructed {
					t.Errorf("operator source label constructed %s capability", kind)
				}
			}
		})
	}
}

func TestRuntimeSnapshotDistinguishesConfiguredFailureFromMissing(t *testing.T) {
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs":    {Configured: true, Name: "Log data source"},
		"metrics": {Configured: false, Name: "Metric data source"},
		"traces":  {Configured: false, Name: "Trace data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	snapshot := buildToolAvailabilitySnapshot(configured, nil, nil, nil, nil, nil, versustools.DetectionHealthSnapshot{})
	logs := aitools.Resolve(aitools.Requirement{Kind: aitools.RequirementDataSource, SignalKind: "logs"}, snapshot, true)
	metrics := aitools.Resolve(aitools.Requirement{Kind: aitools.RequirementDataSource, SignalKind: "metrics"}, snapshot, true)
	if logs.State != aitools.StateUnhealthy || logs.Health != "configuration" {
		t.Fatalf("configured failure = %+v", logs)
	}
	if metrics.State != aitools.StateNeedsDataSource {
		t.Fatalf("missing source = %+v", metrics)
	}
}

func TestRuntimeSnapshotRequiresSourceNativeReadersBeforeFirstObservation(t *testing.T) {
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs":    {Configured: true, Name: "Log data source"},
		"metrics": {Configured: true, Name: "Metric data source"},
		"traces":  {Configured: true, Name: "Trace data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	snapshot := buildToolAvailabilitySnapshot(configured, &signalReaderAdapter{}, nil, nil, nil, nil, versustools.DetectionHealthSnapshot{})
	for _, kind := range []string{"logs", "metrics", "traces"} {
		got := snapshot.DataSources[kind]
		if (kind == "logs" && (!got.Constructed || !got.Healthy)) || (kind != "logs" && got.Constructed) {
			t.Errorf("unobserved %s health = %+v", kind, got)
		}
	}
}

func TestSourceKindHealthAggregatesAllUsableSources(t *testing.T) {
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs": {Configured: true, Name: "Log data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	healthy := versustools.SourceHealth{Name: "healthy", Kind: "logs", Configured: true, Observation: "healthy"}
	unobserved := versustools.SourceHealth{Name: "unobserved", Kind: "logs", Configured: true, Observation: "unknown"}
	failedA := versustools.SourceHealth{Name: "alpha", Kind: "logs", Configured: true, Observation: "unhealthy", ErrorClass: "authentication"}
	failedZ := versustools.SourceHealth{Name: "zeta", Kind: "logs", Configured: true, Observation: "unhealthy", ErrorClass: "connection"}

	tests := []struct {
		name        string
		sources     []versustools.SourceHealth
		wantHealthy bool
		wantName    string
		wantClass   string
	}{
		{name: "healthy and failed", sources: []versustools.SourceHealth{failedZ, healthy}, wantHealthy: true},
		{name: "unobserved and failed", sources: []versustools.SourceHealth{failedZ, unobserved}, wantHealthy: true},
		{name: "all failed", sources: []versustools.SourceHealth{failedZ, failedA}, wantName: "alpha", wantClass: "authentication"},
		{name: "one source recovers", sources: []versustools.SourceHealth{failedZ, healthy}, wantHealthy: true},
		{name: "all failed reverse order", sources: []versustools.SourceHealth{failedA, failedZ}, wantName: "alpha", wantClass: "authentication"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := buildToolAvailabilitySnapshot(configured, &signalReaderAdapter{}, nil, nil, nil, nil, versustools.DetectionHealthSnapshot{Sources: test.sources})
			got := snapshot.DataSources["logs"]
			wantName := test.wantName
			if wantName == "" {
				wantName = "Log data source"
			}
			if got.Healthy != test.wantHealthy || got.Name != wantName || got.Health != test.wantClass {
				t.Fatalf("status = %+v", got)
			}
		})
	}
}

func TestSpecializedRuntimeCapabilitiesResolveWithoutGenericReaders(t *testing.T) {
	configured := aitools.Snapshot{
		DataSources: map[string]aitools.DependencyStatus{
			"logs":    {Configured: true, Name: "Log data source"},
			"metrics": {Configured: true, Name: "Metric data source"},
			"traces":  {Configured: true, Name: "Trace data source"},
		},
		Capabilities: map[string]aitools.DependencyStatus{},
	}
	wantCounts := map[string]int{
		"discover_log_fields": 2, "read_log_records": 2,
		"discover_trace_fields": 3, "read_trace_spans": 3,
		"discover_metrics": 4, "read_metric_series": 4,
	}
	runtime := make([]core.Tool, 0, len(wantCounts)+2)
	for name, count := range wantCounts {
		metadata, ok := aitools.Lookup(name)
		if !ok {
			t.Fatalf("catalog missing %q", name)
		}
		runtime = append(runtime, capabilityTestTool{settingsCompatibleTool: settingsCompatibleTool{name: name}, signalKind: metadata.Requirement.SignalKind, sourceCount: count})
	}
	snapshot := buildToolAvailabilitySnapshot(configured, nil, nil, nil, nil, nil, versustools.DetectionHealthSnapshot{})
	snapshot = aitools.BindRuntimeCapabilities(snapshot, runtime)
	for _, kind := range []string{"logs", "metrics", "traces"} {
		if got := snapshot.DataSources[kind]; !got.Constructed || !got.Healthy {
			t.Errorf("%s card status = %+v", kind, got)
		}
	}
	for name, wantCount := range wantCounts {
		metadata, _ := aitools.Lookup(name)
		if got := aitools.Resolve(metadata.Requirement, snapshot, true); got.State != aitools.StateAvailable {
			t.Errorf("Resolve(%s) = %+v", name, got)
		}
		if got := snapshot.Capabilities[name]; got.Count != wantCount || !got.Constructed || !got.Healthy {
			t.Errorf("capability %s = %+v, want count %d", name, got, wantCount)
		}
	}
	for _, generic := range []string{"query_metrics", "query_traces"} {
		if _, ok := aitools.Lookup(generic); ok {
			t.Errorf("retired tool %s remains in catalog", generic)
		}
	}
	manager := aitools.NewManager(storage.NewMemory())
	for _, agentKind := range []aitools.AgentKind{aitools.AgentChat, aitools.AgentAnalyze} {
		filtered, err := manager.Filter(tenancy.DefaultOrgScope(), agentKind, runtime, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		got := toolNamesForTest(filtered)
		if len(got) != len(wantCounts) {
			t.Fatalf("%s filtered = %v", agentKind, got)
		}
		for _, generic := range []string{"query_metrics", "query_traces"} {
			if slices.Contains(got, generic) {
				t.Errorf("%s capability unlocked generic tool %s", agentKind, generic)
			}
		}
	}
}

func TestCloudWatchLogToolSourceRouting(t *testing.T) {
	cloud := config.AgentSourceConfig{Name: "cloud", Type: "cloudwatchlogs", Enable: true, CloudWatchLogs: config.AgentCloudWatchLogsSourceConfig{Region: "us-east-1", LogGroupName: "/prod"}}
	tools, errs := contributedTools(tenancy.DefaultOrgScope(), []config.AgentSourceConfig{cloud}, nil)
	if len(errs) != 0 || len(tools) != 0 {
		t.Fatalf("OSS CloudWatch source exposed native tools: %v %v", tools, errs)
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	api, err := cloudwatchlogapp.NewToolAPI(context.Background(), "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := cloudwatchlogapp.NewReader(api, cloudwatchlogapp.Scope{Region: "us-east-1", LogGroupName: "/prod"}, nil)
	if err != nil || len(cloudwatchlogtools.New([]cloudwatchlogtools.Source{{Name: "cloud", Reader: reader}})) != 2 {
		t.Fatalf("OSS CloudWatch adapter: %v", err)
	}
}

func TestSourceReadCapabilitiesReflectConstructedLicensedReaders(t *testing.T) {
	base := knowledgeCapabilities(tenancy.DefaultOrgScope(), nil, nil, nil, nil, nil, nil, nil, nil, ChatKnowledgeProviders{})
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"metrics": {Configured: true, Name: "Metric data source"},
		"traces":  {Configured: true, Name: "Trace data source"},
	}}
	metric := capabilityTestTool{settingsCompatibleTool: settingsCompatibleTool{name: "read_metric_series"}, signalKind: "metrics", sourceCount: 1}
	trace := capabilityTestTool{settingsCompatibleTool: settingsCompatibleTool{name: "read_trace_spans"}, signalKind: "traces", sourceCount: 1}
	check := func(t *testing.T, runtime []core.Tool, wantMetrics, wantTraces bool) {
		t.Helper()
		statuses := sourceReadCapabilities(append([]versustools.CapabilityStatus(nil), base...), aitools.BindRuntimeCapabilities(configured, runtime), runtime)
		for _, status := range statuses {
			if status.Name == "metrics" && (status.Configured != wantMetrics || (status.Available == versustools.CapabilityStatusTrue) != wantMetrics) {
				t.Fatalf("metrics = %+v, want available=%v", status, wantMetrics)
			}
			if status.Name == "traces" && (status.Configured != wantTraces || (status.Available == versustools.CapabilityStatusTrue) != wantTraces) {
				t.Fatalf("traces = %+v, want available=%v", status, wantTraces)
			}
		}
	}
	check(t, nil, false, false)
	check(t, []core.Tool{metric}, true, false)
	check(t, []core.Tool{metric, trace}, true, true)
	aitools.SetEntitlementResolver(func(requirement aitools.Requirement, _ aitools.DependencyStatus) aitools.EntitlementDecision {
		return aitools.EntitlementDecision{Required: requirement.SignalKind == "metrics"}
	})
	t.Cleanup(func() { aitools.SetEntitlementResolver(nil) })
	missing := sourceReadCapabilities(append([]versustools.CapabilityStatus(nil), base...), configured, nil)
	for _, status := range missing {
		if status.Name == "metrics" && (status.Licensed != versustools.CapabilityStatusFalse || status.Available != versustools.CapabilityStatusFalse) {
			t.Fatalf("unlicensed absent metrics = %+v", status)
		}
	}
	statuses := sourceReadCapabilities(append([]versustools.CapabilityStatus(nil), base...), aitools.BindRuntimeCapabilities(configured, []core.Tool{metric, trace}), []core.Tool{metric, trace})
	for _, status := range statuses {
		if status.Name == "metrics" && (status.Licensed != versustools.CapabilityStatusFalse || status.Available != versustools.CapabilityStatusFalse) {
			t.Fatalf("unlicensed metrics = %+v", status)
		}
		if status.Name == "traces" && status.Available != versustools.CapabilityStatusTrue {
			t.Fatalf("licensed traces = %+v", status)
		}
	}
}

func TestElasticsearchToolSourcesAreSourceDrivenAndDeterministic(t *testing.T) {
	valid := func(name string) config.AgentSourceConfig {
		return config.AgentSourceConfig{Name: name, Type: "elasticsearch", Enable: true, Elasticsearch: config.AgentElasticsearchSourceConfig{Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: name + "-*"}}
	}
	tests := []struct {
		name       string
		sources    []config.AgentSourceConfig
		wantNames  []string
		wantErrors int
		configured bool
	}{
		{name: "absent"},
		{name: "other log source", sources: []config.AgentSourceConfig{{Name: "file", Type: "file", Enable: true}}},
		{name: "disabled", sources: []config.AgentSourceConfig{{Name: "disabled", Type: "elasticsearch", Enable: false}}},
		{name: "invalid", sources: []config.AgentSourceConfig{{Name: "invalid", Type: "elasticsearch", Enable: true}}, wantErrors: 1, configured: true},
		{name: "single", sources: []config.AgentSourceConfig{valid("primary")}, wantNames: []string{"primary"}, configured: true},
		{name: "multiple sorted", sources: []config.AgentSourceConfig{valid("zeta"), valid("alpha")}, wantNames: []string{"alpha", "zeta"}, configured: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			built, errs := buildElasticsearchToolSources(test.sources)
			if len(errs) != test.wantErrors {
				t.Fatalf("errors = %d, want %d", len(errs), test.wantErrors)
			}
			names := make([]string, 0, len(built))
			for _, source := range built {
				names = append(names, source.Name)
			}
			wantNames := test.wantNames
			if wantNames == nil {
				wantNames = []string{}
			}
			if !reflect.DeepEqual(names, wantNames) {
				t.Fatalf("source names = %v, want %v", names, test.wantNames)
			}
			snapshot := configuredToolAvailabilitySnapshot(config.AgentConfig{Sources: test.sources}, nil)
			if snapshot.DataSources["elasticsearch"].Configured != test.configured {
				t.Fatalf("configured = %t, want %t", snapshot.DataSources["elasticsearch"].Configured, test.configured)
			}
			tools := elasticsearchtools.New(built)
			wantTools := 0
			if len(test.wantNames) > 0 {
				wantTools = 4
			}
			if got, want := len(tools), wantTools; got != want {
				t.Fatalf("tool count = %d, want %d", got, want)
			}
		})
	}
}

func TestElasticsearchCatalogRequirementDoesNotUnlockForOtherLogs(t *testing.T) {
	configured := configuredToolAvailabilitySnapshot(config.AgentConfig{Sources: []config.AgentSourceConfig{{Name: "file", Type: "file", Enable: true}}}, nil)
	configured.DataSources["logs"] = aitools.DependencyStatus{Configured: true, Constructed: true, Healthy: true}
	view := aitools.NewManager(storage.NewMemory())
	runtime := []core.Tool{
		settingsCompatibleTool{name: "get_related_logs"},
		settingsCompatibleTool{name: "list_log_indices"},
	}
	filtered, err := view.Filter(tenancy.DefaultOrgScope(), aitools.AgentAnalyze, runtime, configured)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Name() != "get_related_logs" {
		t.Fatalf("filtered tools = %v", toolNamesForTest(filtered))
	}
}

func TestElasticsearchConstructionFailsClosedAndReadinessIsConfigurationBased(t *testing.T) {
	valid := config.AgentSourceConfig{Name: "valid", Type: "elasticsearch", Enable: true, Elasticsearch: config.AgentElasticsearchSourceConfig{Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*"}}
	invalid := config.AgentSourceConfig{Name: "invalid", Type: "elasticsearch", Enable: true}
	built, errs := buildElasticsearchToolSources([]config.AgentSourceConfig{valid, invalid})
	if len(errs) != 1 || len(built) != 0 {
		t.Fatalf("partial construction built=%v errors=%v", built, errs)
	}
	configured := configuredToolAvailabilitySnapshot(config.AgentConfig{Sources: []config.AgentSourceConfig{valid, invalid}}, nil).DataSources["elasticsearch"]
	status := elasticsearchConstructionStatus(configured, built, errs)
	if !status.Configured || status.Constructed || status.Healthy || status.Health != "configuration" {
		t.Fatalf("partial construction status = %+v", status)
	}

	built, errs = buildElasticsearchToolSources([]config.AgentSourceConfig{valid})
	status = elasticsearchConstructionStatus(configured, built, errs)
	if !status.Constructed || !status.Healthy || status.Health != "" {
		t.Fatalf("complete construction status = %+v", status)
	}
}

func toolNamesForTest(tools []core.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

func TestMetricsObservationNeverUnlocksLogs(t *testing.T) {
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs":    {Configured: true, Name: "Log data source"},
		"metrics": {Configured: true, Name: "Metric data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	health := versustools.DetectionHealthSnapshot{Sources: []versustools.SourceHealth{{Kind: "metrics", Configured: true, Observation: "healthy"}}}
	snapshot := buildToolAvailabilitySnapshot(configured, nil, nil, nil, nil, nil, health)
	if got := snapshot.DataSources["metrics"]; got.Constructed {
		t.Fatalf("metric source constructed generic reader = %+v", got)
	}
	if got := snapshot.DataSources["logs"]; got.Healthy || got.Health != "configuration" {
		t.Fatalf("logs health = %+v", got)
	}
}

func TestLiveSourceHealthFiltersAndRestoresLogTools(t *testing.T) {
	scope := tenancy.DefaultOrgScope()
	health := newDetectionHealthAdapter(scope,
		[]config.AgentSourceConfig{{Name: "logs", Type: "file", Enable: true}},
		[]core.SignalSource{panicPullSource{name: "logs"}}, nil,
	)
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs": {Configured: true, Name: "Log data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	runtime := []core.Tool{settingsCompatibleTool{name: "get_related_logs"}}
	manager := aitools.NewManager(storage.NewMemory())
	states := []struct {
		err       error
		wantClass string
		wantTools int
	}{
		{nil, "", 1},
		{errors.New("401 unauthorized"), "authentication", 0},
		{errors.New("connection refused"), "connection", 0},
		{context.DeadlineExceeded, "timeout", 0},
		{nil, "", 1},
	}
	for _, state := range states {
		health.Observe("logs", state.err, time.Now())
		snapshot := buildToolAvailabilitySnapshot(configured, &signalReaderAdapter{}, nil, nil, nil, nil, health.DetectionHealth(scope))
		filtered, err := manager.Filter(scope, aitools.AgentChat, runtime, snapshot)
		if err != nil || len(filtered) != state.wantTools || snapshot.DataSources["logs"].Health != state.wantClass {
			t.Fatalf("error=%v class=%q tools=%d filterErr=%v", state.err, snapshot.DataSources["logs"].Health, len(filtered), err)
		}
	}
}

type settingsCompatibleTool struct{ name string }

func (tool settingsCompatibleTool) Name() string          { return tool.name }
func (settingsCompatibleTool) Description() string        { return "test" }
func (settingsCompatibleTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (tool settingsCompatibleTool) Invoke(context.Context, json.RawMessage) (*core.ToolResult, error) {
	return &core.ToolResult{Tool: tool.name, Found: true}, nil
}

type capabilityTestTool struct {
	settingsCompatibleTool
	signalKind  string
	sourceCount int
}

func (tool capabilityTestTool) AvailabilityCapability() (string, string, int) {
	return tool.name, tool.signalKind, tool.sourceCount
}

type generationProvider struct {
	storage.Provider
	err error
}

func (provider *generationProvider) ReadBlob(name string) ([]byte, error) {
	if provider.err != nil {
		return nil, provider.err
	}
	return provider.Provider.ReadBlob(name)
}

func (provider *generationProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

func TestToolGenerationIsAtomicAndRecoversWithoutFailingOpen(t *testing.T) {
	provider := &generationProvider{Provider: storage.NewMemory()}
	manager := aitools.NewManager(provider)
	scope := tenancy.DefaultOrgScope()
	snapshotCalls := 0
	generation := newToolGeneration(manager, scope, func(tenancy.OrgScope) aitools.Snapshot {
		snapshotCalls++
		return aitools.Snapshot{}
	})
	runtime := []core.Tool{settingsCompatibleTool{name: "get_incident"}}

	if _, ok := generation.Revision(context.Background()); !ok {
		t.Fatal("initial revision unavailable")
	}
	filtered, err := generation.Filter(aitools.AgentChat, runtime)
	if err != nil || len(filtered) != 1 || snapshotCalls != 1 {
		t.Fatalf("initial generation tools=%d err=%v snapshots=%d", len(filtered), err, snapshotCalls)
	}
	if _, err := manager.SetEnabled(scope, aitools.AgentChat, "get_incident", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := generation.Revision(context.Background()); !ok {
		t.Fatal("disabled revision unavailable")
	}
	filtered, err = generation.Filter(aitools.AgentChat, runtime)
	if err != nil || len(filtered) != 0 {
		t.Fatalf("disabled generation tools=%d err=%v", len(filtered), err)
	}

	provider.err = errors.New("transient read failure")
	if _, ok := generation.Revision(context.Background()); ok {
		t.Fatal("failed revision reported available")
	}
	if _, err := generation.Filter(aitools.AgentChat, runtime); err == nil {
		t.Fatal("failed generation returned a tool graph")
	}
	provider.err = nil
	if _, ok := generation.Revision(context.Background()); !ok {
		t.Fatal("recovered revision unavailable")
	}
	filtered, err = generation.Filter(aitools.AgentChat, runtime)
	if err != nil || len(filtered) != 0 {
		t.Fatalf("recovered generation failed open: tools=%d err=%v", len(filtered), err)
	}
}

func TestSeedLoadsDurableSettingsWithoutHolderRevision(t *testing.T) {
	provider := storage.NewMemory()
	manager := aitools.NewManager(provider)
	scope := tenancy.DefaultOrgScope()
	runtime := []core.Tool{settingsCompatibleTool{name: "get_system_overview"}}
	snapshot := func(tenancy.OrgScope) aitools.Snapshot { return aitools.Snapshot{} }

	if _, err := manager.SetEnabled(scope, aitools.AgentChat, "get_system_overview", false); err != nil {
		t.Fatal(err)
	}
	filtered, err := loadCurrentTools(manager, scope, snapshot, aitools.AgentChat, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("new session seed tools = %d, want 0 before any holder revision", len(filtered))
	}
}

type registrationReliability struct{}

func (registrationReliability) ServiceReliability(context.Context, tenancy.OrgScope, string) (versustools.ServiceReliabilitySnapshot, error) {
	return versustools.ServiceReliabilitySnapshot{}, nil
}

type registrationDecision struct{}

func (registrationDecision) AlertDecision(context.Context, tenancy.OrgScope, string) (versustools.AlertDecisionSnapshot, error) {
	return versustools.AlertDecisionSnapshot{}, nil
}

type registrationStatus struct{}

func (registrationStatus) CapabilityStatuses(tenancy.OrgScope) []versustools.CapabilityStatus {
	return []versustools.CapabilityStatus{
		{Name: "service_reliability", Group: "reliability", Configured: true, Licensed: versustools.CapabilityStatusTrue, Available: versustools.CapabilityStatusTrue, Observation: "reliability ready"},
		{Name: "alert_decisions", Group: "decisions", Configured: false, Licensed: versustools.CapabilityStatusTrue, Available: versustools.CapabilityStatusFalse, SetupAction: "Enable decision storage."},
	}
}

func TestSetChatKnowledgeProvidersFeedsToolsAndCapabilities(t *testing.T) {
	SetChatKnowledgeProviders(ChatKnowledgeProviders{ServiceReliability: registrationReliability{}, AlertDecision: registrationDecision{}, CapabilityStatus: registrationStatus{}})
	t.Cleanup(func() { SetChatKnowledgeProviders(ChatKnowledgeProviders{}) })
	catalog, store := newBuildCatalog(t)
	tools := buildAnalyzeTools(store, tenancy.DefaultOrgScope(), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil)

	var service versustools.GetService
	var decision versustools.GetAlertDecision
	var capability versustools.ListCapabilities
	for _, tool := range tools {
		switch typed := tool.(type) {
		case versustools.GetService:
			service = typed
		case versustools.GetAlertDecision:
			decision = typed
		case versustools.ListCapabilities:
			capability = typed
		}
	}
	if service.Reliability == nil || decision.Provider == nil {
		t.Fatal("registered providers were not threaded into tools")
	}
	if len(capability.Capabilities) != 12 || capability.Capabilities[6].Name != "service_reliability" || !capability.Capabilities[6].Configured || capability.Capabilities[7].Configured || capability.Capabilities[7].Available != versustools.CapabilityStatusFalse {
		t.Fatalf("capabilities = %+v", capability.Capabilities)
	}
}

func TestDataProvidersDoNotImplyCapabilityConfiguration(t *testing.T) {
	providers := ChatKnowledgeProviders{ServiceReliability: registrationReliability{}, AlertDecision: registrationDecision{}}
	capabilities := knowledgeCapabilities(tenancy.DefaultOrgScope(), nil, nil, nil, nil, nil, nil, nil, nil, providers)
	for _, capability := range capabilities {
		if capability.Name == "service_reliability" || capability.Name == "alert_decisions" {
			if capability.Configured || capability.Available != versustools.CapabilityStatusUnknown {
				t.Fatalf("provider-only capability = %+v", capability)
			}
		}
	}
}

type scopedAnswerabilityStatus struct{ scope tenancy.OrgScope }

func (provider *scopedAnswerabilityStatus) CapabilityStatuses(scope tenancy.OrgScope) []versustools.CapabilityStatus {
	provider.scope = scope
	return []versustools.CapabilityStatus{
		{Name: "incidents", Licensed: versustools.CapabilityStatusFalse, Available: versustools.CapabilityStatusFalse},
		{Name: "service_reliability", Group: "reliability", Configured: true, Licensed: "TRUE", Available: "true", Observation: strings.Repeat("ready", 100)},
		{Name: "alert_decisions", Group: "decisions", Configured: false, Licensed: "invalid", Available: "invalid", SetupAction: "Enable decision storage."},
		{Name: "not_registered", Configured: true, Licensed: "true", Available: "true"},
	}
}

func TestGenericKnowledgeCatalogWithFakeProviders(t *testing.T) {
	statusProvider := &scopedAnswerabilityStatus{}
	SetChatKnowledgeProviders(ChatKnowledgeProviders{
		ServiceReliability: registrationReliability{},
		AlertDecision:      registrationDecision{},
		CapabilityStatus:   statusProvider,
	})
	t.Cleanup(func() { SetChatKnowledgeProviders(ChatKnowledgeProviders{}) })
	catalog, store := newBuildCatalog(t)
	tools := buildAnalyzeTools(store, tenancy.NewOrgScope("licensed", "default"), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, registrationHealth{})
	byName := make(map[string]any, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	for _, name := range []string{"get_system_overview", "list_services", "get_incident", "get_pattern", "get_service", "get_alert_decision", "list_capabilities"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("answerability tool %q is not registered", name)
		}
	}

	capabilityTool := byName["list_capabilities"].(versustools.ListCapabilities)
	result, err := capabilityTool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	capabilities := result.Data["capabilities"].([]versustools.CapabilityStatus)
	if len(capabilities) != 12 || len(statusProvider.scope.OrgIDs()) != 2 || statusProvider.scope.OrgIDs()[0] != "licensed" {
		t.Fatalf("capability inventory=%+v scope=%v", capabilities, statusProvider.scope.OrgIDs())
	}
	byCapability := make(map[string]versustools.CapabilityStatus, len(capabilities))
	for _, capability := range capabilities {
		byCapability[capability.Name] = capability
	}
	if !byCapability["incidents"].Configured || byCapability["incidents"].Available != versustools.CapabilityStatusTrue {
		t.Fatalf("OSS incident status was overridden: %+v", byCapability["incidents"])
	}
	if got := byCapability["service_reliability"]; got.Licensed != versustools.CapabilityStatusTrue || got.Available != versustools.CapabilityStatusTrue || len([]rune(got.Observation)) != 240 {
		t.Fatalf("reliability status = %+v", got)
	}
	if got := byCapability["alert_decisions"]; got.Licensed != versustools.CapabilityStatusUnknown || got.Available != versustools.CapabilityStatusUnknown || got.SetupAction == "" {
		t.Fatalf("decision status = %+v", got)
	}
	if _, ok := byCapability["not_registered"]; ok {
		t.Fatal("provider added an unregistered capability")
	}

	serviceResult, err := (versustools.GetService{Catalog: newCatalogAdapter(catalog)}).Invoke(context.Background(), json.RawMessage(`{"service":"checkout"}`))
	if err != nil || !serviceResult.IsAvailable() || serviceResult.Data["reliability"].(versustools.ServiceReliabilitySnapshot).SetupAction == "" {
		t.Fatalf("service guidance = %+v, err=%v", serviceResult, err)
	}
	unavailableDecision, err := (versustools.GetAlertDecision{}).Invoke(context.Background(), json.RawMessage(`{"identifier":"missing"}`))
	if err != nil || unavailableDecision.IsAvailable() || unavailableDecision.Found || unavailableDecision.Data["reason_known"] != false || unavailableDecision.Data["setup_action"] == "" {
		t.Fatalf("decision guidance = %+v, err=%v", unavailableDecision, err)
	}
}

func TestCatalogAdapterCarriesEffectiveAutoPromoteThreshold(t *testing.T) {
	catalog, _ := newBuildCatalog(t)
	catalog.Upsert("pattern", "template", "source", 1, 0, "default", "service")
	adapter := newCatalogAdapterWithThreshold(catalog, 42)
	if got := adapter.Get("pattern").AutoPromoteAfter; got != 42 {
		t.Fatalf("AutoPromoteAfter = %d, want 42", got)
	}
}

func TestCatalogAdapterWithThresholdPreservesNilInterface(t *testing.T) {
	adapter := newCatalogAdapterWithThreshold(nil, 42)
	if adapter != nil {
		t.Fatalf("adapter = %T, want nil interface", adapter)
	}
	tools := buildAnalyzeTools(nil, tenancy.DefaultOrgScope(), adapter, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, tool := range tools {
		if tool.Name() == "get_pattern" || tool.Name() == "get_service" || tool.Name() == "get_system_overview" || tool.Name() == "list_services" || tool.Name() == "list_patterns" {
			t.Fatalf("catalog tool %q registered with nil catalog", tool.Name())
		}
		if capability, ok := tool.(versustools.ListCapabilities); ok {
			for _, status := range capability.Capabilities {
				if status.Name == "catalog" && status.Configured {
					t.Fatalf("catalog capability = %+v, want unconfigured", status)
				}
			}
		}
	}
}

func TestBuildAnalyzeToolsThreadsOrgScope(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	scope := tenancy.NewOrgScope("licensed", "default")
	tools := buildAnalyzeTools(store, scope, newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil)
	incident, ok := tools[0].(versustools.GetIncident)
	if !ok {
		t.Fatalf("tool[0] = %T, want GetIncident", tools[0])
	}
	if got := incident.Scope.OrgIDs(); len(got) != 2 || got[0] != "licensed" || got[1] != "default" {
		t.Fatalf("incident scope = %v, want [licensed default]", got)
	}
}
