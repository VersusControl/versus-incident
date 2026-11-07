package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestStorageFailuresThroughTheSavePathNeverCloseTheCredentialGate runs every
// kind of store failure through the real save path, with a working credential,
// for every storage stage. Each must be named a storage failure and none may
// reach the credential recovery gate or be reported as a credential problem.
func TestStorageFailuresThroughTheSavePathNeverCloseTheCredentialGate(t *testing.T) {
	failures := map[string]struct {
		fail   func(ctx context.Context) error
		reason string
	}{
		"save deadline": {func(ctx context.Context) error {
			<-ctx.Done()
			return fmt.Errorf("journal statement on 10.0.0.7: %w", ctx.Err())
		}, "storage_unavailable"},
		"statement timeout": {func(context.Context) error {
			return fmt.Errorf("pq: canceling statement due to statement timeout: %w", context.DeadlineExceeded)
		}, "storage_unavailable"},
		"connection reset": {func(context.Context) error { return errors.New("read tcp 10.0.0.7:5432: connection reset by peer") }, "storage_unavailable"},
		"permission denied": {func(context.Context) error {
			return errors.New("pq: permission denied for table kubernetes_evidence_events")
		}, "storage_unavailable"},
		"inconsistent saved state": {func(context.Context) error { return fmt.Errorf("occurrences: %w", storage.ErrInvalidEvidence) }, "storage_unavailable"},
		"evidence limit":           {func(context.Context) error { return fmt.Errorf("batch: %w", storage.ErrEvidenceLimit) }, "storage_rejected"},
		"unsupported store":        {func(context.Context) error { return fmt.Errorf("blob: %w", storage.ErrUnsupported) }, "storage_rejected"},
	}
	for _, stage := range []string{"payload", "cas", "journal", "lease", "read"} {
		for name, failure := range failures {
			t.Run(stage+"/"+name, func(t *testing.T) {
				service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "save-gate", CredentialID: "unchanged"})
				service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return "Bearer synthetic", nil }}
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				ran := false
				err := service.commitStorage(ctx, stage, func() error { ran = true; return failure.fail(ctx) })
				if !ran || err == nil {
					t.Fatalf("save did not run or did not fail: ran=%t err=%v", ran, err)
				}
				// The worker may wrap the cycle error further before deciding.
				for _, observed := range []error{err, fmt.Errorf("inventory cycle: %w", err)} {
					if countsAgainstCredential(observed) {
						t.Fatalf("store failure closed the credential gate: %v", observed)
					}
					if reason := recoveryReason(observed, stage); reason != failure.reason {
						t.Fatalf("recovery reason %q, want %q", reason, failure.reason)
					}
					if class := errorClass(observed); class != failure.reason {
						t.Fatalf("error class %q, want %q", class, failure.reason)
					}
					if errors.Is(observed, ErrCredentialUnavailable) || errors.Is(observed, ErrUnauthorized) || errors.Is(observed, ErrForbidden) {
						t.Fatalf("store failure carries a credential answer: %v", observed)
					}
					switch code := DiagnoseError(observed).Code; code {
					case "connector_credential_unresolved", "credential_unavailable", "cluster_authentication_failed":
						t.Fatalf("store failure reported as %q", code)
					}
				}
				if service.identity.ctx.Err() != nil {
					t.Fatal("a store failure retired the credential epoch")
				}
			})
		}
	}
}

// TestCredentialGateFollowsTheAnswerBehindAnUnverifiedAuthority checks the
// wrapper collection uses when it cannot verify its authority: only an
// answered denial behind it counts against the credential, while a storage or
// cluster outage behind it does not.
func TestCredentialGateFollowsTheAnswerBehindAnUnverifiedAuthority(t *testing.T) {
	for name, test := range map[string]struct {
		cause  error
		counts bool
	}{
		"forbidden":         {fmt.Errorf("list pods: %w", ErrForbidden), true},
		"unauthorized":      {ErrUnauthorized, true},
		"storage timeout":   {storageStageFailure("journal", context.DeadlineExceeded), false},
		"storage rejected":  {storageStageFailure("journal", storage.ErrEvidenceLimit), false},
		"cluster deadline":  {context.DeadlineExceeded, false},
		"cluster throttled": {&ErrThrottled{RetryAfter: time.Second}, false},
		"cluster down":      {ErrClusterUnavailable, false},
	} {
		if got := countsAgainstCredential(unverifiedInventoryAuthority(test.cause)); got != test.counts {
			t.Errorf("%s behind an unverified authority: counts=%t, want %t", name, got, test.counts)
		}
	}
	if countsAgainstCredential(&ErrThrottled{RetryAfter: time.Second}) {
		t.Error("API throttling closed the credential gate")
	}
}

// TestHistorySaveStatusContract pins what a history page reports about the
// capture's save, from the published capture, the committed checkpoint and the
// stored rejection note.
func TestHistorySaveStatusContract(t *testing.T) {
	now := time.Now().UTC()
	saved := now.Add(-10 * time.Minute)
	checkpoint := storage.EvidenceCheckpoint{ID: "saved", Generation: 4, At: saved}
	pending := func(id string, generation uint64, at time.Time) *storage.EvidenceBatch {
		return &storage.EvidenceBatch{ID: id, Generation: generation, At: at}
	}
	for _, test := range []struct {
		name       string
		cache      bool
		batch      *storage.EvidenceBatch
		failure    string
		checkpoint storage.EvidenceCheckpoint
		note       string
		want       string
	}{
		{name: "no published capture", want: ""},
		{name: "store failed before the save was prepared", cache: true, failure: "storage_unavailable", checkpoint: checkpoint, want: "history_save_delayed"},
		{name: "unprepared save never reads as rejected", cache: true, failure: "storage_unavailable", checkpoint: checkpoint, note: "unrelated", want: "history_save_delayed"},
		{name: "unprepared save with no history yet", cache: true, failure: "storage_unavailable", want: "history_save_delayed"},
		{name: "any other failure without a save is a save delay", cache: true, failure: "collection_unavailable", checkpoint: checkpoint, want: "history_save_delayed"},
		{name: "a capture the store would refuse is rejected", cache: true, failure: "storage_rejected", checkpoint: checkpoint, want: "history_save_rejected"},
		{name: "save outstanding past the grace", cache: true, batch: pending("next", 4, now.Add(-2*time.Minute)), checkpoint: checkpoint, want: "history_save_delayed"},
		{name: "save refused by the store", cache: true, batch: pending("next", 4, now.Add(-2*time.Minute)), checkpoint: checkpoint, note: "next", want: "history_save_rejected"},
		{name: "refusal of an older capture is not this one", cache: true, batch: pending("next", 4, now.Add(-2*time.Minute)), checkpoint: checkpoint, note: "older", want: "history_save_delayed"},
		{name: "save still inside the grace", cache: true, batch: pending("next", 4, now.Add(-10*time.Second)), checkpoint: checkpoint, want: ""},
		{name: "refused save still inside the grace", cache: true, batch: pending("next", 4, now.Add(-10*time.Second)), checkpoint: checkpoint, note: "next", want: ""},
		{name: "capture already saved", cache: true, batch: pending("saved", 4, now.Add(-2*time.Minute)), checkpoint: checkpoint, want: ""},
		{name: "capture fenced by a history deletion", cache: true, batch: pending("next", 3, now.Add(-2*time.Minute)), checkpoint: checkpoint, want: ""},
		{name: "capture no newer than the checkpoint", cache: true, batch: pending("next", 4, saved), checkpoint: checkpoint, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "save-contract", CredentialID: "unchanged"})
			service.historySaveGrace = time.Minute
			provider := storage.NewMemory()
			t.Cleanup(func() { _ = provider.Close() })
			service.SetChangeStorage(provider)
			if test.note != "" {
				service.recordHistorySaveRejection(test.note)
			}
			if test.cache {
				cache := &inventoryCache{generation: inventoryGeneration{EvidenceBatch: test.batch, EvidenceFailure: test.failure}}
				service.changes.mu.Lock()
				service.changes.inventories[service.indexScope()] = cache
				service.changes.mu.Unlock()
			}
			status := service.historyCapture(test.checkpoint)
			if status == nil {
				t.Fatal("history page carried no capture status")
			}
			save := status.HistorySave
			if test.want == "" {
				if save != nil {
					t.Fatalf("reported %+v, want no save status", save)
				}
				return
			}
			if save == nil || save.Code != test.want || !status.Behind {
				t.Fatalf("save=%+v behind=%t, want %s", save, status.Behind, test.want)
			}
			if save.Retryable != (test.want == "history_save_delayed") {
				t.Fatalf("%s retryable=%t", save.Code, save.Retryable)
			}
			switch {
			case test.checkpoint.At.IsZero() && save.SavedThrough != nil:
				t.Fatalf("no saved history reported as saved through %s", save.SavedThrough)
			case !test.checkpoint.At.IsZero() && (save.SavedThrough == nil || !save.SavedThrough.Equal(test.checkpoint.At) || save.SavedThrough.Location() != time.UTC):
				t.Fatalf("saved through %v, want %s in UTC", save.SavedThrough, test.checkpoint.At)
			}
			text := strings.ToLower(save.Message + " " + save.Action)
			if strings.Contains(text, "credential") || !strings.Contains(text, "history") {
				t.Fatalf("save status names the wrong layer: %q", text)
			}
		})
	}
}

// A published capture that could not be prepared for saving, for any reason,
// leaves saved history behind, and every history page says so. A capture the
// store would refuse whatever the retry (over its limits even with only the
// records current state needs, invalid in itself or holding text it cannot
// store) reads as rejected; anything else as delayed. A capture whose change
// and termination records alone are over the limits is trimmed and saved
// instead (TestCaptureOverTheSaveLimitKeepsTheNewestRecords).
func TestEveryUnpreparedCaptureIsReportedBehind(t *testing.T) {
	t.Parallel()
	for _, refused := range []error{
		storage.ErrEvidenceLimit,
		fmt.Errorf("record payload: %w", storage.ErrInvalidEvidence),
		storage.ErrEvidenceUnstorable,
	} {
		if class := errorClass(capturePreparationRefused(refused)); class != "storage_rejected" {
			t.Fatalf("a capture the store would refuse (%v) was classed %q, want storage_rejected", refused, class)
		}
	}
	service := issueHistoryService(t, storage.NewMemory())
	seedInventory(t, service, []kubeindex.Record{healthyDeploymentRecord("web", "web")})
	cache := service.changes.inventories[service.indexScope()]
	savedThrough := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	cases := map[string]string{
		"unavailable":         "history_save_delayed",
		"storage_unavailable": "history_save_delayed",
		"timeout":             "history_save_delayed",
		"storage_rejected":    "history_save_rejected",
		"storage_limit":       "history_save_rejected",
		"invalid_evidence":    "history_save_rejected",
		"storage_unsupported": "history_save_rejected",
	}
	// The classes a refused preparation actually records are permanent too.
	for _, refused := range []error{storage.ErrEvidenceLimit, fmt.Errorf("record payload: %w", storage.ErrInvalidEvidence), storage.ErrEvidenceUnstorable} {
		cases[errorClass(capturePreparationRefused(refused))] = "history_save_rejected"
	}
	for failure, want := range cases {
		cache.mu.Lock()
		cache.generation.EvidenceBatch, cache.generation.EvidenceFailure = nil, failure
		cache.mu.Unlock()
		status := service.historyCapture(storage.EvidenceCheckpoint{ID: "saved", At: savedThrough})
		if !status.Behind || status.HistorySave == nil || status.HistorySave.Code != want || status.HistorySave.SavedThrough == nil || !status.HistorySave.SavedThrough.Equal(savedThrough) {
			t.Fatalf("unprepared capture with failure %q reported %+v, want %s through %s", failure, status.HistorySave, want, savedThrough)
		}
	}
	cache.mu.Lock()
	cache.generation.EvidenceBatch, cache.generation.EvidenceFailure = nil, ""
	cache.mu.Unlock()
	if status := service.historyCapture(storage.EvidenceCheckpoint{ID: "saved", At: savedThrough}); status.HistorySave != nil {
		t.Fatalf("a capture without a save failure was reported behind: %+v", status.HistorySave)
	}
}
