package agent

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/graylog"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/splunk"
)

// Reads Kubernetes-shaped lines from real harness provider backends through the
// linked break-log adapters. Each provider runs only when its harness URL is set.
func TestBreakLogAdaptersNativeHarnessBackends(t *testing.T) {
	redactor, errs := NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	insecure := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}}}
	post := func(t *testing.T, url string, headers map[string]string, body []byte) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err := insecure.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode >= 300 {
			t.Fatalf("seed %s HTTP %d", url, response.StatusCode)
		}
	}
	for _, provider := range []struct {
		kind, env string
		source    func(address string) config.AgentSourceConfig
		seed      func(t *testing.T, namespace, pod string, lines []breaklogs.LinkedLine)
	}{
		{"graylog", "K13_NATIVE_GRAYLOG_URL", func(address string) config.AgentSourceConfig {
			return config.AgentSourceConfig{Name: "native-graylog", Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: address, Username: "admin", Password: os.Getenv("GRAYLOG_PASSWORD"), Query: "*", MessageField: "message"}}
		}, func(t *testing.T, namespace, pod string, lines []breaklogs.LinkedLine) {
			connection, err := net.Dial("udp", os.Getenv("K13_NATIVE_GRAYLOG_GELF"))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			for _, line := range lines {
				encoded, _ := json.Marshal(map[string]any{"version": "1.1", "host": "k13-native", "short_message": line.Text, "timestamp": float64(line.At.UnixMilli()) / 1000, "_kubernetes_namespace_name": namespace, "_kubernetes_pod_name": pod, "_kubernetes_container_name": "api"})
				if _, err := connection.Write(encoded); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"splunk", "K13_NATIVE_SPLUNK_URL", func(address string) config.AgentSourceConfig {
			return config.AgentSourceConfig{Name: "native-splunk", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: address, Username: "admin", Password: os.Getenv("SPLUNK_PASSWORD"), InsecureSkipVerify: true, Search: "search index=main AND sourcetype=_json", TimeField: "_time", MessageField: "message"}}
		}, func(t *testing.T, namespace, pod string, lines []breaklogs.LinkedLine) {
			var body bytes.Buffer
			for _, line := range lines {
				encoded, _ := json.Marshal(map[string]any{"time": float64(line.At.UnixMilli()) / 1000, "index": "main", "sourcetype": "_json", "event": map[string]string{"namespace": namespace, "pod": pod, "container_name": "api", "message": line.Text}})
				body.Write(encoded)
			}
			post(t, os.Getenv("K13_NATIVE_SPLUNK_HEC")+"/services/collector/event", map[string]string{"Authorization": "Splunk " + os.Getenv("SPLUNK_HEC_TOKEN")}, body.Bytes())
		}},
		{"signoz", "K13_NATIVE_SIGNOZ_URL", func(address string) config.AgentSourceConfig {
			return config.AgentSourceConfig{Name: "native-signoz", Type: "signoz", Enable: true, Signoz: config.AgentSignozSourceConfig{Address: address, APIKey: os.Getenv("SIGNOZ_API_KEY"), AllowLoopback: true, Query: "service.name = 'k13-native'"}}
		}, func(t *testing.T, namespace, pod string, lines []breaklogs.LinkedLine) {
			attribute := func(key, value string) map[string]any {
				return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
			}
			records := []map[string]any{}
			for _, line := range lines {
				records = append(records, map[string]any{"timeUnixNano": strconv.FormatInt(line.At.UnixNano(), 10), "severityText": "INFO", "body": map[string]string{"stringValue": line.Text}})
			}
			encoded, _ := json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{"resource": map[string]any{"attributes": []any{attribute("service.name", "k13-native"), attribute("k8s.namespace.name", namespace), attribute("k8s.pod.name", pod), attribute("k8s.container.name", "api")}}, "scopeLogs": []any{map[string]any{"logRecords": records}}}}})
			post(t, os.Getenv("K13_NATIVE_SIGNOZ_OTLP")+"/v1/logs", nil, encoded)
		}},
	} {
		t.Run(provider.kind, func(t *testing.T) {
			address := os.Getenv(provider.env)
			if address == "" {
				t.Skip(provider.env + " not set")
			}
			now := time.Now().UTC().Truncate(time.Second)
			namespace := "k13native"
			prefix := fmt.Sprintf("native-%s-%d", provider.kind, now.Unix())
			pod := prefix + "-6c8d4-a1b2c"
			lines := []breaklogs.LinkedLine{}
			for index := range 5 {
				text := fmt.Sprintf("INFO native %s line %d", provider.kind, index)
				if index == 2 {
					text = "ERROR native refused token=k13-native-canary-value"
				}
				lines = append(lines, breaklogs.LinkedLine{At: now.Add(time.Duration(index-120) * time.Second), Text: text})
			}
			provider.seed(t, namespace, pod, lines)
			built := BuildBreakLogSources([]config.AgentSourceConfig{provider.source(address)}, redactor, "cluster")
			if len(built) != 1 {
				t.Fatal("configured source rejected")
			}
			reader, err := built[0].Build(breaklogs.Fields{})
			if err != nil {
				t.Fatal(err)
			}
			for _, pods := range [][]string{{pod}, nil} {
				query := breaklogs.LinkedQuery{Namespace: namespace, PodPrefix: prefix, Pods: pods, Container: "api", From: now.Add(-10 * time.Minute), To: now.Add(time.Minute), Limit: 500}
				var result breaklogs.Result
				deadline := time.Now().Add(150 * time.Second)
				for {
					result, err = reader.Read(t.Context(), query)
					if err == nil && len(result.Lines) == len(lines) || time.Now().After(deadline) {
						break
					}
					time.Sleep(2 * time.Second)
				}
				if err != nil || len(result.Lines) != len(lines) || result.Truncated {
					if provider.kind == "graylog" {
						cfg := provider.source(address).Graylog
						cfg.Fields = []string{"kubernetes_namespace_name", "kubernetes_pod_name", "kubernetes_container_name"}
						if service, serviceErr := graylog.NewService(cfg, redactor); serviceErr == nil {
							direct, directErr := service.ReadWindow(t.Context(), graylog.ReadRequest{Filters: map[string]string{"kubernetes_namespace_name": namespace}, Limit: 50}, query.From, query.To)
							t.Logf("graylog truncation=%v count=%d err=%v", direct.Truncation, direct.Count, directErr)
						}
					}
					t.Fatalf("pods=%v lines=%d truncated=%v err=%v", pods, len(result.Lines), result.Truncated, err)
				}
				for _, line := range result.Lines {
					if line.Pod != pod || line.At.Before(query.From) || line.At.After(query.To) || strings.Contains(line.Text, "k13-native-canary-value") {
						t.Fatalf("unscoped or unscrubbed line: %+v", line)
					}
				}
				preview := reader.Preview(query)
				if key := os.Getenv("SIGNOZ_API_KEY"); !strings.Contains(preview, namespace) || key != "" && strings.Contains(preview, key) {
					t.Fatalf("preview missing scope: %s", preview)
				}
				t.Logf("pods=%v lines=%d preview=%s", pods, len(result.Lines), preview)
			}
			other, err := reader.Read(t.Context(), breaklogs.LinkedQuery{Namespace: "k13native-other", Pods: []string{pod}, From: now.Add(-10 * time.Minute), To: now.Add(time.Minute), Limit: 500})
			if err != nil || len(other.Lines) != 0 {
				t.Fatalf("namespace filter leaked lines=%d err=%v", len(other.Lines), err)
			}
		})
	}
	// Custom Splunk field mapping without a container filter must still project the mapped container.
	t.Run("splunk-custom-mapping", func(t *testing.T) {
		address := os.Getenv("K13_NATIVE_SPLUNK_URL")
		if address == "" {
			t.Skip("K13_NATIVE_SPLUNK_URL not set")
		}
		now := time.Now().UTC().Truncate(time.Second)
		namespace := "k13nativemap"
		prefix := fmt.Sprintf("native-map-%d", now.Unix())
		pod := prefix + "-6c8d4-a1b2c"
		var body bytes.Buffer
		for index := range 5 {
			encoded, _ := json.Marshal(map[string]any{"time": float64(now.Add(time.Duration(index-120)*time.Second).UnixMilli()) / 1000, "index": "main", "sourcetype": "_json", "event": map[string]string{"k8s_ns": namespace, "k8s_pod": pod, "k8s_ctr": "api", "message": fmt.Sprintf("INFO mapped line %d", index)}})
			body.Write(encoded)
			encoded, _ = json.Marshal(map[string]any{"time": float64(now.Add(time.Duration(index-120)*time.Second).UnixMilli()) / 1000, "index": "main", "sourcetype": "_json", "event": map[string]string{"namespace": namespace, "pod": pod, "container_name": "api", "message": fmt.Sprintf("INFO default-key line %d", index)}})
			body.Write(encoded)
		}
		encoded, _ := json.Marshal(map[string]any{"time": float64(now.Add(-60*time.Second).UnixMilli()) / 1000, "index": "main", "sourcetype": "_json", "event": map[string]string{"k8s_ns": namespace, "k8s_ctr": "api", "message": "INFO identity-less " + prefix + "-x"}})
		body.Write(encoded)
		post(t, os.Getenv("K13_NATIVE_SPLUNK_HEC")+"/services/collector/event", map[string]string{"Authorization": "Splunk " + os.Getenv("SPLUNK_HEC_TOKEN")}, body.Bytes())
		source := config.AgentSourceConfig{Name: "native-splunk-map", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: address, Username: "admin", Password: os.Getenv("SPLUNK_PASSWORD"), InsecureSkipVerify: true, Search: "search index=main AND sourcetype=_json", TimeField: "_time", MessageField: "message"}}
		built := BuildBreakLogSources([]config.AgentSourceConfig{source}, redactor, "cluster")
		if len(built) != 1 {
			t.Fatal("configured source rejected")
		}
		if service, serviceErr := splunk.NewService(source.Splunk, redactor); serviceErr == nil {
			direct, directErr := service.ReadWindowWithFields(t.Context(), map[string]string{"k8s_ns": namespace, "k8s_pod": pod}, now.Add(-10*time.Minute), now.Add(time.Minute), 50, []string{"k8s_ctr"})
			keys := []string{}
			if len(direct.Records) > 0 {
				for key := range direct.Records[0].Fields {
					keys = append(keys, key)
				}
			}
			t.Logf("direct splunk custom-key read count=%d truncation=%v first_record_fields=%v err=%v", direct.Count, direct.Truncation, keys, directErr)
		}
		failed := []string{}
		for _, variant := range []struct {
			name      string
			fields    breaklogs.Fields
			container string
		}{
			{"default-keys-no-container-filter", breaklogs.Fields{}, ""},
			{"custom-keys-container-filter", breaklogs.Fields{Namespace: "k8s_ns", Pod: "k8s_pod", Container: "k8s_ctr"}, "api"},
			{"custom-keys-no-container-filter", breaklogs.Fields{Namespace: "k8s_ns", Pod: "k8s_pod", Container: "k8s_ctr"}, ""},
		} {
			reader, err := built[0].Build(variant.fields)
			if err != nil {
				t.Fatal(err)
			}
			for _, pods := range [][]string{{pod}, nil} {
				query := breaklogs.LinkedQuery{Namespace: namespace, PodPrefix: prefix, Pods: pods, Container: variant.container, From: now.Add(-10 * time.Minute), To: now.Add(time.Minute), Limit: 500}
				var result breaklogs.Result
				deadline := time.Now().Add(60 * time.Second)
				for {
					result, err = reader.Read(t.Context(), query)
					if err == nil && len(result.Lines) == 5 || time.Now().After(deadline) {
						break
					}
					time.Sleep(2 * time.Second)
				}
				ok := err == nil && len(result.Lines) == 5 && !result.Truncated
				for _, line := range result.Lines {
					ok = ok && line.Pod == pod && line.Container == "api" && !strings.Contains(line.Text, "identity-less")
				}
				t.Logf("%s pods=%v lines=%d truncated=%v err=%v ok=%v preview=%s", variant.name, pods, len(result.Lines), result.Truncated, err, ok, reader.Preview(query))
				if !ok {
					failed = append(failed, fmt.Sprintf("%s pods=%v", variant.name, pods))
				}
			}
		}
		if len(failed) > 0 {
			t.Fatalf("mapped projection or identity lost: %v", failed)
		}
	})
	// Natively returned rows with an invalid Pod/container identity keep the good rows and mark the read partial.
	t.Run("splunk-indexed-identities", func(t *testing.T) {
		address := os.Getenv("K13_NATIVE_SPLUNK_URL")
		if address == "" {
			t.Skip("K13_NATIVE_SPLUNK_URL not set")
		}
		now := time.Now().UTC().Truncate(time.Second)
		for variantIndex, variant := range []struct {
			name   string
			fields breaklogs.Fields
			alias  bool
			raw    bool
		}{
			{name: "default-indexed"},
			{name: "custom-indexed", fields: breaklogs.Fields{Namespace: "mapped_ns", Pod: "mapped_pod", Container: "mapped_ctr"}},
			{name: "default-alias", alias: true},
			{name: "custom-alias", fields: breaklogs.Fields{Namespace: "mapped_ns", Pod: "mapped_pod", Container: "mapped_ctr"}, alias: true},
			{name: "raw-string-controls", raw: true},
		} {
			t.Run(variant.name, func(t *testing.T) {
				fields := variant.fields
				if fields.Namespace == "" {
					fields = breaklogs.Fields{Namespace: "namespace", Pod: "pod", Container: "container_name"}
				}
				prefix := fmt.Sprintf("native-indexed-%d-%d", now.UnixNano(), variantIndex)
				pod, namespace := prefix+"-6c8d4-a1b2c", "native-indexed"
				sourcetype := "_json"
				if variant.alias {
					sourcetype = prefix
					configure := func(method, path string, values url.Values) {
						t.Helper()
						request, err := http.NewRequestWithContext(t.Context(), method, address+path, strings.NewReader(values.Encode()))
						if err != nil {
							t.Fatal(err)
						}
						request.SetBasicAuth("admin", os.Getenv("SPLUNK_PASSWORD"))
						request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
						response, err := insecure.Do(request)
						if err != nil {
							t.Fatal("native alias configuration request failed")
						}
						response.Body.Close()
						if response.StatusCode >= 300 {
							t.Fatalf("native alias configuration HTTP %d", response.StatusCode)
						}
					}
					path := "/servicesNS/nobody/search/configs/conf-props"
					configure(http.MethodPost, path, url.Values{"name": {sourcetype}, "FIELDALIAS-native-identity": {fmt.Sprintf("seed_ns AS %s seed_pod AS %s seed_ctr AS %s", fields.Namespace, fields.Pod, fields.Container)}})
					defer configure(http.MethodDelete, path+"/"+url.PathEscape(sourcetype), nil)
				}
				var body bytes.Buffer
				for rowIndex, container := range []string{"api", "null", "42"} {
					event := map[string]any{"message": fmt.Sprintf("INFO indexed line %d token=native-indexed-canary", rowIndex)}
					identities := map[string]string{fields.Namespace: namespace, fields.Pod: pod, fields.Container: container}
					wire := map[string]any{"time": now.Add(time.Duration(rowIndex-120) * time.Second).Unix(), "index": "main", "sourcetype": sourcetype, "event": event}
					switch {
					case variant.raw:
						for key, value := range identities {
							event[key] = value
						}
					case variant.alias:
						event["seed_ns"], event["seed_pod"], event["seed_ctr"] = namespace, pod, container
					default:
						wire["fields"] = identities
					}
					encoded, err := json.Marshal(wire)
					if err != nil {
						t.Fatal(err)
					}
					var seeded struct {
						Event map[string]any `json:"event"`
					}
					if json.Unmarshal(encoded, &seeded) != nil {
						t.Fatal("native indexed fixture wire is invalid JSON")
					}
					for key, identity := range identities {
						value, exists := seeded.Event[key]
						if exists != variant.raw || variant.raw && value != identity {
							t.Fatal("native indexed fixture raw identity presence or value mismatch")
						}
					}
					body.Write(encoded)
				}
				t.Logf("wire_json_valid=true raw_identity_keys=%v", variant.raw)
				post(t, os.Getenv("K13_NATIVE_SPLUNK_HEC")+"/services/collector/event", map[string]string{"Authorization": "Splunk " + os.Getenv("SPLUNK_HEC_TOKEN")}, body.Bytes())
				settings := config.AgentSplunkSourceConfig{Address: address, Username: "admin", Password: os.Getenv("SPLUNK_PASSWORD"), InsecureSkipVerify: true, Search: "search index=main AND sourcetype=" + sourcetype, TimeField: "_time", MessageField: "message"}
				sources := BuildBreakLogSources([]config.AgentSourceConfig{{Name: "native-indexed", Type: "splunk", Enable: true, Splunk: settings}}, redactor, "cluster")
				if len(sources) != 1 {
					t.Fatal("native indexed source rejected")
				}
				reader, err := sources[0].Build(fields)
				if err != nil {
					t.Fatal(err)
				}
				for _, selection := range []string{"exact", "prefix"} {
					query := breaklogs.LinkedQuery{Namespace: namespace, PodPrefix: prefix, From: now.Add(-10 * time.Minute), To: now.Add(time.Minute), Limit: 500}
					if selection == "exact" {
						query.Pods = []string{pod}
					}
					deadline := time.Now().Add(60 * time.Second)
					var result breaklogs.Result
					for {
						result, err = reader.Read(t.Context(), query)
						if err == nil && len(result.Lines) == 3 || time.Now().After(deadline) {
							break
						}
						time.Sleep(2 * time.Second)
					}
					if err != nil || len(result.Lines) != 3 || result.Truncated {
						t.Fatalf("%s rows=%d truncated=%v err=%v", selection, len(result.Lines), result.Truncated, err)
					}
					seen := map[string]bool{}
					for _, line := range result.Lines {
						jsonvalid := json.Valid([]byte(line.Text))
						podmatches := line.Pod == pod
						canarypresent := strings.Contains(line.Text, "native-indexed-canary")
						t.Logf("%s jsonvalid=%v podmatches=%v canarypresent=%v", selection, jsonvalid, podmatches, canarypresent)
						if !podmatches || canarypresent {
							t.Fatal("native indexed identity or redaction lost")
						}
						if line.At.Before(query.From) || line.At.After(query.To) || !strings.Contains(line.Text, "INFO indexed line ") || !strings.Contains(line.Text, "<REDACTED:password>") {
							t.Fatal("native indexed time scope or scrubbed message lost")
						}
						seen[line.Container] = true
					}
					if !seen["api"] || !seen["null"] || !seen["42"] {
						t.Fatal("genuine string container identities lost")
					}
					t.Logf("PASS %s %s rows=3 truncated=false raw_identity_keys=%v containers=api,null,42", variant.name, selection, variant.raw)
				}
			})
		}
	})
	t.Run("malformed-identity-rows", func(t *testing.T) {
		for _, kind := range []string{"graylog", "splunk"} {
			t.Run(kind, func(t *testing.T) {
				var source config.AgentSourceConfig
				podKey, containerKey := "kubernetes_pod_name", "kubernetes_container_name"
				if kind == "graylog" {
					if os.Getenv("K13_NATIVE_GRAYLOG_URL") == "" {
						t.Skip("K13_NATIVE_GRAYLOG_URL not set")
					}
					source = config.AgentSourceConfig{Name: "native-graylog-mal", Type: "graylog", Enable: true, Graylog: config.AgentGraylogSourceConfig{Address: os.Getenv("K13_NATIVE_GRAYLOG_URL"), Username: "admin", Password: os.Getenv("GRAYLOG_PASSWORD"), Query: "*", MessageField: "message"}}
				} else {
					if os.Getenv("K13_NATIVE_SPLUNK_URL") == "" {
						t.Skip("K13_NATIVE_SPLUNK_URL not set")
					}
					podKey, containerKey = "pod", "container_name"
					source = config.AgentSourceConfig{Name: "native-splunk-mal", Type: "splunk", Enable: true, Splunk: config.AgentSplunkSourceConfig{Address: os.Getenv("K13_NATIVE_SPLUNK_URL"), Username: "admin", Password: os.Getenv("SPLUNK_PASSWORD"), InsecureSkipVerify: true, Search: "search index=main AND sourcetype=_json", TimeField: "_time", MessageField: "message"}}
				}
				built := BuildBreakLogSources([]config.AgentSourceConfig{source}, redactor, "cluster")
				if len(built) != 1 {
					t.Fatal("configured source rejected")
				}
				reader, err := built[0].Build(breaklogs.Fields{})
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC().Truncate(time.Second)
				namespace := "k13nativemal"
				failed := []string{}
				for index, item := range []struct {
					identity, name string
					value          any
					missing        bool
				}{
					{"none", "control", nil, false},
					{"container", "missing", nil, true},
					{"container", "null", nil, false},
					{"container", "numeric", 42, false},
					{"container", "boolean", true, false},
					{"container", "malformed", "bad identity", false},
					{"pod", "malformed", "-bad identity", false},
				} {
					prefix := fmt.Sprintf("native-mal-%s-%d-%d", kind, now.Unix(), index)
					pod := prefix + "-6c8d4-a1b2c"
					row := func(at time.Time, text string) map[string]any {
						return map[string]any{"namespace": namespace, podKey: pod, containerKey: "api", "message": text, "at": at}
					}
					rows := []map[string]any{}
					if item.identity != "none" {
						bad := row(now.Add(-150*time.Second), "INFO malformed identity row "+item.name)
						key := containerKey
						if item.identity == "pod" {
							key = podKey
						}
						switch {
						case item.missing:
							delete(bad, key)
						case item.identity == "pod":
							bad[key] = prefix + item.value.(string)
						default:
							bad[key] = item.value
						}
						rows = append(rows, bad)
					}
					for line := range 3 {
						rows = append(rows, row(now.Add(time.Duration(line-120)*time.Second), fmt.Sprintf("INFO good %s line %d", item.name, line)))
					}
					if kind == "graylog" {
						connection, err := net.Dial("udp", os.Getenv("K13_NATIVE_GRAYLOG_GELF"))
						if err != nil {
							t.Fatal(err)
						}
						for _, value := range rows {
							message := map[string]any{"version": "1.1", "host": "k13-native", "short_message": value["message"], "timestamp": float64(value["at"].(time.Time).UnixMilli()) / 1000, "_kubernetes_namespace_name": namespace}
							for _, key := range []string{podKey, containerKey} {
								if field, ok := value[key]; ok {
									message["_"+key] = field
								}
							}
							encoded, _ := json.Marshal(message)
							if _, err := connection.Write(encoded); err != nil {
								t.Fatal(err)
							}
							time.Sleep(50 * time.Millisecond)
						}
						connection.Close()
					} else {
						var body bytes.Buffer
						for _, value := range rows {
							event := map[string]any{"namespace": namespace, "message": value["message"]}
							for _, key := range []string{podKey, containerKey} {
								if field, ok := value[key]; ok {
									event[key] = field
								}
							}
							encoded, _ := json.Marshal(map[string]any{"time": float64(value["at"].(time.Time).UnixMilli()) / 1000, "index": "main", "sourcetype": "_json", "event": event})
							body.Write(encoded)
						}
						post(t, os.Getenv("K13_NATIVE_SPLUNK_HEC")+"/services/collector/event", map[string]string{"Authorization": "Splunk " + os.Getenv("SPLUNK_HEC_TOKEN")}, body.Bytes())
					}
					for _, selection := range []string{"prefix", "exact"} {
						query := breaklogs.LinkedQuery{Namespace: namespace, PodPrefix: prefix, From: now.Add(-10 * time.Minute), To: now.Add(time.Minute), Limit: 500}
						if selection == "exact" {
							query.Pods = []string{pod}
						}
						wantPartial := item.identity == "container" || item.identity == "pod" && selection == "prefix"
						var result breaklogs.Result
						deadline := time.Now().Add(90 * time.Second)
						settled := false
						for {
							result, err = reader.Read(t.Context(), query)
							if err == nil && len(result.Lines) == 3 && (result.Truncated || !wantPartial) || time.Now().After(deadline) {
								break
							}
							// Good rows share the bad row's HEC/GELF batch: once visible, allow one short settle window only.
							if err == nil && len(result.Lines) == 3 && !settled {
								settled, deadline = true, time.Now().Add(10*time.Second)
							}
							time.Sleep(2 * time.Second)
						}
						ok := err == nil && len(result.Lines) == 3 && result.Truncated == wantPartial
						for _, line := range result.Lines {
							text := line.Text
							if kind == "splunk" {
								var event struct {
									Message string `json:"message"`
								}
								if json.Unmarshal([]byte(text), &event) != nil {
									ok = false
								}
								text = event.Message
							}
							ok = ok && line.Pod == pod && line.Container == "api" && strings.HasPrefix(text, "INFO good")
						}
						verdict := "PASS"
						if !ok {
							verdict = "FAIL"
							failed = append(failed, fmt.Sprintf("%s/%s/%s", item.identity, item.name, selection))
						}
						t.Logf("%s %s %s/%s/%s lines=%d truncated=%v want_partial=%v err=%v", verdict, kind, item.identity, item.name, selection, len(result.Lines), result.Truncated, wantPartial, err)
						if item.identity == "none" && verdict == "FAIL" {
							t.Fatalf("control read failed; malformed variants not attempted: %v", failed)
						}
					}
				}
				if len(failed) > 0 {
					t.Fatalf("malformed identity rows reported complete or lost good rows: %v", failed)
				}
			})
		}
	})
}
