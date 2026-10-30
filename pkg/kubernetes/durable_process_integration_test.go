package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type processPublicationBarrier struct {
	storage.Provider
	storage.EvidenceStore
	storage.BlobCAS
	armed                  bool
	phase, marker, headKey string
}

func (p *processPublicationBarrier) pause(key string) {
	head, _ := p.Provider.ReadBlob(p.headKey)
	raw, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "phase": p.phase, "payload_key": key, "old_head": json.RawMessage(head)})
	if err := os.WriteFile(p.marker, raw, 0600); err != nil {
		panic(err)
	}
	select {}
}
func (p *processPublicationBarrier) WriteBlob(key string, value []byte) error {
	if p.armed && strings.Contains(key, "/data/") && p.phase == "before_payload" {
		p.pause(key)
	}
	err := p.Provider.WriteBlob(key, value)
	if err == nil && p.armed && strings.Contains(key, "/data/") && p.phase == "after_payload" {
		p.pause(key)
	}
	return err
}
func (p *processPublicationBarrier) CompareAndSwapBlob(key string, old, value []byte) (bool, error) {
	won, err := p.BlobCAS.CompareAndSwapBlob(key, old, value)
	if won && err == nil && p.armed && key == p.headKey && p.phase == "after_cas" {
		p.pause(key)
	}
	return won, err
}
func processProvider(t *testing.T) storage.Provider {
	t.Helper()
	if os.Getenv("HARNESS_PROCESS_BACKEND") == "file" {
		p, e := storage.NewFile(storage.FileOptions{DataDir: filepath.Join(os.Getenv("HARNESS_PROCESS_CASE"), "store")})
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	p, e := storage.NewPostgres(storage.PostgresOptions{DSN: os.Getenv("TEST_POSTGRES_DSN")})
	if e != nil {
		t.Fatal("isolated Postgres unavailable")
	}
	return p
}
func processService(t *testing.T, p storage.Provider) *Service {
	t.Helper()
	client, e := NewClient(Config{Endpoint: os.Getenv("HARNESS_PROCESS_AUTHORITY"), AllowLoopbackHTTP: true, Credentials: staticCredential(t, "synthetic-process-reader")})
	if e != nil {
		t.Fatal(e)
	}
	s := NewService(client, Scope{OrgID: "synthetic-process", ClusterID: os.Getenv("HARNESS_PROCESS_CLUSTER"), CredentialID: "configured"}, time.Hour)
	s.SetChangeStorage(p)
	s.changes.evidenceEnabled = true
	return s
}
func processGeneration(label string) inventoryGeneration {
	at := time.Now().UTC()
	kinds := map[string]kubeindex.KindStatus{}
	for k := range indexedResourceIDs {
		kinds[k] = kubeindex.KindStatus{State: "ready", ObservedAt: at, CompletedAt: at}
	}
	records := []kubeindex.Record{{Kind: "Namespace", Name: "kube-system", UID: "process-physical"}}
	for i := 0; i < 2; i++ {
		records = append(records, kubeindex.Record{Kind: "Pod", Namespace: "shop", Name: fmt.Sprintf("%s-%d", label, i), UID: fmt.Sprintf("%s-%d", label, i), Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 2}}})
	}
	return inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds, Records: records}}
}
func checkProcessGeneration(t *testing.T, g inventoryGeneration, head inventoryLeaseRecord, label string) {
	t.Helper()
	if g.Revision == "" || g.Revision != head.Revision {
		t.Fatal("head/payload mismatch")
	}
	names := map[string]bool{}
	for _, r := range g.View.Records {
		if r.Kind == "Pod" {
			names[r.UID] = true
		}
	}
	if len(names) != 2 || !names[label+"-0"] || !names[label+"-1"] {
		t.Fatalf("partial/wrong payload: expected %s, got %v", label, names)
	}
}

// Invoked as an OS child of the bounded integration parent; never by normal CI.
func TestDurableInventoryProcessChild(t *testing.T) {
	mode := os.Getenv("HARNESS_PROCESS_CHILD")
	if mode == "" {
		t.Skip("owned subprocess only")
	}
	base := processProvider(t)
	defer base.Close()
	s := processService(t, base)
	ctx := t.Context()
	if mode == "reader" {
		g, head, e := readInventoryGeneration(base, s.ActiveScope(), "")
		if e != nil {
			t.Fatal(e)
		}
		checkProcessGeneration(t, g, head, os.Getenv("HARNESS_PROCESS_EXPECT"))
		if e = s.authorizeInventoryRestore(ctx, g); e != nil {
			t.Fatalf("next process authority/restore: %v", e)
		}
		if _, e = hydrateInventory(g.View); e != nil {
			t.Fatal(e)
		}
		key, e := inventoryDataKey(s.ActiveScope(), head)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := base.ReadBlob(key)
		if e != nil || len(raw) == 0 {
			t.Fatal("referenced payload missing")
		}
		t.Logf("next-process restore revision=%s pods=2 namespaceUID=process-physical", head.Revision)
		return
	}
	if mode == "successor" {
		g, head, e := readInventoryGeneration(base, s.ActiveScope(), "")
		if e != nil {
			t.Fatal(e)
		}
		oldRaw, e := base.ReadBlob(inventoryKey(s.ActiveScope()))
		if e != nil {
			t.Fatal(e)
		}
		lease, owned, e := s.claimInventory(ctx, base, "successor-"+os.Getenv("HARNESS_PROCESS_CLUSTER"))
		if e != nil || !owned {
			t.Fatalf("natural expired takeover not admitted: %v", e)
		}
		if e = lease.commitJournal(ctx, s, g); e != nil {
			t.Fatalf("pending journal replay: %v", e)
		}
		s.changes.inventories[s.indexScope()] = &inventoryCache{generation: g}
		next := processGeneration("successor")
		if e = lease.publish(ctx, s, &next); e != nil {
			t.Fatal(e)
		}
		if e = lease.commitJournal(ctx, s, next); e != nil {
			t.Fatal(e)
		}
		current, currentHead, e := readInventoryGeneration(base, s.ActiveScope(), "")
		if e != nil {
			t.Fatal(e)
		}
		checkProcessGeneration(t, current, currentHead, "successor")
		// A replay actor carrying the pre-takeover expected head cannot win CAS.
		won, e := base.(storage.BlobCAS).CompareAndSwapBlob(inventoryKey(s.ActiveScope()), oldRaw, oldRaw)
		if e != nil || won {
			t.Fatalf("retired expected head was accepted: %v", e)
		}
		currentKey, e := inventoryDataKey(s.ActiveScope(), currentHead)
		if e != nil {
			t.Fatal(e)
		}
		before, e := base.ReadBlob(currentKey)
		if e != nil {
			t.Fatal(e)
		}
		stale := &inventoryLease{provider: base, cas: base.(storage.BlobCAS), scope: s.ActiveScope(), record: head, expected: oldRaw}
		stale.record.Expires = time.Now().Add(time.Minute)
		staleGeneration := processGeneration("stale")
		if e = stale.publish(ctx, s, &staleGeneration); !errors.Is(e, errInventoryLeaseLost) {
			t.Fatalf("stale writer publish not fenced: %v", e)
		}
		after, e := base.ReadBlob(currentKey)
		if e != nil || string(before) != string(after) {
			t.Fatal("stale cleanup removed/changed referenced successor blob")
		}
		t.Logf("takeover/replay/fence/reference-safe revision=%s", currentHead.Revision)
		return
	}
	marker := filepath.Join(os.Getenv("HARNESS_PROCESS_CASE"), "barrier.json")
	wrapped := &processPublicationBarrier{Provider: base, EvidenceStore: base.(storage.EvidenceStore), BlobCAS: base.(storage.BlobCAS), phase: os.Getenv("HARNESS_PROCESS_PHASE"), marker: marker, headKey: inventoryKey(s.ActiveScope())}
	s.SetChangeStorage(wrapped)
	s.changes.evidenceEnabled = true
	lease, owned, e := s.claimInventory(ctx, wrapped, "writer-"+os.Getenv("HARNESS_PROCESS_CLUSTER"))
	if e != nil || !owned {
		t.Fatal(e)
	}
	first := processGeneration("baseline")
	if e = lease.publish(ctx, s, &first); e != nil {
		t.Fatal(e)
	}
	if e = lease.commitJournal(ctx, s, first); e != nil {
		t.Fatal(e)
	}
	if first.EvidenceFailure != "" || first.EvidenceBatch == nil {
		t.Fatal("baseline evidence preparation failed")
	}
	s.changes.inventories[s.indexScope()] = &inventoryCache{generation: first}
	wrapped.armed = true
	next := processGeneration("candidate")
	if e = lease.publish(ctx, s, &next); e != nil {
		t.Fatal(e)
	}
	t.Fatal("publication barrier not reached")
}

func TestDurableInventoryOSKillPublicationBoundaries(t *testing.T) {
	if os.Getenv("HARNESS_PROCESS_GATE") != "1" {
		t.Skip("explicit recorded real-provider process gate required")
	}
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			fmt.Fprint(w, `{"metadata":{"name":"kube-system","uid":"process-physical"}}`)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "PartialObjectMetadataList") {
			fmt.Fprint(w, `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","items":[]}`)
			return
		}
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"List","items":[]}`)
	}))
	t.Cleanup(authority.Close)
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for _, backend := range []string{"file", "postgres"} {
		for _, phase := range []string{"before_payload", "after_payload", "after_cas"} {
			t.Run(backend+"/"+phase, func(t *testing.T) {
				t.Parallel()
				dir := filepath.Join(os.Getenv("HARNESS_DATA_DIR"), "kill-"+backend+"-"+phase)
				if e := os.MkdirAll(dir, 0700); e != nil {
					t.Fatal(e)
				}
				env := append(os.Environ(), "HARNESS_PROCESS_BACKEND="+backend, "HARNESS_PROCESS_CASE="+dir, "HARNESS_PROCESS_PHASE="+phase, "HARNESS_PROCESS_CLUSTER="+backend+"-"+phase, "HARNESS_PROCESS_AUTHORITY="+authority.URL)
				child := exec.Command(executable, "-test.run=^TestDurableInventoryProcessChild$", "-test.v", "-test.timeout=8m")
				child.Env = append(env, "HARNESS_PROCESS_CHILD=writer")
				output, e := os.OpenFile(filepath.Join(dir, "writer.log"), os.O_CREATE|os.O_WRONLY, 0600)
				if e != nil {
					t.Fatal(e)
				}
				defer output.Close()
				child.Stdout = output
				child.Stderr = output
				if e = child.Start(); e != nil {
					t.Fatal(e)
				}
				waited := false
				t.Cleanup(func() {
					if !waited {
						_ = child.Process.Kill()
						_ = child.Wait()
					}
				})
				marker := filepath.Join(dir, "barrier.json")
				deadline := time.Now().Add(30 * time.Second)
				for {
					if _, e = os.Stat(marker); e == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("owned writer did not admit barrier")
					}
					time.Sleep(25 * time.Millisecond)
				}
				var receipt struct {
					PID     int                  `json:"pid"`
					Phase   string               `json:"phase"`
					OldHead inventoryLeaseRecord `json:"old_head"`
				}
				raw, e := os.ReadFile(marker)
				if e != nil {
					t.Fatal(e)
				}
				if e = json.Unmarshal(raw, &receipt); e != nil {
					t.Fatal(e)
				}
				if receipt.PID != child.Process.Pid || receipt.Phase != phase {
					t.Fatal("owned writer identity/barrier mismatch")
				}
				if e = child.Process.Kill(); e != nil {
					t.Fatal(e)
				}
				exitErr := child.Wait()
				waited = true
				var exit *exec.ExitError
				if !errors.As(exitErr, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
					t.Fatalf("writer not OS-killed: %v", exitErr)
				}
				expected := "baseline"
				if phase == "after_cas" {
					expected = "candidate"
				}
				run := func(mode, label string) {
					t.Helper()
					cmd := exec.Command(executable, "-test.run=^TestDurableInventoryProcessChild$", "-test.v", "-test.timeout=2m")
					cmd.Env = append(env, "HARNESS_PROCESS_CHILD="+mode, "HARNESS_PROCESS_EXPECT="+label)
					out, e := cmd.CombinedOutput()
					if e != nil {
						t.Fatalf("%s: %v\n%s", mode, e, out)
					}
					if e = os.WriteFile(filepath.Join(dir, mode+".log"), out, 0600); e != nil {
						t.Fatal(e)
					}
				}
				run("reader", expected)
				// Preserve the real3min lease contract: all six cases wait in parallel,
				// rather than mutating expiry or inventing a production clock hook.
				expiry := receipt.OldHead.Expires
				for time.Now().Before(expiry.Add(150 * time.Millisecond)) {
					select {
					case <-t.Context().Done():
						t.Fatal("lease expiry wait canceled")
					case <-time.After(100 * time.Millisecond):
					}
				}
				run("successor", "successor")
				run("reader", "successor")
				t.Logf("backend=%s phase=%s killed_owned_pid=%d SIGKILL next_process=%s natural_takeover=true storage_retained=%s", backend, phase, receipt.PID, expected, dir)
			})
		}
	}
}
