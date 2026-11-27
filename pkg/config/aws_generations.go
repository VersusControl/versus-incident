package config

import "time"

// awsHistoryHorizon is the span every collection kind's kept generations
// must cover at least twice over: the OSS compare and alarm-line horizon.
const awsHistoryHorizon = 24 * time.Hour

// GenerationsFor is the generation count cap of a collection kind that runs
// every interval: at least 2 × 24 h ÷ interval, so a unit that changes on
// every pass still keeps a day of history, and never below max_generations.
// Inventory every 15 min gives 192; health every 2 min gives 1 440.
func (collect AWSCollectConfig) GenerationsFor(interval time.Duration) int {
	floor := collect.MaxGenerations()
	if interval <= 0 {
		return floor
	}
	need := int((2*awsHistoryHorizon + interval - 1) / interval)
	if need > maxAWSHistoryGenerations {
		need = maxAWSHistoryGenerations
	}
	return max(floor, need)
}

// InventoryMaxGenerations is the generation cap of inventory units.
// Between full reads the change feed re-reads a changed unit as often as
// the change poll runs, so the cap is sized from the faster of the two and
// a day of re-reads stays readable for compare and the alarm line.
func (collect AWSCollectConfig) InventoryMaxGenerations() int {
	return collect.GenerationsFor(min(collect.InventoryInterval(), collect.ChangesInterval()))
}

// HealthMaxGenerations is the generation cap of health units (alarms,
// target health, rollouts): the alarm line's history depth.
func (collect AWSCollectConfig) HealthMaxGenerations() int {
	// A unit that is unhealthy or changed runs at the fast cadence.
	return collect.GenerationsFor(collect.HealthHotInterval())
}
