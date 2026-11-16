package services

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/redis/go-redis/v9"
)

type modeOnCallProvider struct{ calls atomic.Int32 }

func (p *modeOnCallProvider) TriggerOnCall(context.Context, string, *config.OnCallConfig) error {
	p.calls.Add(1)
	return nil
}

// onCallMode is one of the three on-call setups an operator can choose.
type onCallMode struct {
	name            string
	enable          bool
	initializedOnly bool
	// providerSetUp is false in the "off" mode: no provider exists, so the
	// workflow below has none and any escalation attempt reports it.
	providerSetUp bool
}

var onCallModes = []onCallMode{
	{name: "enabled", enable: true, providerSetUp: true},
	{name: "initialized only", initializedOnly: true, providerSetUp: true},
	{name: "off", providerSetUp: false},
}

// useOnCallMode applies mode to the loaded config and installs a workflow
// whose provider counts escalations (or has no provider in the off mode).
func useOnCallMode(t *testing.T, mode onCallMode) *modeOnCallProvider {
	t.Helper()
	loadAgentTestConfig(t)
	cfg := config.GetConfig()
	prev := cfg.OnCall
	t.Cleanup(func() { cfg.OnCall = prev })
	cfg.OnCall.Enable = mode.enable
	cfg.OnCall.InitializedOnly = mode.initializedOnly
	cfg.OnCall.WaitMinutes = 0
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // never dialled: escalation is immediate
	t.Cleanup(func() { _ = rdb.Close() })
	provider := &modeOnCallProvider{}
	var p core.OnCallProvider
	if mode.providerSetUp {
		p = provider
	}
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(rdb, p))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })
	return provider
}

func assertOnCallOutcome(t *testing.T, mem storage.Provider, provider *modeOnCallProvider, err error, wantPage bool) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "no on-call provider") {
		t.Fatalf("emit error = %v; on-call must not be attempted without a provider", err)
	}
	if err != nil {
		t.Fatalf("emit error = %v, want none", err)
	}
	rec := onlyStoredIncident(t, mem)
	if rec.OnCallError != "" {
		t.Fatalf("stored oncall_error = %q, want none", rec.OnCallError)
	}
	if got := provider.calls.Load() == 1; got != wantPage {
		t.Fatalf("paged = %v (provider calls %d), want %v", got, provider.calls.Load(), wantPage)
	}
}

// Critical and high agent findings page in the enabled and initialized-only
// modes and never in the off mode; medium findings never page.
func TestAgentFindingOnCallFollowsTheOnCallMode(t *testing.T) {
	for _, mode := range onCallModes {
		for _, severity := range []string{"critical", "high", "medium"} {
			t.Run(mode.name+"/"+severity, func(t *testing.T) {
				provider := useOnCallMode(t, mode)
				mem := useMemoryStore(t)
				finding := &core.AIFinding{Title: "db-pool " + severity, Summary: "burst", Severity: severity}
				result := core.AgentResult{Verdict: core.VerdictSpike, PatternID: "p-" + severity}
				err := CreateIncidentFromFinding(finding, result, "agent:file:app", "db-pool")
				wantPage := mode.providerSetUp && severity != "medium"
				assertOnCallOutcome(t, mem, provider, err, wantPage)
			})
		}
	}
}

// A critical Kubernetes issue pages in the enabled and initialized-only modes
// and never in the off mode; a warning never pages.
func TestKubernetesIssueOnCallFollowsTheOnCallMode(t *testing.T) {
	for _, mode := range onCallModes {
		for _, severity := range []string{"critical", "warning"} {
			t.Run(mode.name+"/"+severity, func(t *testing.T) {
				provider := useOnCallMode(t, mode)
				mem := useMemoryStore(t)
				a := kubernetes.IssueAlert{Cluster: "prod", Namespace: "shop", WorkloadKind: "Deployment", Workload: "checkout", RootUID: "root", Rule: "pod.crash_loop", RuleLabel: "crash looping", Severity: severity, BrokeAt: time.Now()}
				_, err := CreateIncidentFromKubernetesIssue("", a)
				wantPage := mode.providerSetUp && severity == "critical"
				assertOnCallOutcome(t, mem, provider, err, wantPage)
			})
		}
	}
}
