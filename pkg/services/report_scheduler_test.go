package services

import "testing"

// TestReportScheduleJobName_StaysStable locks the exported ownership key the
// daily digest gates on. Both entrypoints (OSS cmd/main.go and the enterprise
// binary) and the enterprise HA ownership test key on this SAME constant, so a
// silent rename here would let two replicas each send their own copy under HA
// (or break the single-owner gate entirely). The value is part of the
// cross-binary contract and must not drift.
func TestReportScheduleJobName_StaysStable(t *testing.T) {
	if ReportScheduleJobName != "report-daily-digest" {
		t.Fatalf("ReportScheduleJobName = %q, want %q — the HA ownership gate keys on this exact string across both binaries", ReportScheduleJobName, "report-daily-digest")
	}
}
