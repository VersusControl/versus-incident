// Package issuerule registers two AWS issue rules for the two-replica issue
// tests. No product build imports it: the tests' launcher links it into the
// server binaries it builds with a go build overlay that adds one blank import
// of this package to the server's main package.
//
// The firing rule raises a critical reliability issue for the tests/fakeaws
// collector world's CloudWatch alarm checkout-5xx while it is in ALARM, as it
// is in the bad-deploy scenario. The listed rule raises a medium issue for
// every other alarm, only while the test asks for it.
//
// Before it answers, the firing rule reports each evaluation to the test's
// gate (AWS_ISSUE_RULE_GATE_URL), naming the replica (AWS_ISSUE_RULE_REPLICA).
// The gate can hold the evaluation there, which keeps the replica inside an
// evaluation with the replica lock held, and its answer can add a volatile
// evidence line (a note) and turn the listed rule on. Without a gate, or when
// it cannot be reached, the rules answer from the estate alone.
package issuerule

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
)

const (
	// FiringRuleID is the critical rule's ID.
	FiringRuleID = "replica-test.alarm-firing"
	// ListedRuleID is the medium rule's ID.
	ListedRuleID = "replica-test.alarm-listed"
	// FiringAlarm is the alarm the firing rule watches.
	FiringAlarm = "checkout-5xx"
	// GateEnv names the gate's URL.
	GateEnv = "AWS_ISSUE_RULE_GATE_URL"
	// ReplicaEnv names the replica in each gate call.
	ReplicaEnv = "AWS_ISSUE_RULE_REPLICA"
	// gateTimeout bounds one gate call; a held evaluation goes on after it.
	gateTimeout = 6 * time.Minute
	// input is the collector unit service the alarms are read by.
	input = "cloudwatch"
)

// Call is what the firing rule sends the gate for each evaluation.
type Call struct {
	Replica  string `json:"replica"`
	Account  string `json:"account"`
	Findings int    `json:"findings"`
}

// Answer is the gate's reply.
type Answer struct {
	// Note, when set, adds a volatile evidence line, so the evaluation
	// writes the issue without changing its material evidence.
	Note string `json:"note,omitempty"`
	// Listed turns the listed rule on for this evaluation.
	Listed bool `json:"listed,omitempty"`
}

// listed carries the gate's answer from the firing rule to the listed rule,
// which the evaluator runs after it (rules run in ID order).
var listed atomic.Bool

func init() {
	for _, rule := range []issues.Rule{firing{}, listedRule{}} {
		if err := issues.Default.Register(rule); err != nil {
			panic("issuerule: " + err.Error())
		}
	}
}

func alarms(view issues.View) []alarm {
	var out []alarm
	for _, resource := range view.Resources() {
		if resource.Service == input && (resource.Type == "alarm" || resource.Type == "composite-alarm") {
			out = append(out, alarm{arn: resource.ARN, name: resource.Name, region: resource.Region, state: resource.State})
		}
	}
	return out
}

type alarm struct{ arn, name, region, state string }

type firing struct{}

func (firing) ID() string             { return FiringRuleID }
func (firing) Kind() issues.Kind      { return issues.KindReliability }
func (firing) Inputs() []issues.Input { return []issues.Input{{Service: input}} }

func (firing) Evaluate(view issues.View) []issues.Finding {
	var findings []issues.Finding
	for _, one := range alarms(view) {
		if one.name != FiringAlarm || !strings.EqualFold(one.state, "alarm") {
			continue
		}
		findings = append(findings, issues.Finding{
			Owner: one.arn, Region: one.region, Severity: issues.SeverityCritical,
			Title:    "CloudWatch alarm " + one.name + " is firing",
			Evidence: []issues.Evidence{{Text: "Alarm " + one.name + " is in ALARM", Status: issues.EvidenceProven, Source: "CloudWatch"}},
			Steps:    []issues.Step{{Text: "Check the targets behind the load balancer the alarm watches"}},
			Material: issues.Material{"alarm": one.arn, "state": "alarm"},
		})
	}
	answer := ask(Call{Replica: os.Getenv(ReplicaEnv), Account: view.Account(), Findings: len(findings)})
	listed.Store(answer.Listed)
	if answer.Note != "" {
		for index := range findings {
			findings[index].Evidence = append(findings[index].Evidence,
				issues.Evidence{Text: "Evaluation note " + answer.Note, Status: issues.EvidenceAssumed, Source: "test gate"})
		}
	}
	return findings
}

type listedRule struct{}

func (listedRule) ID() string             { return ListedRuleID }
func (listedRule) Kind() issues.Kind      { return issues.KindReliability }
func (listedRule) Inputs() []issues.Input { return []issues.Input{{Service: input}} }

func (listedRule) Evaluate(view issues.View) []issues.Finding {
	if !listed.Load() {
		return nil
	}
	var findings []issues.Finding
	for _, one := range alarms(view) {
		if one.name == FiringAlarm {
			continue
		}
		findings = append(findings, issues.Finding{
			Owner: one.arn, Region: one.region, Severity: issues.SeverityMedium,
			Title:    "CloudWatch alarm " + one.name + " is listed",
			Evidence: []issues.Evidence{{Text: "Alarm " + one.name + " exists", Status: issues.EvidenceProven, Source: "CloudWatch"}},
			Material: issues.Material{"alarm": one.arn},
		})
	}
	return findings
}

// ask reports one evaluation to the gate and returns its answer; a missing
// or failing gate answers nothing.
func ask(call Call) Answer {
	url := os.Getenv(GateEnv)
	if url == "" {
		return Answer{}
	}
	body, err := json.Marshal(call)
	if err != nil {
		return Answer{}
	}
	client := &http.Client{Timeout: gateTimeout}
	response, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return Answer{}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil || response.StatusCode != http.StatusOK {
		return Answer{}
	}
	var answer Answer
	if err := json.Unmarshal(data, &answer); err != nil {
		return Answer{}
	}
	return answer
}
