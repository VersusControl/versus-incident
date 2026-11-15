package core

import "time"

// SetOnCallWaitUnitForTest shortens the unit of OnCallConfig.WaitMinutes so
// tests can drive the delayed escalation; the returned func restores it.
func SetOnCallWaitUnitForTest(d time.Duration) func() {
	prev := onCallWaitUnit
	onCallWaitUnit = d
	return func() { onCallWaitUnit = prev }
}
