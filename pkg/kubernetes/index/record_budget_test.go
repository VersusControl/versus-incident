package index

import (
	"strconv"
	"testing"
)

// The per-record byte budget is the bound a wide object hits first: a Node with
// managed-node-group and topology labels passes the label count limit and still
// overruns the size budget. The projection asks this helper before handing a
// record over, so the answer has to match what validRecord enforces.
func TestRecordByteBudgetReportsWhatValidRecordEnforces(t *testing.T) {
	build := func(labels int, value string) Record {
		record := Record{UID: "node", Kind: "Node", Name: "ip-10-0-0-1"}
		if labels > 0 {
			record.Labels = make(map[string]string, labels)
		}
		for index := 0; index < labels; index++ {
			record.Labels["topology.kubernetes.io/zone-"+strconv.Itoa(index)] = value
		}
		return record
	}
	if !RecordWithinBudget(build(2, "eu-west-1a")) || !validRecord(build(2, "eu-west-1a")) {
		t.Fatal("a small record was refused")
	}
	wide := build(64, "eu-west-1a")
	if len(wide.Labels) > maxLabels {
		t.Fatal("fixture exceeds the label bound, so it proves nothing about size")
	}
	if RecordWithinBudget(wide) || validRecord(wide) {
		t.Fatal("a record over the byte budget was accepted")
	}
	// Container log metadata carries its own allowance; it must not change the
	// answer for the base record.
	withLogs := build(2, "eu-west-1a")
	withLogs.LogContainers = []ContainerState{{Name: "app"}}
	if !RecordWithinBudget(withLogs) || !validRecord(withLogs) {
		t.Fatal("the container log allowance was not applied")
	}
	overBase := build(30, "eu-west-1a")
	overBase.LogContainers = []ContainerState{{Name: "app"}}
	if !RecordWithinBudget(overBase) {
		t.Fatal("the container log allowance did not extend the base budget")
	}
}
