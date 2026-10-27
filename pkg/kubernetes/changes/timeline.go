package changes

import "time"

// TimelineWindow takes one scoped retained journal snapshot. Filtering and
// display paging happen after sources are merged, never independently per type.
func (store *Store) TimelineWindow(since, until time.Time) ([]Change, []CrashEvent, []Gap, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	changes, crashes, gaps := []Change{}, []CrashEvent{}, []Gap{}
	seenChanges, seenCrashes := map[string]bool{}, map[string]bool{}
	for _, source := range append([]*Store{store}, store.archives...) {
		current, err := source.read()
		if err != nil {
			return nil, nil, nil, err
		}
		gaps = append(gaps, gapsInWindow(current.Gaps, since, until)...)
		gaps = append(gaps, gapsInWindow(current.CrashGaps, since, until)...)
		for _, change := range current.Changes {
			if seenChanges[change.ID] {
				continue
			}
			seenChanges[change.ID] = true
			if !change.At.Before(since) && !change.At.After(until) {
				changes = append(changes, cloneChange(change))
			}
		}
		for _, crash := range current.Crashes {
			if seenCrashes[crash.ID] {
				continue
			}
			seenCrashes[crash.ID] = true
			if crash.Type == "termination" && !crash.At.Before(since) && !crash.At.After(until) {
				crashes = append(crashes, crash)
			}
		}
	}
	return changes, crashes, gaps, nil
}
