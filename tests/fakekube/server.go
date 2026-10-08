package fakekube

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Scenario   string
	Pods       int
	Namespaces int
	Seed       int64
	Faults     map[string]Fault
}

type Fault struct {
	StatusCode   int
	Remaining    int
	RetryAfter   time.Duration
	ContinueOnly bool
}

type requestCount struct {
	Requests int64 `json:"requests"`
	Bytes    int64 `json:"bytes_sent"`
}

type Counter struct {
	Path     string `json:"path"`
	Verb     string `json:"verb"`
	Accept   string `json:"accept"`
	Requests int64  `json:"requests"`
	Bytes    int64  `json:"bytes_sent"`
}

type requestKey struct {
	path   string
	verb   string
	accept string
}

type logStreamKey struct {
	Path      string `json:"path"`
	Container string `json:"container"`
	Previous  bool   `json:"previous"`
	Follow    bool   `json:"follow"`
}

type LogStreamCounter struct {
	logStreamKey
	Requests     int64  `json:"requests"`
	Active       int64  `json:"active"`
	Cancelled    int64  `json:"cancelled"`
	Completed    int64  `json:"completed"`
	Disconnected int64  `json:"disconnected"`
	Unavailable  int64  `json:"unavailable"`
	Chunks       int64  `json:"chunks"`
	SinceTime    string `json:"since_time,omitempty"`
}

type logControl struct {
	Path                  string `json:"path"`
	Container             string `json:"container"`
	DisconnectAfterChunks int    `json:"disconnect_after_chunks"`
	Remaining             int    `json:"remaining"`
	PreviousAvailable     *bool  `json:"previous_available,omitempty"`
}

type Server struct {
	store       *Store
	scenario    string
	startedAt   time.Time
	mu          sync.Mutex
	faults      map[string]Fault
	counters    map[requestKey]requestCount
	logStreams  map[logStreamKey]LogStreamCounter
	logControls map[logStreamKey]logControl
}

func NewServer(config Config) (*Server, error) {
	store := NewStore()
	server := &Server{store: store, scenario: config.Scenario, startedAt: time.Now(), faults: make(map[string]Fault), counters: make(map[requestKey]requestCount), logStreams: make(map[logStreamKey]LogStreamCounter), logControls: make(map[logStreamKey]logControl)}
	for resource, fault := range config.Faults {
		server.faults[resource] = fault
	}
	switch config.Scenario {
	case "limited-rbac":
		server.faults["nodes"] = Fault{StatusCode: http.StatusForbidden, Remaining: -1}
		server.faults["secrets"] = Fault{StatusCode: http.StatusForbidden, Remaining: -1}
	case "fault-forbidden-secret":
		server.faults["secrets"] = Fault{StatusCode: http.StatusForbidden, Remaining: -1}
	case "fault-transient-pods":
		server.faults["pods"] = Fault{StatusCode: http.StatusServiceUnavailable, Remaining: 1, RetryAfter: time.Second}
	case "fault-partial-pods":
		server.faults["pods"] = Fault{StatusCode: http.StatusServiceUnavailable, Remaining: -1, ContinueOnly: true}
	}
	if config.Scenario != "" {
		if err := SeedScenario(store, config.Scenario, config.Seed); err != nil {
			return nil, err
		}
	}
	if config.Pods > 0 {
		namespaces := config.Namespaces
		if namespaces <= 0 {
			namespaces = max(1, config.Pods/25)
		}
		if err := Generate(store, config.Pods, namespaces, config.Seed); err != nil {
			return nil, err
		}
	}
	return server, nil
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	counting := &countingWriter{ResponseWriter: writer}
	key := requestKey{path: request.URL.Path, verb: request.Method, accept: request.Header.Get("Accept")}
	server.recordRequest(key, 0)
	defer func() { server.recordBytes(key, counting.bytes) }()
	resource := resourceForPath(request.URL.Path)
	if resource != "" {
		if fault, ok := server.takeFault(resource, request.URL.Query().Get("continue") != ""); ok {
			if fault.RetryAfter > 0 {
				counting.Header().Set("Retry-After", strconv.Itoa(max(1, int(fault.RetryAfter.Seconds()))))
			}
			http.Error(counting, http.StatusText(fault.StatusCode), fault.StatusCode)
			return
		}
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/api":
		writeJSON(counting, map[string]any{"kind": "APIVersions", "versions": []string{"v1"}})
	case request.Method == http.MethodGet && request.URL.Path == "/apis":
		writeJSON(counting, apiGroups(server.scenario))
	case request.Method == http.MethodGet && isDiscoveryPath(request.URL.Path):
		writeJSON(counting, discoveryForPath(request.URL.Path, server.scenario))
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/_fake/counters"):
		writeJSON(counting, server.requestCounters())
	case request.Method == http.MethodGet && request.URL.Path == "/_fake/traffic-metrics" && server.scenario == "populated":
		counting.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(counting, trafficMetrics(time.Since(server.startedAt)))
	case request.Method == http.MethodGet && request.URL.Path == "/_fake/log-streams":
		writeJSON(counting, server.logStreamCounters())
	case request.Method == http.MethodPost && request.URL.Path == "/_fake/log-streams":
		server.controlLogs(counting, request)
	case request.Method == http.MethodPost && request.URL.Path == "/_fake/mutate":
		server.mutate(counting, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/apis/metrics.k8s.io/"):
		server.metrics(counting, request)
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/log"):
		server.logs(counting, request)
	case request.Method == http.MethodGet && request.URL.Query().Get("watch") == "1":
		server.watch(counting, request, resource)
	case request.Method == http.MethodGet && resource != "" && isItemPath(request.URL.Path):
		server.get(counting, request, resource)
	case request.Method == http.MethodGet && resource != "":
		server.list(counting, request, resource)
	default:
		http.NotFound(counting, request)
	}
}

type countingWriter struct {
	http.ResponseWriter
	bytes int64
}

func (writer *countingWriter) Write(body []byte) (int, error) {
	written, err := writer.ResponseWriter.Write(body)
	writer.bytes += int64(written)
	return written, err
}

func (writer *countingWriter) Flush() {
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (server *Server) recordRequest(key requestKey, bytes int64) {
	server.mu.Lock()
	defer server.mu.Unlock()
	count := server.counters[key]
	count.Requests++
	count.Bytes += bytes
	server.counters[key] = count
}

func (server *Server) recordBytes(key requestKey, bytes int64) {
	server.mu.Lock()
	defer server.mu.Unlock()
	count := server.counters[key]
	count.Bytes += bytes
	server.counters[key] = count
}

func (server *Server) requestCounters() []Counter {
	server.mu.Lock()
	defer server.mu.Unlock()
	result := make([]Counter, 0, len(server.counters))
	for key, value := range server.counters {
		result = append(result, Counter{Path: key.path, Verb: key.verb, Accept: key.accept, Requests: value.Requests, Bytes: value.Bytes})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		if result[i].Verb != result[j].Verb {
			return result[i].Verb < result[j].Verb
		}
		return result[i].Accept < result[j].Accept
	})
	return result
}

func (server *Server) takeFault(resource string, continued bool) (Fault, bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	fault, exists := server.faults[resource]
	if !exists || fault.Remaining == 0 || fault.ContinueOnly && !continued {
		return Fault{}, false
	}
	if fault.Remaining > 0 {
		fault.Remaining--
		server.faults[resource] = fault
	}
	if fault.StatusCode < 400 || fault.StatusCode > 599 {
		fault.StatusCode = http.StatusServiceUnavailable
	}
	return fault, true
}

func (server *Server) list(writer http.ResponseWriter, request *http.Request, resource string) {
	namespace := namespaceForPath(request.URL.Path)
	if resource == "" || namespace == invalidNamespace {
		http.Error(writer, "invalid resource path", http.StatusNotFound)
		return
	}
	result, err := server.store.List(resource, namespace, request.URL.Query())
	if err != nil {
		if errors.Is(err, ErrExpiredContinue) {
			http.Error(writer, "expired continuation", http.StatusGone)
			return
		}
		http.Error(writer, "invalid list request", http.StatusBadRequest)
		return
	}
	accept := request.Header.Get("Accept")
	items := any(result.Items)
	kind := "List"
	apiVersion := "v1"
	if strings.Contains(accept, "PartialObjectMetadataList") {
		items = metadataItems(result.Items)
		kind = "PartialObjectMetadataList"
		apiVersion = "meta.k8s.io/v1"
	}
	if strings.Contains(accept, "as=Table") {
		writeJSON(writer, tableResult(resource, result))
		return
	}
	writeJSON(writer, map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"resourceVersion": result.ResourceVersion, "continue": result.Continue},
		"items":    items,
	})
}

func (server *Server) watch(writer http.ResponseWriter, request *http.Request, resource string) {
	if request.URL.Query().Get("resourceVersion") == "expired" {
		http.Error(writer, "expired resource version", http.StatusGone)
		return
	}
	writer.Header().Set("Content-Type", "application/json;stream=watch;g=meta.k8s.io;v=v1")
	flusher, ok := writer.(http.Flusher)
	if !ok {
		http.Error(writer, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	event := map[string]any{"type": "BOOKMARK", "object": map[string]any{"metadata": map[string]any{"resourceVersion": server.store.ResourceVersion()}, "kind": resource}}
	if err := json.NewEncoder(writer).Encode(event); err != nil {
		return
	}
	flusher.Flush()
}

func (server *Server) get(writer http.ResponseWriter, request *http.Request, resource string) {
	parts := splitPath(request.URL.Path)
	name := parts[len(parts)-1]
	namespace := namespaceForPath(request.URL.Path)
	object, exists := server.store.Get(resource, namespace, name)
	if !exists {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(object)
}

func (server *Server) mutate(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	var input struct {
		Operation string          `json:"operation"`
		Resource  string          `json:"resource"`
		Namespace string          `json:"namespace"`
		Name      string          `json:"name"`
		Object    json.RawMessage `json:"object"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, "invalid mutation", http.StatusBadRequest)
		return
	}
	switch input.Operation {
	case "upsert":
		if err := server.store.Upsert(input.Resource, input.Namespace, input.Name, input.Object); err != nil {
			http.Error(writer, "invalid mutation", http.StatusBadRequest)
			return
		}
	case "delete":
		if !server.store.Delete(input.Resource, input.Namespace, input.Name) {
			http.NotFound(writer, request)
			return
		}
	default:
		http.Error(writer, "invalid mutation", http.StatusBadRequest)
		return
	}
	writeJSON(writer, map[string]any{"ok": true})
}

func (server *Server) metrics(writer http.ResponseWriter, request *http.Request) {
	if strings.HasSuffix(request.URL.Path, "/pods") {
		writeJSON(writer, map[string]any{"kind": "PodMetricsList", "apiVersion": "metrics.k8s.io/v1beta1", "metadata": map[string]any{}, "items": []any{}})
		return
	}
	if strings.HasSuffix(request.URL.Path, "/nodes") {
		writeJSON(writer, map[string]any{"kind": "NodeMetricsList", "apiVersion": "metrics.k8s.io/v1beta1", "metadata": map[string]any{}, "items": []any{}})
		return
	}
	http.NotFound(writer, request)
}

func (server *Server) logs(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	query := request.URL.Query()
	if query.Get("timestamps") == "true" && (query.Get("follow") == "true" || query.Get("follow") == "false" && query.Get("previous") == "true") {
		server.streamLogs(writer, request)
		return
	}
	container, previousAvailable, code := server.logContainer(request.URL.Path, query.Get("container"))
	if code != http.StatusOK {
		http.Error(writer, "unknown pod or container", code)
		return
	}
	key := logStreamKey{Path: request.URL.Path, Container: container, Previous: query.Get("previous") == "true"}
	control := server.takeLogControl(key)
	if control.PreviousAvailable != nil {
		previousAvailable = *control.PreviousAvailable
	}
	update := server.observeLog(key, query.Get("sinceTime"))
	defer update(func(counter *LogStreamCounter) { counter.Active-- })
	if key.Previous && !previousAvailable {
		update(func(counter *LogStreamCounter) { counter.Unavailable++ })
		http.Error(writer, "previous terminated container instance unavailable", http.StatusBadRequest)
		return
	}
	line := "2026-01-01T00:00:00Z fakekube synthetic container=" + container + " finite log line\n"
	if container == "" {
		line = time.Now().UTC().Format(time.RFC3339Nano) + " fakekube log line\n"
	}
	if key.Previous {
		line = "2026-01-01T00:00:00Z fakekube previous synthetic container=" + container + "\n"
	}
	if request.URL.Path == "/api/v1/namespaces/payments/pods/checkout-api-0/log" && container == "api" {
		line = "2026-01-01T00:00:00Z fakekube checkout api token=super-secret-value\n"
	}
	if _, err := io.WriteString(writer, line); err != nil {
		update(func(counter *LogStreamCounter) { counter.Cancelled++ })
		return
	}
	update(func(counter *LogStreamCounter) { counter.Chunks++; counter.Completed++ })
}

func (server *Server) logContainer(path, selected string) (string, bool, int) {
	parts := splitPath(path)
	if len(parts) != 7 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "namespaces" || parts[4] != "pods" || parts[6] != "log" {
		return "", false, http.StatusNotFound
	}
	object, exists := server.store.Get("pods", parts[3], parts[5])
	if !exists {
		return "", false, http.StatusNotFound
	}
	type namedContainer struct {
		Name string `json:"name"`
	}
	var pod struct {
		Spec struct {
			Containers          []namedContainer `json:"containers"`
			InitContainers      []namedContainer `json:"initContainers"`
			EphemeralContainers []namedContainer `json:"ephemeralContainers"`
		} `json:"spec"`
		Status map[string]json.RawMessage `json:"status"`
	}
	if json.Unmarshal(object, &pod) != nil {
		return "", false, http.StatusBadRequest
	}
	if selected == "" && len(pod.Spec.Containers) == 0 && len(pod.Spec.InitContainers) == 0 && len(pod.Spec.EphemeralContainers) == 0 {
		return "", false, http.StatusOK
	}
	for _, group := range []struct {
		containers []namedContainer
		status     string
	}{{pod.Spec.Containers, "containerStatuses"}, {pod.Spec.InitContainers, "initContainerStatuses"}, {pod.Spec.EphemeralContainers, "ephemeralContainerStatuses"}} {
		for _, container := range group.containers {
			if selected == "" && group.status == "containerStatuses" {
				selected = container.Name
			}
			if container.Name != selected {
				continue
			}
			var statuses []struct {
				Name         string `json:"name"`
				RestartCount int    `json:"restartCount"`
			}
			if json.Unmarshal(pod.Status[group.status], &statuses) == nil {
				for _, status := range statuses {
					if status.Name == selected {
						return selected, status.RestartCount > 0, http.StatusOK
					}
				}
			}
			return selected, false, http.StatusOK
		}
	}
	return "", false, http.StatusBadRequest
}

func (server *Server) controlLogs(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	request.Body = http.MaxBytesReader(writer, request.Body, 4096)
	var control logControl
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil || control.DisconnectAfterChunks < 0 || control.DisconnectAfterChunks > 1024 || control.Remaining < 0 || control.Remaining > 100 || (control.DisconnectAfterChunks == 0) != (control.Remaining == 0) {
		http.Error(writer, "invalid log control", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(writer, "invalid log control", http.StatusBadRequest)
		return
	}
	container, _, code := server.logContainer(control.Path, control.Container)
	if code != http.StatusOK {
		http.Error(writer, "unknown pod or container", code)
		return
	}
	control.Container = container
	server.mu.Lock()
	server.logControls[logStreamKey{Path: control.Path, Container: container}] = control
	server.mu.Unlock()
	writeJSON(writer, control)
}

func (server *Server) takeLogControl(key logStreamKey) logControl {
	server.mu.Lock()
	defer server.mu.Unlock()
	follow := key.Follow
	key.Previous, key.Follow = false, false
	control := server.logControls[key]
	if follow && control.Remaining > 0 {
		next := control
		next.Remaining--
		server.logControls[key] = next
	} else {
		control.DisconnectAfterChunks, control.Remaining = 0, 0
	}
	return control
}

func (server *Server) observeLog(key logStreamKey, since string) func(func(*LogStreamCounter)) {
	update := func(change func(*LogStreamCounter)) {
		server.mu.Lock()
		defer server.mu.Unlock()
		counter := server.logStreams[key]
		counter.logStreamKey = key
		change(&counter)
		server.logStreams[key] = counter
	}
	update(func(counter *LogStreamCounter) { counter.Requests++; counter.Active++; counter.SinceTime = since })
	return update
}

func (server *Server) logStreamCounters() []LogStreamCounter {
	server.mu.Lock()
	defer server.mu.Unlock()
	result := make([]LogStreamCounter, 0, len(server.logStreams))
	for _, counter := range server.logStreams {
		result = append(result, counter)
	}
	sort.Slice(result, func(first, second int) bool {
		if result[first].Path != result[second].Path {
			return result[first].Path < result[second].Path
		}
		if result[first].Container != result[second].Container {
			return result[first].Container < result[second].Container
		}
		if result[first].Follow != result[second].Follow {
			return result[first].Follow
		}
		return !result[first].Previous && result[second].Previous
	})
	return result
}

func (server *Server) streamLogs(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	container, previousAvailable, code := server.logContainer(request.URL.Path, request.URL.Query().Get("container"))
	if code != http.StatusOK {
		http.Error(writer, "unknown pod or container", code)
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		http.Error(writer, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	query := request.URL.Query()
	var since time.Time
	if value := query.Get("sinceTime"); value != "" {
		var err error
		since, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			http.Error(writer, "invalid sinceTime", http.StatusBadRequest)
			return
		}
	}
	key := logStreamKey{Path: request.URL.Path, Container: container, Previous: query.Get("previous") == "true", Follow: query.Get("follow") == "true" && query.Get("previous") != "true"}
	control := server.takeLogControl(key)
	if control.PreviousAvailable != nil {
		previousAvailable = *control.PreviousAvailable
	}
	update := server.observeLog(key, query.Get("sinceTime"))
	completed := false
	disconnected := false
	unavailable := key.Previous && !previousAvailable
	defer func() {
		update(func(counter *LogStreamCounter) {
			counter.Active--
			if unavailable {
				counter.Unavailable++
			} else if disconnected {
				counter.Disconnected++
			} else if completed {
				counter.Completed++
			} else {
				counter.Cancelled++
			}
		})
	}()
	if unavailable {
		http.Error(writer, "previous terminated container instance unavailable", http.StatusBadRequest)
		return
	}
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	chunksWritten := 0
	writeLine := func(timestamp time.Time, text string) bool {
		if timestamp.Before(since) {
			return true
		}
		if container != "api" {
			text += " container=" + container
		}
		line := timestamp.Format(time.RFC3339Nano) + " " + text + "\n"
		chunks := []string{line}
		if split := strings.Index(line, "secret-value"); split >= 0 {
			chunks = []string{line[:split+3], line[split+3:]}
		}
		for _, chunk := range chunks {
			select {
			case <-request.Context().Done():
				return false
			default:
			}
			if _, err := io.WriteString(writer, chunk); err != nil {
				return false
			}
			update(func(counter *LogStreamCounter) { counter.Chunks++ })
			flusher.Flush()
			chunksWritten++
			if key.Follow && control.Remaining > 0 && chunksWritten >= control.DisconnectAfterChunks {
				disconnected = true
				panic(http.ErrAbortHandler)
			}
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-request.Context().Done():
				timer.Stop()
				return false
			case <-timer.C:
			}
		}
		return true
	}
	if key.Previous {
		completed = writeLine(base, "fakekube previous token=previous-secret-value")
		return
	}
	lines := []string{
		"fakekube repeated identical line",
		"fakekube repeated identical line",
		"fakekube token=super-secret-value",
		"fakekube password=split-secret-value",
	}
	if tail, err := strconv.Atoi(query.Get("tailLines")); err == nil && tail > 0 && tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	for _, line := range lines {
		if !writeLine(base, line) {
			return
		}
	}
	if !writeLine(base, "fakekube synthetic oversized password=synthetic-oversized-secret-value "+strings.Repeat("synthetic-record-padding ", 800)) {
		return
	}
	flusher.Flush()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for sequence := int64(1); ; sequence++ {
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			if !writeLine(base.Add(time.Duration(sequence)*time.Second), fmt.Sprintf("fakekube follow line %d", sequence)) {
				return
			}
		}
	}
}

func resourceForPath(path string) string {
	parts := splitPath(path)
	if len(parts) >= 3 && parts[0] == "api" && parts[1] == "v1" {
		if parts[2] == "namespaces" && len(parts) >= 5 {
			return parts[4]
		}
		if len(parts) == 3 || len(parts) == 4 {
			return parts[2]
		}
	}
	if len(parts) >= 4 && parts[0] == "apis" {
		if parts[3] == "namespaces" && len(parts) >= 6 {
			return parts[5]
		}
		if len(parts) == 4 || len(parts) == 5 {
			return parts[3]
		}
	}
	return ""
}

const invalidNamespace = "\x00invalid"

func namespaceForPath(path string) string {
	parts := splitPath(path)
	if len(parts) >= 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" {
		if parts[3] == "" {
			return invalidNamespace
		}
		return parts[3]
	}
	if len(parts) >= 6 && parts[0] == "apis" && parts[3] == "namespaces" {
		if parts[4] == "" {
			return invalidNamespace
		}
		return parts[4]
	}
	return ""
}

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func isDiscoveryPath(path string) bool {
	return path == "/api/v1" || strings.HasPrefix(path, "/apis/") && len(splitPath(path)) == 3
}

func isItemPath(path string) bool {
	parts := splitPath(path)
	if len(parts) >= 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" {
		return len(parts) == 6
	}
	if len(parts) >= 6 && parts[0] == "apis" && parts[3] == "namespaces" {
		return len(parts) == 7
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" {
		return true
	}
	return len(parts) == 5 && parts[0] == "apis"
}

func discoveryForPath(path, scenario string) map[string]any {
	resources := []map[string]any{
		{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list", "watch"}},
		{"name": "services", "kind": "Service", "namespaced": true, "verbs": []string{"get", "list"}},
		{"name": "events", "kind": "Event", "namespaced": true, "verbs": []string{"get", "list"}},
		{"name": "namespaces", "kind": "Namespace", "namespaced": false, "verbs": []string{"get", "list"}},
		{"name": "nodes", "kind": "Node", "namespaced": false, "verbs": []string{"get", "list"}},
		{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []string{"get", "list"}},
		{"name": "configmaps", "kind": "ConfigMap", "namespaced": true, "verbs": []string{"get", "list"}},
		{"name": "serviceaccounts", "kind": "ServiceAccount", "namespaced": true, "verbs": []string{"get", "list"}},
		{"name": "persistentvolumeclaims", "kind": "PersistentVolumeClaim", "namespaced": true, "verbs": []string{"get", "list"}},
	}
	group := ""
	if strings.HasPrefix(path, "/apis/apps/") {
		group = "apps"
		resources = []map[string]any{
			{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}},
			{"name": "replicasets", "kind": "ReplicaSet", "namespaced": true, "verbs": []string{"get", "list"}},
			{"name": "statefulsets", "kind": "StatefulSet", "namespaced": true, "verbs": []string{"get", "list"}},
			{"name": "daemonsets", "kind": "DaemonSet", "namespaced": true, "verbs": []string{"get", "list"}},
		}
	} else if strings.HasPrefix(path, "/apis/batch/") {
		group = "batch"
		resources = []map[string]any{
			{"name": "jobs", "kind": "Job", "namespaced": true, "verbs": []string{"get", "list"}},
			{"name": "cronjobs", "kind": "CronJob", "namespaced": true, "verbs": []string{"get", "list"}},
		}
	} else if strings.HasPrefix(path, "/apis/networking.k8s.io/") {
		group = "networking.k8s.io"
		resources = []map[string]any{
			{"name": "ingresses", "kind": "Ingress", "namespaced": true, "verbs": []string{"get", "list"}},
			{"name": "networkpolicies", "kind": "NetworkPolicy", "namespaced": true, "verbs": []string{"get", "list"}},
		}
	} else if strings.HasPrefix(path, "/apis/autoscaling/") {
		group = "autoscaling"
		resources = []map[string]any{{"name": "horizontalpodautoscalers", "kind": "HorizontalPodAutoscaler", "namespaced": true, "verbs": []string{"get", "list"}}}
	} else if (scenario == "gitops" || scenario == "populated") && strings.HasPrefix(path, "/apis/argoproj.io/") {
		group = "argoproj.io"
		resources = []map[string]any{
			{"name": "applications", "kind": "Application", "namespaced": true, "verbs": []string{"get", "list"}},
			{"name": "rollouts", "kind": "Rollout", "namespaced": true, "verbs": []string{"get", "list"}},
		}
	} else if (scenario == "gitops" || scenario == "populated") && strings.HasPrefix(path, "/apis/kustomize.toolkit.fluxcd.io/") {
		group = "kustomize.toolkit.fluxcd.io"
		resources = []map[string]any{{"name": "kustomizations", "kind": "Kustomization", "namespaced": true, "verbs": []string{"get", "list"}}}
	} else if (scenario == "gitops" || scenario == "populated") && strings.HasPrefix(path, "/apis/helm.toolkit.fluxcd.io/") {
		group = "helm.toolkit.fluxcd.io"
		resources = []map[string]any{{"name": "helmreleases", "kind": "HelmRelease", "namespaced": true, "verbs": []string{"get", "list"}}}
	} else if path != "/api/v1" {
		return map[string]any{"resources": []any{}}
	}
	result := map[string]any{"resources": resources}
	if group == "" {
		result["groupVersion"] = "v1"
	} else {
		parts := splitPath(path)
		version := "v1"
		if len(parts) >= 3 {
			version = parts[2]
		}
		result["groupVersion"] = group + "/" + version
	}
	return result
}

func apiGroups(scenario string) map[string]any {
	groups := []map[string]any{}
	groupVersions := map[string]string{"apps": "v1", "batch": "v1", "networking.k8s.io": "v1", "autoscaling": "v1"}
	if scenario == "gitops" || scenario == "populated" {
		groupVersions["argoproj.io"] = "v1alpha1"
		groupVersions["kustomize.toolkit.fluxcd.io"] = "v1"
		groupVersions["helm.toolkit.fluxcd.io"] = "v2"
	}
	names := make([]string, 0, len(groupVersions))
	for name := range groupVersions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		versionName := groupVersions[name]
		version := map[string]any{"groupVersion": name + "/" + versionName, "version": versionName}
		groups = append(groups, map[string]any{"name": name, "preferredVersion": version, "versions": []any{version}})
	}
	return map[string]any{"groups": groups}
}

func metadataItems(items []json.RawMessage) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		var object map[string]any
		if json.Unmarshal(item, &object) != nil {
			continue
		}
		metadata, _ := object["metadata"].(map[string]any)
		result = append(result, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadata", "metadata": metadata})
	}
	return result
}

func tableResult(resource string, result ListResult) map[string]any {
	rows := make([]map[string]any, 0, len(result.Items))
	for _, item := range result.Items {
		var object map[string]any
		_ = json.Unmarshal(item, &object)
		metadata, _ := object["metadata"].(map[string]any)
		rows = append(rows, map[string]any{"cells": []any{metadata["name"], metadata["namespace"], object["kind"]}, "object": object})
	}
	return map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "Table", "columnDefinitions": []any{map[string]any{"name": "Name"}, map[string]any{"name": "Namespace"}, map[string]any{"name": "Kind"}}, "rows": rows, "metadata": map[string]any{"resourceVersion": result.ResourceVersion, "continue": result.Continue}, "resource": resource}
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		http.Error(writer, fmt.Sprintf("encode response: %v", err), http.StatusInternalServerError)
	}
}

func (server *Server) Handler() http.Handler { return server }

func (server *Server) URLValues(raw string) url.Values {
	values, _ := url.ParseQuery(raw)
	return values
}
