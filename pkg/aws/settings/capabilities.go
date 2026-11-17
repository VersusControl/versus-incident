package settings

import (
	"sync"
	"sync/atomic"
)

// organizationsCapability is the Enterprise seam. OSS leaves it nil, so
// Organizations discovery reports false without importing the license package.
var organizationsCapability atomic.Value // func() bool

// SetOrganizationsCapability installs the Enterprise answer. Passing nil
// returns the capability to its OSS default.
func SetOrganizationsCapability(answer func() bool) {
	if answer == nil {
		organizationsCapability = atomic.Value{}
		return
	}
	organizationsCapability.Store(answer)
}

// OrganizationsCapability reports whether Organizations discovery is
// available in this build.
func OrganizationsCapability() bool {
	answer, ok := organizationsCapability.Load().(func() bool)
	return ok && answer != nil && answer()
}

// privacySelfTestMu serializes the hourly and on-demand runs so two writes
// cannot lose each other's sample.
var privacySelfTestMu sync.Mutex
