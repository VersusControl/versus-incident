package common

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/slack-go/slack"
)

func TestIssueActionsValidateOriginAndBound(t *testing.T) {
	links := []map[string]string{}
	for _, u := range []string{"javascript:alert(1)", "https://evil.invalid/a", "https://good.invalid.evil/a", "https://user@good.invalid/a", "https://good.invalid/" + strings.Repeat("x", 3001)} {
		links = append(links, map[string]string{"id": "bad", "label": "bad", "url": u})
	}
	links = append(links, map[string]string{"id": "timeline", "label": "Open timeline", "url": "https://good.invalid/agent"})
	links = append(links, map[string]string{"id": "agent", "label": "Investigate with DevOps Agent", "url": "https://good.invalid/agent/chat"})
	if got := validIssueActions(links, "https://good.invalid"); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if got := validIssueActions(links, ""); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestSlackIssueButtonsWithAndWithoutAcknowledgement(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../.."); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	for _, origin := range []string{"https://good.invalid", ""} {
		for _, ack := range []bool{false, true} {
			for _, disabled := range []bool{false, true} {
				requests := make(chan map[string]interface{}, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					var attachments []map[string]interface{}
					if err := json.Unmarshal([]byte(r.Form.Get("attachments")), &attachments); err != nil {
						t.Error(err)
					}
					if len(attachments) > 0 {
						requests <- attachments[0]
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"ok":true,"channel":"test","ts":"1"}`))
				}))
				p := NewSlackProvider(config.SlackConfig{MessageProperties: config.SlackMessageProperties{DisableButton: disabled}})
				p.publicHost = origin
				p.client = slack.New("test", slack.OptionAPIURL(server.URL+"/"))
				c := map[string]interface{}{"AlertKind": "kubernetes_issue", "PatternID": "kubernetes:pod.failed", "Severity": "critical", "Links": []map[string]string{{"id": "timeline", "label": "Open timeline", "url": "https://good.invalid/agent"}, {"id": "agent", "label": "Investigate with DevOps Agent", "url": "https://good.invalid/agent/chat"}}}
				c["Links"] = append(c["Links"].([]map[string]string), map[string]string{"id": "bad", "label": "bad", "url": "https://evil.invalid/steal"})
				if ack {
					c["AckURL"] = "https://good.invalid/ack"
				}
				if err := p.SendAlert(&m.Incident{ID: "incident", Content: &c}); err != nil {
					t.Fatal(err)
				}
				server.Close()
				request := <-requests
				encoded, _ := json.Marshal(request)
				if strings.Contains(string(encoded), "evil.invalid") {
					t.Fatal("invalid origin leaked to message")
				}
				if disabled || origin == "" {
					if request["text"] == nil || request["blocks"] != nil {
						t.Fatalf("%+v", request)
					}
					if disabled && origin != "" {
						text := request["text"].(string)
						timeline := strings.Index(text, "Open timeline")
						agent := strings.Index(text, "Investigate with DevOps Agent")
						acknowledge := strings.Index(text, "Acknowledge")
						if timeline < 0 || agent < timeline || (ack && (acknowledge < 0 || acknowledge > timeline)) || (!ack && acknowledge >= 0) {
							t.Fatalf("text actions out of order: %s", text)
						}
					}
					continue
				}
				blocks := request["blocks"].([]interface{})
				actions := blocks[1].(map[string]interface{})["elements"].([]interface{})
				want := 2
				if ack {
					want = 3
				}
				if len(actions) != want {
					t.Fatalf("actions=%v", actions)
				}
				text := blocks[0].(map[string]interface{})["text"].(map[string]interface{})["text"].(string)
				if strings.Contains(text, "https://good.invalid") || !strings.Contains(text, "Kubernetes issue") || strings.Contains(text, "Versus Agent") {
					t.Fatal(text)
				}
				if len(c["Links"].([]map[string]string)) != 3 {
					t.Fatal("mutated original content")
				}
			}
		}
	}
}

func TestDeliveryClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := deliveryClient(nil, ctx).Get("https://example.invalid")
	if err == nil {
		t.Fatal("canceled request sent")
	}
}
