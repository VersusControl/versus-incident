package kubernetes

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkloadCheckpointReferenceIsCompactAndExpiresUncertain(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	options := WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"}
	id := logIdentity("source")
	podOptions := PodLogStreamOptions{Namespace: "shop", Pod: "api-0", Container: "app", sourceIdentity: id}
	raw, _ := json.Marshal(logResumeCursor{Target: service.logTarget(podOptions), Timestamp: time.Now().UTC(), Ordinal: 1})
	checkpoint := base64.RawURLEncoding.EncodeToString(raw)
	session := &WorkloadLogSession{service: service, options: options, cursor: workloadLogCursor{Target: service.workloadLogTarget(options), Process: workloadStreamProcess, Sources: map[string]string{id: checkpoint}}}
	options.Cursor = session.checkpoint()
	defer releaseWorkloadCheckpointOwner(session.cursor.Owner)
	if len(options.Cursor) > 512 {
		t.Fatal("reference cursor exceeds ordinary URL budget", len(options.Cursor))
	}
	restored, err := service.workloadCursor(options)
	if err != nil || restored.Sources[id] != checkpoint || restored.Uncertain {
		t.Fatalf("checkpoint reference lost state: %+v %v", restored, err)
	}
	var token workloadLogCursor
	encoded, _ := base64.RawURLEncoding.DecodeString(options.Cursor)
	if json.Unmarshal(encoded, &token) != nil || token.Reference == "" {
		t.Fatal("missing compact reference")
	}
	workloadCheckpoints.Lock()
	entry := workloadCheckpoints.entries[token.Reference]
	entry.at = time.Now().Add(-MaxLogStreamDuration - time.Second)
	workloadCheckpoints.entries[token.Reference] = entry
	workloadCheckpoints.Unlock()
	expired, err := service.workloadCursor(options)
	if err != nil || len(expired.Sources) != 0 || !expired.Uncertain {
		t.Fatalf("expired reference claimed lossless replay: %+v %v", expired, err)
	}
}

func TestWorkloadCheckpointStoreIsBoundedAndDefensivelyCopied(t *testing.T) {
	cursor := workloadLogCursor{Target: logIdentity("target"), Process: workloadStreamProcess, Sources: map[string]string{"source": "checkpoint"}}
	first, err := retainWorkloadCheckpoint(cursor)
	if err != nil {
		t.Fatal(err)
	}
	cursor.Sources["source"] = "mutated"
	copy, ok := restoreWorkloadCheckpoint(first)
	if !ok || copy.Sources["source"] != "checkpoint" {
		t.Fatal("caller mutated retained checkpoint")
	}
	copy.Sources["source"] = "mutated again"
	again, ok := restoreWorkloadCheckpoint(first)
	if !ok || again.Sources["source"] != "checkpoint" {
		t.Fatal("restored checkpoint shares source map")
	}
	for i := 0; i < maxWorkloadCheckpoints+1; i++ {
		cursor.Sources["source"] = strconv.Itoa(i)
		if _, err := retainWorkloadCheckpoint(cursor); err != nil {
			t.Fatal(err)
		}
	}
	if _, exists := restoreWorkloadCheckpoint(first); exists {
		t.Fatal("oldest reference was not evicted")
	}
	workloadCheckpoints.Lock()
	count, size := len(workloadCheckpoints.entries), workloadCheckpoints.bytes
	workloadCheckpoints.Unlock()
	if count > maxWorkloadCheckpoints || size > maxWorkloadCheckpointBytes {
		t.Fatalf("checkpoint cache unbounded count=%d bytes=%d", count, size)
	}
	// No retained reference can authorize a different target, even if known.
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	options := WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"}
	reference, err := retainWorkloadCheckpoint(workloadLogCursor{Target: strings.Repeat("x", 43), Process: workloadStreamProcess, Sources: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(workloadLogCursor{Target: service.workloadLogTarget(options), Process: workloadStreamProcess, Reference: reference})
	options.Cursor = base64.RawURLEncoding.EncodeToString(raw)
	if _, err := service.workloadCursor(options); err != ErrInvalidArguments {
		t.Fatal("reference crossed its target", err)
	}
}
