package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestEvidenceReadFailurePreservesCauseWithoutSensitiveDiagnostics(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	cause := fmt.Errorf("synthetic-private-provider-detail: %w", storage.ErrUnsupported)
	err := evidenceReadFailure(evidenceStoreQuery, cause)
	if !errors.Is(err, storage.ErrUnsupported) || DiagnoseError(err).Code != DiagnoseError(cause).Code {
		t.Fatal("stage diagnostics changed the underlying error contract")
	}
	var stage *evidenceReadError
	if !errors.As(err, &stage) || stage.stage != evidenceStoreQuery {
		t.Fatal("typed history stage is unavailable")
	}
	if next := evidenceReadFailure(evidencePayloadDecode, err); next != err {
		t.Fatal("outer decoder replaced the actual query failure stage")
	}
	if strings.Contains(err.Error()+output.String(), "synthetic-private-provider-detail") || strings.Count(output.String(), "historical evidence failed") != 1 {
		t.Fatal("diagnostics exposed provider text or emitted duplicate failures")
	}
	if !strings.Contains(output.String(), "stage=store_query class=storage_unsupported") {
		t.Fatal("diagnostic stage and closed cause are missing")
	}
	for _, original := range []error{ErrForbidden, ErrUnauthorized, context.DeadlineExceeded, ErrOperationBudget, invalidInventoryList(), errors.New("synthetic-private deadline detail")} {
		wrapped := evidenceReadFailure(evidenceBaseKindProof, original)
		if !errors.Is(wrapped, original) || errorClass(wrapped) != errorClass(original) || DiagnoseError(wrapped).Code != DiagnoseError(original).Code {
			t.Fatal("safe diagnostic wrapper changed an existing typed error classification")
		}
	}
	if strings.Contains(output.String(), "synthetic-private") {
		t.Fatal("underlying error text escaped into diagnostic logs")
	}
}

func TestPostgresAgedCheckpointMetadataDoesNotGrantUnmappedRows(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("explicit isolated harness Postgres required")
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal("isolated Postgres initialization failed")
	}
	defer provider.Close()
	store := provider.(storage.EvidenceStore)
	for _, optional := range []string{"", "Gateway", "SyntheticOptionalResource"} {
		t.Run("checkpoint-"+optional, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "aged-physical"}})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/gateways") {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				writeJSON(w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{"resourceVersion": "opaque-aged-version"}, "items": nil})
			}))
			defer server.Close()
			cluster := fmt.Sprintf("aged-checkpoint-%d", time.Now().UnixNano())
			service := newTestService(t, server.URL, Scope{OrgID: "synthetic-history", ClusterID: cluster})
			service.SetChangeStorage(provider)
			service.changes.evidenceEnabled = true
			scope := storage.EvidenceScope{OrgID: "synthetic-history", ClusterID: cluster, ClusterUID: "aged-physical"}
			now := time.Now().UTC().Truncate(time.Microsecond)
			if optional == "SyntheticOptionalResource" {
				service.mu.Lock()
				service.cache[service.cacheKey()] = discoveryCacheEntry{expires: now.Add(time.Hour), discovery: Discovery{Resources: []ResourceDefinition{{ID: "synthetic.example~v1~optionalresources", Kind: optional, Group: "synthetic.example", Version: "v1", Resource: "optionalresources", Namespaced: true, Available: true}}}}
				service.mu.Unlock()
			}
			old := now.Add(-72 * time.Hour)
			batch := storage.EvidenceBatch{ID: "capture:old", At: old, Epoch: "synthetic"}
			if optional != "" {
				batch.Records = []storage.EvidenceRecord{{ID: "old-optional", Type: "change", Kind: optional, UID: "old-object", At: old, ObservedAt: old, Epoch: "synthetic", Payload: json.RawMessage(`{}`)}}
			}
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal("isolated aged capture failed")
			}
			checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil {
				t.Fatal("isolated checkpoint read failed")
			}
			batch = storage.EvidenceBatch{ID: "capture:current", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: now, Epoch: "synthetic"}
			for i := 0; i < 501; i++ {
				batch.Records = append(batch.Records, syntheticChangeEvidence(cluster, "Pod", "sample", fmt.Sprintf("current-%03d", i), now.Add(-time.Minute)))
			}
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal("isolated current capture failed")
			}
			for _, namespace := range []string{"", "sample"} {
				for _, at := range []time.Time{now.Add(-30 * time.Minute), old} {
					for _, kind := range []string{"timeline", "change", "crash", "occurrence"} {
						query := storage.EvidenceQuery{Namespace: namespace, RootUID: "synthetic-root", Since: at.Add(-30 * time.Minute), Until: at.Add(30 * time.Minute), Type: kind, Limit: 5}
						page, err := service.queryEvidence(t.Context(), query)
						if err != nil {
							t.Fatalf("unrelated checkpoint metadata poisoned a scoped page: %v", err)
						}
						if page.Next != "" {
							query.Cursor = page.Next
							if _, err := service.queryEvidence(t.Context(), query); err != nil {
								t.Fatalf("pinned aged history continuation unavailable: %v", err)
							}
						}
					}
				}
			}
			if optional == "SyntheticOptionalResource" {
				page, err := service.queryEvidence(t.Context(), storage.EvidenceQuery{Since: old.Add(-time.Minute), Until: old.Add(time.Minute), Type: "change"})
				if err != nil || len(page.Items) != 1 || page.Items[0].Type != "coverage_gap" || page.Counts["change"] != 0 || page.Counts["coverage_gap"] != 1 {
					t.Fatal("unmapped returned resource was not explicitly reported as unavailable")
				}
			}
		})
	}
}

func syntheticChangeEvidence(cluster, kind, namespace, id string, at time.Time) storage.EvidenceRecord {
	change := Change{ID: id, Cluster: cluster, Kind: kind, Namespace: namespace, Name: "synthetic-" + id, UID: "uid-" + id, RootUID: "synthetic-root", Type: kubechanges.Created, Source: "observed", At: at}
	raw, _ := json.Marshal(change)
	return storage.EvidenceRecord{ID: "change:" + change.ID, Type: "change", Kind: change.Kind, Name: change.Name, Namespace: change.Namespace, UID: change.UID, RootUID: change.RootUID, Rule: string(change.Type), At: change.At, ObservedAt: change.At, Epoch: "synthetic", Payload: raw}
}

func syntheticCustomOwnerCrash(t *testing.T, cluster string, at time.Time, count int32) storage.EvidenceRecord {
	t.Helper()
	crash := kubechanges.CrashEvent{Cluster: cluster, Namespace: "sample", Pod: "synthetic-pod", PodUID: "synthetic-pod", RootUID: "synthetic-root", RootKind: "FluentBit", RootName: "synthetic-controller", Container: "app", Type: "termination", Reason: "Error", ExitCode: 1, RestartCount: count, At: at, ObservedAt: at, TimeSource: "observed_at"}
	crash.ID = kubechanges.CrashID(crash)
	if !crash.Valid() {
		t.Fatal("invalid synthetic crash")
	}
	raw, err := json.Marshal(crash)
	if err != nil {
		t.Fatal(err)
	}
	return storage.EvidenceRecord{ID: "crash:" + crash.ID, Type: "crash", Kind: crash.RootKind, UID: crash.PodUID, RootUID: crash.RootUID, PodUID: crash.PodUID, Namespace: crash.Namespace, At: at, ObservedAt: at, Epoch: "synthetic", Payload: raw}
}

func TestLegacyCustomOwnerCrashRequiresValidPodProvenance(t *testing.T) {
	service, _, _ := reviewHistoryService(t)
	for _, scenario := range []string{"valid", "invalid-payload", "pod-mismatch", "root-mismatch", "unknown-change", "unknown-explicit-source"} {
		t.Run(scenario, func(t *testing.T) {
			record := syntheticCustomOwnerCrash(t, "cluster", time.Now().UTC(), 1)
			switch scenario {
			case "invalid-payload":
				record.Payload = json.RawMessage(`{}`)
			case "pod-mismatch":
				record.PodUID = "different-pod"
			case "root-mismatch":
				record.RootUID = "different-owner"
			case "unknown-change":
				record.Type = "change"
			case "unknown-explicit-source":
				record.AuthorizationKind = "FluentBit"
			}
			kind, err := service.evidenceRecordAuthorizationKind(record)
			if scenario == "valid" {
				if err != nil || kind != "Pod" {
					t.Fatal("valid legacy Pod crash did not retain Pod authority")
				}
			} else if err == nil || kind != "" {
				t.Fatal("unknown or malformed source was treated as authorized")
			}
		})
	}
}

func TestCustomOwnerCrashHistoryUsesPodAuthorityAcrossStorageBackends(t *testing.T) {
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var provider storage.Provider
			var err error
			switch backend {
			case "memory":
				provider = storage.NewMemory()
			case "file":
				provider, err = storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("explicit isolated harness Postgres required")
				}
				provider, err = storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			}
			if err != nil {
				t.Fatal("isolated storage initialization failed")
			}
			defer provider.Close()
			var denyPod, denyEvent atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "custom-owner-physical"}})
					return
				}
				if denyPod.Load() && strings.HasSuffix(r.URL.Path, "/pods") || denyEvent.Load() && strings.HasSuffix(r.URL.Path, "/events") {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				writeJSON(w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{"resourceVersion": "opaque-custom-owner-version"}, "items": nil})
			}))
			defer server.Close()
			cluster := fmt.Sprintf("custom-owner-%d", time.Now().UnixNano())
			service := newTestService(t, server.URL, Scope{OrgID: "synthetic-history", ClusterID: cluster})
			service.SetChangeStorage(provider)
			service.changes.evidenceEnabled = true
			store := provider.(storage.EvidenceStore)
			scope := storage.EvidenceScope{OrgID: "synthetic-history", ClusterID: cluster, ClusterUID: "custom-owner-physical"}
			now := time.Now().UTC().Truncate(time.Microsecond)
			old := now.Add(-30 * time.Minute)
			batch := storage.EvidenceBatch{ID: "capture:custom-owner", At: old, Epoch: "synthetic"}
			for i := int32(1); i <= 6; i++ {
				batch.Records = append(batch.Records, syntheticCustomOwnerCrash(t, cluster, old, i))
			}
			issue, _ := json.Marshal(Issue{Root: ObjectRef{Kind: "FluentBit", Namespace: "sample", Name: "synthetic-controller", UID: "synthetic-root"}, Rule: "event.warning", Severity: "warning", Count: 1})
			batch.Issues = []storage.EvidenceIssue{{AuthorizationKind: "Event", Key: "synthetic-warning", Kind: "FluentBit", UID: "synthetic-root", Namespace: "sample", Rule: "event.warning", LastSeen: old, Payload: issue}}
			warning := syntheticChangeEvidence(cluster, "FluentBit", "sample", "synthetic-warning", old)
			var warningChange Change
			_ = json.Unmarshal(warning.Payload, &warningChange)
			warningChange.Type = kubechanges.WarningBurst
			warning.Payload, _ = json.Marshal(warningChange)
			warning.AuthorizationKind, warning.Rule = "Event", string(warningChange.Type)
			batch.Records = append(batch.Records, warning)
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal("isolated legacy custom-owner capture failed")
			}
			checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil || !containsString(checkpoint.Kinds, "FluentBit") {
				t.Fatal("test did not reproduce the legacy display-kind checkpoint")
			}
			batch = storage.EvidenceBatch{ID: "capture:later", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: now, Epoch: "synthetic"}
			for i := 0; i < 501; i++ {
				record := syntheticChangeEvidence(cluster, "Pod", "sample", fmt.Sprintf("later-%03d", i), now.Add(-time.Minute))
				var change Change
				_ = json.Unmarshal(record.Payload, &change)
				change.RootUID, record.RootUID = "", ""
				record.Payload, _ = json.Marshal(change)
				batch.Records = append(batch.Records, record)
			}
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal("isolated later capture failed")
			}
			for _, namespace := range []string{"", "sample"} {
				for _, limit := range []int{0, 5, 50, 100} {
					query := TimelineQuery{Namespace: namespace, Since: now.Add(-time.Hour), Until: now, Limit: limit}
					page, err := service.Timeline(t.Context(), query)
					if err != nil || len(page.Items) == 0 || page.Next == "" {
						t.Fatalf("legacy checkpoint blocked populated Timeline: count=%d next=%t err=%v", len(page.Items), page.Next != "", err)
					}
					query.Cursor = page.Next
					if _, err := service.Timeline(t.Context(), query); err != nil {
						t.Fatalf("legacy checkpoint blocked pinned continuation: %v", err)
					}
				}
				issues, err := service.Issues(t.Context(), IssueOptions{Namespace: namespace, RootUID: "synthetic-root", IncludeChanges: true, Since: now.Add(-time.Hour), Until: now})
				if err != nil || len(issues.Items) == 0 || issues.Items[0].Root.Kind != "FluentBit" {
					t.Fatalf("legacy checkpoint blocked Event-authorized custom-owner Issues: %v", err)
				}
				changes, err := service.Changes(t.Context(), ChangeQuery{Namespace: namespace, RootUID: "synthetic-root", Since: now.Add(-time.Hour), Until: now})
				if err != nil || len(changes.Items) != 1 {
					t.Fatalf("legacy checkpoint blocked Event-authorized custom-owner Changes: count=%d err=%v", len(changes.Items), err)
				}
				crashes, err := service.Crashes(t.Context(), kubechanges.CrashQuery{Namespace: namespace, RootUID: "synthetic-root", Since: now.Add(-time.Hour), Until: now})
				if err != nil || len(crashes.Items) != 6 || crashes.Items[0].RootKind != "FluentBit" {
					t.Fatalf("legacy checkpoint blocked Pod-authorized custom-owner Crashes: %v", err)
				}
				empty, err := service.Timeline(t.Context(), TimelineQuery{Namespace: namespace, RootUID: "synthetic-root", Since: now.Add(-3 * time.Hour), Until: now.Add(-2 * time.Hour)})
				if err != nil || len(empty.Items) != 0 {
					t.Fatalf("legacy metadata blocked an authorized empty window: %v", err)
				}
			}
			for _, permission := range []string{"pod", "event"} {
				denyPod.Store(permission == "pod")
				denyEvent.Store(permission == "event")
				page, err := service.Crashes(t.Context(), kubechanges.CrashQuery{Namespace: "sample", RootUID: "synthetic-root", Since: now.Add(-time.Hour), Until: now})
				if !errors.Is(err, ErrForbidden) || len(page.Items) != 0 {
					t.Fatalf("custom owner bypassed current %s denial: %v", permission, err)
				}
			}
		})
	}
}

func reviewHistoryService(t *testing.T) (*Service, storage.EvidenceStore, storage.EvidenceScope) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "physical"}})
			return
		}
		writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
	}))
	t.Cleanup(server.Close)
	provider := storage.NewMemory()
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetChangeStorage(provider)
	service.changes.evidenceEnabled = true
	return service, provider.(storage.EvidenceStore), storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
}

func TestPersistedRootContextIgnoresEvidenceWindowAndLiveExistence(t *testing.T) {
	service, store, scope := reviewHistoryService(t)
	at := time.Now().UTC().Add(-72 * time.Hour)
	root := ObjectRef{Kind: "Deployment", Namespace: "shop", Name: "retained-controller", UID: "retained-root"}
	raw, _ := json.Marshal(Issue{Root: root, Rule: "pod.crash_loop", Severity: "warning", Count: 1, Broken: true})
	batch := storage.EvidenceBatch{ID: "capture:retained", At: at, Epoch: "synthetic", Issues: []storage.EvidenceIssue{{Key: "retained", Namespace: root.Namespace, Kind: root.Kind, UID: root.UID, Rule: "pod.crash_loop", LastSeen: at, Payload: raw}}}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ := store.EvidenceCheckpoint(t.Context(), scope)
	// A later complete capture closes the occurrence and has no current object
	// or current finding. The historical identity must still resolve.
	if err := store.CommitEvidence(t.Context(), scope, storage.EvidenceBatch{ID: "capture:recovered", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at.Add(time.Hour), CoveredThrough: at.Add(time.Hour), Epoch: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	window, err := service.Issues(t.Context(), IssueOptions{RootUID: root.UID, Namespace: "shop", Since: time.Now().Add(-time.Hour), Until: time.Now()})
	if err != nil || len(window.Items) != 0 {
		t.Fatal("selected empty evidence interval changed")
	}
	options := IssueOptions{Context: "persisted", RootUID: root.UID, Namespace: "shop"}
	page, err := service.Issues(t.Context(), options)
	if err != nil || page.RootContext == nil || page.RootContext.Root != root || page.RootContext.Occurrence.Root != root || !page.RootContext.Retained || page.RootContext.Occurrence.State != "resolved" {
		t.Fatalf("captured deleted/recovered root failed to resolve: %v", err)
	}
	if len(page.Items) != 0 || len(page.Totals) != 0 || page.Broken.Total != 0 || page.Next != "" {
		t.Fatal("context polluted selected-window evidence or counts")
	}
	for _, invalid := range []IssueOptions{
		{Context: "persisted"},
		{Context: "persisted", RootUID: root.UID, Since: at},
		{Context: "persisted", RootUID: root.UID, Cursor: "cursor"},
		{Context: "other", RootUID: root.UID},
		{Context: "persisted", RootUID: "../root"},
	} {
		if _, err := service.Issues(t.Context(), invalid); !errors.Is(err, ErrInvalidArguments) {
			t.Fatal("context accepted an invalid UID or evidence-window contract")
		}
	}
	for _, missing := range []IssueOptions{
		{Context: "persisted", RootUID: root.UID, Namespace: "other"},
		{Context: "persisted", RootUID: "replacement-uid", Namespace: "shop"},
	} {
		page, err := service.Issues(t.Context(), missing)
		if err != nil || page.RootContext != nil || len(page.Items) != 0 {
			t.Fatal("retained identity escaped its UID or namespace")
		}
	}
}

func TestPersistedRootSelectionNeverSubstitutesAnotherOccurrence(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			service, store, scope := reviewHistoryService(t)
			if backend == "file" {
				provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { provider.Close() })
				service.SetChangeStorage(provider)
				store = provider.(storage.EvidenceStore)
			}
			at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second).Add(123456789 * time.Nanosecond)
			root := ObjectRef{Kind: "Deployment", Namespace: "shop", Name: "retained", UID: "retained-root"}
			batch := storage.EvidenceBatch{ID: "capture:selected", At: at, Epoch: "synthetic"}
			for i := 0; i < 3; i++ {
				broke := at
				if i == 2 {
					broke = at.Add(time.Minute)
				}
				rule := fmt.Sprintf("synthetic.rule-%d", i)
				raw, _ := json.Marshal(Issue{Root: root, Rule: rule, Broken: true, BrokeAt: &broke, Severity: "warning"})
				batch.Issues = append(batch.Issues, storage.EvidenceIssue{Key: rule, Namespace: root.Namespace, Kind: root.Kind, UID: root.UID, Rule: rule, LastSeen: at, ObservationOnly: true, Payload: raw})
			}
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
			history, err := service.historicalIssues(t.Context(), IssueOptions{RootUID: root.UID, Namespace: root.Namespace, Since: at.Add(-time.Minute), Until: time.Now()})
			if err != nil || len(history.Items) != 3 {
				t.Fatal("synthetic retained occurrences unavailable")
			}
			selected := history.Items[2]
			for _, test := range []struct {
				name, id, namespace, uid, status string
				center                           time.Time
			}{
				{name: "exact", id: selected.OccurrenceID, status: "resolved"},
				{name: "mismatch", id: "missing-id", center: *selected.BrokeAt, status: "not_found"},
				{name: "unique-center", center: at.Add(time.Minute), status: "resolved"},
				{name: "millisecond-center-does-not-substitute", center: at.Add(time.Minute).Truncate(time.Millisecond), status: "not_found"},
				{name: "nearby-center-does-not-substitute", center: at.Add(time.Minute + time.Nanosecond), status: "not_found"},
				{name: "ambiguous-center", center: at, status: "ambiguous"},
				{name: "no-selector", status: "not_requested"},
				{name: "wrong-namespace", id: selected.OccurrenceID, namespace: "other", status: "not_found"},
				{name: "same-name-new-uid", id: selected.OccurrenceID, uid: "replacement-root", status: "not_found"},
			} {
				t.Run(test.name, func(t *testing.T) {
					options := IssueOptions{Context: "persisted", RootUID: root.UID, Namespace: root.Namespace}
					if test.namespace != "" {
						options.Namespace = test.namespace
					}
					if test.uid != "" {
						options.RootUID = test.uid
					}
					options.OccurrenceID, options.Center = test.id, test.center
					page, err := service.Issues(t.Context(), options)
					if err != nil {
						t.Fatal(err)
					}
					raw, _ := json.Marshal(page)
					var response struct {
						Status   string `json:"selection_status"`
						Selected *Issue `json:"selected_occurrence"`
					}
					if err := json.Unmarshal(raw, &response); err != nil {
						t.Fatal(err)
					}
					if response.Status != test.status {
						t.Fatalf("selection status=%q want=%q", response.Status, test.status)
					}
					if test.status != "resolved" && response.Selected != nil {
						t.Fatal("unresolved selector substituted another recurrence")
					}
					if test.name == "exact" && (response.Selected == nil || response.Selected.OccurrenceID != selected.OccurrenceID) {
						t.Fatal("explicit occurrence did not survive date-free root lookup")
					}
					if test.name == "unique-center" && (response.Selected == nil || response.Selected.BrokeAt == nil || !response.Selected.BrokeAt.Equal(test.center)) {
						t.Fatal("nanosecond center did not preserve the exact retained occurrence")
					}
					if test.name == "no-selector" && (page.RootContext == nil || response.Selected != nil) {
						t.Fatal("legacy root link lost retained context or selected an arbitrary occurrence")
					}
					if len(page.Items) != 0 || len(page.Totals) != 0 || page.Next != "" {
						t.Fatal("selected context polluted historical paging")
					}
				})
			}
		})
	}
}

func TestPersistedRootSelectionReportsBoundedScanWithoutFalseUniqueness(t *testing.T) {
	service, store, scope := reviewHistoryService(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	root := ObjectRef{Kind: "Deployment", Namespace: "shop", Name: "retained", UID: "retained-root"}
	batch := storage.EvidenceBatch{ID: "capture:many", At: at, Epoch: "synthetic"}
	for i := 0; i < 1001; i++ {
		rule := fmt.Sprintf("synthetic.rule-%04d", i)
		raw, _ := json.Marshal(Issue{Root: root, Rule: rule, BrokeAt: &at, Broken: true, Severity: "warning"})
		batch.Issues = append(batch.Issues, storage.EvidenceIssue{Key: rule, Namespace: root.Namespace, Kind: root.Kind, UID: root.UID, Rule: rule, LastSeen: at, ObservationOnly: true, Payload: raw})
	}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	page, err := service.Issues(t.Context(), IssueOptions{Context: "persisted", RootUID: root.UID, Namespace: root.Namespace, OccurrenceID: "not-in-scanned-prefix"})
	if err != nil || page.SelectionStatus != "partial" || page.SelectedOccurrence != nil || page.RootContext == nil || !page.Sync.Partial {
		t.Fatal("bounded scan claimed absence or erased retained root")
	}
}

func TestScopedEvidenceProjectsUnavailableRecordsBeforeCountsAndPagination(t *testing.T) {
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var provider storage.Provider
			var err error
			switch backend {
			case "memory":
				provider = storage.NewMemory()
			case "file":
				provider, err = storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("explicit isolated harness Postgres required")
				}
				provider, err = storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer provider.Close()
			var denySecret, denyPod, denyRoot, wrongCluster atomic.Bool
			var wrongScopeCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					uid := "scoped-physical"
					if wrongCluster.Load() {
						uid = "replacement-physical"
					}
					writeJSON(w, map[string]any{"metadata": map[string]any{"uid": uid}})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/gateways") || strings.HasSuffix(r.URL.Path, "/httproutes") {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if r.URL.Path == "/api/v1/namespaces/sample/nodes" || r.URL.Path == "/api/v1/namespaces/sample/namespaces" {
					wrongScopeCalls.Add(1)
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if denySecret.Load() && strings.HasSuffix(r.URL.Path, "/secrets") || denyPod.Load() && strings.HasSuffix(r.URL.Path, "/pods") || denyRoot.Load() && strings.HasSuffix(r.URL.Path, "/deployments") {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				writeJSON(w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{"resourceVersion": "synthetic-version"}, "items": nil})
			}))
			defer server.Close()
			cluster := fmt.Sprintf("scope-projection-%d", time.Now().UnixNano())
			service := newTestService(t, server.URL, Scope{OrgID: "synthetic-history", ClusterID: cluster})
			service.SetChangeStorage(provider)
			service.changes.evidenceEnabled = true
			store := provider.(storage.EvidenceStore)
			scope := storage.EvidenceScope{OrgID: "synthetic-history", ClusterID: cluster, ClusterUID: "scoped-physical"}
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			batch := storage.EvidenceBatch{ID: "capture:mixed", At: at, Epoch: "synthetic"}
			issueRaw, _ := json.Marshal(Issue{Root: ObjectRef{Kind: "Deployment", Namespace: "sample", Name: "deleted-controller", UID: "retained-root"}, Rule: "pod.crash_loop", Severity: "warning", Count: 1})
			batch.Issues = []storage.EvidenceIssue{{Key: "retained", Namespace: "sample", Kind: "Deployment", UID: "retained-root", Rule: "pod.crash_loop", LastSeen: at, Payload: issueRaw}}
			batch.Issues = append(batch.Issues, storage.EvidenceIssue{Key: "malformed-retained", Namespace: "sample", Kind: "Deployment", UID: "retained-root", Rule: "pod.image_pull", LastSeen: at, Payload: json.RawMessage(`[]`)})
			for i, kind := range []string{"Pod", "Gateway", "HTTPRoute", "UnknownController", "AnotherController", "Secret"} {
				record := syntheticChangeEvidence(cluster, kind, "sample", fmt.Sprintf("row-%d", i), at)
				if kind == "Pod" {
					record.Payload = json.RawMessage(`[]`)
				}
				if kind == "Secret" {
					record.AuthorizationKind = "Secret"
				}
				batch.Records = append(batch.Records, record)
			}
			// Cluster-scoped checkpoint metadata is unrelated to namespaced
			// records, but actual cluster-scoped records still use cluster paths.
			for _, kind := range []string{"Node", "Namespace"} {
				batch.Records = append(batch.Records, syntheticChangeEvidence(cluster, kind, "", strings.ToLower(kind), at))
			}
			batch.Records = append(batch.Records, syntheticChangeEvidence(cluster, "Node", "sample", "wrong-namespace-node", at))
			malformed := []string{"empty-object", "null", "foreign-cluster", "foreign-namespace", "different-uid", "different-root", "different-name", "different-kind", "different-time", "different-id", "different-rule", "known-wrong-source", "unknown-type", "missing-uid"}
			for _, scenario := range malformed {
				record := syntheticChangeEvidence(cluster, "Secret", "sample", scenario, at)
				var change Change
				_ = json.Unmarshal(record.Payload, &change)
				switch scenario {
				case "foreign-cluster":
					change.Cluster = "another-cluster"
				case "foreign-namespace":
					change.Namespace = "another-namespace"
				case "different-uid":
					change.UID = "another-uid"
				case "different-root":
					change.RootUID = "another-root"
				case "different-name":
					change.Name = "another-name"
				case "different-kind":
					change.Kind = "Pod"
				case "different-time":
					change.At = at.Add(-time.Minute)
				case "different-id":
					change.ID = "another-id"
				case "different-rule":
					record.Rule = string(kubechanges.Deleted)
				case "known-wrong-source":
					record.AuthorizationKind = "Pod"
				case "unknown-type":
					change.Type = "unsupported"
				case "missing-uid":
					change.UID = ""
				}
				record.Payload, _ = json.Marshal(change)
				if scenario == "empty-object" {
					record.Payload = json.RawMessage(`{}`)
				}
				if scenario == "null" {
					record.Payload = json.RawMessage(`null`)
				}
				batch.Records = append(batch.Records, record)
			}
			for i, owner := range []string{"FluentBit", "SyntheticOperator", "AnotherOperator", "Deployment"} {
				record := syntheticCustomOwnerCrash(t, cluster, at, int32(i+1))
				var crash kubechanges.CrashEvent
				_ = json.Unmarshal(record.Payload, &crash)
				crash.RootKind = owner
				crash.ID = kubechanges.CrashID(crash)
				record.Kind, record.ID = owner, "crash:"+crash.ID
				record.Payload, _ = json.Marshal(crash)
				batch.Records = append(batch.Records, record)
			}
			for i := range batch.Records {
				batch.Records[i].Epoch = batch.Epoch
			}
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
			contextOptions := IssueOptions{Context: "persisted", RootUID: "retained-root", Namespace: "sample"}
			contextPage, err := service.Issues(t.Context(), contextOptions)
			if err != nil || contextPage.RootContext == nil || contextPage.RootContext.Root.UID != "retained-root" || len(contextPage.Items) != 0 || !contextPage.Sync.Partial {
				t.Fatal("persisted context required a live object, hid a malformed observation, or leaked into history")
			}
			contextOptions.OccurrenceID = contextPage.RootContext.Occurrence.OccurrenceID
			selectedPage, err := service.Issues(t.Context(), contextOptions)
			if err != nil || selectedPage.SelectedOccurrence == nil || selectedPage.SelectionStatus != "resolved" || selectedPage.SelectedOccurrence.BrokeAt != nil {
				t.Fatal("exact missing-time occurrence was substituted or fabricated a timestamp")
			}
			occurrences, err := service.queryEvidence(t.Context(), storage.EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute), Namespace: "sample", RootUID: "retained-root", Type: "occurrence", Limit: 1})
			if err != nil || occurrences.Total != 2 || occurrences.Counts["issue_opened"] != 1 || occurrences.Counts["coverage_gap"] != 1 || len(occurrences.Items) != 1 || occurrences.Next == "" {
				t.Fatal("occurrence projection did not preserve accurate counts and bounded opaque pagination")
			}
			denyRoot.Store(true)
			if _, err := service.Issues(t.Context(), contextOptions); !errors.Is(err, ErrForbidden) {
				t.Fatal("persisted context bypassed current read authority")
			}
			denyRoot.Store(false)
			other := newTestService(t, server.URL, Scope{OrgID: "another-org", ClusterID: cluster})
			other.SetChangeStorage(provider)
			other.changes.evidenceEnabled = true
			otherPage, err := other.Issues(t.Context(), contextOptions)
			if err != nil || otherPage.RootContext != nil {
				t.Fatal("persisted root escaped organization scope")
			}
			query := storage.EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute), Namespace: "sample", Type: "timeline", Limit: 2}
			var seen = map[string]bool{}
			gaps, changes, crashes := 0, 0, 0
			for pages := 0; pages < 50; pages++ {
				page, err := service.queryEvidence(t.Context(), query)
				if err != nil {
					t.Fatal(err)
				}
				if page.Counts["coverage_gap"] != 6+len(malformed) || page.Counts["change"] != 1 || page.Counts["crash"] != 4 || page.Total != 11+len(malformed) || len(page.Items) > 2 {
					t.Fatalf("projected counts or page bound incorrect: %#v", page.Counts)
				}
				for _, record := range page.Items {
					if seen[record.ID] {
						t.Fatal("opaque pagination repeated a slot")
					}
					seen[record.ID] = true
					switch record.Type {
					case "coverage_gap":
						gaps++
					case "change":
						changes++
					case "crash":
						crashes++
					default:
						t.Fatal("unsupported record escaped projection")
					}
				}
				if page.Next == "" {
					break
				}
				query.Cursor = page.Next
			}
			if len(seen) != 11+len(malformed) || gaps != 6+len(malformed) || changes != 1 || crashes != 4 || wrongScopeCalls.Load() != 0 {
				t.Fatal("mixed history lost records, hid gaps, or used an invalid namespaced cluster path")
			}
			timeline, err := service.evidenceTimeline(t.Context(), TimelineQuery{Namespace: "sample", Since: query.Since, Until: query.Until, Limit: 100})
			if err != nil || timeline.Total != 5 || len(timeline.Items) != 5 || len(timeline.Gaps) == 0 {
				t.Fatal("public timeline counts included unavailable evidence")
			}
			global := query
			global.Cursor, global.Namespace, global.Limit = "", "", 100
			if _, err := service.queryEvidence(t.Context(), global); err != nil || wrongScopeCalls.Load() != 0 {
				t.Fatal("cluster-scoped sources did not use cluster paths")
			}
			// A specifically requested absent kind is still an unavailable
			// request scope, not an authorized empty page.
			absent := global
			absent.Kind = "Gateway"
			if _, err := service.queryEvidence(t.Context(), absent); !errors.Is(err, ErrNotFound) {
				t.Fatal("explicit absent request scope did not fail closed")
			}
			denySecret.Store(true)
			if _, err := service.queryEvidence(t.Context(), global); !errors.Is(err, ErrForbidden) {
				t.Fatal("actual returned-source denial was converted into a gap")
			}
			denySecret.Store(false)
			denyPod.Store(true)
			if _, err := service.queryEvidence(t.Context(), global); !errors.Is(err, ErrForbidden) {
				t.Fatal("requested base scope denial was bypassed")
			}
			denyPod.Store(false)
			wrongCluster.Store(true)
			if selected, err := service.Issues(t.Context(), contextOptions); err != nil || selected.SelectedOccurrence != nil || selected.RootContext != nil || selected.SelectionStatus != "not_found" {
				t.Fatal("selected occurrence bypassed physical cluster identity")
			}
			page, err := service.queryEvidence(t.Context(), global)
			if err != nil || len(page.Items) != 0 {
				t.Fatal("physical cluster replacement exposed retained evidence")
			}
			contextPage, err = service.Issues(t.Context(), contextOptions)
			if err != nil || contextPage.RootContext != nil {
				t.Fatal("persisted root escaped physical cluster identity")
			}
		})
	}
}

func TestEvidenceProofErrorPriorityIsOrderIndependent(t *testing.T) {
	for _, denial := range []error{ErrForbidden, fmt.Errorf("wrapped: %w", ErrForbidden), &permissionDeniedError{Verb: "list", Resource: "secrets", Namespace: "sample"}, ErrUnauthorized} {
		for _, transient := range []error{context.DeadlineExceeded, ErrOperationBudget, errors.New("synthetic transport failure")} {
			for _, sequence := range [][]error{{denial, transient, denial, transient}, {transient, denial, transient, denial}} {
				var result error
				for _, next := range sequence {
					result = mergeEvidenceProofError(result, next)
				}
				expected := DiagnoseError(denial)
				actual := DiagnoseError(evidenceReadFailure(evidenceCheckpointKindProof, result))
				if actual.Code != expected.Code || actual.Retryable || !errors.Is(result, denial) {
					t.Fatal("later transient or cached proof replaced the typed authorization fence")
				}
			}
		}
	}
	for _, sequence := range [][]error{{ErrForbidden, ErrUnauthorized}, {ErrUnauthorized, ErrForbidden}} {
		var result error
		for _, next := range sequence {
			result = mergeEvidenceProofError(result, next)
		}
		if !errors.Is(result, ErrUnauthorized) {
			t.Fatal("authentication denial priority depends on scan order")
		}
	}
}

func TestEvidenceSourceProofDenialsOutrankTransientFailuresAcrossStorageBackends(t *testing.T) {
	for _, backend := range []string{"memory", "file", "postgres"} {
		for _, failure := range []string{"server-error", "timeout"} {
			for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
				for _, first := range []string{"Secret", "Gateway"} {
					t.Run(fmt.Sprintf("%s/%s/%d/%s-first", backend, failure, status, first), func(t *testing.T) {
						var provider storage.Provider
						var err error
						switch backend {
						case "memory":
							provider = storage.NewMemory()
						case "file":
							provider, err = storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
						case "postgres":
							dsn := os.Getenv("TEST_POSTGRES_DSN")
							if dsn == "" {
								t.Skip("explicit isolated harness Postgres required")
							}
							provider, err = storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
						}
						if err != nil {
							t.Fatal(err)
						}
						defer provider.Close()
						var secretCalls, gatewayCalls atomic.Int32
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/api/v1/namespaces/kube-system" {
								writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "priority-physical"}})
								return
							}
							switch {
							case strings.HasSuffix(r.URL.Path, "/secrets"):
								secretCalls.Add(1)
								http.Error(w, "denied", status)
							case strings.HasSuffix(r.URL.Path, "/gateways"):
								gatewayCalls.Add(1)
								if failure == "timeout" {
									<-r.Context().Done()
								} else {
									http.Error(w, "unavailable", http.StatusInternalServerError)
								}
							default:
								writeJSON(w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{"resourceVersion": "synthetic-version"}, "items": nil})
							}
						}))
						defer server.Close()
						client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Timeout: 250 * time.Millisecond})
						if err != nil {
							t.Fatal(err)
						}
						cluster := fmt.Sprintf("source-priority-%d", time.Now().UnixNano())
						service := NewService(client, Scope{OrgID: "synthetic-history", ClusterID: cluster}, time.Minute)
						service.SetChangeStorage(provider)
						service.changes.evidenceEnabled = true
						at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
						batch := storage.EvidenceBatch{ID: "capture:priority", At: at, Epoch: "synthetic"}
						second := "Gateway"
						if first == "Gateway" {
							second = "Secret"
						}
						// PostgreSQL traverses descending timestamps. Memory/file may
						// traverse any order; the direct priority test also replays
						// both orders and each repeated memoized failure explicitly.
						for i, kind := range []string{first, second, first, second} {
							batch.Records = append(batch.Records, syntheticChangeEvidence(cluster, kind, "sample", fmt.Sprintf("priority-%d", i), at.Add(-time.Duration(i)*time.Second)))
						}
						unrelated := syntheticChangeEvidence(cluster, "Pod", "sample", "unrelated", at)
						var change Change
						_ = json.Unmarshal(unrelated.Payload, &change)
						change.RootUID = "unrelated-root"
						unrelated.RootUID = change.RootUID
						unrelated.Payload, _ = json.Marshal(change)
						batch.Records = append(batch.Records, unrelated)
						scope := storage.EvidenceScope{OrgID: "synthetic-history", ClusterID: cluster, ClusterUID: "priority-physical"}
						if err := provider.(storage.EvidenceStore).CommitEvidence(t.Context(), scope, batch); err != nil {
							t.Fatal(err)
						}
						query := storage.EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute), Namespace: "sample", RootUID: "synthetic-root", Type: "change", Limit: 1}
						page, err := service.queryEvidence(t.Context(), query)
						detail := DiagnoseError(err)
						expected := ErrForbidden
						if status == http.StatusUnauthorized {
							expected = ErrUnauthorized
						}
						if !errors.Is(err, expected) || detail.Code != DiagnoseError(expected).Code || detail.Retryable || len(page.Items) != 0 || secretCalls.Load() != 1 || gatewayCalls.Load() > 1 || status == http.StatusForbidden && gatewayCalls.Load() != 1 {
							t.Fatalf("denial lost to transient/cache traversal: code=%s secretProofs=%d gatewayProofs=%d", detail.Code, secretCalls.Load(), gatewayCalls.Load())
						}
						if status == http.StatusUnauthorized {
							return // The transport invalidates a rejected credential epoch.
						}
						// A denied source outside the requested root is not a
						// blanket denial of an independently authorized good row.
						query.RootUID = "unrelated-root"
						page, err = service.queryEvidence(t.Context(), query)
						if err != nil || len(page.Items) != 1 || page.Items[0].Type != "change" || page.Counts["change"] != 1 || secretCalls.Load() != 1 || gatewayCalls.Load() != 1 {
							t.Fatal("unrelated denied checkpoint source poisoned authorized scoped evidence")
						}
					})
				}
			}
		}
	}
}

func TestHistoricalIssuesSinceOnlyContinuationUsesPinnedWindow(t *testing.T) {
	service, store, scope := reviewHistoryService(t)
	at := time.Now().UTC().Add(-time.Hour)
	batch := storage.EvidenceBatch{ID: "initial", At: at, Epoch: "synthetic"}
	for i := 0; i < 13; i++ {
		uid := fmt.Sprintf("pod-%02d", i)
		payload, _ := json.Marshal(Issue{Root: ObjectRef{Kind: "Pod", Namespace: "shop", Name: uid, UID: uid}, Rule: "pod.crash_loop", Severity: "warning", Count: 1, Broken: true})
		batch.Issues = append(batch.Issues, storage.EvidenceIssue{Key: uid, Namespace: "shop", Kind: "Pod", UID: uid, Rule: "pod.crash_loop", LastSeen: at, Payload: payload})
	}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	for _, explicitUntil := range []bool{false, true} {
		t.Run(fmt.Sprintf("fixed-until-%t", explicitUntil), func(t *testing.T) {
			options := IssueOptions{Since: at.Add(-time.Minute), Namespace: "shop", Limit: 10}
			if explicitUntil {
				options.Until = time.Now().UTC()
			}
			first, err := service.Issues(t.Context(), options)
			if err != nil || first.Next == "" {
				t.Fatalf("first page: next=%q err=%v", first.Next, err)
			}
			time.Sleep(2 * time.Millisecond)
			options.Cursor = first.Next
			second, err := service.Issues(t.Context(), options)
			if err != nil {
				t.Fatalf("continuation changed its pinned window: %v", err)
			}
			if len(first.Items)+len(second.Items) != 13 || second.Next != "" {
				t.Fatal("continuation lost retained observations")
			}
			invalid := options
			invalid.Namespace = "other"
			if _, err = service.Issues(t.Context(), invalid); !errors.Is(err, ErrInvalidArguments) {
				t.Fatal("cursor escaped its namespace")
			}
			invalid = options
			invalid.Until = time.Now().UTC().Add(time.Minute)
			if _, err = service.Issues(t.Context(), invalid); !errors.Is(err, ErrInvalidArguments) {
				t.Fatal("cursor escaped its fixed window")
			}
			invalid = options
			invalid.Severity = "critical"
			if _, err = service.Issues(t.Context(), invalid); !errors.Is(err, ErrInvalidArguments) {
				t.Fatal("cursor escaped its issue filters")
			}
		})
	}
}

func TestQueuedEvidenceCannotReturnAfterDeletionAndFreshCapture(t *testing.T) {
	service, store, scope := reviewHistoryService(t)
	at := time.Now().UTC().Add(-time.Hour)
	first := storage.EvidenceBatch{ID: "before-delete", At: at, Epoch: "synthetic"}
	if err := store.CommitEvidence(t.Context(), scope, first); err != nil {
		t.Fatal(err)
	}
	queued := storage.EvidenceBatch{ID: "capture:queued-before-delete", Expected: first.ID, At: at.Add(time.Minute), Epoch: "synthetic", Issues: []storage.EvidenceIssue{{Key: "old", Kind: "Pod", UID: "old", Rule: "pod.crash_loop", Payload: json.RawMessage(`{"count":1}`)}}}
	if err := store.DeleteEvidence(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	fresh := storage.EvidenceBatch{ID: "fresh", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at.Add(2 * time.Minute), Epoch: "synthetic"}
	if err = store.CommitEvidence(t.Context(), scope, fresh); err != nil {
		t.Fatal(err)
	}
	if err = service.commitEvidence(t.Context(), inventoryGeneration{EvidenceScope: scope, EvidenceBatch: &queued}); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Type: "issue"})
	if err != nil || page.Total != 0 {
		t.Fatalf("queued pre-delete evidence returned: total=%d err=%v", page.Total, err)
	}
}

func TestHistoryWithoutKubernetesClientFailsClosed(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "org", ClusterID: "cluster"}, time.Minute)
	service.SetChangeStorage(storage.NewMemory())
	service.changes.evidenceEnabled = true
	at := time.Now().UTC().Add(-time.Hour)
	calls := map[string]func() error{
		"authorize": func() error { _, err := service.authorizeEvidence(t.Context(), ""); return err },
		"changes":   func() error { _, err := service.Changes(t.Context(), ChangeQuery{Since: at}); return err },
		"timeline":  func() error { _, err := service.Timeline(t.Context(), TimelineQuery{Since: at}); return err },
		"issues":    func() error { _, err := service.Issues(t.Context(), IssueOptions{Since: at}); return err },
		"nil client GET": func() error {
			var client *Client
			var result any
			return client.GetJSON(context.Background(), "/api", &result)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrCredentialUnavailable) {
				t.Fatal("history read without authority succeeded")
			}
		})
	}
}

func TestEvidencePreparationPinsDeletionGeneration(t *testing.T) {
	service, store, scope := reviewHistoryService(t)
	at := time.Now().UTC().Add(-time.Hour)
	generation := func(uid string, at time.Time) inventoryGeneration {
		kinds := map[string]kubeindex.KindStatus{}
		for kind := range indexedResourceIDs {
			kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: at, CompletedAt: at}
		}
		return inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds, Records: []kubeindex.Record{
			{UID: "physical", Kind: "Namespace", Name: "kube-system"},
			{UID: uid, Kind: "Pod", Name: uid, Namespace: "shop", Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 2}}},
		}}}
	}
	old := generation("old", at)
	if err := service.prepareEvidence(t.Context(), &old, inventoryGeneration{}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteEvidence(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	fresh := generation("fresh", at.Add(time.Minute))
	if err := service.prepareEvidence(t.Context(), &fresh, inventoryGeneration{}); err != nil {
		t.Fatal(err)
	}
	if fresh.EvidenceBatch == nil || fresh.EvidenceBatch.Generation != old.EvidenceBatch.Generation+1 {
		t.Fatal("fresh capture did not pin the durable deletion fence")
	}
	if err := service.commitEvidence(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := service.commitEvidence(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), UID: "old"})
	if err != nil || page.Total != 0 {
		t.Fatalf("queued prepared inventory resurrected: total=%d err=%v", page.Total, err)
	}
	page, err = store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Type: "issue", UID: "fresh"})
	if err != nil || page.Total == 0 {
		t.Fatalf("fresh capture failed to resume: total=%d err=%v", page.Total, err)
	}
}

func TestLiveIssueCarriesPinnedOccurrenceThroughConditionTimeDrift(t *testing.T) {
	service, store, scope := reviewHistoryService(t)
	opened := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second).Add(123456789 * time.Nanosecond)
	root := ObjectRef{Kind: "Deployment", Namespace: "shop", Name: "api", UID: "api-root"}
	commit := func(id, expected string, generation uint64, brokeAt, at time.Time) {
		t.Helper()
		raw, _ := json.Marshal(Issue{Root: root, Rule: "pod.crash_loop", Severity: "warning", Broken: true, BrokeAt: &brokeAt})
		batch := storage.EvidenceBatch{ID: id, Expected: expected, Generation: generation, At: at, Epoch: "synthetic", Issues: []storage.EvidenceIssue{{Key: "api", Namespace: root.Namespace, Kind: root.Kind, UID: root.UID, Rule: "pod.crash_loop", LastSeen: at, Broken: true, Payload: raw}}}
		if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
	}
	commit("capture:open", "", 0, opened, opened)
	checkpoint, _ := store.EvidenceCheckpoint(t.Context(), scope)
	drifted := opened.Add(7*time.Minute + 987*time.Nanosecond)
	commit("capture:later", checkpoint.ID, checkpoint.Generation, drifted, opened.Add(8*time.Minute))

	kinds := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().UTC(), CompletedAt: time.Now().UTC()}
	}
	generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds, Records: []kubeindex.Record{
		{Kind: "Namespace", Name: "kube-system", UID: "physical"},
		{Kind: root.Kind, Namespace: root.Namespace, Name: root.Name, UID: root.UID},
		{Kind: "Pod", Namespace: root.Namespace, Name: "api-0", UID: "pod", Phase: "Running", ReadyStatus: "False", ReadyTransitionAt: drifted, Owners: []kubeindex.OwnerRef{{Kind: root.Kind, UID: root.UID, Controller: true}}, Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 2}}},
	}}}
	live, err := service.Issues(pinInventory(t.Context(), generation), IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var item Issue
	for _, issue := range live.Items {
		if issue.Root.UID == root.UID && issue.Rule == "pod.crash_loop" {
			item = issue
		}
	}
	if item.OccurrenceID == "" || item.BrokeAt == nil || !item.BrokeAt.Equal(opened) {
		t.Fatalf("live list did not adopt the pinned occurrence: id=%q at=%v", item.OccurrenceID, item.BrokeAt)
	}
	for _, options := range []IssueOptions{
		{Context: "persisted", RootUID: root.UID, Namespace: root.Namespace, OccurrenceID: item.OccurrenceID, Center: *item.BrokeAt},
		{Context: "persisted", RootUID: root.UID, Namespace: root.Namespace, Center: *item.BrokeAt},
	} {
		resolved, err := service.persistedRootContext(t.Context(), options)
		if err != nil || resolved.SelectedOccurrence == nil || resolved.SelectedOccurrence.OccurrenceID != item.OccurrenceID {
			t.Fatalf("persisted root did not resolve the live selection %+v: %v", options, err)
		}
	}
	query := breaklogs.Query{Namespace: root.Namespace, RootUID: root.UID, OccurrenceID: item.OccurrenceID, Center: *item.BrokeAt, From: opened.Add(-time.Minute), To: opened.Add(time.Minute)}
	resolvedQuery, err := service.resolveBreakLogSelection(t.Context(), query, breaklogs.State{})
	if err != nil || !resolvedQuery.Center.Equal(opened) {
		t.Fatalf("break-logs selection rejected the live list identity: %v", err)
	}
}

type occurrenceLookupFailureStore struct {
	storage.Provider
	storage.EvidenceStore
	cause error
	page  storage.EvidencePage
}

func (store occurrenceLookupFailureStore) QueryEvidence(context.Context, storage.EvidenceScope, storage.EvidenceQuery) (storage.EvidencePage, error) {
	return store.page, store.cause
}

func TestLiveIssueIncompleteOccurrenceHistoryWithholdsCenter(t *testing.T) {
	service, store, _ := reviewHistoryService(t)
	service.SetChangeStorage(occurrenceLookupFailureStore{Provider: service.usageProvider(), EvidenceStore: store, page: storage.EvidencePage{Items: []storage.EvidenceRecord{{Type: "coverage_gap", Payload: json.RawMessage(`{}`)}}}})
	drifted := time.Now().UTC().Add(-time.Minute)
	page := IssuePage{Items: []Issue{{Root: ObjectRef{Kind: "Pod", Namespace: "shop", UID: "pod"}, Rule: "pod.crash_loop", Broken: true, BrokeAt: &drifted, BrokeAtSource: "condition"}}}
	if err := service.pinOpenOccurrences(t.Context(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Sync.Partial || len(page.Partial) == 0 || !page.Items[0].Partial || page.Items[0].BrokeAt != nil || page.Items[0].OccurrenceID != "" || page.Items[0].BrokeAtSource != "" {
		t.Fatalf("unavailable occurrence history served an authoritative drifting center: %+v", page)
	}
}

func TestLiveIssuesFailClosedWhenOccurrenceLookupFails(t *testing.T) {
	for _, cause := range []error{storage.ErrUnsupported, context.DeadlineExceeded, ErrForbidden, ErrUnauthorized} {
		t.Run(errorClass(cause), func(t *testing.T) {
			service, store, _ := reviewHistoryService(t)
			service.SetChangeStorage(occurrenceLookupFailureStore{Provider: service.usageProvider(), EvidenceStore: store, cause: cause})
			at := time.Now().UTC()
			kinds := map[string]kubeindex.KindStatus{}
			for kind := range indexedResourceIDs {
				kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: at, CompletedAt: at}
			}
			generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds, Records: []kubeindex.Record{
				{Kind: "Namespace", Name: "kube-system", UID: "physical"},
				{Kind: "Pod", Namespace: "shop", Name: "api-0", UID: "pod", Phase: "Running", ReadyStatus: "False", ReadyTransitionAt: at.Add(-time.Minute), Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 2}}},
			}}}
			ctx := pinInventory(t.Context(), generation)
			before, err := service.issuesSnapshot(ctx, IssueOptions{})
			if err != nil || len(before.Items) == 0 || !before.Items[0].Broken {
				t.Fatalf("fixture has no live broken issue: %+v %v", before, err)
			}
			page, err := service.Issues(ctx, IssueOptions{})
			if !errors.Is(err, cause) || len(page.Items) != 0 || DiagnoseError(err).Code != DiagnoseError(cause).Code {
				t.Fatalf("lookup failure served drifting live items or lost its cause: %+v %v", page, err)
			}
		})
	}
}
