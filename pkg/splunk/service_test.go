package splunk

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

type liveJobTrace struct {
	base       http.RoundTripper
	cancel     context.CancelFunc
	requests   int
	statuses   []int
	methods    []string
	paths      []string
	deletePath string
	failStatus bool
}

func (trace *liveJobTrace) RoundTrip(request *http.Request) (*http.Response, error) {
	var response *http.Response
	var err error
	if trace.failStatus && request.Method == http.MethodGet && trace.requests == 1 {
		response = &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: request}
	} else {
		response, err = trace.base.RoundTrip(request)
	}
	trace.requests++
	trace.methods = append(trace.methods, request.Method)
	trace.paths = append(trace.paths, request.URL.Path)
	if err != nil {
		trace.statuses = append(trace.statuses, 0)
		return nil, err
	}
	trace.statuses = append(trace.statuses, response.StatusCode)
	if request.Method == http.MethodDelete {
		trace.deletePath = request.URL.Path
	}
	if request.Method == http.MethodPost && trace.cancel != nil {
		trace.cancel()
	}
	return response, nil
}

func TestLiveSplunkJobLifecycle(t *testing.T) {
	address, password := os.Getenv("HARNESS_SPLUNK_URL"), os.Getenv("HARNESS_SPLUNK_PASSWORD")
	if address == "" || password == "" {
		t.Skip("HARNESS_SPLUNK_URL and HARNESS_SPLUNK_PASSWORD required")
	}
	service, err := NewService(config.AgentSplunkSourceConfig{
		Address: address, Search: "search index=main", Username: "admin", Password: password,
		InsecureSkipVerify: true,
	}, nil)
	if err != nil {
		t.Fatalf("construct live service: %v", err)
	}
	for _, endpoint := range []string{"/services/server/info?output_mode=json", "/services/data/inputs/http/http?output_mode=json"} {
		request, requestErr := http.NewRequest(http.MethodGet, address+endpoint, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.SetBasicAuth("admin", password)
		response, requestErr := service.client.Do(request)
		if requestErr != nil {
			t.Fatalf("backend metadata request failed: %v", requestErr)
		}
		var metadata struct {
			Entry []struct {
				Content struct {
					Version   string `json:"version"`
					Disabled  any    `json:"disabled"`
					EnableSSL any    `json:"enableSSL"`
				} `json:"content"`
			} `json:"entry"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&metadata)
		response.Body.Close()
		if decodeErr != nil || len(metadata.Entry) == 0 {
			t.Fatalf("backend metadata: status=%d valid=%t", response.StatusCode, decodeErr == nil)
		}
		if strings.Contains(endpoint, "server/info") {
			t.Logf("backend version=%s status=%d", metadata.Entry[0].Content.Version, response.StatusCode)
		} else {
			t.Logf("HEC disabled=%v SSL=%v status=%d", metadata.Entry[0].Content.Disabled, metadata.Entry[0].Content.EnableSSL, response.StatusCode)
		}
	}
	for _, operation := range []string{"read", "discover", "cancel", "backend_error"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			trace := &liveJobTrace{base: service.client.Transport}
			if operation == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				trace.cancel = cancel
			}
			trace.failStatus = operation == "backend_error"
			service.client.Transport = trace
			defer func() { service.client.Transport = trace.base }()
			if operation == "discover" {
				result, discoverErr := service.Discover(ctx, DiscoveryRequest{Limit: 20})
				if discoverErr != nil || result.Count == 0 || result.Coverage != "recent_sample" {
					t.Errorf("sampled discovery: count=%d coverage=%s err=%v", result.Count, result.Coverage, discoverErr)
				}
			} else {
				result, readErr := service.Read(ctx, ReadRequest{Limit: 20})
				if operation == "cancel" {
					if !errors.Is(readErr, context.Canceled) || result.Count != 0 {
						t.Errorf("canceled read: count=%d err=%v", result.Count, readErr)
					}
				} else if operation == "backend_error" {
					if !errors.Is(readErr, ErrRead) || result.Count != 0 {
						t.Errorf("failed read: count=%d err=%v", result.Count, readErr)
					}
				} else if readErr != nil || result.Count < 1 {
					t.Errorf("live read: count=%d err=%v", result.Count, readErr)
				}
			}
			if trace.requests > maximumRequests || len(trace.methods) < 2 || trace.methods[0] != http.MethodPost || trace.methods[len(trace.methods)-1] != http.MethodDelete || trace.deletePath == "" || trace.statuses[len(trace.statuses)-1]/100 != 2 {
				t.Errorf("job lifecycle: calls=%d methods=%v statuses=%v deleted=%t", trace.requests, trace.methods, trace.statuses, trace.deletePath != "")
			}
			if trace.deletePath != "" {
				check, checkErr := http.NewRequest(http.MethodGet, address+trace.deletePath+"?output_mode=json", nil)
				if checkErr != nil {
					t.Fatal(checkErr)
				}
				check.SetBasicAuth("admin", password)
				response, checkErr := trace.base.RoundTrip(check)
				if checkErr != nil {
					t.Errorf("verify deleted job: %v", checkErr)
				} else {
					response.Body.Close()
					if response.StatusCode != http.StatusNotFound {
						t.Errorf("deleted job status=%d, want 404", response.StatusCode)
					}
				}
			}
			t.Logf("job lifecycle: calls=%d methods=%v statuses=%v deleted=%t", trace.requests, trace.methods, trace.statuses, trace.deletePath != "")
		})
	}
}

func TestLiveSplunkRestrictedRole(t *testing.T) {
	address, adminPassword := os.Getenv("HARNESS_SPLUNK_URL"), os.Getenv("HARNESS_SPLUNK_PASSWORD")
	if address == "" || adminPassword == "" {
		t.Skip("HARNESS_SPLUNK_URL and HARNESS_SPLUNK_PASSWORD required")
	}
	admin, err := NewService(config.AgentSplunkSourceConfig{
		Address: address, Search: "index=main", Username: "admin", Password: adminPassword, InsecureSkipVerify: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf("qa_splunk_%x", random[:4])
	username := role
	password := fmt.Sprintf("Qa1!%x", random[:])
	adminRequest := func(method, path string, form url.Values) (int, string) {
		t.Helper()
		var body *strings.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		} else {
			body = strings.NewReader("")
		}
		request, requestErr := http.NewRequest(method, address+path, body)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.SetBasicAuth("admin", adminPassword)
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		response, requestErr := admin.client.Do(request)
		if requestErr != nil {
			t.Fatalf("role REST request failed: %v", requestErr)
		}
		defer response.Body.Close()
		var diagnostic string
		if response.StatusCode/100 != 2 && form != nil && path == "/services/authorization/roles" {
			var failure struct {
				Messages []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"messages"`
			}
			if json.NewDecoder(io.LimitReader(response.Body, 2048)).Decode(&failure) == nil && len(failure.Messages) > 0 {
				diagnostic = strings.Map(func(character rune) rune {
					if character < 32 || character == 127 {
						return -1
					}
					return character
				}, strings.ReplaceAll(failure.Messages[0].Text, role, "[qa-role]"))
				if len(diagnostic) > 240 {
					diagnostic = diagnostic[:240]
				}
			}
		}
		return response.StatusCode, diagnostic
	}
	rolePath := "/services/authorization/roles/" + role
	userPath := "/services/authentication/users/" + username
	roleForm := url.Values{"name": {role}, "capabilities": {"search"}, "srchIndexesAllowed": {"main"}, "srchIndexesDefault": {"main"}}
	roleStatus, diagnostic := adminRequest(http.MethodPost, "/services/authorization/roles", roleForm)
	if roleStatus/100 != 2 {
		t.Fatalf("QA role creation status=%d diagnostic=%q", roleStatus, diagnostic)
	}
	defer func() {
		if status, _ := adminRequest(http.MethodDelete, rolePath, nil); status/100 != 2 {
			t.Errorf("QA role cleanup status=%d", status)
		}
	}()
	readContent := func(path string) map[string]json.RawMessage {
		t.Helper()
		request, requestErr := http.NewRequest(http.MethodGet, address+path+"?output_mode=json", nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.SetBasicAuth("admin", adminPassword)
		response, requestErr := admin.client.Do(request)
		if requestErr != nil {
			t.Fatalf("role readback request: %v", requestErr)
		}
		defer response.Body.Close()
		var result struct {
			Entry []struct {
				Content map[string]json.RawMessage `json:"content"`
			} `json:"entry"`
		}
		if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&result) != nil || len(result.Entry) != 1 {
			t.Fatalf("role readback status=%d entries=%d", response.StatusCode, len(result.Entry))
		}
		return result.Entry[0].Content
	}
	verifyRole := func() bool {
		t.Helper()
		roleContent := readContent(rolePath)
		fields := make(map[string][]string)
		for _, field := range []string{"srchIndexesAllowed", "srchIndexesDefault", "imported_roles"} {
			var items []string
			if err := json.Unmarshal(roleContent[field], &items); err != nil {
				t.Fatalf("role readback invalid %s: %v", field, err)
			}
			fields[field] = items
		}
		var capabilities []string
		if err := json.Unmarshal(roleContent["capabilities"], &capabilities); err != nil {
			t.Fatalf("role readback invalid capabilities: %v", err)
		}
		t.Logf("effective role capabilities=%v imported roles=%v allowed indexes=%v default indexes=%v", capabilities, fields["imported_roles"], fields["srchIndexesAllowed"], fields["srchIndexesDefault"])
		return len(capabilities) == 1 && capabilities[0] == "search" && len(fields["imported_roles"]) == 0 && len(fields["srchIndexesAllowed"]) == 1 && fields["srchIndexesAllowed"][0] == "main" && len(fields["srchIndexesDefault"]) == 1 && fields["srchIndexesDefault"][0] == "main"
	}
	if !verifyRole() {
		update := url.Values{"capabilities": {"search"}, "srchIndexesAllowed": {"main"}, "srchIndexesDefault": {"main"}}
		if status, _ := adminRequest(http.MethodPost, rolePath, update); status/100 != 2 {
			t.Logf("QA role update status=%d", status)
		}
		if !verifyRole() {
			t.Fatal("BLOCKED: QA role effective scope exceeds search-only, main-only, no-imported-roles after update")
		}
	}
	userStatus, _ := adminRequest(http.MethodPost, "/services/authentication/users", url.Values{"name": {username}, "password": {password}, "roles": {role}})
	if userStatus/100 != 2 {
		t.Fatalf("QA user creation status=%d", userStatus)
	}
	defer func() {
		if status, _ := adminRequest(http.MethodDelete, userPath, nil); status/100 != 2 {
			t.Errorf("QA user cleanup status=%d", status)
		}
	}()
	userContent := readContent(userPath)
	var assigned []string
	if json.Unmarshal(userContent["roles"], &assigned) != nil || len(assigned) != 1 || assigned[0] != role {
		t.Fatalf("QA user roles not exclusively assigned to QA role (count=%d)", len(assigned))
	}
	newScopedService := func(index, name, secret string) *Service {
		t.Helper()
		service, serviceErr := NewService(config.AgentSplunkSourceConfig{Address: address, Search: "index=" + index, Username: name, Password: secret, InsecureSkipVerify: true}, nil)
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		return service
	}
	checkLifecycle := func(service *Service, operation string, positive bool) (int, int) {
		t.Helper()
		trace := &liveJobTrace{base: service.client.Transport}
		service.client.Transport = trace
		var count int
		var operationErr error
		if operation == "discover" {
			result, discoverErr := service.Discover(context.Background(), DiscoveryRequest{Limit: 5})
			count, operationErr = result.Count, discoverErr
			if positive && result.Coverage != "recent_sample" {
				t.Errorf("restricted discovery coverage=%s", result.Coverage)
			}
		} else {
			result, readErr := service.Read(context.Background(), ReadRequest{Limit: 5})
			count, operationErr = result.Count, readErr
		}
		deleteStatus := 0
		post, status, results := false, false, false
		for index, method := range trace.methods {
			path := ""
			if index < len(trace.paths) {
				path = trace.paths[index]
			}
			if method == http.MethodPost && trace.statuses[index]/100 == 2 {
				post = true
			}
			if method == http.MethodGet && strings.Contains(path, "/v2/jobs/") && trace.statuses[index] == http.StatusOK {
				results = true
			}
			if method == http.MethodGet && strings.Contains(path, "/search/jobs/") && !strings.Contains(path, "/v2/jobs/") && trace.statuses[index] == http.StatusOK {
				status = true
			}
			if method == http.MethodDelete {
				deleteStatus = trace.statuses[index]
			}
		}
		if positive && deleteStatus == http.StatusForbidden {
			t.Logf("restricted %s own-job DELETE status=%d; testing scoped fallback", operation, deleteStatus)
			return count, deleteStatus
		}
		if !post || !status || !results || deleteStatus/100 != 2 || trace.deletePath == "" || trace.requests > maximumRequests {
			t.Errorf("restricted %s lifecycle calls=%d post=%t status=%t results=%t delete_status=%d", operation, trace.requests, post, status, results, deleteStatus)
		}
		if trace.deletePath != "" {
			request, requestErr := http.NewRequest(http.MethodGet, address+trace.deletePath+"?output_mode=json", nil)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			request.SetBasicAuth(service.config.Username, service.config.Password)
			response, requestErr := trace.base.RoundTrip(request)
			if requestErr != nil {
				t.Errorf("deleted own job check: %v", requestErr)
			} else {
				response.Body.Close()
				if response.StatusCode != http.StatusNotFound {
					t.Errorf("deleted own job GET status=%d, want 404", response.StatusCode)
				}
			}
		}
		if positive && (operationErr != nil || count < 1) {
			t.Errorf("restricted %s count=%d err=%v", operation, count, operationErr)
		}
		if !positive && count != 0 {
			t.Errorf("restricted other index exposed count=%d", count)
		}
		t.Logf("restricted %s count=%d calls=%d delete_status=%d", operation, count, trace.requests, deleteStatus)
		return count, deleteStatus
	}
	mainService := newScopedService("main", username, password)
	checkLifecycle(mainService, "read", true)
	checkLifecycle(mainService, "discover", true)
	adminInternal := newScopedService("_internal", "admin", adminPassword)
	adminResult, adminErr := adminInternal.Read(context.Background(), ReadRequest{Limit: 5})
	if adminErr != nil || adminResult.Count == 0 {
		t.Fatalf("admin _internal control count=%d err=%v", adminResult.Count, adminErr)
	}
	checkLifecycle(newScopedService("_internal", username, password), "read", false)
}

func TestSearchJobScopeAndDeletion(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.Method+" "+request.URL.Path)
		switch request.Method + " " + request.URL.Path {
		case "POST /servicesNS/admin/ops/search/jobs":
			search := request.FormValue("search")
			if (search != "search index=main AND service=api AND host=node-1" && search != "search index=main AND service=api") || request.Header.Get("Authorization") != "Bearer secret" || request.FormValue("auto_cancel") != "10" || request.FormValue("ttl") != "10" || request.FormValue("max_time") != "8" {
				t.Errorf("unexpected search: %v", request.Form)
			}
			fmt.Fprint(writer, `{"sid":"1740000000.42"}`)
		case "GET /servicesNS/admin/ops/search/jobs/1740000000.42":
			fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
		case "GET /servicesNS/admin/ops/search/v2/jobs/1740000000.42/results":
			if (request.URL.Query().Get("count") != "2" && request.URL.Query().Get("count") != "50") || len(request.URL.Query()["f"]) != len(resultFields) {
				t.Error("missing result cap or field projection")
			}
			fmt.Fprint(writer, `{"results":[{"index":"main","_time":"2026-09-28T00:00:00Z","_raw":"hello","host":"node-1"}]}`)
		case "DELETE /servicesNS/admin/ops/search/jobs/1740000000.42":
			writer.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "search index=main AND service=api", Owner: "admin", App: "ops", Token: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Read(context.Background(), ReadRequest{Filters: map[string]string{"host": "node-1"}, Limit: 2})
	if err != nil || result.Count != 1 || result.Records[0].Message != "hello" || len(paths) != 4 || !strings.HasPrefix(paths[3], "DELETE ") {
		t.Fatalf("result=%+v err=%v paths=%v", result, err, paths)
	}
	paths = nil
	fields, err := service.Discover(context.Background(), DiscoveryRequest{Limit: 2})
	if err != nil || fields.Coverage != "recent_sample" || !fields.Truncated || len(paths) != 4 {
		t.Fatalf("discovery=%+v err=%v paths=%v", fields, err, paths)
	}
}

func TestResultRowsRequireExactIndexBeforeExposure(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		row     string
		allowed bool
	}{
		{"missing", `{"_raw":"second","host":"node-2"}`, false},
		{"non_string", `{"index":42,"_raw":"second","host":"node-2"}`, false},
		{"blank", `{"index":"  ","_raw":"second","host":"node-2"}`, false},
		{"wrong", `{"index":"other","_raw":"second","host":"node-2"}`, false},
		{"correct", `{"index":"main","_raw":"second","host":"node-2"}`, true},
		{"uppercase", `{"index":"MAIN","_raw":"second","host":"node-2"}`, true},
	} {
		for _, operation := range []string{"read", "discover"} {
			t.Run(operation+"_"+scenario.name, func(t *testing.T) {
				var paths []string
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					paths = append(paths, request.Method+" "+request.URL.Path)
					switch {
					case request.Method == http.MethodPost:
						fmt.Fprint(writer, `{"sid":"123.456"}`)
					case request.Method == http.MethodDelete:
						writer.WriteHeader(http.StatusOK)
					case strings.HasSuffix(request.URL.Path, "/results"):
						fmt.Fprintf(writer, `{"results":[{"index":"main","_raw":"first","host":"node-1"},%s]}`, scenario.row)
					default:
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
					}
				}))
				defer server.Close()
				service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if operation == "read" {
					result, readErr := service.Read(context.Background(), ReadRequest{Limit: 2})
					if scenario.allowed {
						if readErr != nil || result.Count != 2 || len(result.Records) != 2 || result.Records[1].Message != "second" {
							t.Fatalf("read=%+v err=%v", result, readErr)
						}
					} else if !errors.Is(readErr, ErrRead) || result.Count != 0 || len(result.Records) != 0 {
						t.Fatalf("exposed invalid read: result=%+v err=%v", result, readErr)
					}
				} else {
					result, discoverErr := service.Discover(context.Background(), DiscoveryRequest{})
					if scenario.allowed {
						if discoverErr != nil || result.Count == 0 || len(result.Fields) == 0 {
							t.Fatalf("discovery=%+v err=%v", result, discoverErr)
						}
					} else if !errors.Is(discoverErr, ErrRead) || result.Count != 0 || len(result.Fields) != 0 {
						t.Fatalf("exposed invalid fields: result=%+v err=%v", result, discoverErr)
					}
				}
				if len(paths) != 4 || paths[3] != "DELETE /services/search/jobs/123.456" {
					t.Fatalf("job not deleted after results: %v", paths)
				}
			})
		}
	}
}

func TestSearchJobCompletesAfterSevenHundredMilliseconds(t *testing.T) {
	started := time.Now()
	deleted := false
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		switch request.Method {
		case http.MethodPost:
			fmt.Fprint(writer, `{"sid":"123.456"}`)
		case http.MethodDelete:
			deleted = true
		case http.MethodGet:
			if strings.HasSuffix(request.URL.Path, "/results") {
				fmt.Fprint(writer, `{"results":[]}`)
			} else {
				fmt.Fprintf(writer, `{"entry":[{"content":{"isDone":%t}}]}`, time.Since(started) >= 700*time.Millisecond)
			}
		}
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Read(context.Background(), ReadRequest{Limit: 1})
	if err != nil || result.Count != 0 || !deleted || requests > maximumRequests || time.Since(started) >= 9*time.Second {
		t.Fatalf("result=%+v err=%v deleted=%v requests=%d elapsed=%v", result, err, deleted, requests, time.Since(started))
	}
}

func TestSlowJobRespectsRequestBudgetAndDeletes(t *testing.T) {
	requests := 0
	posts := 0
	polls := 0
	results := 0
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		switch {
		case request.Method == http.MethodPost:
			posts++
			fmt.Fprint(writer, `{"sid":"123.456"}`)
		case request.Method == http.MethodDelete:
			deletes++
		case strings.HasSuffix(request.URL.Path, "/results"):
			results++
			fmt.Fprint(writer, `{"results":[]}`)
		default:
			polls++
			fmt.Fprint(writer, `{"entry":[{"content":{"isDone":false}}]}`)
		}
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = service.Read(context.Background(), ReadRequest{})
	if !errors.Is(err, ErrRead) || strings.Contains(fmt.Sprint(err), "123.456") || requests != maximumRequests || posts != 1 || polls != 4 || results != 0 || deletes != 1 || time.Since(started) >= 9*time.Second {
		t.Fatalf("slow job: err=%v requests=%d posts=%d polls=%d results=%d deletes=%d elapsed=%v", err, requests, posts, polls, results, deletes, time.Since(started))
	}
}

func TestJobFinishesWithoutResultsBudget(t *testing.T) {
	polls := 0
	results := 0
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost:
			fmt.Fprint(writer, `{"sid":"123.456"}`)
		case request.Method == http.MethodDelete:
			deletes++
		case strings.HasSuffix(request.URL.Path, "/results"):
			results++
			fmt.Fprint(writer, `{"results":[]}`)
		default:
			polls++
			fmt.Fprintf(writer, `{"entry":[{"content":{"isDone":%t}}]}`, polls == 4)
		}
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Read(context.Background(), ReadRequest{})
	if !errors.Is(err, ErrRead) || polls != 4 || results != 0 || deletes != 1 {
		t.Fatalf("late completion: err=%v polls=%d results=%d deletes=%d", err, polls, results, deletes)
	}
}

func TestCanceledJobStopsPollingAndDeletes(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		status string
	}{
		{"canceled_dispatch", `{"entry":[{"content":{"isDone":false,"dispatchState":"CANCELED"}}]}`},
		{"canceled_flag", `{"entry":[{"content":{"isDone":false,"isCanceled":true}}]}`},
		{"failed", `{"entry":[{"content":{"isDone":false,"isFailed":true}}]}`},
		{"finalized", `{"entry":[{"content":{"isDone":false,"isFinalized":true}}]}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			polls := 0
			results := 0
			deletes := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodPost:
					fmt.Fprint(writer, `{"sid":"123.456"}`)
				case request.Method == http.MethodDelete:
					deletes++
				case strings.HasSuffix(request.URL.Path, "/results"):
					results++
				default:
					polls++
					fmt.Fprint(writer, scenario.status)
				}
			}))
			defer server.Close()
			service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Read(context.Background(), ReadRequest{})
			if !errors.Is(err, ErrRead) || strings.Contains(fmt.Sprint(err), "123.456") || polls != 1 || results != 0 || deletes != 1 {
				t.Fatalf("terminal job: err=%v polls=%d results=%d deletes=%d", err, polls, results, deletes)
			}
		})
	}
}

func TestProjectedRowsFitWithoutInternalMetadata(t *testing.T) {
	rows := make([]map[string]any, 50)
	for index := range rows {
		rows[index] = map[string]any{"index": "main", "_time": "2026-09-28T00:00:00Z", "_raw": "ordinary message", "host": "node-1", "_si": []string{"splunk", "main"}, "_bkt": "bucket", "_cd": "123", "_serial": index}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost:
			fmt.Fprint(writer, `{"sid":"123.456"}`)
		case request.Method == http.MethodDelete:
			writer.WriteHeader(http.StatusOK)
		case strings.HasSuffix(request.URL.Path, "/results"):
			if len(request.URL.Query()["f"]) != len(resultFields) {
				t.Errorf("unbounded projection: %v", request.URL.Query())
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"results": rows})
		default:
			fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
		}
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=MAIN"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Read(context.Background(), ReadRequest{Limit: 50})
	if err != nil || result.Count != 50 || len(result.Truncation) != 1 || result.Truncation[0] != "row_limit" {
		t.Fatalf("50 projected rows: count=%d truncation=%v err=%v", result.Count, result.Truncation, err)
	}
	for _, record := range result.Records {
		if record.Message != "ordinary message" || len(record.Fields) != 2 || record.Fields["host"] != "node-1" {
			t.Fatalf("unexpected projected row: %+v", record)
		}
	}
}

func TestResponseHeaderCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Oversized", strings.Repeat("a", maximumResponseHeaderBytes))
		fmt.Fprint(writer, `{"sid":"123.456"}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Read(context.Background(), ReadRequest{}); !errors.Is(err, ErrRead) {
		t.Fatalf("oversized response header: %v", err)
	}
}

func TestNamespaceIsNotScrubbedButCredentialsAre(t *testing.T) {
	service, err := NewService(config.AgentSplunkSourceConfig{Address: "http://localhost:8089", Search: "index=main", Owner: "admin", App: "ops", Token: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := service.scrub("admin ops secret", 64); got != "admin ops [redacted]" {
		t.Fatalf("unexpected scrub: %q", got)
	}
}

func TestSearchJobFailuresAlwaysDelete(t *testing.T) {
	for _, scenario := range []string{"wrong_index", "poll_stalled", "oversized", "chunked", "canceled", "cleanup_failed", "malformed_status", "missing_status", "failed", "failed_dispatch", "preview", "finalized", "no_data", "false_done_no_data", "malformed_results", "missing_results"} {
		t.Run(scenario, func(t *testing.T) {
			deleted := 0
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				switch {
				case request.Method == http.MethodPost:
					fmt.Fprint(writer, `{"sid":"123.456"}`)
				case request.Method == http.MethodDelete:
					deleted++
					if scenario == "cleanup_failed" {
						writer.WriteHeader(http.StatusServiceUnavailable)
					}
				case strings.HasSuffix(request.URL.Path, "/results"):
					switch scenario {
					case "wrong_index":
						fmt.Fprint(writer, `{"results":[{"index":"other"}]}`)
					case "oversized":
						fmt.Fprint(writer, strings.Repeat("x", maximumResponseBytes+1))
					case "chunked":
						writer.Header().Set("Transfer-Encoding", "chunked")
						fmt.Fprint(writer, strings.Repeat("x", maximumResponseBytes+1))
					case "malformed_results":
						fmt.Fprint(writer, `{`)
					case "missing_results":
						fmt.Fprint(writer, `{}`)
					default:
						fmt.Fprint(writer, `{"results":[]}`)
					}
				default:
					switch scenario {
					case "poll_stalled", "no_data":
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":false,"dispatchState":"NO_DATA"}}]}`)
					case "false_done_no_data":
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true,"dispatchState":"NO_DATA"}}]}`)
					case "malformed_status":
						fmt.Fprint(writer, `{`)
					case "missing_status":
						fmt.Fprint(writer, `{"entry":[{"content":{}}]}`)
					case "failed":
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true,"isFailed":true}}]}`)
					case "failed_dispatch":
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true,"dispatchState":"FAILED"}}]}`)
					case "preview":
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true,"isPreview":true}}]}`)
					case "finalized":
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":false,"isFinalized":true}}]}`)
					default:
						fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
					}
				}
			}))
			defer server.Close()
			service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if scenario == "poll_stalled" || scenario == "no_data" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 650*time.Millisecond)
				defer cancel()
			}
			if scenario == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				service.now = func() time.Time { cancel(); return time.Now() }
			}
			_, err = service.Read(ctx, ReadRequest{Limit: 1})
			if err == nil || (scenario == "cleanup_failed" && !errors.Is(err, ErrCleanup)) || (scenario != "canceled" && deleted != 1) || (scenario != "poll_stalled" && scenario != "no_data" && calls > 6) {
				t.Fatalf("err=%v delete=%d calls=%d", err, deleted, calls)
			}
		})
	}
}

func TestInvalidScopeAndArgumentsHaveNoEgress(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { requests++; writer.WriteHeader(http.StatusOK) }))
	defer server.Close()
	for _, search := range []string{"index=main | stats count", "index=main AND index=other", "index=main OR index=other", "search index=main AND host=\"bad\""} {
		if _, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: search}, nil); !errors.Is(err, ErrUnsupportedScope) {
			t.Fatalf("accepted %q: %v", search, err)
		}
	}
	for _, address := range []string{"http://example.com", "http://169.254.169.254", "http://localhost:80/path"} {
		if _, err := NewService(config.AgentSplunkSourceConfig{Address: address, Search: "index=main", Token: "secret"}, nil); err == nil {
			t.Fatalf("accepted unsafe address %q", address)
		}
	}
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, filters := range []map[string]string{{"index": "other"}, {"host": "api | stats"}, {"eval": "x"}} {
		if _, err := service.Read(context.Background(), ReadRequest{Filters: filters}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("accepted filters: %v", filters)
		}
	}
	if requests != 0 {
		t.Fatalf("unsafe input caused %d requests", requests)
	}
}

func TestInvalidJobIDDoesNotReachJobPath(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(writer, `{"sid":"../secret"}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Read(context.Background(), ReadRequest{}); !errors.Is(err, ErrRead) || calls != 1 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe job response: err=%v calls=%d", err, calls)
	}
}

func TestCanceledPollDeletesWithIndependentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			fmt.Fprint(writer, `{"sid":"123.456"}`)
		case http.MethodDelete:
			deleted = true
			writer.WriteHeader(http.StatusOK)
		default:
			cancel()
			fmt.Fprint(writer, `{"entry":[{"content":{"isDone":false}}]}`)
		}
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Read(ctx, ReadRequest{}); err == nil || !deleted {
		t.Fatalf("cancel did not delete job: err=%v deleted=%v", err, deleted)
	}
}

func TestAggregateBudgetStillDeletes(t *testing.T) {
	polls := 0
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost:
			fmt.Fprint(writer, `{"sid":"123.456"}`)
		case request.Method == http.MethodDelete:
			deletes++
		case strings.HasSuffix(request.URL.Path, "/results"):
			fmt.Fprint(writer, `{"results":[]}`+strings.Repeat(" ", 16<<10))
		default:
			polls++
			fmt.Fprint(writer, `{"entry":[{"content":{"isDone":`)
			if polls == 3 {
				fmt.Fprint(writer, `true`)
			} else {
				fmt.Fprint(writer, `false`)
			}
			fmt.Fprint(writer, `}}]}`+strings.Repeat(" ", 251<<10))
		}
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Read(context.Background(), ReadRequest{}); !errors.Is(err, ErrResponseTooLarge) || polls != 3 || deletes != 1 {
		t.Fatalf("aggregate cap: err=%v polls=%d deletes=%d", err, polls, deletes)
	}
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	redirects := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects++ }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing credential on initial request")
		}
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	service, err := NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main", Token: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Read(context.Background(), ReadRequest{}); err == nil || redirects != 0 {
		t.Fatalf("followed redirect: err=%v requests=%d", err, redirects)
	}
}
