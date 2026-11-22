package collect

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Every AWS denial spelling, bare or wrapped, is a denied gap; only the
// opt-in answers read as a feature that is off, and the support-plan answer
// reads as not on the plan.
func TestAWSErrorCodesMapToTheirReadOutcome(t *testing.T) {
	cases := map[string]model.ReadOutcome{
		"AccessDenied": model.OutcomeNotRead, "AccessDeniedException": model.OutcomeNotRead,
		"AccessDeniedFault": model.OutcomeNotRead, "AccessDeniedForDependencyException": model.OutcomeNotRead,
		"UnauthorizedOperation": model.OutcomeNotRead, "AuthorizationError": model.OutcomeNotRead,
		"AuthorizationErrorException": model.OutcomeNotRead, "NotAuthorized": model.OutcomeNotRead,
		"Forbidden": model.OutcomeNotRead, "UnauthorizedException": model.OutcomeNotRead,
		"SubscriptionRequiredException": model.OutcomeNotRead,
		"OptInRequired":                 model.OutcomeOffInAWS, "RegionDisabledException": model.OutcomeOffInAWS,
	}
	c := &call{}
	for code, want := range cases {
		for name, err := range map[string]error{
			"bare":    &smithy.GenericAPIError{Code: code},
			"wrapped": fmt.Errorf("operation error: %w", &smithy.GenericAPIError{Code: code}),
		} {
			gap := c.detailGap("Op", err)
			if gap.Outcome() != want {
				t.Errorf("%s %s = %s (%s), want %s", name, code, gap.Reason, gap.Outcome(), want)
			}
			if strings.HasPrefix(code, "AccessDenied") && gap.Reason != model.GapDenied {
				t.Errorf("%s %s = %s, want denied", name, code, gap.Reason)
			}
		}
	}
	if gap := c.detailGap("Op", &smithy.GenericAPIError{Code: "SubscriptionRequiredException"}); gap.Reason != model.GapNotOnPlan {
		t.Errorf("subscription required = %s, want not on plan", gap.Reason)
	}
}

// One operation failing for two reasons stays two gaps, and each names how
// many items read: the answered items and every failure add up to the calls
// made, whatever the failures' reasons.
func TestMergedDetailCountsAddUpAcrossReasons(t *testing.T) {
	c := &call{unit: model.Unit{Account: productionAccount, Region: testRegion, Service: "sqs"}}
	denied := &smithy.GenericAPIError{Code: "AccessDeniedException"}
	throttled := &smithy.GenericAPIError{Code: "ThrottlingException"}
	var gaps []model.Gap
	for index := range 35 {
		c.tried("GetQueueAttributes")
		switch {
		case index < 3:
			gaps = append(gaps, c.detailGap("GetQueueAttributes", denied))
		case index < 5:
			gaps = append(gaps, c.detailGap("GetQueueAttributes", throttled))
		}
	}
	merged := c.mergeDetailGaps(gaps)
	if len(merged) != 2 {
		t.Fatalf("merged = %+v, want one gap per reason", merged)
	}
	for _, gap := range merged {
		if gap.Seen != 30 || gap.Total != 35 {
			t.Errorf("%s = %d of %d, want 30 of 35", gap.Reason, gap.Seen, gap.Total)
		}
	}
	if merged[0].Reason != model.GapDenied || merged[1].Reason != model.GapThrottled {
		t.Errorf("order = %s, %s, want first seen first", merged[0].Reason, merged[1].Reason)
	}

	// Every call failed: zero read, never negative, never dropped.
	all := &call{unit: c.unit}
	all.tried("GetQueueAttributes")
	all.tried("GetQueueAttributes")
	got := all.mergeDetailGaps([]model.Gap{all.detailGap("GetQueueAttributes", denied), all.detailGap("GetQueueAttributes", denied)})
	if len(got) != 1 || got[0].Seen != 0 || got[0].Total != 2 {
		t.Fatalf("every call denied = %+v, want 0 of 2", got)
	}

	// More failures than counted calls (a call that failed more than once)
	// never reads as a negative count.
	over := &call{unit: c.unit}
	over.tried("ListStackResources")
	got = over.mergeDetailGaps([]model.Gap{over.detailGap("ListStackResources", denied), over.detailGap("ListStackResources", throttled)})
	for _, gap := range got {
		if gap.Seen < 0 || gap.Total != 1 {
			t.Errorf("over-counted failures = %+v", gap)
		}
	}

	// No gaps stays no gaps; an empty cap of zero items never reads as a
	// count.
	if out := c.mergeDetailGaps(nil); len(out) != 0 {
		t.Fatalf("no gaps = %+v", out)
	}
	if gap := c.capGap("GetQueueAttributes", 0, 0); gap.Seen != 0 || gap.Total != 0 || gap.Reason != model.GapCapped {
		t.Fatalf("empty cap = %+v", gap)
	}
}
