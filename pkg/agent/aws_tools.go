package agent

import (
	"sync/atomic"

	awstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/aws"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// awsService holds the AWS connector service source bound at boot. The
// tools built from it read stored state only.
var awsService atomic.Pointer[awstools.ServiceFunc]

// ConfigureAWS binds the AWS connector service for the agent tools: the
// same service the admin routes use, so tools and UI share one snapshot
// store and one limiter. A nil source removes the tools. Call at boot,
// before the agents are built.
func ConfigureAWS(service awstools.ServiceFunc) {
	if service == nil {
		awsService.Store(nil)
		return
	}
	awsService.Store(&service)
}

func boundAWS() awstools.ServiceFunc {
	if p := awsService.Load(); p != nil {
		return *p
	}
	return nil
}

// awsRuntimeTools are the AWS tools when a service is bound.
func awsRuntimeTools() []core.Tool {
	if s := boundAWS(); s != nil {
		return awstools.New(s)
	}
	return nil
}
