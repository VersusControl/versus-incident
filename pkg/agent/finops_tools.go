package agent

import (
	"sync/atomic"

	finopstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/finops"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// finopsService holds the FinOps service source bound at boot. The tools
// built from it read the cost cache only.
var finopsService atomic.Pointer[finopstools.ServiceFunc]

// ConfigureFinOps binds the FinOps service source for the agent tools. A
// nil source removes the tools. Call at boot, before the agents are built.
func ConfigureFinOps(service finopstools.ServiceFunc) {
	if service == nil {
		finopsService.Store(nil)
		return
	}
	finopsService.Store(&service)
}

func boundFinOps() finopstools.ServiceFunc {
	if p := finopsService.Load(); p != nil {
		return *p
	}
	return nil
}

// finopsRuntimeTools are the FinOps tools when a service is bound.
func finopsRuntimeTools() []core.Tool {
	if s := boundFinOps(); s != nil {
		return finopstools.New(s)
	}
	return nil
}
