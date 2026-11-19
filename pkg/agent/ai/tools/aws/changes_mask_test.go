package aws

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// fixedAnswer returns one prepared answer, so the test sees what the tool
// itself masks rather than what ingest already hid.
type fixedAnswer struct{ answer snapshot.ChangesAnswer }

func (f fixedAnswer) Answer(context.Context, snapshot.FeedRequest) (snapshot.ChangesAnswer, error) {
	return f.answer, nil
}

// An email address that reaches the tool in any answer text (a summary, an
// actor, a field value) is masked before it reaches the model.
func TestChangesToolMasksEmailsItIsHanded(t *testing.T) {
	now := fixedNow
	item := snapshot.FeedItem{Kind: snapshot.ItemChange, ID: "evt-1", Time: now.Add(-time.Hour), Account: account, Region: "us-east-1",
		Service: "cloudformation", EventName: "cloudformation:CreateStack", ActorClass: "person", Actor: "jane@corp.example",
		Summary: "Stack created by jane@corp.example", Resources: []string{}, Fields: []snapshot.FeedField{}}
	answer := snapshot.ChangesAnswer{Complete: true, Items: []snapshot.FeedItem{item}}
	answer.Window.Since, answer.Window.Until, answer.Window.SettledUntil = now.Add(-24*time.Hour), now, now.Add(-time.Hour/4)
	answer.Summary.Total = 1

	_, encoded := invokeChanges(t, fixedAnswer{answer: answer}, `{"window":"24h"}`)
	if strings.Contains(encoded, "jane@corp.example") {
		t.Fatalf("tool output carries an email: %s", encoded)
	}
	if !strings.Contains(encoded, changes.MaskedEmail) {
		t.Fatalf("email not masked: %s", encoded)
	}
}
