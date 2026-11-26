package aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// changeFeedWith stores normalized CloudTrail events, covers the Region and
// the global stream up to coveredUntil, and returns the change list.
func changeFeedWith(t *testing.T, now time.Time, coveredUntil time.Time, events ...changes.Event) *snapshot.ChangeFeed {
	t.Helper()
	return changeFeedOver(t, now, coveredUntil, nil, events...)
}

// changeFeedOver is changeFeedWith with extra feed options.
func changeFeedOver(t *testing.T, now time.Time, coveredUntil time.Time, options []snapshot.FeedOption, events ...changes.Event) *snapshot.ChangeFeed {
	t.Helper()
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope(), snapshot.WithClock(func() time.Time { return now }))
	found := []changes.Interval{{From: now.Add(-48 * time.Hour), To: coveredUntil}}
	normalized := make([]model.Change, 0, len(events))
	for _, event := range events {
		change, ok := changes.Normalize(account, "us-east-1", event)
		if !ok {
			t.Fatalf("event %s dropped", event.ID)
		}
		normalized = append(normalized, change)
	}
	read := []changes.Interval{{From: found[0].From, To: now}}
	for region, list := range map[string][]model.Change{"us-east-1": normalized, "global": nil} {
		poll := snapshot.ChangePoll{Found: list, Covered: found, Read: read, HistoryFrom: found[0].From}
		if err := store.RecordChangePoll(account, region, poll); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot.NewChangeFeed(store, append([]snapshot.FeedOption{snapshot.WithFeedClock(func() time.Time { return now })}, options...)...)
}

func invokeChanges(t *testing.T, feed ChangeAnswerer, args string) (*core.ToolResult, string) {
	t.Helper()
	tools := NewChanges(func() ChangeAnswerer { return feed })
	result, err := tools[0].Invoke(viewerCtx(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	return result, string(encoded)
}

// TestChangesToolRedacts: an email-shaped IAM user, an email and an access
// key ID planted in request identifiers never reach the model.
func TestChangesToolRedacts(t *testing.T) {
	now := fixedNow
	event := changes.Event{ID: "evt-1", Name: "CreateStack", Source: "cloudformation.amazonaws.com", Time: now.Add(-2 * time.Hour),
		Raw: `{"userIdentity":{"type":"IAMUser","arn":"arn:aws:iam::111122223333:user/jane.doe@corp.example"},` +
			`"requestParameters":{"stackName":"ops-AKIA` + `ABCDEFGHIJKLMNOP"}}`}
	feed := changeFeedWith(t, now, now.Add(-20*time.Minute), event)
	result, encoded := invokeChanges(t, feed, `{"window":"24h"}`)
	if !result.Found {
		t.Fatalf("change not found: %s", encoded)
	}
	for _, secret := range []string{"jane.doe@corp.example", "AKIA" + "ABCDEFGHIJKLMNOP"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("tool output carries %q: %s", secret, encoded)
		}
	}
	if !strings.Contains(encoded, changes.MaskedEmail) {
		t.Fatalf("email not masked: %s", encoded)
	}
}

// TestChangesToolNeverSaysNoChangesOverAGap: with part of the window not
// read and nothing found, the answer says the changes are unknown; only a
// complete window may say "No changes".
func TestChangesToolNeverSaysNoChangesOverAGap(t *testing.T) {
	now := fixedNow
	gappy := changeFeedWith(t, now, now.Add(-6*time.Hour))
	result, encoded := invokeChanges(t, gappy, `{}`)
	answer, _ := result.Data["answer"].(string)
	if result.Found || result.Data["complete"] != false || strings.Contains(answer, "No changes") || !strings.Contains(answer, "unknown") {
		t.Fatalf("incomplete window answer: %s", encoded)
	}
	quiet := changeFeedWith(t, now, now.Add(-20*time.Minute))
	result, encoded = invokeChanges(t, quiet, `{"window":"1h"}`)
	answer, _ = result.Data["answer"].(string)
	if result.Data["complete"] != true || !strings.HasPrefix(answer, "No changes") {
		t.Fatalf("complete quiet window answer: %s", encoded)
	}
}

// TestChangesToolRefusesAndExplains: a longer window is locked, a bad
// argument is an argument error, a caller without the permission and an
// unset feed get an unavailable answer.
func TestChangesToolRefusesAndExplains(t *testing.T) {
	feed := changeFeedWith(t, fixedNow, fixedNow.Add(-20*time.Minute))
	if result, encoded := invokeChanges(t, feed, `{"window":"7d"}`); result.Reason != "window_locked" || result.Data["max_window"] != "24h" {
		t.Fatalf("7d window: %s", encoded)
	}
	tools := NewChanges(func() ChangeAnswerer { return feed })
	if _, err := tools[0].Invoke(viewerCtx(), json.RawMessage(`{"account_id":"12"}`)); err == nil {
		t.Fatal("bad account accepted")
	}
	if _, err := tools[0].Invoke(viewerCtx(), json.RawMessage(`{"write":true}`)); err == nil {
		t.Fatal("unknown argument accepted")
	}
	if result, _ := tools[0].Invoke(context.Background(), nil); result.IsAvailable() {
		t.Fatal("caller without infrastructure:view got data")
	}
	unset := NewChanges(func() ChangeAnswerer { return nil })
	if result, _ := unset[0].Invoke(viewerCtx(), nil); result.IsAvailable() {
		t.Fatal("unset feed answered")
	}
	if NewChanges(nil) != nil {
		t.Fatal("nil source built a tool")
	}
}

// TestChangesToolFollowsTheFeedWindowLimits: a change list whose edition
// serves 7 days answers a 7d window, and a longer one is locked naming 7d.
func TestChangesToolFollowsTheFeedWindowLimits(t *testing.T) {
	week := 7 * 24 * time.Hour
	feed := changeFeedOver(t, fixedNow, fixedNow.Add(-20*time.Minute),
		[]snapshot.FeedOption{snapshot.WithChangesLimits(func() snapshot.ChangesLimits {
			return snapshot.ChangesLimits{MaxWindow: week, MaxCompareAge: week}
		})})
	if result, encoded := invokeChanges(t, feed, `{"window":"7d"}`); result.Reason == "window_locked" || !result.IsAvailable() {
		t.Fatalf("7d window with a 7-day limit: %s", encoded)
	}
	if result, encoded := invokeChanges(t, feed, `{"window":"8d"}`); result.Reason != "window_locked" || result.Data["max_window"] != "7d" {
		t.Fatalf("8d window with a 7-day limit: %s", encoded)
	}
}

// An expired or hand-made cursor is an invalid-arguments answer telling the
// model to read the first page again, never a raw backend error.
func TestChangesToolExpiredCursorIsInvalidArguments(t *testing.T) {
	feed := changeFeedWith(t, fixedNow, fixedNow.Add(-20*time.Minute))
	cursor := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"r":"` + fixedNow.Format(time.RFC3339) + `","t":"` + fixedNow.Format(time.RFC3339) + `","i":"~"}`))
	tools := NewChanges(func() ChangeAnswerer { return feed })
	_, err := tools[0].Invoke(viewerCtx(), json.RawMessage(`{"cursor":"`+cursor+`"}`))
	var toolErr *core.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != core.ToolErrorInvalidArguments {
		t.Fatalf("expired cursor = %v", err)
	}
}
