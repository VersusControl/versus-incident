package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	awslogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

var linkedProviderKinds = []string{"loki", "elasticsearch", "cloudwatchlogs", "graylog", "splunk", "signoz"}

type providerRow struct {
	at                   time.Time
	pod, container, text string
}

type cloudPage struct {
	rows []providerRow
	next string
}

// providerBackend answers in each provider's native response shape, honours
// the requested window and row limit like the real backend, and counts the
// logical query calls (one per adapter read of one Pod) and raw requests.
type providerBackend struct {
	kind       string
	mu         sync.Mutex
	rows       []providerRow
	calls      int
	requests   int
	nextCursor bool
	fail       bool
	pages      map[string]cloudPage
	splunkFrom time.Time
	splunkTo   time.Time
	splunkMax  int
	queries    []string
}

func (backend *providerBackend) window(from, to time.Time, newestFirst bool) []providerRow {
	selected := []providerRow{}
	for _, row := range backend.rows {
		if !row.at.Before(from) && !row.at.After(to) {
			selected = append(selected, row)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		if newestFirst {
			return selected[i].at.After(selected[j].at)
		}
		return selected[i].at.Before(selected[j].at)
	})
	return selected
}

func findRange(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		if bounds, ok := typed["range"].(map[string]any); ok {
			for _, inner := range bounds {
				if limits, ok := inner.(map[string]any); ok {
					return limits
				}
			}
		}
		for _, child := range typed {
			if found := findRange(child); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := findRange(child); found != nil {
				return found
			}
		}
	}
	return nil
}

func (backend *providerBackend) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.requests++
	if backend.fail {
		backend.calls++
		http.Error(writer, "backend unavailable", http.StatusServiceUnavailable)
		return
	}
	values := request.URL.Query()
	switch backend.kind {
	case "loki":
		backend.queries = append(backend.queries, values.Get("query"))
		backend.calls++
		start, _ := strconv.ParseInt(values.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(values.Get("end"), 10, 64)
		limit, _ := strconv.Atoi(values.Get("limit"))
		rows := backend.window(time.Unix(0, start), time.Unix(0, end), values.Get("direction") != "forward")
		if len(rows) > limit {
			rows = rows[:limit]
		}
		streams := []any{}
		for _, row := range rows {
			streams = append(streams, map[string]any{"stream": map[string]string{"namespace": "shop", "pod": row.pod, "container": row.container}, "values": [][]string{{strconv.FormatInt(row.at.UnixNano(), 10), row.text}}})
		}
		json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": streams}})
	case "elasticsearch":
		backend.calls++
		var body map[string]any
		json.NewDecoder(request.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		backend.queries = append(backend.queries, string(encoded))
		size := int(body["size"].(float64))
		limits := findRange(body["query"])
		from, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(limits["gte"]))
		to, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(limits["lte"]))
		rows := backend.window(from, to, false)
		total := len(rows)
		if len(rows) > size {
			rows = rows[:size]
		}
		hits := []any{}
		for _, row := range rows {
			hits = append(hits, map[string]any{"_source": map[string]any{"@timestamp": row.at.Format(time.RFC3339Nano), "message": row.text, "kubernetes": map[string]string{"namespace_name": "shop", "pod_name": row.pod, "container_name": row.container}}})
		}
		json.NewEncoder(writer).Encode(map[string]any{"hits": map[string]any{"total": map[string]any{"value": total, "relation": "eq"}, "hits": hits}})
	case "graylog":
		backend.queries = append(backend.queries, values.Get("query"))
		backend.calls++
		from, _ := time.Parse("2006-01-02T15:04:05.000Z", values.Get("from"))
		to, _ := time.Parse("2006-01-02T15:04:05.000Z", values.Get("to"))
		limit, _ := strconv.Atoi(values.Get("limit"))
		offset, _ := strconv.Atoi(values.Get("offset"))
		rows := backend.window(from, to.Add(999*time.Millisecond), true)
		total := len(rows)
		rows = rows[min(offset, len(rows)):]
		if len(rows) > limit {
			rows = rows[:limit]
		}
		messages := []any{}
		for position, row := range rows {
			messages = append(messages, map[string]any{"message": map[string]any{"_id": fmt.Sprintf("message-%d-%d", offset, position), "timestamp": row.at.Format("2006-01-02T15:04:05.000Z"), "message": row.text, "kubernetes_namespace_name": "shop", "kubernetes_pod_name": row.pod, "kubernetes_container_name": row.container}})
		}
		json.NewEncoder(writer).Encode(map[string]any{"total_results": total, "messages": messages})
	case "splunk":
		switch {
		case request.Method == http.MethodPost:
			backend.calls++
			request.ParseForm()
			backend.queries = append(backend.queries, request.Form.Get("search"))
			earliest, _ := strconv.ParseInt(request.Form.Get("earliest_time"), 10, 64)
			latest, _ := strconv.ParseInt(request.Form.Get("latest_time"), 10, 64)
			backend.splunkFrom, backend.splunkTo = time.Unix(earliest, 0), time.Unix(latest, 0)
			backend.splunkMax, _ = strconv.Atoi(request.Form.Get("max_count"))
			fmt.Fprint(writer, `{"sid":"linked-job"}`)
		case request.Method == http.MethodDelete:
			fmt.Fprint(writer, `{}`)
		case strings.Contains(request.URL.Path, "/results"):
			count, _ := strconv.Atoi(values.Get("count"))
			rows := backend.window(backend.splunkFrom, backend.splunkTo, true)
			if backend.splunkMax > 0 && len(rows) > backend.splunkMax {
				rows = rows[:backend.splunkMax]
			}
			if len(rows) > count {
				rows = rows[:count]
			}
			results := []any{}
			for _, row := range rows {
				full := map[string]string{"index": "main", "_time": row.at.Format(time.RFC3339Nano), "_raw": row.text, "namespace": "shop", "pod": row.pod, "container_name": row.container}
				projected := map[string]string{}
				for _, field := range values["f"] {
					if value, ok := full[field]; ok {
						projected[field] = value
					}
				}
				results = append(results, projected)
			}
			json.NewEncoder(writer).Encode(map[string]any{"results": results})
		default:
			fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
		}
	case "signoz":
		backend.calls++
		var body map[string]any
		json.NewDecoder(request.Body).Decode(&body)
		encodedQuery, _ := json.Marshal(body)
		backend.queries = append(backend.queries, string(encodedQuery))
		spec := body["compositeQuery"].(map[string]any)["queries"].([]any)[0].(map[string]any)["spec"].(map[string]any)
		limit := int(spec["limit"].(float64))
		rows := backend.window(time.UnixMilli(int64(body["start"].(float64))), time.UnixMilli(int64(body["end"].(float64))), true)
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		encoded := []any{}
		for _, row := range rows {
			encoded = append(encoded, map[string]any{"timestamp": row.at.Format(time.RFC3339Nano), "data": map[string]any{"body": row.text, "resources_string": map[string]string{"k8s.namespace.name": "shop", "k8s.pod.name": row.pod, "k8s.container.name": row.container}}})
		}
		result := map[string]any{"queryName": "A", "rows": encoded}
		if more && backend.nextCursor {
			result["nextCursor"] = "linked-next-page"
		}
		json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"type": "raw", "data": map[string]any{"results": []any{result}}}})
	}
}

func (backend *providerBackend) FilterLogEvents(ctx context.Context, input *awslogs.FilterLogEventsInput, _ ...func(*awslogs.Options)) (*awslogs.FilterLogEventsOutput, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.requests++
	if input.FilterPattern != nil {
		backend.queries = append(backend.queries, *input.FilterPattern)
	}
	if backend.fail {
		backend.calls++
		return nil, fmt.Errorf("ServiceUnavailableException: backend unavailable")
	}
	token := ""
	if input.NextToken != nil {
		token = *input.NextToken
	} else {
		backend.calls++
	}
	page := cloudPage{}
	if backend.pages != nil {
		page = backend.pages[token]
	} else {
		rows := backend.window(time.UnixMilli(*input.StartTime), time.UnixMilli(*input.EndTime), false)
		offset, _ := strconv.Atoi(token)
		rows = rows[min(offset, len(rows)):]
		limit := int(*input.Limit)
		if len(rows) > limit {
			page.next = strconv.Itoa(offset + limit)
			rows = rows[:limit]
		}
		page.rows = rows
	}
	output := &awslogs.FilterLogEventsOutput{}
	for _, row := range page.rows {
		message, _ := json.Marshal(map[string]any{"kubernetes": map[string]string{"namespace_name": "shop", "pod_name": row.pod, "container_name": row.container}, "log": row.text})
		text, stream, at := string(message), "fluent-bit-"+row.pod, row.at.UnixMilli()
		output.Events = append(output.Events, types.FilteredLogEvent{Timestamp: &at, Message: &text, LogStreamName: &stream})
	}
	if page.next != "" {
		next := page.next
		output.NextToken = &next
	}
	return output, nil
}

func (backend *providerBackend) counts() (int, int) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.calls, backend.requests
}

type linkedProviderHarness struct {
	service  *kubernetes.Service
	provider storage.Provider
	cluster  string
	backend  *providerBackend
	now      time.Time
}

func newLinkedProviderHarness(t *testing.T, kind string, delay *int) *linkedProviderHarness {
	t.Helper()
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	kube := httptest.NewServer(fixture)
	t.Cleanup(kube.Close)
	backend := &providerBackend{kind: kind}
	server := httptest.NewTLSServer(backend)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	cluster := fmt.Sprintf("providers-%s-%d", kind, time.Now().UnixNano())
	source := config.AgentSourceConfig{Name: "linked-" + kind, Type: kind, Enable: true}
	source.Loki = config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="db"}`, InsecureSkipVerify: true}
	source.Elasticsearch = config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, AllowLoopback: true, InsecureSkipVerify: true, Index: "logs-*"}
	source.Graylog = config.AgentGraylogSourceConfig{Address: server.URL, Query: "environment:prod", StreamID: "linked-stream", InsecureSkipVerify: true}
	source.Splunk = config.AgentSplunkSourceConfig{Address: server.URL, Search: "search index=main AND sourcetype=kube", InsecureSkipVerify: true}
	source.Signoz = config.AgentSignozSourceConfig{Address: server.URL, APIKey: "linked-signoz-key", AllowLoopback: true, RootCAs: roots}
	source.CloudWatchLogs = config.AgentCloudWatchLogsSourceConfig{Region: "us-east-1", LogGroupName: "/eks/linked/containers"}
	built := BuildBreakLogSources([]config.AgentSourceConfig{source}, linkedCanaryScrubber{}, cluster)
	if len(built) != 1 {
		t.Fatalf("%s source not built", kind)
	}
	build := built[0].Build
	if _, err := build(breaklogs.Fields{}); err != nil {
		t.Fatalf("%s adapter rejected its default mapping: %v", kind, err)
	}
	built[0].Build = func(fields breaklogs.Fields) (breaklogs.LinkedReader, error) {
		reader, err := build(fields)
		if err == nil && kind == "cloudwatchlogs" {
			reader.(*breakLogAdapter).cloudAPI = backend
		}
		return reader, err
	}
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: kube.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	harness := &linkedProviderHarness{now: time.Now().UTC()}
	service := kubernetes.NewServiceWithObservationClock(client, kubernetes.Scope{OrgID: "default", ClusterID: cluster}, time.Minute, func() time.Time { return harness.now })
	service.SetScrubber(linkedCanaryScrubber{})
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	t.Cleanup(service.StopCapture)
	service.SetBreakLogSources(func(string) []breaklogs.Source { return built })
	if err := service.SaveAgentSettings(context.Background(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: source.Name, IngestDelay: delay}}); err != nil {
		t.Fatal(err)
	}
	harness.service, harness.provider, harness.cluster, harness.backend = service, provider, cluster, backend
	return harness
}

func (harness *linkedProviderHarness) commit(t *testing.T, breakAt, now time.Time, pods ...breaklogs.ManifestPod) breaklogs.Query {
	t.Helper()
	harness.now = now
	manifest := breaklogs.Manifest{ID: breaklogs.ManifestIdentity("root-db", breakAt), Namespace: "shop", RootUID: "root-db", BreakAt: breakAt, From: breakAt.Add(-5 * time.Minute), To: breakAt.Add(2 * time.Minute), CapturedAt: now, Pods: pods}
	scope := harness.service.ActiveScope()
	store := kubechanges.NewStore(harness.provider, kubeindex.Scope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, CredentialID: scope.CredentialID})
	if err := store.CommitBreakLogs(nil, nil, "", "", now, manifest); err != nil {
		t.Fatal(err)
	}
	return breaklogs.Query{Namespace: "shop", RootUID: "root-db", OccurrenceID: manifest.ID, Center: breakAt, From: manifest.From, To: manifest.To, Container: breaklogs.AllContainers}
}

func linkedReusedPods(t0 time.Time) (breaklogs.ManifestPod, breaklogs.ManifestPod) {
	old := breaklogs.ManifestPod{Namespace: "shop", RootUID: "root-db", Name: "db-0", UID: "uid-old", CreatedAt: t0.Add(-time.Hour), LastSeen: t0.Add(-6 * time.Minute), DeletedAt: t0.Add(-6 * time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", Restarts: 1, StartedAt: t0.Add(-8*time.Minute + 5*time.Second), TerminationAt: t0.Add(-8 * time.Minute)}}}
	replacement := breaklogs.ManifestPod{Namespace: "shop", RootUID: "root-db", Name: "db-0", UID: "uid-new", CreatedAt: t0.Add(-5 * time.Minute), LastSeen: t0.Add(-60 * time.Second), DeletedAt: t0.Add(-60 * time.Second), Containers: []breaklogs.ManifestContainer{
		{Name: "migrate", Init: true, StartedAt: t0.Add(-5 * time.Minute), TerminationAt: t0.Add(-280 * time.Second), CurrentTermination: true},
		{Name: "app", StartedAt: t0.Add(-275 * time.Second)},
	}}
	return old, replacement
}

func readPage(t *testing.T, harness *linkedProviderHarness, query breaklogs.Query) kubernetes.BreakLogPage {
	t.Helper()
	before, _ := harness.backend.counts()
	page, err := harness.service.BreakLogs(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := harness.backend.counts()
	if after-before > 3 {
		t.Fatalf("one detail read issued %d provider queries, budget is 3", after-before)
	}
	return page
}

func pageReasons(page kubernetes.BreakLogPage) []string {
	reasons := []string{}
	for _, gap := range page.Coverage.Gaps {
		reasons = append(reasons, gap.Reason)
	}
	return reasons
}

func TestLinkedLokiPinnedTerminatedBurstMergesHeadWithNativeTail(t *testing.T) {
	zero := 0
	harness := newLinkedProviderHarness(t, "loki", &zero)
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-4 * time.Minute)
	end := start.Add(time.Second)
	pod := breaklogs.ManifestPod{
		Namespace: "shop", RootUID: "root-db", Name: "db-0", UID: "pinned",
		CreatedAt: start.Add(-time.Minute), LastSeen: now,
		Containers: []breaklogs.ManifestContainer{{Name: "app", Restarts: 3, StartedAt: start, PreviousStartedAt: start.Add(-30 * time.Second), TerminationAt: end, CurrentTermination: true}},
	}
	query := harness.commit(t, end, now, pod)
	query.Limit = 500
	// Retained source history includes both older and later unpinned restarts.
	// Neither may consume the excerpt intended for this terminated incarnation.
	for i := 0; i < 100; i++ {
		harness.backend.rows = append(harness.backend.rows,
			providerRow{start.Add(-time.Second).Add(time.Duration(i)), pod.Name, "app", "older restart"},
			providerRow{end.Add(2 * time.Second).Add(time.Duration(i)), pod.Name, "app", "later restart"})
	}
	native := breaklogs.Snapshot{Namespace: pod.Namespace, RootUID: pod.RootUID, Pod: pod.Name, PodUID: pod.UID, PodCreatedAt: pod.CreatedAt, Container: "app", Slot: "previous", Restarts: 3, TerminationAt: end, CapturedAt: now}
	for i := 0; i < 2000; i++ {
		at := start.Add(time.Duration(i+1) * time.Microsecond)
		text := fmt.Sprintf("burst-line seq=%d", i)
		harness.backend.rows = append(harness.backend.rows, providerRow{at, pod.Name, "app", text})
		if i >= 1500 {
			native.Lines = append(native.Lines, breaklogs.Line{At: at, Text: text})
		}
	}
	native.From, native.To = native.Lines[0].At, native.Lines[len(native.Lines)-1].At
	native.ID = breaklogs.Identity(native)
	scope := harness.service.ActiveScope()
	store := kubechanges.NewStore(harness.provider, kubeindex.Scope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, CredentialID: scope.CredentialID})
	if err := store.CommitBreakLogs([]breaklogs.Snapshot{native}, nil, "", "", now); err != nil {
		t.Fatal(err)
	}
	page := readPage(t, harness, query)
	// The UI's first chronological page must already expose the linked head.
	if len(page.Lines) != 500 || page.Lines[0].Source != "linked" || page.Lines[0].Text != "burst-line seq=0" || page.Next == "" {
		t.Fatalf("first UI page hid linked head: lines=%d first=%+v next=%q", len(page.Lines), page.Lines[0], page.Next)
	}
	firstPage := page
	query.Cursor = page.Next
	page = readPage(t, harness, query)
	page.Lines = append(firstPage.Lines, page.Lines...)
	linked, captured := 0, 0
	seen := map[string]bool{}
	for _, line := range page.Lines {
		key := line.At.Format(time.RFC3339Nano) + line.Text
		if seen[key] || line.PodUID != pod.UID || line.Slot != "previous" || strings.Contains(line.Text, "restart") {
			t.Fatalf("duplicate or unpinned line: %+v", line)
		}
		seen[key] = true
		if line.Source == "linked" {
			linked++
		} else if line.Source == "captured" {
			captured++
		}
	}
	if linked != 50 || captured != 500 || page.Counts.All != 550 || page.Coverage.State == "complete" {
		t.Fatalf("not a true bounded head+tail merge: linked=%d native=%d counts=%+v coverage=%+v", linked, captured, page.Counts, page.Coverage)
	}
	for _, gap := range page.Coverage.Gaps {
		if gap.Reason == "linked_pending" {
			t.Fatalf("ready excerpt stayed pending: %+v", page.Coverage)
		}
	}
}

// Every supported provider, read through the real adapter and the real detail
// path, attributes a reused Pod name to the right UID by lifetime, keeps init
// and container identity, derives previous/current slots consistently with
// the mode filter, and scrubs canaries.
func TestLinkedProvidersDeletedReusedPodIdentityAndModes(t *testing.T) {
	for _, kind := range linkedProviderKinds {
		t.Run(kind, func(t *testing.T) {
			zero := 0
			harness := newLinkedProviderHarness(t, kind, &zero)
			t0 := time.Now().UTC().Truncate(time.Millisecond)
			old, replacement := linkedReusedPods(t0)
			harness.backend.rows = []providerRow{
				{t0.Add(-8*time.Minute - 30*time.Second), "db-0", "app", "old instance crash LINKED_CANARY"},
				{t0.Add(-7 * time.Minute), "db-0", "app", "old instance restarted"},
				{t0.Add(-290 * time.Second), "db-0", "migrate", "schema migration ran"},
				{t0.Add(-3 * time.Minute), "db-0", "app", "replacement failing"},
			}
			query := harness.commit(t, t0.Add(-4*time.Minute), t0, old, replacement)
			page := readPage(t, harness, query)
			type identity struct {
				uid, container, slot string
				init                 bool
			}
			got := map[string]identity{}
			for _, line := range page.Lines {
				got[line.Text] = identity{line.PodUID, line.Container, line.Slot, line.Init}
				if line.Source != "linked" || strings.Contains(line.Text, "LINKED_CANARY") {
					t.Fatalf("unsafe or unlabelled line: %+v", line)
				}
			}
			want := map[string]identity{
				"old instance crash [redacted]": {"uid-old", "app", "previous", false},
				"old instance restarted":        {"uid-old", "app", "current", false},
				"replacement failing":           {"uid-new", "app", "current", false},
			}
			for text, expected := range want {
				if got[text] != expected {
					t.Errorf("%q: got %+v want %+v", text, got[text], expected)
				}
			}
			initLine, found := got["schema migration ran"]
			if !found || initLine.uid != "uid-new" || initLine.container != "migrate" || !initLine.init {
				t.Fatalf("init container identity lost: %+v (lines %+v, gaps %v)", initLine, page.Lines, pageReasons(page))
			}
			t.Logf("completed init container line reported in slot %q", initLine.slot)
			for _, mode := range []string{"previous", "current"} {
				moded := query
				moded.Mode = mode
				page := readPage(t, harness, moded)
				for _, line := range page.Lines {
					if line.Slot != mode {
						t.Fatalf("mode %s returned %s line %q", mode, line.Slot, line.Text)
					}
				}
				count := 0
				for _, line := range got {
					if line.slot == mode {
						count++
					}
				}
				if len(page.Lines) != count {
					t.Fatalf("mode %s returned %d lines, merged view has %d", mode, len(page.Lines), count)
				}
			}
			initOnly := query
			initOnly.Container = "migrate"
			page = readPage(t, harness, initOnly)
			if len(page.Lines) != 1 || !page.Lines[0].Init || page.Lines[0].Container != "migrate" {
				t.Fatalf("init container filter: %+v", page.Lines)
			}
		})
	}
}

// When the window holds more lines than the provider returns per request,
// each provider must report truncation so the reader splits the window, stops
// at three queries, and labels the remainder as budget instead of complete.
func TestLinkedProvidersTruncationStopsAtBudgetAndNeverClaimsComplete(t *testing.T) {
	for _, kind := range linkedProviderKinds {
		t.Run(kind, func(t *testing.T) {
			zero := 0
			harness := newLinkedProviderHarness(t, kind, &zero)
			harness.backend.nextCursor = true
			t0 := time.Now().UTC().Truncate(time.Millisecond)
			_, replacement := linkedReusedPods(t0)
			for position := 0; position < 120; position++ {
				harness.backend.rows = append(harness.backend.rows, providerRow{t0.Add(-270*time.Second + time.Duration(position)*1250*time.Millisecond), "db-0", "app", fmt.Sprintf("burst line %03d", position)})
			}
			query := harness.commit(t, t0.Add(-4*time.Minute), t0, replacement)
			page := readPage(t, harness, query)
			calls, requests := harness.backend.counts()
			t.Logf("%s: %d lines, %d logical queries, %d raw requests, state=%s gaps=%v", kind, len(page.Lines), calls, requests, page.Coverage.State, pageReasons(page))
			for _, line := range page.Lines {
				if line.PodUID != "uid-new" {
					t.Fatalf("burst line attributed to %s", line.PodUID)
				}
			}
			if len(page.Lines) >= 120 {
				t.Fatalf("returned all %d lines; truncation not exercised", len(page.Lines))
			}
			budget := false
			for _, gap := range page.Coverage.Gaps {
				budget = budget || gap.Reason == "budget"
			}
			if page.Coverage.State == "complete" || !budget {
				t.Fatalf("%d of 120 lines returned but coverage=%s gaps=%v: provider truncation not detected", len(page.Lines), page.Coverage.State, pageReasons(page))
			}
			if kind == "cloudwatchlogs" && requests > 9 {
				t.Fatalf("CloudWatch issued %d API pages for one detail read", requests)
			}
		})
	}
}

// With no operator override, each provider type gets its built-in ingestion
// delay: an empty answer is final no_logs only after window end + delay.
func TestLinkedProvidersBuiltInIngestDelayPendingVersusNoLogs(t *testing.T) {
	delays := map[string]time.Duration{"loki": 30 * time.Second, "elasticsearch": time.Minute, "graylog": time.Minute, "signoz": time.Minute, "splunk": 2 * time.Minute, "cloudwatchlogs": 2 * time.Minute}
	for _, kind := range linkedProviderKinds {
		t.Run(kind, func(t *testing.T) {
			harness := newLinkedProviderHarness(t, kind, nil)
			t0 := time.Now().UTC().Truncate(time.Millisecond)
			_, replacement := linkedReusedPods(t0)
			replacement.LastSeen, replacement.DeletedAt = t0.Add(-30*time.Second), t0.Add(-30*time.Second)
			breakAt := t0.Add(-3*time.Minute - 30*time.Second)
			query := harness.commit(t, breakAt, t0, replacement)
			page := readPage(t, harness, query)
			ready := query.To.Add(delays[kind])
			pending, noLogs := false, false
			for _, gap := range page.Coverage.Gaps {
				pending = pending || gap.Reason == "linked_pending" && gap.ReadyAt.Equal(ready)
				noLogs = noLogs || gap.Reason == "no_logs"
			}
			if ready.After(time.Now()) {
				if !pending || noLogs || page.Coverage.Reason != "linked_pending" {
					t.Fatalf("before ready %s: reason=%s gaps=%+v", ready, page.Coverage.Reason, page.Coverage.Gaps)
				}
			} else if !noLogs || pending {
				t.Fatalf("after ready %s: reason=%s gaps=%+v", ready, page.Coverage.Reason, page.Coverage.Gaps)
			}
		})
	}
}

// CloudWatch pagination: an empty final page ends a complete read, while a
// replayed token or an endless run of empty pages is reported as truncated.
func TestLinkedCloudWatchEmptyAndReplayedPages(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	rows := []providerRow{{t0.Add(-200 * time.Second), "db-0", "app", "page one a"}, {t0.Add(-190 * time.Second), "db-0", "app", "page one b"}}
	for _, scenario := range []struct {
		name     string
		pages    map[string]cloudPage
		complete bool
	}{
		{"empty-last-page", map[string]cloudPage{"": {rows, "p2"}, "p2": {}}, true},
		{"replayed-token", map[string]cloudPage{"": {rows, "p2"}, "p2": {nil, "p2"}}, false},
		{"empty-pages-forever", map[string]cloudPage{"": {rows, "p2"}, "p2": {nil, "p3"}, "p3": {nil, "p4"}, "p4": {nil, "p5"}}, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			zero := 0
			harness := newLinkedProviderHarness(t, "cloudwatchlogs", &zero)
			harness.backend.pages = scenario.pages
			_, replacement := linkedReusedPods(t0)
			query := harness.commit(t, t0.Add(-4*time.Minute), t0, replacement)
			page := readPage(t, harness, query)
			calls, requests := harness.backend.counts()
			budget := false
			for _, gap := range page.Coverage.Gaps {
				budget = budget || gap.Reason == "budget"
			}
			t.Logf("%s: lines=%d calls=%d pages=%d gaps=%v", scenario.name, len(page.Lines), calls, requests, pageReasons(page))
			if len(page.Lines) != 2 {
				t.Fatalf("lines from first page lost: %+v", page.Lines)
			}
			if scenario.complete && budget || !scenario.complete && !budget {
				t.Fatalf("complete=%v but budget gap=%v (%v)", scenario.complete, budget, pageReasons(page))
			}
		})
	}
}

// A break window with more than three verified Pods is read for every Pod by
// every provider through the real detail path, within the three-call budget.
func TestLinkedProvidersMultiPodWindowReadsEveryVerifiedPod(t *testing.T) {
	for _, kind := range linkedProviderKinds {
		t.Run(kind, func(t *testing.T) {
			zero := 0
			harness := newLinkedProviderHarness(t, kind, &zero)
			t0 := time.Now().UTC().Truncate(time.Millisecond)
			pods := []breaklogs.ManifestPod{}
			for position := 0; position < 5; position++ {
				name := fmt.Sprintf("db-%d", position)
				pods = append(pods, breaklogs.ManifestPod{Namespace: "shop", RootUID: "root-db", Name: name, UID: "uid-" + name, CreatedAt: t0.Add(-time.Hour), LastSeen: t0.Add(-60 * time.Second), DeletedAt: t0.Add(-60 * time.Second), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-time.Hour)}}})
				harness.backend.rows = append(harness.backend.rows, providerRow{t0.Add(-200*time.Second + time.Duration(position)*time.Second), name, "app", "failure from " + name})
			}
			query := harness.commit(t, t0.Add(-4*time.Minute), t0, pods...)
			page := readPage(t, harness, query)
			calls, requests := harness.backend.counts()
			t.Logf("%s: %d lines, %d logical queries, %d raw requests, state=%s gaps=%v", kind, len(page.Lines), calls, requests, page.Coverage.State, pageReasons(page))
			seen := map[string]bool{}
			for _, line := range page.Lines {
				if line.Text != "failure from "+strings.TrimPrefix(line.PodUID, "uid-") {
					t.Fatalf("line attributed to the wrong Pod: %+v", line)
				}
				seen[line.PodUID] = true
			}
			for _, pod := range pods {
				if !seen[pod.UID] {
					t.Errorf("verified Pod %s never read (lines %d, gaps %v)", pod.Name, len(page.Lines), pageReasons(page))
				}
				if calls != 1 || requests > map[string]int{"loki": 1, "elasticsearch": 1, "cloudwatchlogs": 3, "graylog": 3, "splunk": 6, "signoz": 1}[kind] {
					t.Fatalf("multi-Pod query was not genuinely batched/bounded: calls=%d raw=%d", calls, requests)
				}
				harness.backend.mu.Lock()
				queries := append([]string(nil), harness.backend.queries...)
				harness.backend.mu.Unlock()
				if len(queries) != 1 {
					t.Fatalf("expected one native selection query, got %v", queries)
				}
				for _, pod := range pods {
					if !strings.Contains(queries[0], pod.Name) {
						t.Errorf("native query omitted verified Pod %s: %s", pod.Name, queries[0])
					}
				}
			}
			for _, gap := range page.Coverage.Gaps {
				if gap.Reason == "budget" {
					t.Errorf("five small Pods exhausted the budget: %v", pageReasons(page))
				}
			}
		})
	}
}

// A non-auth provider error before the window is ingested keeps the panel
// pending with ready_at (the open panel retries) and is reported, not hidden;
// after recovery past ready_at the line appears and nothing stays pending.
func TestLinkedProvidersErrorBeforeReadyStaysPendingAndRecovers(t *testing.T) {
	for _, kind := range linkedProviderKinds {
		t.Run(kind, func(t *testing.T) {
			delay := 123
			harness := newLinkedProviderHarness(t, kind, &delay)
			harness.backend.fail = true
			t0 := time.Now().UTC().Truncate(time.Millisecond)
			_, replacement := linkedReusedPods(t0)
			harness.backend.rows = []providerRow{{t0.Add(-3 * time.Minute), "db-0", "app", "replacement failing"}}
			query := harness.commit(t, t0.Add(-4*time.Minute), t0, replacement)
			ready := query.To.Add(time.Duration(delay) * time.Second)
			page := readPage(t, harness, query)
			pending, failure := false, ""
			for _, gap := range page.Coverage.Gaps {
				pending = pending || gap.Reason == "linked_pending" && gap.ReadyAt.Equal(ready)
				if gap.Reason != "linked_pending" && gap.Reason != "pending" && gap.Reason != "budget" {
					failure = gap.Reason
				}
			}
			t.Logf("%s before ready: state=%s reason=%s gaps=%v", kind, page.Coverage.State, page.Coverage.Reason, pageReasons(page))
			if !pending || page.Coverage.Reason != "linked_pending" || page.Coverage.State == "complete" {
				t.Fatalf("provider error before ready stopped polling: reason=%s gaps=%+v", page.Coverage.Reason, page.Coverage.Gaps)
			}
			if failure != "unavailable" {
				t.Fatalf("provider error was hidden or shown as no_logs: gaps=%v", pageReasons(page))
			}
			harness.backend.mu.Lock()
			harness.backend.fail = false
			harness.backend.mu.Unlock()
			harness.now = ready.Add(500 * time.Millisecond)
			page = readPage(t, harness, query)
			t.Logf("%s after ready: lines=%d state=%s gaps=%v", kind, len(page.Lines), page.Coverage.State, pageReasons(page))
			if len(page.Lines) != 1 || page.Lines[0].PodUID != "uid-new" {
				t.Fatalf("recovered provider line missing after ready: %+v gaps=%v", page.Lines, pageReasons(page))
			}
			for _, gap := range page.Coverage.Gaps {
				if gap.Reason == "linked_pending" {
					t.Fatalf("still pending after ready and recovery: %+v", page.Coverage.Gaps)
				}
			}
		})
	}
}
