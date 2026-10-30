package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The fixed production STS endpoint intentionally cannot be redirected by env.
// Inject only the SDK client's local endpoint here; production AssumeRole cache,
// EKS SigV4 signer, epoch recovery, inventory and log sessions are unchanged.
func TestNativeRoleRenewalWithPermittedPodDeploymentStreamsAndLaterOutageIntegration(t *testing.T) {
	if os.Getenv("HARNESS_NATIVE_LOG_BOUNDARY") != "1" {
		t.Skip("explicit recorded local native boundary required")
	}
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	var providerDown atomic.Bool
	var stsCalls, logCalls, mints, resumedHTTP atomic.Int64
	stsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stsCalls.Add(1)
		if providerDown.Load() {
			w.WriteHeader(503)
			fmt.Fprint(w, `<ErrorResponse><Error><Code>ServiceUnavailable</Code><Message>synthetic provider outage</Message></Error></ErrorResponse>`)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/synthetic/boundary</Arn><AssumedRoleId>synthetic:boundary</AssumedRoleId></AssumedRoleUser><Credentials><AccessKeyId>SYNTHETICLOCALKEY</AccessKeyId><SecretAccessKey>synthetic-local-only-secret</SecretAccessKey><SessionToken>synthetic-session-%d</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleResult><ResponseMetadata><RequestId>synthetic</RequestId></ResponseMetadata></AssumeRoleResponse>`, stsCalls.Load(), now().Add(30*time.Minute).Format(time.RFC3339))
	}))
	defer stsServer.Close()
	sdk := sts.New(sts.Options{Region: "us-east-1", BaseEndpoint: aws.String(stsServer.URL), Credentials: credentials.NewStaticCredentialsProvider("synthetic-bootstrap", "synthetic-bootstrap-secret", ""), HTTPClient: stsServer.Client(), RetryMaxAttempts: 1})
	role := assumeRoleCredentials(sdk, "arn:aws:iam::123456789012:role/synthetic")
	source := newEKSSource("synthetic-native-boundary", "us-east-1", role, now).(*expiringBearerSource)
	fixture := newFakeCluster(t, "initially-unaccepted")
	root := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"uid": "root", "name": "api", "namespace": "shop"}, "spec": map[string]any{"replicas": 1}, "status": map[string]any{"replicas": 1, "readyReplicas": 1, "availableReplicas": 1}}
	rs := map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": map[string]any{"uid": "rs", "name": "api-rs", "namespace": "shop", "ownerReferences": []any{map[string]any{"kind": "Deployment", "name": "api", "uid": "root", "controller": true}}}}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"uid": "pod-00", "name": "api-00", "namespace": "shop", "ownerReferences": []any{map[string]any{"kind": "ReplicaSet", "name": "api-rs", "uid": "rs", "controller": true}}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app-0"}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "app-0", "ready": true, "restartCount": 0, "state": map[string]any{"running": map[string]any{"startedAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}}}}}
	broken := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"uid": "broken", "name": "broken", "namespace": "shop"}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app"}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "app", "restartCount": 3, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}}}
	fixture.serve("deployments", root)
	fixture.serve("replicasets", rs)
	fixture.serve("pods", pod, broken)
	fixture.serve("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": "native-physical"}})
	fixture.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		accepted := r.Header.Get("Authorization") == "Bearer "+fixture.token || fixture.extra[r.Header.Get("Authorization")]
		fixture.mu.Unlock()
		if !accepted {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			fmt.Fprint(w, `{"kind":"Status","status":"Failure","reason":"Unauthorized","code":401}`)
			return
		}
		switch r.URL.Path {
		case "/api/v1/namespaces/kube-system":
			writeJSON(w, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": "native-physical"}})
			return
		case "/api/v1/namespaces/shop/pods/api-00/log":
			logCalls.Add(1)
			if r.URL.Query().Get("sinceTime") != "" {
				resumedHTTP.Add(1)
			}
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "%s synthetic-native-log-%d\n", time.Now().UTC().Format(time.RFC3339Nano), logCalls.Load())
			return
		case "/api/v1/namespaces/shop/pods/api-00":
			writeJSON(w, pod)
			return
		case "/apis/apps/v1/namespaces/shop/deployments/api":
			writeJSON(w, root)
			return
		}
		fixture.ServeHTTP(w, r)
	})
	mint := source.refresh
	source.refresh = func(ctx context.Context) (string, time.Time, error) {
		mints.Add(1)
		token, expiry, err := mint(ctx)
		if err == nil {
			fixture.acceptToken(token)
		}
		return token, expiry, err
	}
	data := filepath.Join(os.Getenv("HARNESS_DATA_DIR"), "native-log-file")
	provider, err := storage.NewFile(storage.FileOptions{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	fleet := newBootFleet(t, "synthetic-native", provider, "native-log-boundary")
	cluster := fleet.add("native", fixture, source, 0)
	// The older boot-fleet broker forwards blobs, not the optional EvidenceStore.
	// Use the actual file provider so this integration proves journal capture too.
	cluster.service.SetChangeStorage(provider)
	cluster.service.SetScrubber(replacingScrubber{})
	fleet.startWithin(time.Second)
	current := func() *Service {
		fleet.registry.mu.Lock()
		defer fleet.registry.mu.Unlock()
		return fleet.registry.entries[0].Service
	}
	ready := func() bool {
		s := current()
		g, ok := publishedGeneration(fleet.view, s.ActiveScope())
		return ok && len(podNames(g)) == 2
	}
	waitUntil(t, 12*time.Second, "actual autonomous nonzero publication", ready)
	waitUntil(t, 8*time.Second, "complete first publication before renewal", func() bool { return !current().recordedFirstReadyAt().IsZero() })
	first := current()
	firstReady := first.recordedFirstReadyAt()
	assertBodies := func() {
		o, e := current().Overview(t.Context())
		if e != nil || o.Pods != 2 || o.Nodes < 1 || o.AllocatableCPU == "" || o.AllocatableMemory == "" {
			t.Fatalf("nonzero capacity: %+v %v", o, e)
		}
		issues, e := current().Issues(t.Context(), IssueOptions{Broken: true})
		if e != nil || issues.Broken.Total < 1 {
			t.Fatalf("nonzero current issues: %+v %v", issues, e)
		}
	}
	assertBodies()
	var podResume, workloadResume time.Time
	seenLines := map[string]bool{}
	readStreams := func(label string) {
		s := current()
		s.SetScrubber(replacingScrubber{})
		stop := errors.New("bounded viewer pause")
		var podLines, workloadLines int
		ctx, cancel := context.WithTimeout(logStreamTestContext(), 10*time.Second)
		podOptions := PodLogStreamOptions{Namespace: "shop", Pod: "api-00", Container: "app-0", SinceSeconds: 60, TailLines: 10, Timestamps: true}
		if !podResume.IsZero() {
			podOptions.SinceTime, podOptions.SinceSeconds, podOptions.TailLines = podResume, 0, 0
		}
		err := s.StreamPodLogs(ctx, podOptions, func(event PodLogStreamEvent) error {
			if event.Event == "line" {
				if seenLines[event.Text] {
					t.Error("duplicate Pod line across reconnect")
				}
				seenLines[event.Text] = true
				podResume, _ = time.Parse(time.RFC3339Nano, event.Timestamp)
				podLines++
				return stop
			}
			return nil
		})
		cancel()
		if podLines == 0 {
			t.Fatalf("%s permitted Pod stream failed: %v", label, err)
		}
		ctx, cancel = context.WithTimeout(logStreamTestContext(), 10*time.Second)
		workloadOptions := WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root", Container: "app-0", SinceSeconds: 60, TailLines: 10, Timestamps: true}
		if !workloadResume.IsZero() {
			workloadOptions.SinceTime, workloadOptions.SinceSeconds, workloadOptions.TailLines = workloadResume, 0, 0
		}
		err = s.StreamWorkloadLogs(ctx, workloadOptions, func(event WorkloadLogStreamEvent) error {
			if event.Event == "line" {
				if seenLines[event.Text] {
					t.Error("duplicate Deployment line across reconnect")
				}
				seenLines[event.Text] = true
				workloadResume, _ = time.Parse(time.RFC3339Nano, event.Timestamp)
				workloadLines++
				return stop
			}
			return nil
		})
		cancel()
		if workloadLines == 0 {
			t.Fatalf("%s permitted Deployment stream failed: %v", label, err)
		}
		if _, err = s.OpenPodLogStream(context.Background(), PodLogStreamOptions{Namespace: "shop", Pod: "api-00", Container: "app-0"}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("no product permission admitted logstream: %v", err)
		}
		t.Logf("phase=%s pod_lines=%d deployment_lines=%d sts_calls=%d mints=%d log_http_requests=%d", label, podLines, workloadLines, stsCalls.Load(), mints.Load(), logCalls.Load())
	}
	readStreams("baseline")
	clock.Add(int64(13 * time.Minute))
	waitUntil(t, 15*time.Second, "native bearer renewal/recollection without page reads", func() bool { return current().identity != first.identity && ready() })
	assertBodies()
	readStreams("after-13minute-native-renewal")
	renewed := current()
	providerDown.Store(true)
	fixture.mu.Lock()
	clear(fixture.extra)
	fixture.token = "reject-all"
	fixture.mu.Unlock()
	// Advance past the signing session too, so failure reaches actual SDK STS HTTP.
	clock.Add(int64(19 * time.Minute))
	ctx, cancel := renewed.credentialContext(t.Context())
	body, logErr := renewed.client.openPodLogStream(ctx, "/api/v1/namespaces/shop/pods/api-00/log?follow=true", false)
	if body != nil {
		body.Close()
	}
	cancel()
	if logErr == nil {
		t.Fatal("later rejected native log read succeeded")
	}
	waitUntil(t, 12*time.Second, "actual SDK provider outage observed", func() bool { return stsCalls.Load() >= 2 })
	before := mints.Load()
	for i := 0; i < 100; i++ {
		_, _ = fleet.registry.ResolveCluster("synthetic-native", "native")
	}
	if mints.Load() > before+1 {
		t.Fatalf("UI storm bypassed native cooldown: before=%d after=%d", before, mints.Load())
	}
	providerDown.Store(false)
	clock.Add(int64(2 * time.Second))
	waitUntil(t, 20*time.Second, "autonomous later-outage recovery", func() bool { return current().identity != renewed.identity && ready() })
	assertBodies()
	readStreams("after-later-provider-outage-recovery")
	if !firstReady.IsZero() && !current().recordedFirstReadyAt().Equal(firstReady) {
		t.Fatal("native recovery reopened preparation")
	}
	if _, err = renewed.Overview(t.Context()); err == nil {
		t.Fatal("retired epoch retained inventory access")
	}
	if resumedHTTP.Load() < 4 || podResume.IsZero() || workloadResume.IsZero() {
		t.Fatalf("native reconnect did not use bounded timestamp resume: requests=%d", resumedHTTP.Load())
	}
	stored, storedErr := provider.(storage.EvidenceStore).QueryEvidence(t.Context(), storage.EvidenceScope{OrgID: "synthetic-native", ClusterID: "native", ClusterUID: "native-physical"}, storage.EvidenceQuery{Since: time.Now().Add(-time.Hour), Until: time.Now(), Type: "occurrence", Limit: 100})
	if storedErr != nil || stored.Total < 1 {
		t.Fatalf("autonomous native file capture missing history: total=%d err=%v", stored.Total, storedErr)
	}
	if stsCalls.Load() > 8 || mints.Load() > 10 {
		t.Fatalf("unbounded native retries: sts=%d mints=%d", stsCalls.Load(), mints.Load())
	}
	t.Logf("actual SDK STS HTTP + native EKS signer + real-file producer + Pod/Deployment log HTTP: PASS; sts=%d mints=%d logs=%d; no AWS/EKS endpoint contacted", stsCalls.Load(), mints.Load(), logCalls.Load())
}
