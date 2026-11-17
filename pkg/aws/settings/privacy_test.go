package settings

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

func TestPrivacySelfTestPassesAndCounts24Hours(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 11, 3, 14, 15, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })
	tester := NewSelfTester(store, func() time.Time { return now })

	record, err := tester.Run(context.Background(), "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Passing || record.Leaks24h != 0 {
		t.Fatalf("self-test = %+v, want passing with no leaks", record)
	}
	if !record.LastRunAt.Equal(now) {
		t.Fatalf("last run = %v", record.LastRunAt)
	}

	now = now.Add(25 * time.Hour)
	second, err := tester.Run(context.Background(), "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Leaks24h != 0 {
		t.Fatalf("leaks 24h = %d, want 0", second.Leaks24h)
	}
	stored, err := store.LoadSelfTest("default")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Passing || stored.Leaks24h != 0 {
		t.Fatalf("stored self-test = %+v", stored)
	}
}

func TestPrivacySelfTestHonoursTheAllowlistedCanary(t *testing.T) {
	// A value whose key the allowlist permits is sent on purpose. It must not
	// be reported as a leak, or a valid configuration could never pass.
	for _, key := range []string{"private-notes", "secret-config"} {
		store := testStore(t)
		tester := NewSelfTester(store, time.Now)
		record, err := tester.Run(context.Background(), "default", []string{"app", key})
		if err != nil {
			t.Fatal(err)
		}
		if !record.Passing || record.Leaks24h != 0 {
			t.Fatalf("allowlisting %q reported a leak: %+v", key, record)
		}
	}
}

func TestPrivacySelfTestCatchesNonAllowedLeak(t *testing.T) {
	// The detection half: a value that survives without an allowlist sanction
	// is a leak.
	projection := strings.Join([]string{canaryEnvValue, canaryRequestBody, canaryConfigTagValue}, "|")
	if leaks := countLeaks(projection, []string{"private-notes"}); leaks != 3 {
		t.Fatalf("detected %d leaks, want 3", leaks)
	}
	// Allowlisting the key that carries a canary removes exactly that one.
	if leaks := countLeaks(projection, []string{"secret-config"}); leaks != 2 {
		t.Fatalf("allowlist did not exempt its own value: %d leaks", leaks)
	}
}

func TestPrivacySelfTestUsesConfigWinsAllowlist(t *testing.T) {
	store := testStore(t)
	connector := NewService(store,
		WithConfig(func() config.AWSConnectorConfig {
			return config.AWSConnectorConfig{
				Enable: true,
				Tags:   config.AWSTagsConfig{ValueAllowlist: []string{"app", "private-notes"}},
			}
		}),
		WithClock(time.Now),
	)
	connector.runSelfTest(context.Background())
	record, err := store.LoadSelfTest("default")
	if err != nil {
		t.Fatal(err)
	}
	if !record.Passing || record.Leaks24h != 0 {
		t.Fatalf("config-allowed tag value was treated as a leak: %+v", record)
	}

	// A saved wizard allowlist that permits the other canary must not override
	// the config: the card and the self-test must agree.
	if err := store.SavePrivacy("default", privacyRecord{TagValueAllowlist: []string{"secret-config"}}); err != nil {
		t.Fatal(err)
	}
	connector.runSelfTest(context.Background())
	record, err = store.LoadSelfTest("default")
	if err != nil {
		t.Fatal(err)
	}
	if !record.Passing {
		t.Fatalf("saved allowlist overrode the config allowlist: %+v", record)
	}
}

func TestPrivacyFixtureRedactsEnvKeysAndTrailBodies(t *testing.T) {
	store := testStore(t)
	tester := NewSelfTester(store, time.Now)
	projection, err := tester.project(context.Background(), "default", DefaultTagValueAllowlist())
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range canaries {
		if containsString(projection, canary) {
			t.Fatalf("projection kept canary %q: %s", canary, projection)
		}
	}
	if !containsString(projection, "checkout") {
		t.Fatal("allowlisted tag value was dropped")
	}
}
