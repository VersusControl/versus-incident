package utils

import "testing"

func TestExtractService_TopLevelShapesUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		content map[string]interface{}
		want    string
	}{
		{"ServiceName", map[string]interface{}{"ServiceName": "checkout"}, "checkout"},
		{"Service", map[string]interface{}{"Service": "checkout"}, "checkout"},
		{"lowercase service", map[string]interface{}{"service": "checkout"}, "checkout"},
		{"service_name", map[string]interface{}{"service_name": "checkout"}, "checkout"},
		{"servicename", map[string]interface{}{"servicename": "checkout"}, "checkout"},
		{"app", map[string]interface{}{"app": "checkout"}, "checkout"},
		{"component", map[string]interface{}{"component": "checkout"}, "checkout"},
		{"top level wins over labels", map[string]interface{}{
			"service": "checkout",
			"labels":  map[string]interface{}{"service": "other"},
		}, "checkout"},
		{"no service", map[string]interface{}{"title": "boom"}, ""},
		{"nil content", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractService(tc.content); got != tc.want {
				t.Fatalf("ExtractService = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtract_AlertmanagerNestedLabels covers the Prometheus/Alertmanager shape
// where BOTH service and severity live only under labels/commonLabels.
func TestExtract_AlertmanagerNestedLabels(t *testing.T) {
	cases := []struct {
		name    string
		content map[string]interface{}
	}{
		{"labels", map[string]interface{}{
			"alertname": "HighErrorRate",
			"labels":    map[string]interface{}{"service": "checkout", "severity": "warning"},
		}},
		{"commonLabels", map[string]interface{}{
			"alertname":    "HighErrorRate",
			"commonLabels": map[string]interface{}{"service": "checkout", "severity": "warning"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractService(tc.content); got != "checkout" {
				t.Fatalf("ExtractService = %q, want checkout", got)
			}
			if got := ExtractSeverity(tc.content); got != "warning" {
				t.Fatalf("ExtractSeverity = %q, want warning", got)
			}
		})
	}
}

// TestExtractService_CloudWatchDimensions covers the CloudWatch-alarm-via-SNS
// shape, where the service is only reachable through Trigger.Dimensions[].
func TestExtractService_CloudWatchDimensions(t *testing.T) {
	cases := []struct {
		name    string
		trigger map[string]interface{}
		want    string
	}{
		{"ECS service name beats cluster name", map[string]interface{}{
			"Namespace":  "AWS/ECS",
			"MetricName": "CPUUtilization",
			"Dimensions": []interface{}{
				map[string]interface{}{"name": "ClusterName", "value": "prod"},
				map[string]interface{}{"name": "ServiceName", "value": "checkout"},
			},
		}, "checkout"},
		{"capitalised Name/Value spelling", map[string]interface{}{
			"Dimensions": []interface{}{
				map[string]interface{}{"Name": "ServiceName", "Value": "checkout"},
			},
		}, "checkout"},
		{"lambda function name", map[string]interface{}{
			"Dimensions": []interface{}{
				map[string]interface{}{"name": "FunctionName", "value": "billing-worker"},
			},
		}, "billing-worker"},
		{"rds instance identifier", map[string]interface{}{
			"Dimensions": []interface{}{
				map[string]interface{}{"name": "DBInstanceIdentifier", "value": "orders-db"},
			},
		}, "orders-db"},
		{"dynamodb table name", map[string]interface{}{
			"Dimensions": []interface{}{
				map[string]interface{}{"name": "TableName", "value": "sessions"},
			},
		}, "sessions"},
		{"sqs queue name", map[string]interface{}{
			"Dimensions": []interface{}{
				map[string]interface{}{"name": "QueueName", "value": "emails"},
			},
		}, "emails"},
		{"resource name beats bare instance id", map[string]interface{}{
			"Dimensions": []interface{}{
				map[string]interface{}{"name": "InstanceId", "value": "i-0abc123"},
				map[string]interface{}{"name": "FunctionName", "value": "billing-worker"},
			},
		}, "billing-worker"},
		{"instance id is the last resort", map[string]interface{}{
			"Dimensions": []interface{}{
				map[string]interface{}{"name": "InstanceId", "value": "i-0abc123"},
			},
		}, "i-0abc123"},
		{"unknown dimension names yield nothing", map[string]interface{}{
			"Dimensions": []interface{}{
				map[string]interface{}{"name": "Currency", "value": "usd"},
			},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := map[string]interface{}{"AlarmName": "cpu-high", "Trigger": tc.trigger}
			if got := ExtractService(content); got != tc.want {
				t.Fatalf("ExtractService = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractSeverity(t *testing.T) {
	cases := []struct {
		name    string
		content map[string]interface{}
		want    string
	}{
		{"top-level severity", map[string]interface{}{"severity": "critical"}, "critical"},
		{"Severity", map[string]interface{}{"Severity": "critical"}, "critical"},
		{"level", map[string]interface{}{"level": "warning"}, "warning"},
		{"priority", map[string]interface{}{"priority": "P1"}, "P1"},
		// A pattern verdict is operator-set free text. It must NOT surface as a
		// severity here: this value drives the priority floor and the dedup
		// fingerprint, so a typed-in "critical" would steer suppression.
		{"Verdict is not a severity", map[string]interface{}{"Verdict": "critical"}, ""},
		{"lowercase verdict is not a severity", map[string]interface{}{"verdict": "anomaly"}, ""},
		{"real severity still wins alongside a verdict", map[string]interface{}{"severity": "low", "Verdict": "critical"}, "low"},
		{"nested labels severity beats a verdict", map[string]interface{}{
			"verdict": "critical",
			"labels":  map[string]interface{}{"severity": "warning"},
		}, "warning"},
		{"cloudwatch severity dimension", map[string]interface{}{
			"Trigger": map[string]interface{}{"Dimensions": []interface{}{
				map[string]interface{}{"name": "Severity", "value": "high"},
			}},
		}, "high"},
		{"alarm state is never a severity", map[string]interface{}{
			"AlarmName":     "cpu-high",
			"NewStateValue": "ALARM",
			"Trigger": map[string]interface{}{"Dimensions": []interface{}{
				map[string]interface{}{"name": "ServiceName", "value": "checkout"},
			}},
		}, ""},
		{"nil content", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractSeverity(tc.content); got != tc.want {
				t.Fatalf("ExtractSeverity = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractTitle covers the shared title key set: the pre-existing shapes are
// unchanged and a CloudWatch alarm is now titled by its AlarmName instead of
// falling through to an unrelated key.
func TestExtractTitle(t *testing.T) {
	cases := []struct {
		name    string
		content map[string]interface{}
		want    string
	}{
		{"title", map[string]interface{}{"title": "disk full"}, "disk full"},
		{"alertname", map[string]interface{}{"alertname": "HighErrorRate"}, "HighErrorRate"},
		{"AlertName", map[string]interface{}{"AlertName": "pool exhausted"}, "pool exhausted"},
		{"summary", map[string]interface{}{"summary": "cpu saturated"}, "cpu saturated"},
		{"subject", map[string]interface{}{"subject": "nightly job failed"}, "nightly job failed"},
		{"name", map[string]interface{}{"name": "checkout-latency"}, "checkout-latency"},
		{"title wins over summary", map[string]interface{}{"title": "disk full", "summary": "node-1 at 98%"}, "disk full"},
		{"cloudwatch AlarmName (the reported bug)", map[string]interface{}{
			"AlarmName":     "checkout-5xx",
			"NewStateValue": "ALARM",
		}, "checkout-5xx"},
		{"cloudwatch AlarmName does not lose to an unrelated name key", map[string]interface{}{
			"AlarmName": "checkout-5xx",
			"name":      "i-0abc123",
		}, "checkout-5xx"},
		{"nested alertmanager labels", map[string]interface{}{
			"labels": map[string]interface{}{"alertname": "PostgresqlDown", "severity": "critical"},
		}, "PostgresqlDown"},
		{"nested commonLabels", map[string]interface{}{
			"commonLabels": map[string]interface{}{"alertname": "PaymentGatewayDown"},
		}, "PaymentGatewayDown"},
		{"top level wins over nested labels", map[string]interface{}{
			"title":  "outer",
			"labels": map[string]interface{}{"alertname": "inner"},
		}, "outer"},
		{"no title key stays blank", map[string]interface{}{"service": "checkout"}, ""},
		{"nil content", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractTitle(tc.content); got != tc.want {
				t.Fatalf("ExtractTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPayloadString covers the exported content accessor callers use to read
// one extra key of their own (the report's verdict fallback): exact match
// first, then case-insensitive, trimmed, in the caller's priority order.
func TestPayloadString(t *testing.T) {
	content := map[string]interface{}{"Verdict": "  known  ", "other": 42}
	if got := PayloadString(content, "Verdict"); got != "known" {
		t.Fatalf("PayloadString exact = %q, want known", got)
	}
	if got := PayloadString(content, "verdict"); got != "known" {
		t.Fatalf("PayloadString case-insensitive = %q, want known", got)
	}
	if got := PayloadString(content, "missing"); got != "" {
		t.Fatalf("PayloadString missing key = %q, want empty", got)
	}
	if got := PayloadString(content, "other"); got != "" {
		t.Fatalf("PayloadString non-string value = %q, want empty", got)
	}
	if got := PayloadString(nil, "Verdict"); got != "" {
		t.Fatalf("PayloadString(nil) = %q, want empty", got)
	}
	first := map[string]interface{}{"a": "one", "b": "two"}
	if got := PayloadString(first, "b", "a"); got != "two" {
		t.Fatalf("PayloadString priority order = %q, want two", got)
	}
}

// TestExtract_CaseInsensitiveDuplicateKeysAreDeterministic covers a payload
// that carries two spellings of the same key. Resolving the match by ranging
// the content map made the extracted value — and the dedup key built from it —
// flip between reads, because Go randomises map iteration order. The winner is
// now fixed: the exact spelling, else the lexicographically smallest match.
func TestExtract_CaseInsensitiveDuplicateKeysAreDeterministic(t *testing.T) {
	const iterations = 2000

	cases := []struct {
		name    string
		content map[string]interface{}
		extract func(map[string]interface{}) string
		want    string
	}{
		{
			name:    "title, exact spelling of the candidate key wins",
			content: map[string]interface{}{"AlarmName": "alpha", "alarmname": "beta"},
			extract: ExtractTitle,
			want:    "beta",
		},
		{
			name:    "title, no exact spelling falls back to the smallest match",
			content: map[string]interface{}{"ALARMNAME": "alpha", "AlarmName": "beta"},
			extract: ExtractTitle,
			want:    "alpha",
		},
		{
			name:    "title, an empty duplicate never shadows the usable spelling",
			content: map[string]interface{}{"ALERTNAME": "   ", "AlertName": "beta"},
			extract: ExtractTitle,
			want:    "beta",
		},
		{
			name: "title inside duplicate-spelling nested label maps",
			content: map[string]interface{}{
				"LABELS": map[string]interface{}{"alertname": "alpha"},
				"Labels": map[string]interface{}{"alertname": "beta"},
			},
			extract: ExtractTitle,
			want:    "alpha",
		},
		{
			name:    "service",
			content: map[string]interface{}{"SERVICENAME": "alpha", "Servicename": "beta"},
			extract: ExtractService,
			want:    "alpha",
		},
		{
			name:    "severity",
			content: map[string]interface{}{"SEVERITY": "critical", "sEVERITY": "warning"},
			extract: ExtractSeverity,
			want:    "critical",
		},
		{
			name: "cloudwatch dimensions parent key",
			content: map[string]interface{}{"Trigger": map[string]interface{}{
				"DIMENSIONS": []interface{}{
					map[string]interface{}{"name": "ServiceName", "value": "alpha"},
				},
				"Dimensions": []interface{}{
					map[string]interface{}{"name": "ServiceName", "value": "beta"},
				},
			}},
			extract: ExtractService,
			want:    "beta",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]int{}
			for i := 0; i < iterations; i++ {
				seen[tc.extract(tc.content)]++
			}
			if len(seen) != 1 {
				t.Fatalf("extraction is non-deterministic over %d reads: %v", iterations, seen)
			}
			if got := tc.extract(tc.content); got != tc.want {
				t.Fatalf("extraction = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractTitle_DuplicateSpellingsSurviveRemarshalling proves the fix holds
// for the shape the bug arrives in: a payload decoded from JSON, where the map
// is rebuilt (and its iteration order reseeded) on every delivery.
func TestExtractTitle_DuplicateSpellingsSurviveRemarshalling(t *testing.T) {
	want := ExtractTitle(map[string]interface{}{"AlarmName": "alpha", "alarmname": "beta"})
	if want == "" {
		t.Fatal("ExtractTitle yielded no title")
	}
	for i := 0; i < 2000; i++ {
		content := map[string]interface{}{"AlarmName": "alpha", "alarmname": "beta"}
		if got := ExtractTitle(content); got != want {
			t.Fatalf("ExtractTitle on read %d = %q, want %q", i, got, want)
		}
	}
}

// TestExtract_FailSoft proves malformed payloads yield "" instead of panicking.
func TestExtract_FailSoft(t *testing.T) {
	cases := []map[string]interface{}{
		{"service": 42},
		{"labels": "not-a-map"},
		{"Trigger": "not-a-map"},
		{"Trigger": map[string]interface{}{"Dimensions": "not-a-list"}},
		{"Trigger": map[string]interface{}{"Dimensions": []interface{}{"not-a-map", nil}}},
		{"Trigger": map[string]interface{}{"Dimensions": []interface{}{
			map[string]interface{}{"name": "ServiceName", "value": 7},
		}}},
		{"service": "   "},
		{"title": 42},
		{"AlarmName": nil},
		{"labels": []interface{}{"not-a-map"}},
	}
	for _, content := range cases {
		if got := ExtractService(content); got != "" {
			t.Fatalf("ExtractService(%v) = %q, want empty", content, got)
		}
		if got := ExtractSeverity(content); got != "" {
			t.Fatalf("ExtractSeverity(%v) = %q, want empty", content, got)
		}
		if got := ExtractTitle(content); got != "" {
			t.Fatalf("ExtractTitle(%v) = %q, want empty", content, got)
		}
	}
}
