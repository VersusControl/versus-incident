package cost

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// TestNoneFoundNeedsTheGlobalStream: a cost line's search covers the
// account's global stream as well as the line's Region, so with the
// Region fully read but the global stream never read the answer is
// history_incomplete, not "no change found".
func TestNoneFoundNeedsTheGlobalStream(t *testing.T) {
	startDay := finops.MustDate("2026-10-20")
	sc := &scenario{
		Inventory: []model.Resource{goldenResource(goldenARN("lambda", "function:api"), "lambda", "function", "api", nil)},
		CloudTrail: []cloudTrailEvent{goldenEvent(t, "evt-other", "lambda.amazonaws.com", "UpdateFunctionConfiguration20150331v2",
			startDay.Time().Add(-5*time.Hour), map[string]any{"functionName": "other"})},
		ChangeCoverage: []changes.Interval{{From: startDay.Time().Add(-10 * 24 * time.Hour), To: startDay.Time().Add(10 * 24 * time.Hour)}},
		GlobalUnread:   true,
	}
	line := finops.Detected{Key: finops.LineKey{Account: goldenAccount, Service: "compute.function", UsageType: "USE1-Lambda-GB-Second", RecordType: "usage"},
		ServiceName: "AWS Lambda", StartedOn: startDay}
	list, _, err := NewSuspectFinder(loadInventory(t, sc)).Suspects(ctx, line)
	if err != nil || len(list) != 1 || list[0].State != "history_incomplete" {
		t.Fatalf("unread global stream: %+v %v", list, err)
	}
	sc.GlobalUnread = false
	if list, _, err := NewSuspectFinder(loadInventory(t, sc)).Suspects(ctx, line); err != nil || len(list) != 1 || list[0].State != "none_found" {
		t.Fatalf("read global stream: %+v %v", list, err)
	}
}
