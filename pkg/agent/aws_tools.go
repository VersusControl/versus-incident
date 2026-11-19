package agent

import (
	"sync/atomic"

	"github.com/VersusControl/versus-incident/pkg/config"

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

// awsChanges holds the AWS change list bound at boot: the same feed the
// Changes page reads.
var awsChanges atomic.Pointer[awstools.ChangesFunc]

// ConfigureAWSChanges binds the AWS change list for get_aws_changes and
// the recent_changes change feed. A nil source removes both. Call at boot,
// before the agents are built.
func ConfigureAWSChanges(feed awstools.ChangesFunc) {
	if feed == nil {
		awsChanges.Store(nil)
		return
	}
	awsChanges.Store(&feed)
}

// awsChangeSource is the AWS change list when the AWS connector is on and a
// list is bound; otherwise nil, so recent_changes and the change_feed
// capability never claim an AWS source that is not there.
func awsChangeSource(connectors config.ConnectorsConfig) awstools.ChangesFunc {
	if !connectors.AWS.Enable {
		return nil
	}
	return boundAWSChanges()
}

func boundAWSChanges() awstools.ChangesFunc {
	if p := awsChanges.Load(); p != nil {
		return *p
	}
	return nil
}

// awsRuntimeTools are the AWS tools when a service or the change list is
// bound.
func awsRuntimeTools() []core.Tool {
	var out []core.Tool
	if s := boundAWS(); s != nil {
		out = append(out, awstools.New(s)...)
	}
	return append(out, awstools.NewChanges(boundAWSChanges())...)
}
