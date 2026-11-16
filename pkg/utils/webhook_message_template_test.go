package utils

import (
	"bytes"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"unicode/utf8"
)

// webhookMessageChannels lists the shipped universal webhook templates that
// detect the alert source (CloudWatch, Fluent Bit, Sentry, ...) themselves.
var webhookMessageChannels = []string{"slack", "telegram", "lark", "msteams", "viber"}

func renderWebhookMessage(t *testing.T, channel string, payload map[string]interface{}) string {
	t.Helper()
	path := filepath.Join("../../config", channel+"_message.tmpl")
	tpl, err := template.New(filepath.Base(path)).Funcs(GetTemplateFuncMaps()).ParseFiles(path)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, payload); err != nil {
		t.Fatalf("render %s: %v", path, err)
	}
	return out.String()
}

func TestWebhookMessageCloudWatchRegionFromAlarmArn(t *testing.T) {
	payload := map[string]interface{}{
		"AlarmName":       "checkout-high-cpu",
		"AlarmArn":        "arn:aws:cloudwatch:eu-west-1:123456789012:alarm:checkout-high-cpu",
		"AWSAccountId":    "123456789012",
		"NewStateValue":   "ALARM",
		"NewStateReason":  "Threshold crossed",
		"StateChangeTime": "2026-11-15T10:00:00Z",
	}
	for _, channel := range webhookMessageChannels {
		t.Run(channel, func(t *testing.T) {
			out := renderWebhookMessage(t, channel, payload)
			if !strings.Contains(out, "eu-west-1") || strings.Contains(out, "us-east-1") {
				t.Fatalf("expected region eu-west-1 only:\n%s", out)
			}
			if channel != "viber" && !strings.Contains(out, "https://eu-west-1.console.aws.amazon.com/cloudwatch/home?region=eu-west-1#alarmsV2:alarm/checkout-high-cpu") {
				t.Fatalf("expected eu-west-1 console link:\n%s", out)
			}
			if channel == "viber" && !strings.Contains(out, "123456789012") {
				t.Fatalf("expected account id parsed from the alarm ARN:\n%s", out)
			}
		})
	}
}

func TestWebhookMessageFluentBitLongMultilineError(t *testing.T) {
	first := "ERROR payment-service failed to charge card: upstream timeout after 30s"
	trace := strings.Repeat("\n    at com.example.payments.Gateway.charge(Gateway.java:88)", 40)
	payload := map[string]interface{}{
		"log":  first + trace,
		"date": 1763200800.5,
		"kubernetes": map[string]interface{}{
			"pod_name":       "payment-7d9f",
			"namespace_name": "shop",
			"container_name": "payment",
			"labels":         map[string]interface{}{"app": "payment"},
		},
	}
	for _, channel := range webhookMessageChannels {
		t.Run(channel, func(t *testing.T) {
			out := renderWebhookMessage(t, channel, payload)
			if !strings.Contains(out, "Fluent Bit") || !strings.Contains(out, "ERROR") || !strings.Contains(out, first) || !strings.Contains(out, "Gateway.java:88") {
				t.Fatalf("expected Fluent Bit ERROR with the full error text:\n%s", out)
			}
		})
	}
}

func TestWebhookMessageFluentBitCriticalSeverity(t *testing.T) {
	for _, log := range []string{"CRITICAL disk /var is full", "critical: database connection error, giving up"} {
		payload := map[string]interface{}{
			"log":        log,
			"kubernetes": map[string]interface{}{"pod_name": "db-0", "namespace_name": "data"},
		}
		for _, channel := range webhookMessageChannels {
			t.Run(channel, func(t *testing.T) {
				out := renderWebhookMessage(t, channel, payload)
				if !strings.Contains(out, "Severity:") || !strings.Contains(out, "CRITICAL") || strings.Contains(out, "WARNING") {
					t.Fatalf("expected CRITICAL severity for %q:\n%s", log, out)
				}
			})
		}
	}
}

func TestWebhookMessageSentryProjectSlug(t *testing.T) {
	issue := func(project interface{}) map[string]interface{} {
		item := map[string]interface{}{
			"id":       "4242",
			"title":    "TypeError: cart is undefined",
			"culprit":  "checkout/cart.js",
			"level":    "error",
			"metadata": map[string]interface{}{"value": "cart is undefined"},
		}
		if project != nil {
			item["project"] = project
		}
		return item
	}
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    string
		notWant string
	}{
		{
			name:    "nested project only",
			payload: map[string]interface{}{"action": "created", "data": map[string]interface{}{"issue": issue(map[string]interface{}{"slug": "web-frontend"})}},
			want:    "web-frontend/checkout/cart.js",
		},
		{
			name:    "top level wins",
			payload: map[string]interface{}{"project_slug": "legacy-slug", "data": map[string]interface{}{"issue": issue(map[string]interface{}{"slug": "web-frontend"})}},
			want:    "legacy-slug/checkout/cart.js",
			notWant: "web-frontend",
		},
		{
			name:    "top level only",
			payload: map[string]interface{}{"project_slug": "legacy-slug", "data": map[string]interface{}{"issue": issue(nil)}},
			want:    "legacy-slug/checkout/cart.js",
		},
	}
	for _, tc := range cases {
		for _, channel := range webhookMessageChannels {
			t.Run(tc.name+"/"+channel, func(t *testing.T) {
				out := renderWebhookMessage(t, channel, tc.payload)
				if !strings.Contains(out, tc.want) || (tc.notWant != "" && strings.Contains(out, tc.notWant)) {
					t.Fatalf("expected resource %q:\n%s", tc.want, out)
				}
			})
		}
	}
}

func TestWebhookMessageFluentBitWithoutLogStillRenders(t *testing.T) {
	payload := map[string]interface{}{
		"kubernetes": map[string]interface{}{"pod_name": "worker-5c2a", "namespace_name": "jobs"},
	}
	for _, channel := range webhookMessageChannels {
		t.Run(channel, func(t *testing.T) {
			out := renderWebhookMessage(t, channel, payload)
			if !strings.Contains(out, "Fluent Bit") || !strings.Contains(out, "worker-5c2a") || !strings.Contains(out, "INFO") {
				t.Fatalf("expected an INFO Fluent Bit message for the pod:\n%s", out)
			}
		})
	}
}

func TestWebhookMessageNullParentsStillRender(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    string
	}{
		{"custom data null", map[string]interface{}{"title": "x", "data": nil}, "Unknown Alert"},
		{"custom event null", map[string]interface{}{"title": "x", "event": nil}, "Unknown Alert"},
		{"fluent bit kubernetes null", map[string]interface{}{"log": "ERROR boom", "kubernetes": nil}, "ERROR boom"},
		{"fluent bit kubernetes labels null", map[string]interface{}{"log": "ERROR boom", "kubernetes": map[string]interface{}{"pod_name": "api-1", "labels": nil}}, "api-1"},
		{"alertmanager common annotations null", map[string]interface{}{"receiver": "r", "commonAnnotations": nil, "alerts": []interface{}{map[string]interface{}{"status": "firing", "labels": map[string]interface{}{"alertname": "HighLatency"}}}}, "HighLatency"},
		{"alertmanager labels and annotations null", map[string]interface{}{"receiver": "r", "alerts": []interface{}{map[string]interface{}{"status": "firing", "labels": nil, "annotations": nil}}}, "Prometheus Alert"},
		{"cloudwatch without arn", map[string]interface{}{"AlarmName": "cpu", "NewStateValue": "ALARM"}, "us-east-1"},
		{"cloudwatch arn null", map[string]interface{}{"AlarmName": "cpu", "AlarmArn": nil, "Trigger": nil}, "us-east-1"},
		{"sentry nested parents null", map[string]interface{}{"event": nil, "data": map[string]interface{}{"issue": map[string]interface{}{"id": "7", "title": "Boom", "project": nil, "metadata": nil}}}, "Boom"},
	}
	for _, tc := range cases {
		for _, channel := range webhookMessageChannels {
			t.Run(tc.name+"/"+channel, func(t *testing.T) {
				out := renderWebhookMessage(t, channel, tc.payload)
				if !strings.Contains(out, tc.want) || strings.Contains(out, "<no value>") {
					t.Fatalf("expected %q in the rendered message:\n%s", tc.want, out)
				}
			})
		}
	}
}

func TestWebhookMessageCloudWatchRegionOutsideCommonList(t *testing.T) {
	payload := map[string]interface{}{
		"AlarmName":     "orders-5xx",
		"AlarmArn":      "arn:aws:cloudwatch:ap-south-1:123456789012:alarm:orders-5xx",
		"NewStateValue": "ALARM",
	}
	for _, channel := range webhookMessageChannels {
		t.Run(channel, func(t *testing.T) {
			out := renderWebhookMessage(t, channel, payload)
			if !strings.Contains(out, "ap-south-1") || strings.Contains(out, "us-east-1") {
				t.Fatalf("expected region ap-south-1 only:\n%s", out)
			}
			if channel != "viber" && !strings.Contains(out, "https://ap-south-1.console.aws.amazon.com/cloudwatch/home?region=ap-south-1#alarmsV2:alarm/orders-5xx") {
				t.Fatalf("expected ap-south-1 console link:\n%s", out)
			}
		})
	}
	t.Run("malformed region falls back", func(t *testing.T) {
		out := renderWebhookMessage(t, "slack", map[string]interface{}{
			"AlarmName": "x",
			"AlarmArn":  "arn:aws:cloudwatch:evil.example/x?:1:alarm:x",
		})
		if strings.Contains(out, "evil.example") || !strings.Contains(out, "https://us-east-1.console.aws.amazon.com/") {
			t.Fatalf("expected an unrecognised region to fall back to us-east-1:\n%s", out)
		}
	})
}

func TestWebhookMessageTelegramEscapesAndBoundsDescription(t *testing.T) {
	trace := "ERROR java.lang.IllegalStateException: bad List<String> & state\n    at com.example.Cart.<init>(Cart.java:12)"
	payload := map[string]interface{}{
		"log":        trace,
		"kubernetes": map[string]interface{}{"pod_name": "cart<0>", "labels": map[string]interface{}{"app": "a&b"}},
	}
	out := renderWebhookMessage(t, "telegram", payload)
	for _, raw := range []string{"<init>", "List<String>", "cart<0>", "a&b"} {
		if strings.Contains(out, raw) {
			t.Fatalf("expected %q to be HTML-escaped:\n%s", raw, out)
		}
	}
	for _, escaped := range []string{"&lt;init&gt;", "List&lt;String&gt;", "&amp; state", "cart&lt;0&gt;", "a&amp;b"} {
		if !strings.Contains(out, escaped) {
			t.Fatalf("expected %q in the rendered message:\n%s", escaped, out)
		}
	}

	long := "ERROR " + strings.Repeat("é", 6000)
	out = renderWebhookMessage(t, "telegram", map[string]interface{}{"log": long})
	if !utf8.ValidString(out) || utf8.RuneCountInString(out) > 4096 || !strings.Contains(out, "…") {
		t.Fatalf("expected a valid UTF-8 message under Telegram's 4096-character limit, got %d characters", utf8.RuneCountInString(out))
	}
}

func TestWebhookMessageCloudWatchConsoleLinkEscapesAlarmName(t *testing.T) {
	name := "prod api 5xx & errors #1?x=y"
	payload := map[string]interface{}{
		"AlarmName":     name,
		"AlarmArn":      "arn:aws:cloudwatch:eu-west-1:123456789012:alarm:prod",
		"NewStateValue": "ALARM",
	}
	const prefix = "https://eu-west-1.console.aws.amazon.com/cloudwatch/home?region=eu-west-1#alarmsV2:alarm/"
	for _, channel := range []string{"slack", "telegram", "lark", "msteams"} {
		t.Run(channel, func(t *testing.T) {
			out := renderWebhookMessage(t, channel, payload)
			start := strings.Index(out, prefix)
			if start < 0 {
				t.Fatalf("console link missing:\n%s", out)
			}
			link := out[start:]
			if end := strings.IndexAny(link, " \n|)\"<>"); end >= 0 {
				link = link[:end]
			}
			if channel == "telegram" {
				link = strings.ReplaceAll(link, "&amp;", "&")
			}
			parsed, err := url.Parse(link)
			if err != nil {
				t.Fatalf("link %q is not a valid URL: %v", link, err)
			}
			if parsed.Host != "eu-west-1.console.aws.amazon.com" || parsed.Query().Get("region") != "eu-west-1" || len(parsed.Query()) != 1 {
				t.Fatalf("link %q has unexpected host or query", link)
			}
			if parsed.Fragment != "alarmsV2:alarm/"+name {
				t.Fatalf("link %q fragment decodes to %q, want the full alarm name", link, parsed.Fragment)
			}
		})
	}
}
