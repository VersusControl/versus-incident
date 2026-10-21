package common

import (
	"net/url"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	htmlparser "golang.org/x/net/html"
)

func TestKubernetesIssueEmailHTMLRendererPreservesSignedQueries(t *testing.T) {
	targets := []string{
		"https://versus.invalid/api/ack/incident?expires=123&signature=a%2Bb%2Fc%3D&token=x%26y",
		"https://versus.invalid/agent/kubernetes?cluster=prod&view=timeline&root_uid=root%26uid&namespace=shop",
		"https://versus.invalid/agent/chat?provider=kubernetes&cluster=prod&include_changes=true&issue=pod.failed",
	}
	content := map[string]interface{}{
		"AlertKind": "kubernetes_issue", "PatternID": "kubernetes:pod.failed",
		"AlertName": `Checkout & shop <script>alert("x")</script>`, "AckURL": targets[0],
		"Links": []map[string]string{{"label": "Open timeline", "url": targets[1]}, {"label": "Investigate with DevOps Agent", "url": targets[2]}},
	}
	provider := NewEmailProvider(config.EmailConfig{KubernetesIssueTemplatePath: "../../config/kubernetes_issue_email.tmpl"})
	rendered, err := provider.renderAlertTemplate(&m.Incident{Content: &content})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "&amp;amp;") || strings.Contains(rendered, "<script>") || !strings.Contains(rendered, "Checkout &amp; shop") {
		t.Fatal(rendered)
	}
	document, err := htmlparser.Parse(strings.NewReader(rendered))
	if err != nil {
		t.Fatal(err)
	}
	var links []string
	var visit func(*htmlparser.Node)
	visit = func(node *htmlparser.Node) {
		if node.Type == htmlparser.ElementNode && node.Data == "a" {
			for _, attr := range node.Attr {
				if attr.Key == "href" {
					links = append(links, attr.Val)
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(document)
	if len(links) != len(targets) {
		t.Fatal(links)
	}
	for i, target := range targets {
		if links[i] != target {
			t.Fatalf("link %d changed: %q want %q", i, links[i], target)
		}
		got, _ := url.Parse(links[i])
		want, _ := url.Parse(target)
		if got.Query().Encode() != want.Query().Encode() {
			t.Fatalf("query changed: %v", got.Query())
		}
	}
}
