package kubernetes

import (
	"crypto/rand"
	"encoding/base64"
	"reflect"
	"sync"
	"time"
)

const maxWorkloadCheckpointBytes = 16 << 20
const maxWorkloadCheckpoints = 256

type workloadCheckpoint struct {
	cursor workloadLogCursor
	at     time.Time
	bytes  int
	owner  string
}

var workloadCheckpoints = struct {
	sync.Mutex
	entries map[string]workloadCheckpoint
	bytes   int
	active  map[string]bool
	latest  map[string]string
}{entries: make(map[string]workloadCheckpoint), active: make(map[string]bool), latest: make(map[string]string)}

func registerWorkloadCheckpointOwner() string {
	var random [18]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ""
	}
	owner := base64.RawURLEncoding.EncodeToString(random[:])
	workloadCheckpoints.Lock()
	workloadCheckpoints.active[owner] = true
	workloadCheckpoints.Unlock()
	return owner
}

func releaseWorkloadCheckpointOwner(owner string) {
	workloadCheckpoints.Lock()
	delete(workloadCheckpoints.active, owner)
	workloadCheckpoints.Unlock()
}

func removeWorkloadCheckpoint(key string) {
	entry := workloadCheckpoints.entries[key]
	workloadCheckpoints.bytes -= entry.bytes
	delete(workloadCheckpoints.entries, key)
	if workloadCheckpoints.latest[entry.owner] == key {
		delete(workloadCheckpoints.latest, entry.owner)
	}
}

func copyWorkloadCursor(cursor workloadLogCursor) workloadLogCursor {
	result := cursor
	result.Sources = make(map[string]string, len(cursor.Sources))
	for id, checkpoint := range cursor.Sources {
		result.Sources[id] = checkpoint
	}
	return result
}

// Retained references keep the browser's resume URL below ordinary HTTP header
// limits. Missing/evicted references are explicitly replay-uncertain, never
// silently substituted with a newer checkpoint.
func retainWorkloadCheckpoint(cursor workloadLogCursor) (string, error) {
	if len(cursor.Sources) > MaxWorkloadLogSources {
		return "", ErrInvalidArguments
	}
	var random [18]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	key := base64.RawURLEncoding.EncodeToString(random[:])
	now := time.Now()
	size := len(cursor.Target) + len(cursor.Process) + 128
	for id, value := range cursor.Sources {
		if len(value) > 16<<10 {
			return "", ErrInvalidArguments
		}
		size += len(id) + len(value) + 64
	}
	workloadCheckpoints.Lock()
	defer workloadCheckpoints.Unlock()
	owner := cursor.Owner
	if owner == "" {
		owner = cursor.Target
	}
	if latest, ok := workloadCheckpoints.entries[workloadCheckpoints.latest[owner]]; ok && reflect.DeepEqual(latest.cursor, cursor) {
		latest.at = now
		workloadCheckpoints.entries[workloadCheckpoints.latest[owner]] = latest
		return workloadCheckpoints.latest[owner], nil
	}
	owned := 0
	for _, entry := range workloadCheckpoints.entries {
		if entry.owner == owner {
			owned++
		}
	}
	for owned >= 4 || len(workloadCheckpoints.entries) >= maxWorkloadCheckpoints || workloadCheckpoints.bytes+size > maxWorkloadCheckpointBytes {
		oldestKey := ""
		oldest := now.Add(time.Hour)
		for id, entry := range workloadCheckpoints.entries {
			if owned >= 4 && entry.owner != owner {
				continue
			}
			if entry.owner != owner && workloadCheckpoints.active[entry.owner] && workloadCheckpoints.latest[entry.owner] == id {
				continue
			}
			if entry.at.Before(oldest) {
				oldestKey, oldest = id, entry.at
			}
		}
		if oldestKey == "" {
			return "", ErrLogStreamBusy
		}
		if workloadCheckpoints.entries[oldestKey].owner == owner {
			owned--
		}
		removeWorkloadCheckpoint(oldestKey)
	}
	workloadCheckpoints.entries[key] = workloadCheckpoint{cursor: copyWorkloadCursor(cursor), at: now, bytes: size, owner: owner}
	workloadCheckpoints.latest[owner] = key
	workloadCheckpoints.bytes += size
	return key, nil
}

func restoreWorkloadCheckpoint(key string) (workloadLogCursor, bool) {
	workloadCheckpoints.Lock()
	defer workloadCheckpoints.Unlock()
	entry, exists := workloadCheckpoints.entries[key]
	if !exists {
		return workloadLogCursor{}, false
	}
	if time.Since(entry.at) > MaxLogStreamDuration {
		removeWorkloadCheckpoint(key)
		return workloadLogCursor{}, false
	}
	return copyWorkloadCursor(entry.cursor), true
}
